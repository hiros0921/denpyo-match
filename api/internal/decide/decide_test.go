package decide

import "testing"

// 境界の扱いを固定する。ここを間違えると、承認すべきでないものを
// 自動承認したり、その逆が起きる。
func TestDecide(t *testing.T) {
	th := Threshold{ID: 42, Upper: 95, Lower: 70}
	cases := []struct {
		score float64
		want  Decision
		note  string
	}{
		{100.0, AutoApprove, "完全一致"},
		{95.0, AutoApprove, "上限ちょうどは自動承認に含む"},
		{94.9, NeedsReview, "上限を下回れば要確認"},
		{70.0, NeedsReview, "下限ちょうどは要確認に含む"},
		{69.9, Reject, "下限を下回れば却下"},
		{0.0, Reject, "スコア0"},
	}
	for _, c := range cases {
		got := Decide(c.score, true, th)
		if got.Decision != c.want {
			t.Errorf("%s: score=%.1f → %v（期待 %v）", c.note, c.score, got.Decision, c.want)
		}
		if got.ThresholdID != th.ID {
			t.Errorf("%s: どの閾値で判定したかが残っていない", c.note)
		}
		if got.Why == "" {
			t.Errorf("%s: 判定の理由が空", c.note)
		}
	}
}

// 候補が無いのに自動承認されないこと。
// 呼び出し側がスコア0を渡しても、ここで止める。
func TestDecideNoCandidate(t *testing.T) {
	th := Threshold{ID: 1, Upper: 0, Lower: 0} // 極端な設定でも
	got := Decide(100, false, th)
	if got.Decision != Reject {
		t.Errorf("候補が無いのに %v になった", got.Decision)
	}
}

// 閾値の検査。設定画面から不正な値が来ても弾く。
func TestThresholdValid(t *testing.T) {
	ok := []Threshold{{Upper: 95, Lower: 70}, {Upper: 100, Lower: 0}, {Upper: 80, Lower: 80}}
	for _, th := range ok {
		if err := th.Valid(); err != nil {
			t.Errorf("正しい閾値が弾かれた %v: %v", th, err)
		}
	}
	ng := []Threshold{{Upper: 50, Lower: 90}, {Upper: 101, Lower: 0}, {Upper: 95, Lower: -1}}
	for _, th := range ng {
		if err := th.Valid(); err == nil {
			t.Errorf("不正な閾値が通ってしまった %v", th)
		}
	}
}

// 実測にもとづく確認。100枚の通し検証（Tesseract / Vision 両方）で
// 出た分布を回帰テストとして固定する。既定値を変えたらここが落ちる。
//
// 第5段階では20枚で「70を境に分離」としていたが、100枚で測り直した。
// 不正解の最大は Tesseract 26.0 / Vision 50.9。
// Vision の正解に 62.9 と 66.4 があり、下限70はこの2枚を却下していた。
func TestDefaultMatchesMeasurement(t *testing.T) {
	if Default.Upper != 95 || Default.Lower != 55 {
		t.Errorf("既定が 95/55 でない（%v/%v）。100枚の実測と食い違う",
			Default.Upper, Default.Lower)
	}
	// 実測で正解だったスコア。要確認までは許すが、却下してはいけない。
	// 62.9 と 66.4 は Vision の正解（旧下限70では却下されていた2枚）。
	for _, s := range []float64{100.0, 91.4, 82.3, 76.3, 74.0, 66.4, 62.9} {
		if Decide(s, true, Default).Decision == Reject {
			t.Errorf("正解だったスコア %.1f が却下された", s)
		}
	}
	// 実測で不正解だったスコア。自動承認してはいけない。
	// 50.9 は Vision の不正解の最大。要確認に回るのは構わない（人が見る）。
	for _, s := range []float64{50.9, 26.0, 23.8, 12.0, 0.0} {
		if Decide(s, true, Default).Decision == AutoApprove {
			t.Errorf("不正解だったスコア %.1f が自動承認された", s)
		}
	}
	// 不正解の最大（50.9）は、候補として人に見せない側に落ちること。
	// 下限をこれより下げるときは、シミュレーションで根拠を見てから。
	if Decide(50.9, true, Default).Decision != Reject {
		t.Errorf("不正解の最大 50.9 が却下されていない")
	}
}
