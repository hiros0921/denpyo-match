// weightsweep は照合スコアの重みを総当たりで比べる。
//
// なぜ要るか
//
//	第9段階で、先頭の1文字が違うだけで先頭一致が 0.000 になり、
//	重み0.2＝20点が丸ごと失われることが分かった。
//	  バハルタ商事87 vs ハルタ商事87
//	    編集距離 0.875 / n-gram 0.857 / 先頭一致 0.000 → 合計 69.5
//	下限70に0.5点届かず却下された。
//
//	第4段階で「日本語の企業名を識別しているのは末尾」と実測しながら、
//	指標は先頭のままだった。末尾一致を足したので、どう配分すべきかを測る。
//
// なぜ通しで測らないか
//
//	パイプライン全体を回すと1回155秒かかる。組み合わせを10通り試すと26分。
//	OCRの読み取り結果はDBに残っているので、そこから先だけを回せば速い。
//	読み取りは変わらないので、比較として正しい。
//
//	go run ./cmd/weightsweep -images ../testdata/samples/scanned
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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hiros0921/denpo-match/api/internal/core"
)

type sample struct {
	name   string
	read   string // OCRが読んだ取引先名
	norm   string
	wantID int64
	cands  []cand
}

type cand struct {
	ID   int64
	Name string
	Vars []string
}

type weights struct{ lev, jac, pre, suf float64 }

func (w weights) String() string {
	return fmt.Sprintf("lev%.1f jac%.1f pre%.1f suf%.1f", w.lev, w.jac, w.pre, w.suf)
}

func main() {
	images := flag.String("images", "../testdata/samples/scanned", "正解JSONの場所")
	bin := flag.String("bin", "/tmp/dmbin", "C++の実行ファイル")
	work := flag.String("work", "", "候補ファイルの置き場所（コンテナと共有）")
	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "DB")
	client := flag.Int64("client", 3, "顧問先ID")
	offset := flag.Int64("offset", 3, "正解IDに足す値")
	upper := flag.Float64("upper", 95, "自動承認の下限")
	lower := flag.Float64("lower", 70, "却下の上限")
	par := flag.Int("par", 4, "同時実行数")
	flag.Parse()

	if *work == "" || *dsn == "" {
		die("-work と DATABASE_URL が要ります")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		die("DBに繋がりません: %v", err)
	}
	defer pool.Close()

	samples := load(ctx, pool, *images, *client, *offset, *bin)
	fmt.Printf("  %d件の読み取りを対象にする（候補が出たものだけ）\n\n", len(samples))

	// 試す組み合わせ。
	// 合計が1になるようにしてある（combine 側で正規化されるので必須ではないが、
	// 見比べるときに分かりやすい）。
	list := []weights{
		{0.5, 0.3, 0.2, 0.0}, // 現行
		{0.5, 0.3, 0.0, 0.2}, // 先頭を末尾に置き換え
		{0.5, 0.3, 0.1, 0.1}, // 半々
		{0.5, 0.5, 0.0, 0.0}, // 先頭も末尾も使わない
		{0.6, 0.4, 0.0, 0.0},
		{0.4, 0.3, 0.0, 0.3}, // 末尾を厚く
		{0.6, 0.2, 0.0, 0.2},
		{0.5, 0.2, 0.0, 0.3},
	}

	fmt.Printf("  %-26s %6s %6s %8s %8s %8s\n",
		"重み", "1位正解", "誤承認", "自動承認", "要確認", "却下")
	fmt.Println("  " + strings.Repeat("-", 72))
	for _, w := range list {
		r := run(ctx, samples, w, *bin, *work, *upper, *lower, *par)
		mark := ""
		if r.wrongAuto > 0 {
			mark = "  ❌"
		}
		fmt.Printf("  %-26s %6d %6d %8d %8d %8d%s\n",
			w.String(), r.top1, r.wrongAuto, r.auto, r.review, r.reject, mark)
	}
	fmt.Println("\n  ※ 1位正解 は候補が出た件数に対する数。読めなかった伝票は含まない")
}

type result struct{ top1, auto, review, reject, wrongAuto int }

func run(ctx context.Context, ss []sample, w weights, bin, work string,
	upper, lower float64, par int) result {
	r := core.New(bin, "")
	var mu sync.Mutex
	var out result
	sem := make(chan struct{}, par)
	var wg sync.WaitGroup

	for i := range ss {
		wg.Add(1)
		go func(s sample) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			recs := make([]map[string]any, 0, len(s.cands))
			for _, c := range s.cands {
				recs = append(recs, map[string]any{
					"id": c.ID, "canonical": c.Name, "variants": c.Vars})
			}
			b, _ := json.Marshal(recs)
			f := filepath.Join(work, fmt.Sprintf("sw_%s.json", s.name))
			if os.WriteFile(f, b, 0o644) != nil {
				return
			}
			defer os.Remove(f)

			m, err := r.MatchWeighted(ctx, f, s.read, len(s.cands),
				w.lev, w.jac, w.pre, w.suf)
			if err != nil || len(m.Results) == 0 {
				mu.Lock()
				out.reject++
				mu.Unlock()
				return
			}
			top := m.Results[0]
			correct := top.ID == s.wantID

			mu.Lock()
			defer mu.Unlock()
			if correct {
				out.top1++
			}
			switch {
			case top.Score >= upper:
				out.auto++
				if !correct {
					out.wrongAuto++
				}
			case top.Score >= lower:
				out.review++
			default:
				out.reject++
			}
		}(ss[i])
	}
	wg.Wait()
	return out
}

// load は「OCRが読んだ名前」と「そのとき出た候補」をDBから集める。
func load(ctx context.Context, pool *pgxpool.Pool, images string,
	client, offset int64, bin string) []sample {

	// 正解（ファイル側）を伝票番号で引けるようにする。
	// dbtest は伝票とファイル名の対応をDBに残していないので、
	// 伝票番号（OCRで読めているもの）で突き合わせる。
	type tr struct {
		PartnerID int64  `json:"partner_id"`
		DocNo     string `json:"doc_no"`
	}
	byDocNo := map[string]int64{}
	files, _ := filepath.Glob(filepath.Join(images, "*.json"))
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var t tr
		if json.Unmarshal(b, &t) == nil && t.DocNo != "" {
			byDocNo[t.DocNo] = t.PartnerID + offset
		}
	}

	rows, err := pool.Query(ctx, `
		SELECT d.id,
		       coalesce(nm.value_text,''), coalesce(no.value_text,'')
		  FROM documents d
		  LEFT JOIN extracted_fields nm
		         ON nm.document_id=d.id AND nm.field_key='partner_name'
		  LEFT JOIN extracted_fields no
		         ON no.document_id=d.id AND no.field_key='doc_no'
		 WHERE d.client_id=$1 ORDER BY d.id`, client)
	if err != nil {
		die("%v", err)
	}
	defer rows.Close()

	type raw struct {
		id    int64
		read  string
		docNo string
	}
	var raws []raw
	for rows.Next() {
		var x raw
		if rows.Scan(&x.id, &x.read, &x.docNo) == nil && x.read != "" {
			raws = append(raws, x)
		}
	}

	// 正規化はまとめて1回で呼ぶ。
	r := core.New(bin, "")
	names := make([]string, 0, len(raws))
	for _, x := range raws {
		names = append(names, x.read)
	}
	norms, err := r.Normalize(ctx, names)
	if err != nil {
		die("正規化に失敗: %v", err)
	}

	var out []sample
	for i, x := range raws {
		want, ok := byDocNo[x.docNo]
		if !ok {
			continue // 伝票番号が読めていないものは突き合わせられない
		}
		cs := candidates(ctx, pool, client, norms[i])
		if len(cs) == 0 {
			continue
		}
		out = append(out, sample{name: fmt.Sprint(x.id), read: x.read,
			norm: norms[i], wantID: want, cands: cs})
	}
	return out
}

func candidates(ctx context.Context, pool *pgxpool.Pool, client int64, norm string) []cand {
	rows, err := pool.Query(ctx, `
		WITH hit AS (
		  SELECT p.id, similarity(p.norm,$2) sim FROM partners p
		   WHERE p.client_id=$1 AND p.norm % $2
		  UNION ALL
		  SELECT a.partner_id, similarity(a.norm,$2) FROM partner_aliases a
		    JOIN partners p ON p.id=a.partner_id
		   WHERE p.client_id=$1 AND a.norm % $2)
		SELECT p.id, p.name,
		       coalesce(array_agg(a.alias) FILTER (WHERE a.alias IS NOT NULL),'{}')
		  FROM hit h JOIN partners p ON p.id=h.id
		  LEFT JOIN partner_aliases a ON a.partner_id=p.id
		 GROUP BY p.id,p.name ORDER BY max(h.sim) DESC, p.id LIMIT 50`,
		client, norm)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []cand
	for rows.Next() {
		var c cand
		if rows.Scan(&c.ID, &c.Name, &c.Vars) == nil {
			out = append(out, c)
		}
	}
	return out
}

func die(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "  "+f+"\n", a...)
	os.Exit(1)
}
