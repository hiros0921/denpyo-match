// Package invoice はインボイス制度の登録番号を検査する。
//
// なぜこれを作るか
//
//	2023年10月から、受け取った請求書・領収書に「T＋13桁」の登録番号が
//	正しく記載されているかを確かめる必要が出た。番号が無効だと
//	仕入税額控除が取れない。つまり払う税額が変わる。
//
//	この作業は、
//	  受領した帳票にしか発生しない
//	  全件やらなければいけない
//	  純粋に機械的で、人がやる意味がまったく無い
//	  間違えると税額に直結する
//	という条件が揃っている。会計事務所に持っていく機能として、
//	「OCRで読み取ります」より刺さるのはこちらだと考えている。
//
// 【重要】検査数字が合わないことを「無効」と言い切らない。
//
//	登録番号は2種類ある。
//	  法人番号を持つ課税事業者   T + 法人番号13桁
//	  それ以外（個人事業者など） T + 新たに付番された13桁
//	後者は法人番号ではない。国税庁は、この番号が法人番号と同じ
//	検査数字の規則に従うとは公表していない。
//
//	だから検査数字の不一致で「無効」と断定すると、
//	個人事業者の正しい番号を弾く恐れがある。仕入税額控除の判断を
//	誤らせることになるので、ここは「要確認」に倒す。
//	最終的な有効・無効は国税庁の公表システムが答える。
//
// 【重要】検査数字を通っても「正しい」ではない。
//
//	9で割った余りを使う計算なので、9だけ違う変化＝0と9の取り違えは
//	素通りする。実測（1文字だけ変えた117通り）: 115通りは弾いたが、
//	0→9 と 9→0 の2通りは通ってしまった。
//	「登録番号を全件自動照合します」と言えるのは、
//	国税庁の公表システムに問い合わせて初めて。形式検査はその前段。
package invoice

import (
	"strings"
)

// Status は登録番号の状態。数値は DB に入るので、値を変えない。
type Status int16

const (
	StatusMissing    Status = 1 // 記載なし
	StatusBadFormat  Status = 2 // 形式が違う（T以外・桁数違い・数字以外）
	StatusBadCheck   Status = 3 // 検査数字が合わない
	StatusFormatOK   Status = 4 // 形式は正しい。実在は未確認
	StatusRegistered Status = 5 // 国税庁で実在を確認した
	StatusNotFound   Status = 6 // 国税庁に無い
)

func (s Status) String() string {
	switch s {
	case StatusMissing:
		return "記載なし"
	case StatusBadFormat:
		return "形式が違う"
	case StatusBadCheck:
		return "検査数字が合わない"
	case StatusFormatOK:
		return "形式は正しい（実在は未確認）"
	case StatusRegistered:
		return "登録あり"
	case StatusNotFound:
		return "登録が見つからない"
	}
	return "不明"
}

// NeedsAttention は人が見るべきかを返す。
//
// 一覧の既定の絞り込みに使う。全件を見せても現場は読まない。
// 「記載なし」も含める。免税事業者からの請求では正常だが、
// 課税事業者からの請求で抜けているなら控除が取れない。
// どちらかは帳票からは分からないので、人に渡す。
func (s Status) NeedsAttention() bool {
	return s != StatusFormatOK && s != StatusRegistered
}

// Result は1件の検査結果。
type Result struct {
	// 取り出した番号。Tを含む14文字。取り出せなければ空。
	RegNo  string
	Status Status
	// なぜその判定になったか。画面にそのまま出す。
	// 「無効です」だけでは、現場は先方に何を問い合わせればよいか分からない。
	Why string
}

// Normalize は表記のゆれを落とす。
//
// 全角の T と数字、間の空白・ハイフンを取り除く。
// 人が手で入力した番号も通るようにするため。
func Normalize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == 'T' || r == 't' || r == 'Ｔ' || r == 'ｔ':
			b.WriteByte('T')
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= '０' && r <= '９':
			b.WriteRune(r - '０' + '0')
		default:
			// 空白・ハイフン・その他は落とす
		}
	}
	return b.String()
}

// CheckDigit は法人番号13桁の検査数字を確かめる。
//
//	検査用数字 = 9 −（Σ(Pn × Qn) を 9 で割った余り）
//	  Pn  基礎番号（下位12桁）の下から n 桁目
//	  Qn  n が奇数なら 1、偶数なら 2
//	検査用数字は最上位の1桁。
//
// 13桁でない、数字以外が混ざっている場合は false。
func CheckDigit(d string) bool {
	if len(d) != 13 {
		return false
	}
	for i := 0; i < 13; i++ {
		if d[i] < '0' || d[i] > '9' {
			return false
		}
	}
	// d[0] が検査用数字、d[1..12] が基礎番号。
	// 基礎番号の「下から n 桁目」は d[13-n]。
	sum := 0
	for n := 1; n <= 12; n++ {
		p := int(d[13-n] - '0')
		q := 1
		if n%2 == 0 {
			q = 2
		}
		sum += p * q
	}
	return int(d[0]-'0') == 9-(sum%9)
}

// Evaluate は取り出した文字列を検査する。国税庁には問い合わせない。
//
// 通信を伴わないので、ここだけで単体テストが書ける。
// 実在の確認は Lookup に分けてある。
func Evaluate(raw string) Result {
	n := Normalize(raw)
	if n == "" {
		return Result{Status: StatusMissing,
			Why: "登録番号が読み取れませんでした。記載が無いか、読み取りに失敗しています"}
	}
	if !strings.HasPrefix(n, "T") {
		return Result{RegNo: n, Status: StatusBadFormat,
			Why: "登録番号は「T」で始まります"}
	}
	d := n[1:]
	if len(d) != 13 {
		return Result{RegNo: n, Status: StatusBadFormat,
			Why: "「T」のあとは数字13桁です。読み取れたのは" +
				itoa(len(d)) + "桁でした"}
	}
	if !CheckDigit(d) {
		// 【重要】ここで「無効」と言わない。
		// 個人事業者に付される13桁は法人番号ではないため、
		// この検査に通らない番号が正しいことがありうる。
		return Result{RegNo: n, Status: StatusBadCheck,
			Why: "法人番号としての検査数字が合いません。" +
				"読み取りの誤りか、個人事業者の番号である可能性があります"}
	}
	return Result{RegNo: n, Status: StatusFormatOK,
		Why: "形式は正しい番号です。実在の確認は国税庁の公表システムで行います"}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
