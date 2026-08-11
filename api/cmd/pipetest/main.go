// パイプラインの動作確認。第9段階の通し検証の原型。
//
//	go run ./cmd/pipetest -bin /tmp/dmbin -masters /testdata/samples/masters.json \
//	  -images /testdata/samples/scanned -truth <mac側のパス>
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

	"github.com/hiros0921/denpo-match/api/internal/core"
	"github.com/hiros0921/denpo-match/api/internal/decide"
	"github.com/hiros0921/denpo-match/api/internal/pipeline"
)

type truth struct {
	PartnerID    int64  `json:"partner_id"`
	DocNo        string `json:"doc_no"`
	TotalWithTax int64  `json:"total_with_tax"`
}

func main() {
	bin := flag.String("bin", "/tmp/dmbin", "C++ の実行ファイルがある場所")
	masters := flag.String("masters", "/testdata/samples/masters.json", "取引先マスタ")
	images := flag.String("images", "/testdata/samples/scanned", "コンテナ内の画像フォルダ")
	truthDir := flag.String("truth", "", "正解JSONがあるMac側のフォルダ")
	limit := flag.Int("limit", 0, "先頭から何枚まで（0は全部）")
	workHost := flag.String("work", "/tmp/dmwork", "Mac側の作業フォルダ")
	// コンテナ側の見え方。
	// colima が共有しているのはプロジェクトの下だけなので、
	// 作業フォルダはそこに置き、コンテナにも同じ絶対パスで見せる。
	// 既定を空にして workHost と同じにする。別名で渡す必要はまず無い。
	workCont := flag.String("workc", "", "コンテナから見た作業フォルダ（既定は -work と同じ）")
	// エンジンの指定。閾値の根拠を出すには両方の分布が要る。
	// vision は GOOGLE_VISION_API_KEY が要る（画像が Google に送られる）。
	engine := flag.String("engine", "tesseract", "tesseract / vision")
	// 1枚ごとの明細。閾値の分析は集計値では足りない。
	// 「不正解の最大スコア」「正解の最小スコア」は明細からしか出ない。
	csvPath := flag.String("csv", "", "1枚ごとの明細をCSVで書き出す先")
	flag.Parse()

	if *truthDir == "" {
		fmt.Fprintln(os.Stderr, "-truth に正解JSONのフォルダを指定してください")
		os.Exit(2)
	}
	files, err := filepath.Glob(filepath.Join(*truthDir, "*.json"))
	if err != nil || len(files) == 0 {
		fmt.Fprintf(os.Stderr, "正解JSONが見つかりません: %s\n", *truthDir)
		os.Exit(1)
	}
	sort.Strings(files)
	if *limit > 0 && *limit < len(files) {
		files = files[:*limit]
	}

	r := core.New(*bin, *masters)
	if *workCont == "" {
		*workCont = *workHost
	}
	p := pipeline.New(r, *workHost, *workCont)
	if *engine == "vision" {
		key := os.Getenv("GOOGLE_VISION_API_KEY")
		if key == "" {
			fmt.Fprintln(os.Stderr, "vision には GOOGLE_VISION_API_KEY が要ります")
			os.Exit(2)
		}
		p.Vision = core.NewVision(key, 0.22, r)
	}
	th := decide.Default

	var csvF *os.File
	if *csvPath != "" {
		f, err := os.Create(*csvPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "CSVを作れません: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		csvF = f
		fmt.Fprintln(csvF, "name,engine,decision,score,correct,truth_id,top1_id,top1_name,query,cands")
	}
	ctx := context.Background()

	var okMatch, total int
	counts := map[decide.Decision]int{}
	// 判定ごとに正誤を数える。自動承認に誤りが混じっていたら、
	// このプロダクトは成立しない。合計の正解率より、こちらが本質。
	byDecision := map[decide.Decision][2]int{} // [正解, 誤り]
	var sumMs int64

	fmt.Printf("  %-11s %-10s %7s %-8s %s\n", "伝票", "判定", "スコア", "時間", "取引先")
	fmt.Println("  " + strings.Repeat("-", 66))

	for i, tf := range files {
		name := strings.TrimSuffix(filepath.Base(tf), ".json")
		b, err := os.ReadFile(tf)
		if err != nil {
			continue
		}
		var tr truth
		if err := json.Unmarshal(b, &tr); err != nil {
			continue
		}
		total++

		res, err := p.RunWith(ctx, int64(i+1), filepath.Join(*images, name+".png"),
			*engine, pipeline.Issued, th, nil, nil)
		if err != nil {
			fmt.Printf("  %-11s ❌ %v\n", name, err)
			continue
		}
		sumMs += res.TotalMs
		counts[res.Decision.Decision]++

		got := ""
		correct := false
		if res.Match != nil && len(res.Match.Results) > 0 {
			got = res.Match.Results[0].Name
			if res.Match.Results[0].ID == tr.PartnerID {
				okMatch++
				correct = true
			}
		}
		c := byDecision[res.Decision.Decision]
		if correct {
			c[0]++
		} else {
			c[1]++
		}
		byDecision[res.Decision.Decision] = c
		fmt.Printf("  %-11s %-10s %7.1f %6dms  %s\n",
			name, res.Decision.Decision, res.Decision.Score, res.TotalMs, got)

		if csvF != nil {
			var top1ID int64
			query := ""
			cands := 0
			if res.Match != nil {
				query = res.Match.Query
				cands = len(res.Match.Results)
				if cands > 0 {
					top1ID = res.Match.Results[0].ID
				}
			}
			// 名前にカンマが入ることはまず無いが、入ったら壊れるので落とす。
			clean := strings.NewReplacer(",", " ", "\n", " ")
			fmt.Fprintf(csvF, "%s,%s,%s,%.1f,%t,%d,%d,%s,%s,%d\n",
				name, *engine, res.Decision.Decision, res.Decision.Score,
				correct, tr.PartnerID, top1ID,
				clean.Replace(got), clean.Replace(query), cands)
		}
	}

	fmt.Println("  " + strings.Repeat("-", 66))
	fmt.Printf("  %d枚  平均 %dms\n", total, sumMs/int64(max(total, 1)))
	fmt.Printf("  自動承認 %d / 要確認 %d / 却下 %d\n",
		counts[decide.AutoApprove], counts[decide.NeedsReview], counts[decide.Reject])
	fmt.Printf("  照合の1位が正解  %d / %d\n\n", okMatch, total)
	fmt.Printf("  %-10s %6s %6s   %s\n", "判定", "正解", "誤り", "評価")
	fmt.Println("  " + strings.Repeat("-", 48))
	for _, d := range []decide.Decision{decide.AutoApprove, decide.NeedsReview, decide.Reject} {
		c := byDecision[d]
		note := ""
		switch {
		case d == decide.AutoApprove && c[1] > 0:
			note = "❌ 誤承認あり。このままでは使えない"
		case d == decide.AutoApprove:
			note = "✅ 誤承認ゼロ"
		case d == decide.Reject && c[0] > 0:
			note = "△ 正しいものまで却下している"
		case d == decide.NeedsReview:
			note = "人が見る"
		}
		fmt.Printf("  %-10s %6d %6d   %s\n", d, c[0], c[1], note)
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
