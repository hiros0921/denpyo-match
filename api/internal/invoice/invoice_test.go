package invoice

import "testing"

// 実物のレシート10枚に印字されていた登録番号。
//
// この10件は、こちらとは別に作られたもの（諏訪さんが法人番号の
// 検査数字を通して生成した）。同じ計算を2通り書いて一致するなら、
// 実装が仕様どおりである確からしさが高い。
// 自分で作った値を自分で検算しても、何も確かめたことにならない。
var realReceipts = []string{
	"T9234567890123", // サンプルマート
	"T1345678901234", // 見本石油サービス
	"T2901234567890",
	"T3456789012345",
	"T4567890123456",
	"T6012345678901",
	"T6678901234567",
	"T7123456789012", // サンプル行政書士事務所
	"T7789012345678",
	"T9890123456789",
}

func TestCheckDigit_実物のレシート(t *testing.T) {
	for _, s := range realReceipts {
		if !CheckDigit(s[1:]) {
			t.Errorf("%s は正しいはずなのに検査数字が合わないと判定した", s)
		}
	}
}

func TestCheckDigit_1桁の読み違いをどこまで見抜けるか(t *testing.T) {
	// 検査数字の役目は「1文字の読み違いを見つけること」。
	// ただし全部は見つけられない。9で割った余りを使う計算なので、
	// 9だけ違う変化＝0と9の取り違えは、余りが変わらず素通りする。
	//
	//   Q=1 の桁  差が9の倍数なら素通り → 0↔9
	//   Q=2 の桁  2×差が9の倍数なら素通り → 差が9の倍数 → 0↔9
	//
	// これは実装の不足ではなく、この検査方式の性質。
	// だから検査数字だけで「全件照合しました」とは言えない。
	// 実在の確認は国税庁の公表システムに問い合わせて初めて成立する。
	base := "T9234567890123"
	var missed []string
	total, found := 0, 0
	for i := 1; i < len(base); i++ {
		for c := byte('0'); c <= '9'; c++ {
			if base[i] == c {
				continue
			}
			m := []byte(base)
			m[i] = c
			total++
			if !CheckDigit(string(m[1:])) {
				found++
			} else {
				missed = append(missed, string(m))
			}
		}
	}
	// 基礎番号12桁のうち 0 が1つ、9 が1つ。その2通りだけ通る。
	if total != 117 || found != 115 {
		t.Errorf("1文字違いを弾けたのは %d/%d 件。素通りしたもの: %v",
			found, total, missed)
	}
	for _, m := range missed {
		// 素通りするのは 0↔9 の取り違えだけであることを確かめる。
		i := 0
		for ; i < len(base); i++ {
			if base[i] != m[i] {
				break
			}
		}
		a, b := base[i], m[i]
		if !((a == '0' && b == '9') || (a == '9' && b == '0')) {
			t.Errorf("0と9の取り違え以外が素通りした: %s（%c→%c）", m, a, b)
		}
	}
}

func TestCheckDigit_形式(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"923456789012", false},   // 12桁
		{"92345678901234", false}, // 14桁
		{"923456789012A", false},  // 数字以外
	}
	for _, c := range cases {
		if got := CheckDigit(c.in); got != c.want {
			t.Errorf("CheckDigit(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestNormalize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"T9234567890123", "T9234567890123"},
		{"Ｔ９２３４５６７８９０１２３", "T9234567890123"}, // 全角
		{"登録番号 T9234-5678-90123", "T9234567890123"},
		{"t 9234 5678 90123", "T9234567890123"},
		{"", ""},
	}
	for _, c := range cases {
		if got := Normalize(c.in); got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEvaluate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Status
	}{
		{"記載なし", "", StatusMissing},
		{"読めたが空", "   ", StatusMissing},
		{"正しい番号", "T9234567890123", StatusFormatOK},
		{"全角でも通る", "Ｔ９２３４５６７８９０１２３", StatusFormatOK},
		{"桁が足りない", "T923456789012", StatusBadFormat},
		{"桁が多い", "T92345678901234", StatusBadFormat},
		{"Tが無い", "9234567890123", StatusBadFormat},
		// 検査数字だけを変えた番号。「無効」ではなく「要確認」。
		// 個人事業者の番号は法人番号ではないので、断定してはいけない。
		{"検査数字が違う", "T1234567890123", StatusBadCheck},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Evaluate(c.in)
			if got.Status != c.want {
				t.Errorf("Evaluate(%q).Status = %v(%s), want %v(%s)",
					c.in, got.Status, got.Status, c.want, c.want)
			}
			if got.Why == "" {
				t.Error("理由が空。画面に出す文言なので必ず要る")
			}
		})
	}
}

func TestEvaluate_桁数を理由に書く(t *testing.T) {
	// 「形式が違います」だけでは、先方に何を問い合わせればよいか分からない。
	r := Evaluate("T923456789012")
	if !contains(r.Why, "12桁") {
		t.Errorf("理由に読み取れた桁数が無い: %q", r.Why)
	}
}

func TestNeedsAttention(t *testing.T) {
	cases := []struct {
		s    Status
		want bool
	}{
		{StatusMissing, true},
		{StatusBadFormat, true},
		{StatusBadCheck, true},
		{StatusNotFound, true},
		{StatusFormatOK, false},
		{StatusRegistered, false},
	}
	for _, c := range cases {
		if got := c.s.NeedsAttention(); got != c.want {
			t.Errorf("%s.NeedsAttention() = %v, want %v", c.s, got, c.want)
		}
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
