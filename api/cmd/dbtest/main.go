// dbtest は REST API 経由で通し検証を行う。
//
// pipetest との違い
//
//	pipetest  … Go から C++ を直接呼ぶ。DBを使わない。工程の確認用。
//	dbtest    … 実際の経路を通す。アップロード → 待ち行列 → ワーカー
//	            → C++ → DB → 状態照会。本番と同じ道を通る。
//
// 「動いている」と言えるのは、こちらが通ってからになる。
//
//	go run ./cmd/dbtest -api http://localhost:58080 -client 3 \
//	  -images ../testdata/samples/scanned -offset 3
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type truth struct {
	PartnerID    int64  `json:"partner_id"`
	DocNo        string `json:"doc_no"`
	TotalWithTax int64  `json:"total_with_tax"`
}

type docView struct {
	ID       int64  `json:"id"`
	Status   int16  `json:"status"`
	StatusJa string `json:"status_ja"`
	Job      *struct {
		Status    int16  `json:"status"`
		StatusJa  string `json:"status_ja"`
		Stage     string `json:"stage"`
		Progress  int16  `json:"progress"`
		Attempts  int    `json:"attempts"`
		LastError string `json:"last_error"`
	} `json:"job"`
	Result *struct {
		Decision    int16    `json:"decision"`
		DecisionJa  string   `json:"decision_ja"`
		PartnerID   *int64   `json:"partner_id"`
		PartnerName string   `json:"partner_name"`
		Score       *float64 `json:"score"`
		ThresholdID *int64   `json:"threshold_id"`
		Candidates  []struct {
			PartnerID int64   `json:"partner_id"`
			Name      string  `json:"name"`
			Score     float64 `json:"score"`
			Rank      int16   `json:"rank"`
		} `json:"candidates"`
	} `json:"result"`
}

func main() {
	api := flag.String("api", "http://localhost:58080", "APIのアドレス")
	clientID := flag.Int64("client", 3, "顧問先ID")
	images := flag.String("images", "../testdata/samples/scanned", "画像と正解JSONのフォルダ")
	// masters.json の id と DB の partner_id のずれ。
	// 検証データ側の都合であって、製品の仕様ではない。
	offset := flag.Int64("offset", 3, "正解の partner_id に足す値")
	limit := flag.Int("limit", 0, "先頭から何枚まで（0は全部）")
	timeout := flag.Duration("timeout", 3*time.Minute, "全体の待ち時間の上限")
	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "DB（マスタの構造を調べるのに使う）")
	flag.Parse()

	files, _ := filepath.Glob(filepath.Join(*images, "*.json"))
	sort.Strings(files)
	if len(files) == 0 {
		die("正解JSONが見つかりません: %s", *images)
	}
	if *limit > 0 && *limit < len(files) {
		files = files[:*limit]
	}

	cl := &http.Client{Timeout: 60 * time.Second}

	// 危険な取引先を先に洗い出す（自分の名前が、より長い別社の先頭に含まれる）。
	risky := loadRisky(*dsn, *clientID)
	fmt.Printf("  マスタのうち、より長い別社に先頭を含まれるもの: %d件\n", len(risky))

	// ── 受付 ──
	//
	// 全部投げてから待つ。1枚ずつ投げて待つと、待ち行列とワーカーが
	// 意味を持っているかを確かめられない。同時に積んで、順に捌かれることを見る。
	type item struct {
		name  string
		docID int64
		tr    truth
	}
	var items []item
	tUpload := time.Now()
	for _, tf := range files {
		name := strings.TrimSuffix(filepath.Base(tf), ".json")
		b, err := os.ReadFile(tf)
		if err != nil {
			continue
		}
		var tr truth
		if err := json.Unmarshal(b, &tr); err != nil {
			continue
		}
		docID, err := upload(cl, *api, *clientID, filepath.Join(*images, name+".png"))
		if err != nil {
			die("受付に失敗 %s: %v", name, err)
		}
		items = append(items, item{name: name, docID: docID, tr: tr})
	}
	fmt.Printf("  受付  %d枚 %dms（処理の完了は待っていない）\n\n",
		len(items), time.Since(tUpload).Milliseconds())

	// ── 完了待ち ──
	tRun := time.Now()
	done := map[int64]docView{}
	deadline := time.Now().Add(*timeout)
	lastLine := ""
	for len(done) < len(items) && time.Now().Before(deadline) {
		for _, it := range items {
			if _, ok := done[it.docID]; ok {
				continue
			}
			v, err := get(cl, *api, it.docID)
			if err != nil {
				continue
			}
			// 3:完了 4:失敗。どちらも「もう動かない」状態。
			if v.Job != nil && (v.Job.Status == 3 || v.Job.Status == 4) {
				done[it.docID] = *v
			} else if v.Job != nil {
				// 進捗は同じ行を上書きする。変化したときだけ出す。
				// 毎回出すと、20枚の処理で数千行が流れて何も読めなくなる。
				line := fmt.Sprintf("%d/%d %s %d%%",
					len(done), len(items), v.Job.Stage, v.Job.Progress)
				if line != lastLine {
					fmt.Printf("\r  処理中 %-40s", line)
					lastLine = line
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Printf("\r  完了  %d/%d  %.1f秒                    \n\n",
		len(done), len(items), time.Since(tRun).Seconds())

	// ── 集計 ──
	//
	// 【100枚で分かったこと】OCRが末尾を落とすと、別の実在する取引先の
	// 名前そのものになることがある。マスタに「ハルタ商会」と「ハルタ商会284」が
	// 両方あると、284 が落ちた瞬間に前者と完全一致してスコア100になる。
	// 閾値では防げない（100が最大値）。危険な組み合わせの数を数える。
	var okMatch, okCand int
	var riskyAuto, riskyAutoCorrect int
	// 閾値の根拠を出すために、1位のスコアを正誤で分けて持つ。
	// 「正解の平均」と「不正解の平均」だけでは境目が決められない。
	// 不正解の最大値がどこにあるかが、上限を決める材料になる。
	var scoresOK, scoresNG []float64
	counts := map[int16]int{}
	byDecision := map[int16][2]int{}
	var failed []string

	fmt.Printf("  %-11s %-10s %7s %5s  %s\n", "伝票", "判定", "スコア", "順位", "取引先")
	fmt.Println("  " + strings.Repeat("-", 70))

	for _, it := range items {
		v, ok := done[it.docID]
		if !ok {
			failed = append(failed, it.name+"（時間切れ）")
			continue
		}
		if v.Job != nil && v.Job.Status == 4 {
			failed = append(failed, fmt.Sprintf("%s（%s）", it.name, v.Job.LastError))
			continue
		}
		if v.Result == nil {
			failed = append(failed, it.name+"（結果なし）")
			continue
		}
		want := it.tr.PartnerID + *offset
		counts[v.Result.Decision]++

		correct := v.Result.PartnerID != nil && *v.Result.PartnerID == want
		if correct {
			okMatch++
		}
		// 候補生成（第1段）が正解を拾えていたか。
		// ここで落としていたら、精密採点をいくら直しても当たらない。
		rank := 0
		for _, c := range v.Result.Candidates {
			if c.PartnerID == want {
				rank = int(c.Rank)
				okCand++
				break
			}
		}
		c := byDecision[v.Result.Decision]
		if correct {
			c[0]++
		} else {
			c[1]++
		}
		byDecision[v.Result.Decision] = c

		// 選ばれた取引先の名前が、別の取引先の名前の先頭に丸ごと含まれるか。
		// 含まれるなら、OCRが末尾を落とした結果かもしれない＝判断できない。
		if v.Result.Decision == 1 && v.Result.PartnerID != nil {
			if risky[*v.Result.PartnerID] {
				riskyAuto++
				if correct {
					riskyAutoCorrect++
				}
			}
		}

		score := 0.0
		if v.Result.Score != nil {
			score = *v.Result.Score
		}
		// 候補が1件も無いもの（スコア0）は分布に入れない。
		// 「照合できなかった」であって「照合して低かった」ではない。
		if len(v.Result.Candidates) > 0 {
			if correct {
				scoresOK = append(scoresOK, score)
			} else {
				scoresNG = append(scoresNG, score)
			}
		}
		rankStr := "圏外"
		if rank > 0 {
			rankStr = fmt.Sprintf("%d位", rank)
		}
		mark := " "
		if !correct {
			mark = "✗"
		}
		fmt.Printf("  %-11s %-10s %7.1f %5s %s %s\n",
			it.name, v.Result.DecisionJa, score, rankStr, mark, v.Result.PartnerName)
	}

	fmt.Println("  " + strings.Repeat("-", 70))
	// 分母は「結果が出た枚数」。投入枚数を分母にすると、時間切れの分だけ
	// 精度が低く見える。逆に、時間切れを黙って除くと通し検証の意味が無い。
	// 両方を出す。
	n := len(items)
	ok := n - len(failed)
	el := time.Since(tRun)
	fmt.Printf("  投入 %d枚 / 結果が出た %d枚  %.1f秒", n, ok, el.Seconds())
	if ok > 0 {
		fmt.Printf("  1枚あたり %.0fms", float64(el.Milliseconds())/float64(ok))
	}
	fmt.Println()
	fmt.Printf("  自動承認 %d / 要確認 %d / 却下 %d\n",
		counts[1], counts[5], counts[4])
	fmt.Printf("  候補生成が正解を含む  %d / %d（第1段 pg_trgm）\n", okCand, ok)
	fmt.Printf("  最終の1位が正解      %d / %d（第2段 C++）\n\n", okMatch, ok)

	reportThreshold(scoresOK, scoresNG)

	if riskyAuto > 0 {
		fmt.Printf("  ── 自動承認のうち、より長い別社が存在するもの ──\n")
		fmt.Printf("  %d件（うち正解 %d / 誤り %d）\n",
			riskyAuto, riskyAutoCorrect, riskyAuto-riskyAutoCorrect)
		fmt.Printf("  これを要確認に回すと、自動承認は %d → %d 件になる\n\n",
			counts[1], counts[1]-riskyAuto)
	}

	fmt.Printf("  %-10s %6s %6s   %s\n", "判定", "正解", "誤り", "評価")
	fmt.Println("  " + strings.Repeat("-", 52))
	for _, d := range []int16{1, 5, 4} {
		c := byDecision[d]
		ja := map[int16]string{1: "自動承認", 5: "要確認", 4: "却下"}[d]
		note := ""
		switch {
		case d == 1 && c[1] > 0:
			note = "❌ 誤承認あり。このままでは使えない"
		case d == 1:
			note = "✅ 誤承認ゼロ"
		case d == 4 && c[0] > 0:
			note = "△ 正しいものまで却下している"
		case d == 5:
			note = "人が見る"
		}
		fmt.Printf("  %-10s %6d %6d   %s\n", ja, c[0], c[1], note)
	}
	if len(failed) > 0 {
		fmt.Printf("\n  処理できなかったもの %d件\n", len(failed))
		for _, f := range failed {
			fmt.Printf("    %s\n", f)
		}
	}
}

func upload(cl *http.Client, api string, clientID int64, path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("client_id", fmt.Sprint(clientID))
	_ = mw.WriteField("doc_type", "1")
	fw, err := mw.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return 0, err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return 0, err
	}
	mw.Close()

	req, _ := http.NewRequest("POST", api+"/v1/documents", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := cl.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(res.Body)
		return 0, fmt.Errorf("%s: %s", res.Status, strings.TrimSpace(string(b)))
	}
	var out struct {
		DocumentID int64 `json:"document_id"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return 0, err
	}
	return out.DocumentID, nil
}

func get(cl *http.Client, api string, id int64) (*docView, error) {
	res, err := cl.Get(fmt.Sprintf("%s/v1/documents/%d?candidates=50", api, id))
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", res.Status)
	}
	var v docView
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		return nil, err
	}
	return &v, nil
}

func die(f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(1)
}

// loadRisky は「自分の名前が、より長い別社の名前の先頭に丸ごと含まれる」
// 取引先を集める。
//
// OCRが末尾を落とすと、この取引先の名前に化ける。
// 化けた先は実在する取引先なので、照合スコアは100になる。
// 閾値では止まらない（100が最大値）。
func loadRisky(dsn string, clientID int64) map[int64]bool {
	out := map[int64]bool{}
	if dsn == "" {
		return out
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		return out
	}
	defer pool.Close()
	rows, err := pool.Query(context.Background(), `
		SELECT DISTINCT a.id FROM partners a JOIN partners b
		  ON b.client_id = a.client_id AND a.id <> b.id
		 AND b.norm LIKE a.norm || '_%'
		WHERE a.client_id = $1`, clientID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// reportThreshold は閾値の根拠になる数字を出す。
//
// 「正解の平均」と「不正解の平均」だけでは境目が決められない。
// 決め手になるのは、
//
//	不正解の最大値   ここより上なら、誤って自動承認することがない
//	正解の最小値     ここより下げると、正しいものまで却下する
//
// 2つが重なっていれば、スコアだけでは完全には分けられない。
// そのときは「どちらの誤りが痛いか」で決める。
func reportThreshold(ok, ng []float64) {
	if len(ok) == 0 && len(ng) == 0 {
		return
	}
	sort.Float64s(ok)
	sort.Float64s(ng)

	fmt.Println("  ── 1位のスコア分布（候補が出たものだけ）──")
	fmt.Printf("  %-8s %5s %8s %8s %8s %8s\n", "", "件数", "最小", "中央", "平均", "最大")
	stat := func(label string, v []float64) {
		if len(v) == 0 {
			fmt.Printf("  %-8s %5d %8s\n", label, 0, "―")
			return
		}
		var sum float64
		for _, x := range v {
			sum += x
		}
		fmt.Printf("  %-8s %5d %8.1f %8.1f %8.1f %8.1f\n", label, len(v),
			v[0], v[len(v)/2], sum/float64(len(v)), v[len(v)-1])
	}
	stat("正解", ok)
	stat("不正解", ng)

	if len(ng) > 0 && len(ok) > 0 {
		maxNG := ng[len(ng)-1]
		minOK := ok[0]
		fmt.Println()
		if minOK > maxNG {
			fmt.Printf("  ✅ スコアだけで完全に分かれる（不正解の最大 %.1f < 正解の最小 %.1f）\n",
				maxNG, minOK)
			fmt.Printf("     上限は %.1f 〜 %.1f のどこに置いてもよい\n", maxNG, minOK)
		} else {
			// 重なっている。どこで切っても、どちらかの誤りが残る。
			var okBelow, ngAbove int
			for _, x := range ok {
				if x < maxNG {
					okBelow++
				}
			}
			for _, x := range ng {
				if x >= minOK {
					ngAbove++
				}
			}
			fmt.Printf("  ⚠ 重なっている（不正解の最大 %.1f ≧ 正解の最小 %.1f）\n", maxNG, minOK)
			fmt.Printf("     重なりの中に 正解%d件 / 不正解%d件\n", okBelow, ngAbove)
		}
	}

	// 上限をいくつにすると何が起きるか。
	// 誤承認が出ない一番低い値が、自動化の上限になる。
	fmt.Println("\n  ── 上限をいくつにすると何が起きるか ──")
	fmt.Printf("  %6s %10s %10s %s\n", "上限", "自動承認", "うち誤り", "")
	best := -1.0
	for _, th := range []float64{100, 99, 95, 90, 85, 80, 75, 70} {
		var auto, wrong int
		for _, x := range ok {
			if x >= th {
				auto++
			}
		}
		for _, x := range ng {
			if x >= th {
				auto++
				wrong++
			}
		}
		note := ""
		if wrong == 0 {
			note = "誤承認なし"
			if best < 0 || th < best {
				best = th
			}
		} else {
			note = "❌"
		}
		fmt.Printf("  %6.0f %10d %10d   %s\n", th, auto, wrong, note)
	}
	if best > 0 {
		fmt.Printf("\n  誤承認が出ない一番低い上限は %.0f\n", best)
	}
}
