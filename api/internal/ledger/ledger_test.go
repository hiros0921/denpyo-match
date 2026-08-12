package ledger

import (
	"strings"
	"testing"

	"golang.org/x/text/encoding/japanese"
)

const bankCSV = `日付,摘要,出金額,入金額,残高
2026/07/31,ﾌﾘｺﾐ ｶ)ﾐﾗｲﾊｲｿｳｻｰﾋﾞｽ,"175,010",,"1,000,000"
2026/08/05,ﾌﾘｺﾐ ｶ)ﾐﾗｲﾊｲｿｳｻｰﾋﾞｽ,"44,132",,"955,868"
2026/07/20,ﾘｿｸ,,"12",955,880
2026/07/15,ﾃﾞﾋﾞｯﾄ ｻﾝﾌﾟﾙﾏｰﾄ,"1,258",,
2026/07/15,ﾃﾞﾋﾞｯﾄ ｻﾝﾌﾟﾙﾏｰﾄ,"1,258",,
`

func TestParseBank(t *testing.T) {
	rows, err := Parse(Bank, []byte(bankCSV))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Fatalf("5行のはずが %d 行", len(rows))
	}
	r := rows[0]
	if r.Amount != 175010 || r.Direction != Out {
		t.Errorf("1行目: %+v", r)
	}
	if r.Description != "ﾌﾘｺﾐ ｶ)ﾐﾗｲﾊｲｿｳｻｰﾋﾞｽ" {
		t.Errorf("摘要は原文のまま持つ: %q", r.Description)
	}
	// 利息は入金
	if rows[2].Direction != In || rows[2].Amount != 12 {
		t.Errorf("入金の行: %+v", rows[2])
	}
	// 同内容の2行は出現順で区別される（正当な重複を捨てないため）
	if rows[3].Occurrence != 0 || rows[4].Occurrence != 1 {
		t.Errorf("出現順: %d, %d", rows[3].Occurrence, rows[4].Occurrence)
	}
	if rows[3].Fingerprint(Bank) == rows[4].Fingerprint(Bank) {
		t.Error("正当な同内容の行が同じ指紋になっている")
	}
}

func TestParseBank_ShiftJIS(t *testing.T) {
	// 銀行のCSVはほぼ Shift_JIS。UTF-8 と間違えて読むと摘要が全部壊れる。
	sjis, err := japanese.ShiftJIS.NewEncoder().String(bankCSV)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := Parse(Bank, []byte(sjis))
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Description != "ﾌﾘｺﾐ ｶ)ﾐﾗｲﾊｲｿｳｻｰﾋﾞｽ" {
		t.Errorf("Shift_JISの摘要が壊れた: %q", rows[0].Description)
	}
}

func TestParseCard(t *testing.T) {
	csv := `利用日,利用店名,利用金額
2026/07/15,ｻﾝﾌﾟﾙﾏｰﾄ ﾌｼﾞﾐﾉｴｷﾏｴ,1258
2026/07/22,ﾓﾃﾞﾙﾀｸｼｰ,4350
2026/07/30,ｶﾃﾞﾝﾃｽﾄﾗﾝﾄﾞ,-15660
`
	rows, err := Parse(Card, []byte(csv))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("3行のはずが %d 行", len(rows))
	}
	if rows[0].Direction != Out || rows[0].Amount != 1258 {
		t.Errorf("カードは出金: %+v", rows[0])
	}
	// マイナスは返金＝入金
	if rows[2].Direction != In || rows[2].Amount != 15660 {
		t.Errorf("返金の行: %+v", rows[2])
	}
}

func TestParseBank_列名の揺れ(t *testing.T) {
	csv := `お取引日,お取引内容,お支払金額,お預入金額
2026-07-31,ﾌﾘｺﾐ ﾃｽﾄ,1000,
`
	rows, err := Parse(Bank, []byte(csv))
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Amount != 1000 {
		t.Errorf("列名の揺れを吸収できていない: %+v", rows[0])
	}
}

func TestParseDate(t *testing.T) {
	for _, s := range []string{"2026/07/15", "2026-07-15", "2026年7月15日", "20260715"} {
		d, err := parseDate(s)
		if err != nil {
			t.Errorf("%q を読めない: %v", s, err)
			continue
		}
		if d.Format("2006-01-02") != "2026-07-15" {
			t.Errorf("%q → %s", s, d.Format("2006-01-02"))
		}
	}
	if _, err := parseDate("繰越"); err == nil {
		t.Error("日付でないものが通った")
	}
}

func TestDecode_不明な文字コード(t *testing.T) {
	// UTF-16 など、想定外のものは明確に断る。黙って壊れた文字列を返さない。
	data := []byte{0xFF, 0xFE, 0x00, 0x00, 0x80, 0x80}
	if _, err := Parse(Bank, data); err == nil {
		t.Error("不明な文字コードが通った")
	}
	_ = strings.TrimSpace("")
}
