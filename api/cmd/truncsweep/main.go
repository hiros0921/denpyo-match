// 銀行明細の摘要が桁切れしたとき、照合スコアがどう崩れるかを測る。
//
//	go run ./cmd/truncsweep -bin /tmp/dmbin \
//	  -data <bank_desc.json のMac側パス> -work <作業フォルダ>
//
// 【重要】本番と同じ経路を通す。
//
//	摘要の正規化   dm_match --normalize --bank
//	名前の採点     dm_match（伝票の取引先1件をマスタにして照会）
//	3点の合算      settle.Score
//
// 同じ計算をここに書き直すと、測っているのは本番ではなく写しになる。
//
// 何を見るか
//
//	① 正解の取引先に付くスコアが、桁数でどう落ちるか
//	② 誤った取引先に付くスコアが、桁数でどう上がるか
//	③ その2つが逆転する桁数（ここが崩壊点）
//	④ 金額と日付でどこまで救えるか
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
	"time"

	"github.com/hiros0921/denpo-match/api/internal/core"
	"github.com/hiros0921/denpo-match/api/internal/decide"
	"github.com/hiros0921/denpo-match/api/internal/ledger"
	"github.com/hiros0921/denpo-match/api/internal/settle"
)

type partner struct {
	ID        int64    `json:"id"`
	Canonical string   `json:"canonical"`
	Kana      string   `json:"kana"`
	Variants  []string `json:"variants"`
	PairWith  int64    `json:"pair_with"`
}

type desc struct {
	PartnerID int64  `json:"partner_id"`
	Trunc     int    `json:"trunc"`
	Variant   string `json:"variant"`
	Text      string `json:"text"`
	FullLen   int    `json:"full_len"`
	Cut       bool   `json:"cut"`
}

type dataset struct {
	Partners []partner `json:"partners"`
	Descs    []desc    `json:"descs"`
}

// 1件の測定。「この摘要を、この取引先として採点したら何点か」。
type cell struct {
	desc
	ScoredPartner int64
	Correct       bool // 摘要の出どころと採点相手が同じ
	Pair          bool // 紛らわしい組の相手
	Name          float64
	Total         float64 // 金額・日付が完全一致したときの合算
}

func main() {
	bin := flag.String("bin", "/tmp/dmbin", "C++ の実行ファイル")
	data := flag.String("data", "", "bank_desc.json（Mac側のパス）")
	workHost := flag.String("work", "", "作業フォルダ（Mac側）")
	workCont := flag.String("workc", "", "同じ場所をC++から見たパス（既定は -work と同じ）")
	// 学習済みの状態も測る。人が1回承認すると、カナ読みが別名に入る。
	learned := flag.Bool("learned", false, "カナ読みを別名として学習済みにする")
	// ── 曖昧さの検出のシミュレーション ──
	//
	// 【重要】製品には何も入れていない。ここで数字を出して、
	// 入れる価値があるかを先に確かめるための試算。
	cap_ := flag.Float64("cap", 0, "曖昧と判定した摘要の名前スコアの上限（0で無効）")
	mode := flag.String("ambig", "b", "a:濁点あり / b:濁点なしでも引く / c:bかつ桁が上限に達した行だけ")
	flag.Parse()

	if *data == "" || *workHost == "" {
		fmt.Fprintln(os.Stderr, "-data と -work を指定してください")
		os.Exit(2)
	}
	if *workCont == "" {
		*workCont = *workHost
	}

	raw, err := os.ReadFile(*data)
	must(err)
	var ds dataset
	must(json.Unmarshal(raw, &ds))

	r := core.New(*bin, "")
	ctx := context.Background()

	// ── 摘要をまとめて正規化する（本番と同じ --bank）──
	texts := make([]string, len(ds.Descs))
	for i, d := range ds.Descs {
		texts[i] = d.Text
	}
	norms, err := r.NormalizeBank(ctx, texts)
	must(err)

	// ── 取引先ごとにマスタを1件書く（本番の突合と同じ形）──
	//
	// 突合では「伝票の取引先1件」に対して摘要を採点する。
	// 1万件から探すのではないので、マスタは1件でよい。
	byID := map[int64]partner{}
	mastersPath := map[int64]string{}
	must(os.MkdirAll(*workHost, 0o755))
	for _, p := range ds.Partners {
		byID[p.ID] = p
		variants := append([]string{}, p.Variants...)
		if *learned {
			// 学習後は、正規化済みのカナ読みが別名に入る。
			// 【重要】生のカナではなく正規化後を入れる。
			// 生を入れると本番と条件が変わる（実装の誤りをそこで踏んだ）。
			kn, err := r.NormalizeBank(ctx, []string{p.Kana})
			must(err)
			variants = append(variants, kn[0])
		}
		body, err := json.Marshal([]map[string]any{{
			"id": p.ID, "canonical": p.Canonical, "variants": variants,
		}})
		must(err)
		f := filepath.Join(*workHost, fmt.Sprintf("tsweep_%d.json", p.ID))
		must(os.WriteFile(f, body, 0o644))
		mastersPath[p.ID] = filepath.Join(*workCont, fmt.Sprintf("tsweep_%d.json", p.ID))
	}

	// ── 採点 ──
	//
	// 各摘要を「正解の取引先」と「紛らわしい組の相手」の両方で採点する。
	// 全取引先との総当たりにしないのは、突合の候補が金額で絞られており、
	// 現実に取り違えうるのは同額が偶然ぶつかった相手だけだから。
	// 紛らわしい組を作ってあるので、そこを見れば最悪の場合が測れる。
	var cells []cell
	for i, d := range ds.Descs {
		n := norms[i]
		if n == "" {
			continue
		}
		targets := []int64{d.PartnerID}
		if pw := byID[d.PartnerID].PairWith; pw != 0 {
			targets = append(targets, pw)
		}
		for _, tid := range targets {
			score := 0.0
			m, err := r.MatchWith(ctx, mastersPath[tid], n, 1)
			must(err)
			if len(m.Results) > 0 {
				score = m.Results[0].Score
			}
			// 金額と日付が完全一致した場合の合算。
			// 名前の低下をどこまで救えるかを見る。
			tx := settle.Tx{Date: day("2026-08-25"), Amount: 100000,
				Source: ledger.Bank, NameScore: score}
			doc := settle.Doc{Total: 100000, IssueDate: day("2026-07-31")}
			cells = append(cells, cell{
				desc: d, ScoredPartner: tid,
				Correct: tid == d.PartnerID,
				Pair:    tid != d.PartnerID,
				Name:    score,
				Total:   settle.Score(doc, tx).Score,
			})
		}
	}

	if *cap_ > 0 {
		applyCap(cells, ds, byID, norms, *cap_, *mode)
	}
	report(cells, byID, *learned)
	if *cap_ > 0 {
		fmt.Printf("   ※ 曖昧と判定した摘要の名前スコアを %.0f で頭打ちにした試算（方式 %s）\n\n",
			*cap_, *mode)
	}
}

// ── 曖昧さの検出（試算）──
//
// 摘要の正規形 N で始まる取引先が2件以上あれば、その摘要は一意に定まらない。
// 桁切れで長い社名が短い別会社と完全一致する事故は、これで事前に分かる。
//
// 方式
//
//	a  正規形そのままで前方一致
//	b  a に加えて、濁点・半濁点を落とした形でも引く
//	   （半角カナでは濁点が1桁を食うので ｷﾞ が ｷ に化ける。
//	    実測: ハルタ工業 の8桁切れは a では拾えず b で拾えた）
//	c  b に加えて、桁が上限に達している行だけを対象にする
//	   （切れていない摘要まで止めないため）
func applyCap(cells []cell, ds dataset, byID map[int64]partner,
	norms []string, capAt float64, mode string) {

	// マスタ側の正規形。本番では partners.norm と partner_aliases.norm。
	forms := map[int64][]string{}
	for _, p := range ds.Partners {
		forms[p.ID] = append(forms[p.ID], normKey(p.Kana), normKey(p.Canonical))
	}
	// 桁の上限。本番では取り込み単位で max(length(description)) を取る。
	maxLen := map[int]int{}
	for _, d := range ds.Descs {
		if n := len([]rune(d.Text)); n > maxLen[d.Trunc] {
			maxLen[d.Trunc] = n
		}
	}

	// 摘要ごとに、前方一致する取引先の数を数える
	ambig := map[string]bool{}
	for i, d := range ds.Descs {
		n := norms[i]
		if n == "" {
			continue
		}
		if mode == "c" && len([]rune(d.Text)) < maxLen[d.Trunc] {
			continue // 上限に達していない＝切れていない
		}
		hit := map[int64]bool{}
		for pid, fs := range forms {
			for _, f := range fs {
				if strings.HasPrefix(f, n) {
					hit[pid] = true
				}
				if mode != "a" && strings.HasPrefix(stripDaku(f), stripDaku(n)) {
					hit[pid] = true
				}
			}
		}
		if len(hit) >= 2 {
			ambig[cellKey(d)] = true
		}
	}
	// 【重要】頭打ちの計算は本番と同じ関数を使う。
	//
	// 上限値は閾値から導く（settle.CapFor）。ここに数式を書き直すと、
	// 測っているのは本番ではなく写しになる。-cap で渡した値は
	// 「頭打ちを有効にするか」の指定としてだけ使い、実際に当てる値は
	// 本番と同じ導出に任せる。
	th := decide.Default
	capVal, ok := settle.CapFor(th.Upper, th.Lower)
	if capAt > 0 && capAt != capVal {
		fmt.Printf("   ※ -cap %.0f は無視した。本番の導出（閾値%.0f/%.0f）から %.1f を使う\n",
			capAt, th.Upper, th.Lower, capVal)
	}
	for i := range cells {
		if !ambig[cellKey(cells[i].desc)] {
			continue
		}
		capped := settle.CapName(cells[i].Name, capVal, ok)
		if capped == cells[i].Name {
			continue
		}
		cells[i].Name = capped
		tx := settle.Tx{Date: day("2026-08-25"), Amount: 100000,
			Source: ledger.Bank, NameScore: capped}
		doc := settle.Doc{Total: 100000, IssueDate: day("2026-07-31")}
		cells[i].Total = settle.Score(doc, tx).Score
	}
}

func cellKey(d desc) string {
	return fmt.Sprintf("%d|%d|%s", d.PartnerID, d.Trunc, d.Variant)
}

// normKey はマスタ側の正規形。dm_match と同じ規則で落とすのが本筋だが、
// ここは試算なので、比較に効く範囲（空白・法人格・記号）だけを落とす。
// 本番では partners.norm をそのまま使うので、この関数は要らなくなる。
func normKey(s string) string {
	for _, k := range []string{"株式会社", "有限会社", "合同会社", "(株)", "(有)",
		"㈱", "㈲", " ", "　", "・"} {
		s = strings.ReplaceAll(s, k, "")
	}
	return s
}

var dakuMap = map[rune]rune{}

func init() {
	base := []rune("カキクケコサシスセソタチツテトハヒフヘホハヒフヘホ")
	daku := []rune("ガギグゲゴザジズゼゾダヂヅデドバビブベボパピプペポ")
	for i, d := range daku {
		dakuMap[d] = base[i]
	}
	dakuMap['ヴ'] = 'ウ'
}

func stripDaku(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if b, ok := dakuMap[r]; ok {
			r = b
		}
		out = append(out, r)
	}
	return string(out)
}

func report(cells []cell, byID map[int64]partner, learned bool) {
	th := decide.Default
	state := "未学習（マスタは正式名だけ）"
	if learned {
		state = "学習済み（カナ読みを別名に持つ）"
	}
	fmt.Printf("\n╔══ 摘要の桁切れ　%s ══╗\n", state)
	fmt.Printf("   閾値 上限%.0f / 下限%.0f（変更していない）\n\n", th.Upper, th.Lower)

	truncs := []int{8, 12, 16, 20, 24, 0}
	label := func(n int) string {
		if n == 0 {
			return "無切断"
		}
		return fmt.Sprintf("%d桁", n)
	}

	// ── ① 正解のスコア分布 ──
	fmt.Println("① 正解の取引先に付いた名前スコア")
	fmt.Printf("   %-8s %6s %6s %6s %6s   %s\n",
		"桁数", "最小", "中央", "最大", "件数", "実際に切れた件数")
	fmt.Println("   " + strings.Repeat("-", 62))
	for _, n := range truncs {
		var v []float64
		cut := 0
		for _, c := range cells {
			if c.Trunc == n && c.Correct {
				v = append(v, c.Name)
				if c.Cut {
					cut++
				}
			}
		}
		if len(v) == 0 {
			continue
		}
		sort.Float64s(v)
		fmt.Printf("   %-8s %6.1f %6.1f %6.1f %6d   %d\n",
			label(n), v[0], v[len(v)/2], v[len(v)-1], len(v), cut)
	}

	// ── ② 誤った取引先に付いたスコア ──
	fmt.Println("\n② 紛らわしい相手（別会社）に付いた名前スコア")
	fmt.Printf("   %-8s %6s %6s %6s   %s\n", "桁数", "最大", "中央", "件数", "上限95を超えた件数")
	fmt.Println("   " + strings.Repeat("-", 58))
	for _, n := range truncs {
		var v []float64
		over := 0
		for _, c := range cells {
			if c.Trunc == n && c.Pair {
				v = append(v, c.Name)
				if c.Name >= th.Upper {
					over++
				}
			}
		}
		if len(v) == 0 {
			continue
		}
		sort.Float64s(v)
		fmt.Printf("   %-8s %6.1f %6.1f %6d   %d\n",
			label(n), v[len(v)-1], v[len(v)/2], len(v), over)
	}

	// ── ③ 三分岐（名前だけ／金額日付こみ）──
	fmt.Println("\n③ 現在の閾値での三分岐（正解の摘要のみ）")
	fmt.Printf("   %-8s │ %-22s │ %s\n", "桁数",
		"名前スコアだけで判定", "金額・日付が一致した合算で判定")
	fmt.Printf("   %-8s │ %6s %6s %6s │ %6s %6s %6s\n",
		"", "自動", "要確認", "却下", "自動", "要確認", "却下")
	fmt.Println("   " + strings.Repeat("-", 66))
	for _, n := range truncs {
		var a1, r1, j1, a2, r2, j2 int
		for _, c := range cells {
			if c.Trunc != n || !c.Correct {
				continue
			}
			for _, x := range []struct {
				s       float64
				a, r, j *int
			}{{c.Name, &a1, &r1, &j1}, {c.Total, &a2, &r2, &j2}} {
				switch {
				case x.s >= th.Upper:
					*x.a++
				case x.s >= th.Lower:
					*x.r++
				default:
					*x.j++
				}
			}
		}
		fmt.Printf("   %-8s │ %6d %6d %6d │ %6d %6d %6d\n",
			label(n), a1, r1, j1, a2, r2, j2)
	}

	// ── ④ 逆転 ──
	//
	// 同じ摘要で、別会社のほうが正解と同点以上になった件数。
	// 突合で同額がぶつかったとき、誤ったほうを選ぶ。最も危険。
	fmt.Println("\n④ 逆転（同じ摘要で、別会社が正解と同点以上）")
	type key struct {
		pid     int64
		trunc   int
		variant string
	}
	corr := map[key]float64{}
	wrong := map[key]float64{}
	for _, c := range cells {
		k := key{c.PartnerID, c.Trunc, c.Variant}
		if c.Correct {
			corr[k] = c.Name
		} else {
			wrong[k] = c.Name
		}
	}
	byTrunc := map[int]int{}
	type ex struct {
		k      key
		cs, ws float64
	}
	var examples []ex
	for k, w := range wrong {
		cv, ok := corr[k]
		if !ok {
			continue
		}
		if w >= cv {
			byTrunc[k.trunc]++
			examples = append(examples, ex{k, cv, w})
		}
	}
	fmt.Printf("   %-8s %s\n", "桁数", "逆転件数")
	fmt.Println("   " + strings.Repeat("-", 30))
	for _, n := range truncs {
		fmt.Printf("   %-8s %d\n", label(n), byTrunc[n])
	}
	if len(examples) > 0 {
		sort.Slice(examples, func(i, j int) bool {
			if examples[i].k.trunc != examples[j].k.trunc {
				return examples[i].k.trunc < examples[j].k.trunc
			}
			return examples[i].ws-examples[i].cs > examples[j].ws-examples[j].cs
		})
		fmt.Println("\n   具体例（差が大きい順・各桁数から）")
		shown := map[int]int{}
		for _, e := range examples {
			if shown[e.k.trunc] >= 3 {
				continue
			}
			shown[e.k.trunc]++
			p := byID[e.k.pid]
			q := byID[p.PairWith]
			fmt.Printf("     %-6s %-16s  正解 %s=%.1f  ←  誤り %s=%.1f\n",
				label(e.k.trunc), e.k.variant,
				short(p.Canonical), e.cs, short(q.Canonical), e.ws)
		}
	}

	// ── ⑤ 金額・日付で救えた割合 ──
	fmt.Println("\n⑤ 金額・日付の一致で、名前の低下をどこまで救えたか")
	fmt.Printf("   %-8s %10s %10s %10s\n",
		"桁数", "名前で却下", "うち救済", "救済率")
	fmt.Println("   " + strings.Repeat("-", 46))
	for _, n := range truncs {
		rej, saved := 0, 0
		for _, c := range cells {
			if c.Trunc != n || !c.Correct {
				continue
			}
			if c.Name < th.Lower {
				rej++
				if c.Total >= th.Lower {
					saved++
				}
			}
		}
		if rej == 0 {
			fmt.Printf("   %-8s %10d %10s %10s\n", label(n), 0, "-", "-")
			continue
		}
		fmt.Printf("   %-8s %10d %10d %9.0f%%\n",
			label(n), rej, saved, float64(saved)/float64(rej)*100)
	}
	fmt.Println()
}

func short(s string) string {
	rs := []rune(s)
	if len(rs) <= 12 {
		return s
	}
	return string(rs[:12]) + "…"
}

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	must(err)
	return t
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
