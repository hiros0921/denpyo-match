package settle

import (
	"math"
	"testing"

	"github.com/hiros0921/denpo-match/api/internal/decide"
)

// 上限値の境界を固定する。
//
// 【この試験が生まれた経緯】
// 最初 上限値 = min(80, (閾値 − 60) ÷ 0.40) と書いた。閾値90では 75 になり、
// 合算が 0.40×75 + 60 = 90.0 でちょうど閾値と同値になる。
// 判定は >= なので自動承認を通ってしまい、頭打ちが無効になっていた。
// 閾値95では第1項の80が選ばれるため、この誤りは表面化しなかった。
func TestCapFor_自動承認に届かない(t *testing.T) {
	for _, c := range []struct{ upper, lower float64 }{
		{95, 55}, {92, 55}, {90, 55}, {85, 55}, {70, 40},
	} {
		cap, ok := CapFor(c.upper, c.lower)
		if !ok {
			t.Errorf("上限%.0f/下限%.0f: 成立するはずが成立しなかった", c.upper, c.lower)
			continue
		}
		// 金額・日付が満点でも自動承認に届かないこと
		best := wName*cap + wAmount*100 + wDate*100
		if best >= c.upper {
			t.Errorf("上限%.0f/下限%.0f: 上限値%.1f で合算%.1f。自動承認を通ってしまう",
				c.upper, c.lower, cap, best)
		}
		// 金額・日付が最悪でも却下に落とさないこと
		worst := wName*cap + wAmount*70 + wDate*30
		if worst < c.lower {
			t.Errorf("上限%.0f/下限%.0f: 上限値%.1f で最悪%.1f。却下に落ちる",
				c.upper, c.lower, cap, worst)
		}
	}
}

func TestCapFor_閾値90の再発防止(t *testing.T) {
	// 誤っていた式が返していた値を名指しで弾く
	cap, ok := CapFor(90, 55)
	if !ok {
		t.Fatal("上限90/下限55 は成立するはず")
	}
	if math.Abs(cap-75.0) < 0.01 {
		t.Errorf("上限値が75.0。これは合算がちょうど90.0になり自動承認を通る値")
	}
	if got := wName*cap + 60; got >= 90 {
		t.Errorf("上限値%.1f で合算%.1f。90未満でなければならない", cap, got)
	}
}

func TestCapFor_成立しない設定(t *testing.T) {
	// 天井が床を下回る組み合わせ。頭打ちでは守れないので、
	// 呼び出し側が判定を直接「要確認」にする。
	for _, c := range []struct{ upper, lower float64 }{
		{95, 70}, {80, 55}, {75, 50},
	} {
		if _, ok := CapFor(c.upper, c.lower); ok {
			t.Errorf("上限%.0f/下限%.0f: 成立しないはずが成立した", c.upper, c.lower)
		}
	}
}

func TestCapFor_既定の閾値(t *testing.T) {
	cap, ok := CapFor(decide.Default.Upper, decide.Default.Lower)
	if !ok || math.Abs(cap-80.0) > 0.01 {
		t.Errorf("既定(95/55)での上限値は80。得た値 %.1f ok=%v", cap, ok)
	}
}

func TestCapName(t *testing.T) {
	// 上限を超えるものだけ頭打ち。下は触らない。
	if got := CapName(100, 80, true); got != 80 {
		t.Errorf("100 → %.0f", got)
	}
	if got := CapName(51.4, 80, true); got != 51.4 {
		t.Errorf("上限より低いスコアを触った: %.1f", got)
	}
	// 頭打ちできない設定では何もしない（呼び出し側が要確認にする）
	if got := CapName(100, 0, false); got != 100 {
		t.Errorf("成立しない設定でスコアを変えた: %.0f", got)
	}
}

func TestAmbiguity_理由に必要な情報が入る(t *testing.T) {
	a := Ambiguity{
		Norm:    "ミホンインサツ",
		Matched: []string{"見本印刷株式会社", "見本印刷工業株式会社"},
		Cap:     80,
	}
	w := a.Why()
	for _, must := range []string{"ミホンインサツ", "見本印刷株式会社", "見本印刷工業株式会社", "80"} {
		if !contains(w, must) {
			t.Errorf("理由に %q が無い: %s", must, w)
		}
	}

	f := Ambiguity{Norm: "サンプ", Matched: []string{"A", "B", "C"}, Forced: true}
	if !contains(f.Why(), "人の確認") {
		t.Errorf("頭打ちできないときの理由: %s", f.Why())
	}
}

// 実測した逆転の型を、判定として固定する。
//
// 12桁で切れた 見本印刷工業 の摘要は、見本印刷（別会社）と完全一致して
// 100点になる。頭打ちが無ければ、金額・日付が合った瞬間に自動突合される。
func TestCap_実測した逆転が自動突合されない(t *testing.T) {
	th := decide.Default
	cap, ok := CapFor(th.Upper, th.Lower)
	if !ok {
		t.Fatal("既定の閾値では成立するはず")
	}
	doc := Doc{Total: 100000, IssueDate: day("2026-07-31")}
	tx := Tx{ID: 1, Date: day("2026-08-25"), Amount: 100000, Source: 1,
		NameScore: CapName(100.0, cap, ok)}
	got := Decide([]Scored{Score(doc, tx)}, th)
	if got.Status == StatusAuto {
		t.Errorf("曖昧な摘要が自動突合された: %+v", got)
	}
	if got.Status != StatusReview {
		t.Errorf("要確認になるはず: %v", got.Status)
	}
}

// Decide が Ambiguity.Forced を必ず優先することを確かめる。
//
// 頭打ちできない設定（CapFor が ok=false）では、名前スコアは触っていない。
// つまりスコア単体は上限を超えうる。スコアの比較より先に、
// Forced を見て要確認へ落とさないと、頭打ちが機能しない設定でだけ
// 曖昧な摘要が自動突合されてしまう。
func TestDecide_Forcedはスコアより優先される(t *testing.T) {
	th := decide.Threshold{Upper: 95, Lower: 70} // CapFor(95,70) は ok=false
	if _, ok := CapFor(th.Upper, th.Lower); ok {
		t.Fatal("この閾値では ok=false のはず。前提が崩れている")
	}
	s := Scored{Score: 100.0, // 上限を優に超える
		Ambiguity: &Ambiguity{Norm: "サンプ", Matched: []string{"A", "B"}, Forced: true}}
	got := Decide([]Scored{s}, th)
	if got.Status != StatusReview {
		t.Errorf("Forced な候補がスコア100.0でも自動承認された: %v", got.Status)
	}
	if !contains(got.Why, "人の確認") {
		t.Errorf("理由に曖昧さの説明が無い: %s", got.Why)
	}
}

// 頭打ちで守れた場合（Forced=false）は、通常どおりスコアで判定し、
// 理由に曖昧さの注記だけを添える。
func TestDecide_頭打ちできた場合は通常どおりスコアで判定(t *testing.T) {
	th := decide.Default // 95/55、CapFor は ok=true
	cap, ok := CapFor(th.Upper, th.Lower)
	if !ok {
		t.Fatal("既定の閾値では ok=true のはず")
	}
	doc := Doc{Total: 100000, IssueDate: day("2026-07-31")}
	tx := Tx{Date: day("2026-08-25"), Amount: 100000, Source: 1, NameScore: cap}
	s := Score(doc, tx)
	s.Ambiguity = &Ambiguity{Norm: "ミホンインサツ",
		Matched: []string{"見本印刷株式会社", "見本印刷工業株式会社"}, Cap: cap, Forced: false}

	got := Decide([]Scored{s}, th)
	if got.Status != StatusReview {
		t.Errorf("頭打ち後のスコアで要確認になるはずが %v", got.Status)
	}
	if !contains(got.Why, "見本印刷株式会社") {
		t.Errorf("理由に前方一致した相手が出ていない: %s", got.Why)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
