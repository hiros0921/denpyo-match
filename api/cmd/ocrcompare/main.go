// ocrcompare は「前処理をかけてからOCR」と「元画像を直接OCR」を突き合わせる。
//
// なぜ要るか
//
//	100枚の通し検証で誤承認が1件出た。原因を辿ると、前処理が
//	取引先名の末尾（数字3桁）を消していた。
//
//	  元画像     バルタ商会284   ← 284 が残る
//	  前処理後   ハルタ商会      ← 284 が消え、別の実在する会社の名前になる
//
//	消えた結果が「実在する別の会社の名前と完全一致」なので、
//	照合スコアは100になり、閾値では止まらない。
//
//	第5段階でも「前処理はOCRの精度を上げなかった」と記録していたが、
//	そのまま前処理後の画像を渡し続けていた。上げないだけでなく
//	下げている可能性を、100枚で測り直す。
//
//	go run ./cmd/ocrcompare -images ../testdata/samples/scanned
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hiros0921/denpo-match/api/internal/core"
)

type degradation struct {
	Preset string `json:"preset"`
}

type truth struct {
	Degradation      degradation `json:"degradation"`
	PartnerID        int64       `json:"partner_id"`
	PartnerName      string      `json:"partner_name"`
	PartnerCanonical string      `json:"partner_canonical"`
	DocNo            string      `json:"doc_no"`
	TotalWithTax     int64       `json:"total_with_tax"`
	IssueDate        string      `json:"issue_date"`
}

type row struct {
	name string
	tr   truth
	// それぞれの読み取り結果
	pre, raw fieldSet
}

type fieldSet struct {
	// 正規化した名前と、それで候補が出るか。
	// 「読めた」ではなく「照合に届いた」で測る。
	// 読めても候補ゼロなら、パイプラインでは却下になる。
	norm    string
	nCand   int
	hasAns  bool
	partner string
	docNo   string
	total   string
	date    string
	conf    float64
	err     string
}

func main() {
	images := flag.String("images", "../testdata/samples/scanned", "画像と正解JSON")
	bin := flag.String("bin", "/tmp/dmbin", "C++の実行ファイル")
	work := flag.String("work", "", "前処理の出力先（コンテナと共有される場所）")
	par := flag.Int("par", 4, "同時に走らせる数")
	limit := flag.Int("limit", 0, "先頭から何枚まで")
	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "DB（候補生成を試すのに使う）")
	flag.Parse()

	if *work == "" {
		die("-work に作業フォルダを指定してください")
	}
	files, _ := filepath.Glob(filepath.Join(*images, "*.json"))
	sort.Strings(files)
	if *limit > 0 && *limit < len(files) {
		files = files[:*limit]
	}
	if len(files) == 0 {
		die("正解JSONがありません: %s", *images)
	}

	r := core.New(*bin, "")
	ctx := context.Background()

	rows := make([]row, len(files))
	sem := make(chan struct{}, *par)
	var wg sync.WaitGroup
	t0 := time.Now()

	for i, tf := range files {
		wg.Add(1)
		go func(i int, tf string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			name := strings.TrimSuffix(filepath.Base(tf), ".json")
			var tr truth
			if b, err := os.ReadFile(tf); err == nil {
				_ = json.Unmarshal(b, &tr)
			}
			img := filepath.Join(*images, name+".png")

			// ① 前処理してからOCR（いまのパイプラインと同じ）
			out := filepath.Join(*work, "cmp_"+name+".png")
			var pre fieldSet
			if _, err := r.Preprocess(ctx, img, out); err != nil {
				pre.err = err.Error()
			} else if o, err := r.Ocr(ctx, out); err != nil {
				pre.err = err.Error()
			} else {
				pre = pick(o)
			}

			// ② 元画像を直接OCR
			var rawf fieldSet
			if o, err := r.Ocr(ctx, img); err != nil {
				rawf.err = err.Error()
			} else {
				rawf = pick(o)
			}

			rows[i] = row{name: name, tr: tr, pre: pre, raw: rawf}
		}(i, tf)
	}
	wg.Wait()
	fmt.Printf("\n  %d枚 %.1f秒\n\n", len(rows), time.Since(t0).Seconds())

	// 正規化と候補生成は、まとめて後から測る。
	// 1枚ずつ C++ を起動すると100回の起動になる。
	fill(rows, *bin, *dsn)
	report(rows)
}

// fill は読み取り結果を正規化し、DBで候補が出るかを調べる。
func fill(rows []row, bin, dsn string) {
	if dsn == "" {
		return
	}
	r := core.New(bin, "")
	ctx := context.Background()
	var names []string
	for _, x := range rows {
		names = append(names, x.pre.partner, x.raw.partner)
	}
	norms, err := r.Normalize(ctx, names)
	if err != nil {
		fmt.Println("  正規化に失敗:", err)
		return
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return
	}
	defer pool.Close()

	look := func(norm string, wantID int64) (int, bool) {
		if norm == "" {
			return 0, false
		}
		rs, err := pool.Query(ctx, `
			SELECT p.id FROM partners p
			 WHERE p.client_id = 3 AND p.norm % $1
			 ORDER BY similarity(p.norm,$1) DESC LIMIT 50`, norm)
		if err != nil {
			return 0, false
		}
		defer rs.Close()
		n, found := 0, false
		for rs.Next() {
			var id int64
			if rs.Scan(&id) == nil {
				n++
				if id == wantID {
					found = true
				}
			}
		}
		return n, found
	}
	for i := range rows {
		want := rows[i].tr.PartnerID + 3
		rows[i].pre.norm = norms[i*2]
		rows[i].raw.norm = norms[i*2+1]
		rows[i].pre.nCand, rows[i].pre.hasAns = look(rows[i].pre.norm, want)
		rows[i].raw.nCand, rows[i].raw.hasAns = look(rows[i].raw.norm, want)
	}
}

func pick(o *core.OcrResult) fieldSet {
	f := fieldSet{}
	if v, ok := o.Fields["partner_name"]; ok {
		f.partner, f.conf = v.Value, v.Confidence
	}
	f.docNo = o.Fields["doc_no"].Value
	f.total = o.Fields["total"].Value
	f.date = o.Fields["issue_date"].Value
	return f
}

// 取引先名は完全一致では測れない（OCRの誤読が必ず混ざる）。
// 「正解の名前に含まれる数字が残っているか」を見る。
// 末尾が落ちたかどうかが、今回の問題の本質だから。
func digitsOf(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func report(rows []row) {
	type stat struct {
		exact, digitsKept, digitsLost, empty int
		docNoOK, totalOK, dateOK             int
		confSum                              float64
		n                                    int
	}
	var pre, raw stat

	count := func(s *stat, f fieldSet, tr truth) {
		s.n++
		s.confSum += f.conf
		if f.partner == "" {
			s.empty++
		}
		if f.partner == tr.PartnerName {
			s.exact++
		}
		// 正解の表記に数字があるとき、それが残っているか
		want := digitsOf(tr.PartnerName)
		if want != "" {
			if strings.Contains(digitsOf(f.partner), want) {
				s.digitsKept++
			} else {
				s.digitsLost++
			}
		}
		if f.docNo == tr.DocNo {
			s.docNoOK++
		}
		if f.total == fmt.Sprint(tr.TotalWithTax) {
			s.totalOK++
		}
		// 正解は「2026年12月12日」、抽出は「2026-12-12」。
		// 形をそろえてから比べる。そろえずに測って 0% と誤報した。
		if f.date != "" && f.date == isoDate(tr.IssueDate) {
			s.dateOK++
		}
	}
	for _, r := range rows {
		count(&pre, r.pre, r.tr)
		count(&raw, r.raw, r.tr)
	}

	line := func(label string, a, b int, n int) {
		fmt.Printf("  %-28s %5d (%5.1f%%)   %5d (%5.1f%%)\n", label,
			a, pct(a, n), b, pct(b, n))
	}
	fmt.Printf("  %-28s %-16s %-16s\n", "", "前処理してからOCR", "元画像を直接OCR")
	fmt.Println("  " + strings.Repeat("-", 64))
	line("取引先名が正解と完全一致", pre.exact, raw.exact, pre.n)
	line("取引先名が空", pre.empty, raw.empty, pre.n)
	fmt.Println()
	fmt.Println("  ── 末尾の数字が残っているか（今回の問題の本質）──")
	line("数字が残った", pre.digitsKept, raw.digitsKept, pre.digitsKept+pre.digitsLost)
	line("数字が落ちた", pre.digitsLost, raw.digitsLost, pre.digitsKept+pre.digitsLost)
	fmt.Println()
	fmt.Println("  ── 他の項目 ──")
	line("伝票番号が一致", pre.docNoOK, raw.docNoOK, pre.n)
	line("金額が一致", pre.totalOK, raw.totalOK, pre.n)
	line("日付が一致", pre.dateOK, raw.dateOK, pre.n)
	fmt.Printf("\n  抽出の信頼度（平均）        %14.3f %16.3f\n",
		pre.confSum/float64(pre.n), raw.confSum/float64(raw.n))

	// 数字が落ちた枚数のうち、落ちた結果が「別の実在する会社」になるものが危険。
	fmt.Println("\n  ── 前処理で数字が落ちた例（上位5件）──")
	shown := 0
	for _, r := range rows {
		wantD := digitsOf(r.tr.PartnerName)
		if wantD == "" {
			continue
		}
		lost := !strings.Contains(digitsOf(r.pre.partner), wantD)
		kept := strings.Contains(digitsOf(r.raw.partner), wantD)
		if lost && kept {
			fmt.Printf("    %-11s 正解 %-18s 前処理後 %-16s 元画像 %s\n",
				r.name, r.tr.PartnerName, r.pre.partner, r.raw.partner)
			shown++
			if shown >= 5 {
				break
			}
		}
	}
	if shown == 0 {
		fmt.Println("    なし")
	}

	// ── ここからが本題 ──
	//
	// 「読めた」ではなく「照合に届いた」で測る。
	// 読めても候補が出なければ、パイプラインでは却下になる。
	fmt.Println("\n  ── 候補生成に正解が含まれたか（劣化の強さ別）──")
	fmt.Printf("  %-8s %6s   %-14s %-14s %s\n",
		"劣化", "枚数", "前処理してから", "元画像", "どちらかで届く")
	fmt.Println("  " + strings.Repeat("-", 62))
	presets := []string{"light", "normal", "heavy"}
	var tp, tr, te, tn int
	for _, ps := range presets {
		var n, okPre, okRaw, okEither int
		for _, x := range rows {
			if x.tr.Degradation.Preset != ps {
				continue
			}
			n++
			if x.pre.hasAns {
				okPre++
			}
			if x.raw.hasAns {
				okRaw++
			}
			if x.pre.hasAns || x.raw.hasAns {
				okEither++
			}
		}
		if n == 0 {
			continue
		}
		fmt.Printf("  %-8s %6d   %5d (%4.0f%%)   %5d (%4.0f%%)   %5d (%4.0f%%)\n",
			ps, n, okPre, pct(okPre, n), okRaw, pct(okRaw, n), okEither, pct(okEither, n))
		tn += n
		tp += okPre
		tr += okRaw
		te += okEither
	}
	fmt.Println("  " + strings.Repeat("-", 62))
	fmt.Printf("  %-8s %6d   %5d (%4.0f%%)   %5d (%4.0f%%)   %5d (%4.0f%%)\n",
		"合計", tn, tp, pct(tp, tn), tr, pct(tr, tn), te, pct(te, tn))
	fmt.Printf("\n  両方を試して良いほうを採ると、%d枚 → %d枚（+%d）\n", tr, te, te-tr)
}

// 和風表記を ISO に直す。2026年12月12日 → 2026-12-12
func isoDate(s string) string {
	r := strings.NewReplacer("年", "-", "月", "-", "日", "")
	p := strings.Split(r.Replace(s), "-")
	if len(p) < 3 {
		return s
	}
	return fmt.Sprintf("%s-%02s-%02s", p[0],
		strings.TrimLeft(p[1], "0"), strings.TrimLeft(p[2], "0"))
}

func pct(a, n int) float64 {
	if n == 0 {
		return 0
	}
	return float64(a) * 100 / float64(n)
}

func die(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "  "+f+"\n", a...)
	os.Exit(1)
}
