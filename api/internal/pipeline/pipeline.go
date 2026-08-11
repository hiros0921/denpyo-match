// Package pipeline は1枚の伝票を最後まで通す。
//
//	画像 → 前処理 → OCR・項目抽出 → 照合 → 三分岐
//
// 各工程の結果と所要時間を全部残す。どこで時間を使ったか、
// どこで失敗したかが後から分からないと、運用で困る。
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hiros0921/denpo-match/api/internal/core"
	"github.com/hiros0921/denpo-match/api/internal/decide"
)

type Stage string

const (
	StagePreprocess Stage = "preprocess"
	StageOcr        Stage = "ocr"
	StageOcrRetry   Stage = "ocr_retry"
	StageMatch      Stage = "match"
	StageDecide     Stage = "decide"
)

// StageResult は1工程の記録。失敗しても記録は残す。
type StageResult struct {
	Stage Stage  `json:"stage"`
	Ms    int64  `json:"ms"`
	Err   string `json:"error,omitempty"`
}

type Result struct {
	DocumentID int64                  `json:"document_id"`
	Stages     []StageResult          `json:"stages"`
	Preprocess *core.PreprocessResult `json:"preprocess,omitempty"`
	Ocr        *core.OcrResult        `json:"ocr,omitempty"`
	Match      *core.MatchResult      `json:"match,omitempty"`
	Decision   *decide.Result         `json:"decision,omitempty"`
	TotalMs    int64                  `json:"total_ms"`
	CostYen    float64                `json:"cost_yen"`

	// 前処理後の画像の置き場所。DBに保存するのは呼び出し側の仕事。
	ProcessedKey string `json:"processed_key,omitempty"`
	// 前処理後の画像で読み直したか。どれだけ再挑戦が効いているかを測る。
	OcrRetried bool `json:"ocr_retried,omitempty"`
}

// Candidate は第1段（pg_trgm）が返す候補。
// pipeline は DB を知らない。候補をどう作るかは呼び出し側が決める。
type Candidate struct {
	ID   int64
	Name string
	// 覚えた表記。C++ はこれも採点の対象にし、一番近いものを採る。
	Variants []string
}

// CandidateFunc は取引先名から候補を返す。
// nil を渡すと、全マスタを載せたファイルで照合する（開発時の確認用）。
type CandidateFunc func(ctx context.Context, name string) ([]Candidate, error)

// ProgressFunc は工程が変わったときに呼ばれる。進捗をDBへ書くのに使う。
type ProgressFunc func(Stage)

type Pipeline struct {
	Runner *core.Runner
	// Google Cloud Vision。設定されていなければ nil。
	Vision *core.Vision

	// 前処理後の画像を置く場所。開発中は Go と C++ が別の場所で動くため、
	// 同じフォルダが2つの名前を持つ。作るのは Go 側、渡すのは C++ 側。
	//   WorkDirHost      Go から見たパス（作成・削除に使う）
	//   WorkDirContainer C++ に渡すパス
	// 本番では両方とも同じ値になる。
	WorkDirHost      string
	WorkDirContainer string
}

func New(r *core.Runner, workHost, workContainer string) *Pipeline {
	if workContainer == "" {
		workContainer = workHost
	}
	return &Pipeline{Runner: r, WorkDirHost: workHost, WorkDirContainer: workContainer}
}

// Run は1枚を通す。候補生成はせず、Runner が持つ全マスタで照合する。
// 開発時の確認用（cmd/pipetest）。
func (p *Pipeline) Run(ctx context.Context, docID int64, imagePath string,
	th decide.Threshold) (*Result, error) {
	return p.RunWithCandidates(ctx, docID, imagePath, th, nil, nil)
}

// RunWithCandidates は二段構えで照合する。
//
//	第1段  cand(...)   … pg_trgm で1万件を50件へ絞る（呼び出し側＝DB）
//	第2段  C++ dm_match … 50件を編集距離などで精密に採点する
//
// 全件を C++ に渡すと1万件分の編集距離計算になり、目標の50msに入らない。
// 逆に pg_trgm だけで決めると、日本語の企業名の識別に足りない
// （末尾の「運輸／建設／商事」で分かれる、という重みを付けられない）。
//
// 途中で失敗しても、そこまでの記録を返す。
// エラーだけ返して結果を捨てると、「どこまで進んだか」が分からなくなる。
func (p *Pipeline) RunWithCandidates(ctx context.Context, docID int64, imagePath string,
	th decide.Threshold, cand CandidateFunc, onStage ProgressFunc) (*Result, error) {
	// 向きを指定しない古い入口は発行側とみなす。
	// 100枚の検証データは自社が発行した請求書として作ってある。
	return p.run(ctx, docID, imagePath, "tesseract", Issued, th, cand, onStage)
}

// Direction は帳票の向き。照合する相手が向きで逆になる。
//
//	発行  自社が出した請求書。相手＝宛先（「○○商事 御中」の側）
//	受領  他社から来た請求書・領収書。相手＝発行元
//
// 受領側で宛先を照合すると、宛先は自社なので毎回自分に当たる。
// 実物のPDF5枚で、宛先側（しかも「代表」「経理部」という役職・部署）を
// 取引先名として返していた。向きは画像からは決められない。
// 「どちらが自社か」を知っている側、つまり設定で決める。
type Direction int16

const (
	Received Direction = 1 // 受領
	Issued   Direction = 2 // 発行
)

// NameField は照合に使う名前の項目名を返す。
//
// 【重要】ここを partner_name に固定しない。
// C++ は issuer_name と recipient_name の両方を返す。
// partner_name は recipient_name と同じ値で、古い呼び出し口のために残してある。
func (d Direction) NameField() string {
	if d == Received {
		return "issuer_name"
	}
	return "recipient_name"
}

func (d Direction) String() string {
	if d == Received {
		return "受領"
	}
	return "発行"
}

// RunWith はエンジンを指定して1枚を通す。
//
// エンジンの選択は顧問先（clients.ocr_engine）単位。
// 会計事務所は機密性を強く気にするので、「この顧問先だけローカル完結」
// ができることが商談上の武器になる。組織単位だとそれができない。
func (p *Pipeline) RunWith(ctx context.Context, docID int64, imagePath, engine string,
	dir Direction, th decide.Threshold, cand CandidateFunc,
	onStage ProgressFunc) (*Result, error) {
	return p.run(ctx, docID, imagePath, engine, dir, th, cand, onStage)
}

func (p *Pipeline) run(ctx context.Context, docID int64, imagePath, engine string,
	dir Direction, th decide.Threshold, cand CandidateFunc,
	onStage ProgressFunc) (*Result, error) {

	// 照合に使う名前は向きで変わる。以降はこの1つの変数だけを見る。
	// 2か所で別々に決めると、再挑戦のときだけ逆を見る、という壊れ方をする。
	nameKey := dir.NameField()

	res := &Result{DocumentID: docID}
	start := time.Now()
	defer func() { res.TotalMs = time.Since(start).Milliseconds() }()

	record := func(s Stage, t time.Time, err error) {
		sr := StageResult{Stage: s, Ms: time.Since(t).Milliseconds()}
		if err != nil {
			sr.Err = err.Error()
		}
		res.Stages = append(res.Stages, sr)
	}

	// ── 前処理 ──
	if err := os.MkdirAll(p.WorkDirHost, 0o755); err != nil {
		return res, fmt.Errorf("作業フォルダを作れません: %w", err)
	}
	// 出力は必ず png にする。
	//
	// 入力の拡張子をそのまま引き継ぐと、PDF を入れたときに
	// 「x.pdf に書き出せ」となり、OpenCV が imwrite で落ちる。
	//   OpenCV(4.6.0) could not find a writer for the specified extension
	// C++ 側が abort するので Go には exit status 133 としか返らず、
	// 3回再試行して打ち切られる。原因が拡張子だとは分からない。
	base := filepath.Base(imagePath)
	base = strings.TrimSuffix(base, filepath.Ext(base)) + ".png"
	outName := fmt.Sprintf("%d_%s", docID, base)
	processed := filepath.Join(p.WorkDirContainer, outName)

	t := time.Now()
	pre, err := p.Runner.Preprocess(ctx, imagePath, processed)
	record(StagePreprocess, t, err)
	if err != nil {
		return res, fmt.Errorf("前処理で失敗: %w", err)
	}
	res.Preprocess = pre
	res.ProcessedKey = outName

	// ── 向きを直す ──
	//
	// OCR には元画像を渡す（第9段階で、前処理はOCRの精度を下げると実測した）。
	// ただし「元画像」に EXIF の回転情報が付いていると、OCR は倒れた画素を
	// 受け取る。文字は読めるが座標が倒れたまま返り、抽出が全部外れる。
	//
	// 実物の請求書（iPhoneで撮影・EXIF Orientation=6）で確認:
	//   元画像のまま  取引先名「発書」/ 伝票番号「03-5832-9227」(FAX番号)
	//                 金額「113」(郵便番号の一部)  → すべて誤り
	//
	// 向きだけを直した画像を作って、それを OCR に渡す。
	// 直せなくても止めない。EXIF が無い画像は元のままで正しい。
	ocrTarget := imagePath
	uprightName := "up_" + outName
	if err := p.Runner.Upright(ctx,
		imagePath, filepath.Join(p.WorkDirContainer, uprightName)); err == nil {
		ocrTarget = filepath.Join(p.WorkDirContainer, uprightName)
	}

	// ── OCRと項目抽出 ──
	//
	// 【重要】OCRには元画像を渡す。前処理後ではない。
	//
	// 第5段階で「前処理はOCRの精度を上げなかった」と記録しながら、
	// 前処理後の画像を渡し続けていた。第9段階の100枚で測り直したところ、
	// 上げないどころか下げていた。
	//
	//   　　　　　　　　　前処理後   元画像
	//   取引先名が一致      52       54
	//   伝票番号が一致      77       92    ← 15枚の差
	//   金額が一致          93       98
	//
	// さらに悪いことに、前処理は取引先名の末尾を消すことがあった。
	//   正解 ハルタ商会284 → 前処理後「ハルタ商会」
	// 消えた結果が別の実在する会社の名前と完全一致し、スコア100で
	// 自動承認された（100枚で1件の誤承認）。閾値では止められない。
	//
	// 前処理そのものは捨てない。傾きの検出・セルの検出・人が見る画像として
	// 使う。OCRに渡すのをやめるだけ。
	if onStage != nil {
		onStage(StageOcr)
	}
	t = time.Now()
	ocr, err := p.ocr(ctx, engine, ocrTarget)
	record(StageOcr, t, err)
	if err != nil {
		return res, fmt.Errorf("OCRで失敗: %w", err)
	}
	res.Ocr = ocr
	res.CostYen = ocr.CostYen

	// ── 照合 ──
	//
	// 【重要】元画像で候補が1件も出なかったときだけ、前処理後の画像で
	// もう一度読む。順番と条件に意味がある。
	//
	// 100枚での実測（候補生成に正解が含まれた枚数）:
	//
	//   劣化      前処理してから   元画像    どちらかで届く
	//   light        19 (70%)     19 (70%)    23 (85%)
	//   normal       28 (82%)     28 (82%)    30 (88%)
	//   heavy        32 (82%)     29 (74%)    37 (95%)
	//   合計         79 (79%)     76 (76%)    90 (90%)
	//
	// 重い劣化では前処理が効き、軽い劣化では元画像が良い。役割が分かれている。
	//
	// 【なぜ「スコアの高いほうを採る」にしないか】
	// 前処理は名前の末尾を消すことがある。消えた結果が別の実在する会社と
	// 完全一致すると、スコア100（最大値）になる。
	//   正解 ハルタ商会284 → 前処理後「ハルタ商会」→ 別会社に100点
	// 高いほうを採ると、この誤りを必ず選んでしまう。
	//
	// 元画像で候補が出たなら、それを信じる。出なかったときだけ前処理後を試す。
	// こうすると、正しい結果を誤った結果で上書きすることが構造上ありえない。
	name, ok := ocr.Fields[nameKey]
	if !ok || name.Value == "" {
		// 名前すら取れなかった場合も、前処理後で読み直す価値がある。
		if alt, aerr := p.ocr(ctx, engine, processed); aerr == nil {
			if v, ok2 := alt.Fields[nameKey]; ok2 && v.Value != "" {
				record(StageOcrRetry, t, nil)
				ocr, name, ok = alt, v, true
				res.Ocr = alt
				res.OcrRetried = true
			}
		}
	}
	if !ok || name.Value == "" {
		// 取引先名が取れないなら照合しようがない。却下ではなく要確認に回す。
		// 画像は残っているので、人が見れば読める場合が多い。
		res.Decision = &decide.Result{
			Decision:    decide.NeedsReview,
			ThresholdID: th.ID,
			Why:         "取引先名を抽出できませんでした。画像を見て入力してください",
		}
		return res, nil
	}

	t = time.Now()
	var m *core.MatchResult
	if cand == nil {
		m, err = p.Runner.Match(ctx, name.Value, 50)
	} else {
		m, err = p.matchWithCandidates(ctx, docID, name.Value, cand)
	}
	record(StageMatch, t, err)
	if err != nil {
		return res, fmt.Errorf("照合で失敗: %w", err)
	}

	// 候補がゼロなら、前処理後の画像で読み直して再挑戦する。
	// 上書きするのは「何も無い」ときだけなので、正しい結果を壊せない。
	if len(m.Results) == 0 && !res.OcrRetried {
		t2 := time.Now()
		if alt, aerr := p.ocr(ctx, engine, processed); aerr == nil {
			if v, ok2 := alt.Fields[nameKey]; ok2 && v.Value != "" &&
				v.Value != name.Value {
				var m2 *core.MatchResult
				if cand == nil {
					m2, err = p.Runner.Match(ctx, v.Value, 50)
				} else {
					m2, err = p.matchWithCandidates(ctx, docID, v.Value, cand)
				}
				if err == nil && len(m2.Results) > 0 {
					m, ocr, res.Ocr = m2, alt, alt
					res.OcrRetried = true
				}
			}
		}
		record(StageOcrRetry, t2, nil)
	}
	res.Match = m

	// ── 三分岐 ──
	if onStage != nil {
		onStage(StageDecide)
	}
	t = time.Now()
	var score float64
	has := len(m.Results) > 0
	if has {
		score = m.Results[0].Score
	}
	d := decide.Decide(score, has, th)
	record(StageDecide, t, nil)
	res.Decision = &d

	return res, nil
}

// ocr は指定されたエンジンで読む。
//
// 【重要】Vision が設定されていないのに vision を指定されたら、
// 黙って Tesseract に落とさない。顧問先が精度を選んでいるのに
// 別のエンジンが動き、記録にも残らない状態を作らない。
// 設定漏れは、動かないことで気付けるようにする。
func (p *Pipeline) ocr(ctx context.Context, engine, image string) (*core.OcrResult, error) {
	if engine == "vision" {
		if p.Vision == nil || !p.Vision.Configured() {
			return nil, fmt.Errorf(
				"この顧問先は Vision を使う設定ですが、Vision の設定がありません。" +
					"顧問先の設定を tesseract に変えるか、APIキーを設定してください")
		}
		return p.Vision.Recognize(ctx, image)
	}
	return p.Runner.Ocr(ctx, image)
}

// matchWithCandidates は第1段が返した候補だけを C++ に採点させる。
//
// 候補は一時ファイルで渡す。dm_match は --masters にファイルを取るので、
// C++ 側を変えずに済む。id は DB の partner_id をそのまま入れる。
// 並び順で対応付けると、片方の並べ替えを直した瞬間に別の取引先へ紐づく。
func (p *Pipeline) matchWithCandidates(ctx context.Context, docID int64,
	name string, cand CandidateFunc) (*core.MatchResult, error) {

	cs, err := cand(ctx, name)
	if err != nil {
		return nil, err
	}
	if len(cs) == 0 {
		// 候補ゼロ。却下になる。照合を呼ぶ必要はない。
		return &core.MatchResult{Query: name}, nil
	}

	type rec struct {
		ID        int64    `json:"id"`
		Canonical string   `json:"canonical"`
		Variants  []string `json:"variants"`
	}
	recs := make([]rec, 0, len(cs))
	for _, c := range cs {
		recs = append(recs, rec{ID: c.ID, Canonical: c.Name, Variants: c.Variants})
	}
	b, err := json.Marshal(recs)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(p.WorkDirHost, 0o755); err != nil {
		return nil, err
	}
	fname := fmt.Sprintf("cand_%d.json", docID)
	hostPath := filepath.Join(p.WorkDirHost, fname)
	if err := os.WriteFile(hostPath, b, 0o644); err != nil {
		return nil, fmt.Errorf("候補ファイルを書けません: %w", err)
	}
	defer os.Remove(hostPath)

	return p.Runner.MatchWith(ctx,
		filepath.Join(p.WorkDirContainer, fname), name, len(cs))
}
