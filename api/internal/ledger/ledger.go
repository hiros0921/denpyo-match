// Package ledger は入出金（銀行明細・カード明細）の取り込みを扱う。
//
// 取り込み口はアダプタで分け、中は transactions 1本に寄せる。
// 銀行CSVも、カード明細も、将来の仕訳データも、全部同じ形（Row）にして
// 返す。こうしておけば、突合ロジックはアダプタが増えても1本のまま。
//
// 【重要】摘要の正規化はここでやらない。
// 正規化の実装は C++（dm_match --normalize --bank）の1箇所だけ。
// Go に書き直すと、partners.norm を作った実装といつか必ずずれ、
// 候補生成が静かに当たらなくなる。
package ledger

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/transform"
)

type SourceType int16

const (
	Bank    SourceType = 1
	Card    SourceType = 2
	Journal SourceType = 3 // 将来。会計ソフトの仕訳
)

func (t SourceType) String() string {
	switch t {
	case Bank:
		return "銀行"
	case Card:
		return "カード"
	case Journal:
		return "仕訳"
	}
	return "不明"
}

type Direction int16

const (
	In  Direction = 1 // 入金
	Out Direction = 2 // 出金
)

// Row は取り込んだ1行。アダプタの出力はすべてこの形。
type Row struct {
	Date        time.Time
	Amount      int64
	Direction   Direction
	Description string            // 摘要。原文のまま
	Raw         map[string]string // 元の行。列名→値
	// 同じ内容の行がファイル内で何番目か（0から）。
	// 指紋の材料。同じ日に同じ店で同額を2回払うのは正当なので、
	// 内容だけで指紋を作ると2回目を重複として捨ててしまう。
	Occurrence int
}

// Fingerprint は期間の重なった再取り込みを見つけるための指紋。
//
// 同じ物理取引は、別のファイルに入っていても同じ指紋になる。
// 正当な同内容の行は Occurrence で区別される。
// ファイルの切り出し期間が重なっていても、同じ取引の Occurrence は
// 同じ順序で数えられるので一致する。
func (r Row) Fingerprint(src SourceType) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d|%s|%d|%d|%s|%d",
		src, r.Date.Format("2006-01-02"), r.Amount, r.Direction,
		r.Description, r.Occurrence)
	return hex.EncodeToString(h.Sum(nil))
}

// Parse はCSVを読み、行に落とす。
//
// 文字コードは自動で判定する。銀行のCSVはほぼ Shift_JIS で、
// UTF-8 だと思って読むと摘要が全部壊れる。壊れた摘要は正規化しても
// 直らないので、ここで間違えると後段のすべてが無駄になる。
func Parse(src SourceType, data []byte) ([]Row, error) {
	text, err := decode(data)
	if err != nil {
		return nil, err
	}
	rd := csv.NewReader(strings.NewReader(text))
	rd.FieldsPerRecord = -1 // 銀行CSVは行によって列数が揺れることがある
	records, err := rd.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("CSVとして読めません: %w", err)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("空のファイルです")
	}

	var rows []Row
	switch src {
	case Bank:
		rows, err = parseBank(records)
	case Card:
		rows, err = parseCard(records)
	default:
		return nil, fmt.Errorf("この種類の取り込みはまだありません: %s", src)
	}
	if err != nil {
		return nil, err
	}

	// 出現順を数える。指紋の材料。
	seen := map[string]int{}
	for i := range rows {
		k := fmt.Sprintf("%s|%d|%d|%s",
			rows[i].Date.Format("2006-01-02"), rows[i].Amount,
			rows[i].Direction, rows[i].Description)
		rows[i].Occurrence = seen[k]
		seen[k]++
	}
	return rows, nil
}

// decode は Shift_JIS か UTF-8 かを判定して UTF-8 に揃える。
//
// 判定の順序に意味がある。UTF-8 として正しく読めるならそのまま使う。
// Shift_JIS のバイト列は偶然 UTF-8 として不正になることが多いので、
// 「UTF-8として不正なら Shift_JIS とみなす」で実用上足りる。
func decode(data []byte) (string, error) {
	// BOM は落とす。Excel が付ける。
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	if utf8.Valid(data) {
		return string(data), nil
	}
	out, err := io.ReadAll(transform.NewReader(
		bytes.NewReader(data), japanese.ShiftJIS.NewDecoder()))
	if err != nil {
		return "", fmt.Errorf("文字コードを判定できません（UTF-8でもShift_JISでもない）: %w", err)
	}
	return string(out), nil
}

// ── 銀行CSV ──
//
// 想定する形:
//   日付, 摘要, 出金額, 入金額, 残高
// 列名は銀行ごとに揺れる（お取引日/取引日/日付、お預入金額/入金額…）ので、
// 部分一致で探す。順序にも頼らない。
func parseBank(records [][]string) ([]Row, error) {
	header := records[0]
	di := findCol(header, "日付", "取引日", "年月日")
	mi := findCol(header, "摘要", "内容", "取引内容", "備考")
	oi := findCol(header, "出金", "支払", "引出", "お支払")
	ii := findCol(header, "入金", "預入", "お預入")
	if di < 0 || mi < 0 || (oi < 0 && ii < 0) {
		return nil, fmt.Errorf(
			"銀行明細の列が見つかりません（日付・摘要・出金/入金が要ります）。先頭行: %v",
			header)
	}

	var out []Row
	for n, rec := range records[1:] {
		get := func(i int) string {
			if i < 0 || i >= len(rec) {
				return ""
			}
			return strings.TrimSpace(rec[i])
		}
		d, err := parseDate(get(di))
		if err != nil {
			return nil, fmt.Errorf("%d行目の日付を読めません: %q", n+2, get(di))
		}
		raw := rawMap(header, rec)

		if v := parseAmount(get(oi)); v > 0 {
			out = append(out, Row{Date: d, Amount: v, Direction: Out,
				Description: get(mi), Raw: raw})
		} else if v := parseAmount(get(ii)); v > 0 {
			out = append(out, Row{Date: d, Amount: v, Direction: In,
				Description: get(mi), Raw: raw})
		}
		// 出金も入金も0の行（繰越など）は取引ではないので読み飛ばす
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("読める行がありませんでした")
	}
	return out, nil
}

// ── カード明細 ──
//
// 想定する形:
//   利用日, 利用店名, 利用金額
// カードはすべて出金。返金（マイナス額）は入金として扱う。
func parseCard(records [][]string) ([]Row, error) {
	header := records[0]
	di := findCol(header, "利用日", "日付", "ご利用日")
	mi := findCol(header, "店名", "利用店", "ご利用先", "摘要", "内容")
	ai := findCol(header, "金額", "利用金額", "ご利用金額")
	if di < 0 || mi < 0 || ai < 0 {
		return nil, fmt.Errorf(
			"カード明細の列が見つかりません（利用日・店名・金額が要ります）。先頭行: %v",
			header)
	}

	var out []Row
	for n, rec := range records[1:] {
		get := func(i int) string {
			if i < 0 || i >= len(rec) {
				return ""
			}
			return strings.TrimSpace(rec[i])
		}
		d, err := parseDate(get(di))
		if err != nil {
			return nil, fmt.Errorf("%d行目の日付を読めません: %q", n+2, get(di))
		}
		amt := parseSignedAmount(get(ai))
		if amt == 0 {
			continue
		}
		dir := Out
		if amt < 0 {
			dir, amt = In, -amt
		}
		out = append(out, Row{Date: d, Amount: amt, Direction: dir,
			Description: get(mi), Raw: rawMap(header, rec)})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("読める行がありませんでした")
	}
	return out, nil
}

// ── 小さな道具 ──

func findCol(header []string, keys ...string) int {
	for i, h := range header {
		h = strings.TrimSpace(h)
		for _, k := range keys {
			if strings.Contains(h, k) {
				return i
			}
		}
	}
	return -1
}

func rawMap(header, rec []string) map[string]string {
	m := make(map[string]string, len(header))
	for i, h := range header {
		if i < len(rec) {
			m[strings.TrimSpace(h)] = rec[i]
		}
	}
	return m
}

// parseDate は銀行・カードで見かける形を読む。
// 2026/07/15, 2026-07-15, 2026年7月15日, 20260715
func parseDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, r := range []struct{ from, to string }{
		{"年", "/"}, {"月", "/"}, {"日", ""}, {"-", "/"}, {".", "/"},
	} {
		s = strings.ReplaceAll(s, r.from, r.to)
	}
	for _, layout := range []string{"2006/01/02", "2006/1/2", "20060102"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("日付ではありません: %q", s)
}

// parseAmount は "1,234" "¥1,234" を読む。空・非数値は0。
func parseAmount(s string) int64 {
	v := parseSignedAmount(s)
	if v < 0 {
		return 0
	}
	return v
}

func parseSignedAmount(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	neg := strings.HasPrefix(s, "-") || strings.HasPrefix(s, "▲")
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
		// 全角数字も来る
		if r >= '０' && r <= '９' {
			b.WriteRune(r - '０' + '0')
		}
	}
	if b.Len() == 0 {
		return 0
	}
	n, err := strconv.ParseInt(b.String(), 10, 64)
	if err != nil {
		return 0
	}
	if neg {
		return -n
	}
	return n
}
