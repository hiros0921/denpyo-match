package core

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// Vision は Google Cloud Vision で読む。
//
// なぜ C++ ではなく Go に置くか
//
//	C++ 側に Google の SDK を持ち込むと、画像処理の実行ファイルが
//	ネットワークと認証情報を扱うことになる。前処理とOCRは
//	壊れた画像で落ちうる処理なので、そこに鍵を置きたくない。
//
// 【重要】項目の抽出は Go 側に書かない
//
//	「どれが取引先名か・日付か・金額か」を決める処理は、
//	エンジンが違っても同じであるべき。ここを Go に書き直すと実装が2つになる。
//	取引先名の決め方には「御中の左」「上部左」「法人格を含む」といった
//	規則が積み重なっていて、片方だけ直せば必ず食い違う。
//	食い違っても例外は出ない。「Vision にした顧問先だけ精度が落ちる」
//	という形でしか表面化しないので、気付くのが最も遅れる。
//
//	読んだブロックを dm_ocr --extract に渡し、抽出は C++ に任せる。
//
// 【機密性について】
//
//	Vision を使うと、顧問先の帳票画像が Google のサーバーへ送られる。
//	会計事務所はここを強く気にする。だから選択は顧問先（clients）単位にしてあり、
//	既定を Vision にしていない。
type Vision struct {
	APIKey string
	// 1ページあたりの費用。実費の記録に使う。
	// Google の料金は枚数で段階的に変わるので、設定で上書きできるようにする。
	CostPerPageYen float64
	Client         *http.Client
	// 抽出は C++ に任せる。そのための実行ファイル。
	Runner *Runner
}

func NewVision(apiKey string, costPerPage float64, r *Runner) *Vision {
	if costPerPage <= 0 {
		// 目安。1000枚まで無料、以降 $1.50/1000枚。
		// 為替と料金改定で動くので、設定で上書きできるようにしてある。
		costPerPage = 0.22
	}
	return &Vision{
		APIKey: apiKey, CostPerPageYen: costPerPage, Runner: r,
		// 画像を送るので長めに取る。ただし無限には待たない。
		Client: &http.Client{Timeout: 60 * time.Second},
	}
}

func (v *Vision) Configured() bool { return v != nil && v.APIKey != "" }

var ErrVisionNotConfigured = errors.New("Google Cloud Vision の設定がされていません")

// Block は OCR が読んだ1つの塊。C++ 側の TextBlock と同じ形。
type Block struct {
	Text       string  `json:"text"`
	Confidence float64 `json:"confidence"`
	X          int     `json:"x"`
	Y          int     `json:"y"`
	W          int     `json:"w"`
	H          int     `json:"h"`
}

// Recognize は画像を読み、Tesseract と同じ形の結果を返す。
//
// 同じ形にするのが要点。呼び出し側が「どちらのエンジンで読んだか」を
// 意識せずに済むようにする。意識が必要になった時点で、
// エンジンを差し替えられるという設計が崩れる。
func (v *Vision) Recognize(ctx context.Context, imagePath string) (*OcrResult, error) {
	if !v.Configured() {
		return nil, ErrVisionNotConfigured
	}
	start := time.Now()

	blocks, err := v.annotate(ctx, imagePath)
	if err != nil {
		return nil, err
	}
	if len(blocks) == 0 {
		return &OcrResult{Engine: "vision", Ms: int(time.Since(start).Milliseconds()),
			CostYen: v.CostPerPageYen, Fields: map[string]Field{}}, nil
	}

	fields, err := v.Runner.ExtractFields(ctx, blocks)
	if err != nil {
		return nil, fmt.Errorf("項目の抽出に失敗: %w", err)
	}

	return &OcrResult{
		Engine:  "vision",
		Ms:      int(time.Since(start).Milliseconds()),
		CostYen: v.CostPerPageYen,
		Blocks:  len(blocks),
		Fields:  fields,
	}, nil
}

func (v *Vision) annotate(ctx context.Context, imagePath string) ([]Block, error) {
	raw, err := os.ReadFile(imagePath)
	if err != nil {
		return nil, fmt.Errorf("画像を読めません: %w", err)
	}

	body, _ := json.Marshal(map[string]any{
		"requests": []any{map[string]any{
			"image": map[string]string{"content": base64.StdEncoding.EncodeToString(raw)},
			// DOCUMENT_TEXT_DETECTION は帳票向け。段落と行の構造を返す。
			// TEXT_DETECTION は看板や写真向けで、帳票だと粗い結果になる。
			"features": []any{map[string]any{"type": "DOCUMENT_TEXT_DETECTION"}},
			// 日本語を明示する。指定しないと英字として読まれることがある。
			"imageContext": map[string]any{"languageHints": []string{"ja"}},
		}},
	})

	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://vision.googleapis.com/v1/images:annotate?key="+v.APIKey,
		bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := v.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Vision に繋がりません: %w", err)
	}
	defer res.Body.Close()

	var out visionResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("Vision の応答を読めません: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		msg := out.Error.Message
		if msg == "" && len(out.Responses) > 0 {
			msg = out.Responses[0].Error.Message
		}
		return nil, fmt.Errorf("Vision がエラーを返しました（%d）: %s", res.StatusCode, msg)
	}
	if len(out.Responses) == 0 {
		return nil, fmt.Errorf("Vision の応答が空です")
	}
	if m := out.Responses[0].Error.Message; m != "" {
		return nil, fmt.Errorf("Vision がエラーを返しました: %s", m)
	}
	return out.Responses[0].blocks(), nil
}

// ── Vision の応答 ──

type visionVertex struct{ X, Y int }

type visionPoly struct {
	Vertices []visionVertex `json:"vertices"`
}

func (b visionPoly) rect() (x, y, w, h int) {
	if len(b.Vertices) == 0 {
		return 0, 0, 0, 0
	}
	minX, minY := b.Vertices[0].X, b.Vertices[0].Y
	maxX, maxY := minX, minY
	for _, v := range b.Vertices {
		if v.X < minX {
			minX = v.X
		}
		if v.X > maxX {
			maxX = v.X
		}
		if v.Y < minY {
			minY = v.Y
		}
		if v.Y > maxY {
			maxY = v.Y
		}
	}
	return minX, minY, maxX - minX, maxY - minY
}

type visionSymbol struct {
	Text string `json:"text"`
}

type visionWord struct {
	Symbols     []visionSymbol `json:"symbols"`
	Confidence  float64        `json:"confidence"`
	BoundingBox visionPoly     `json:"boundingBox"`
}

type visionParagraph struct {
	Words []visionWord `json:"words"`
}

type visionBlock struct {
	Paragraphs []visionParagraph `json:"paragraphs"`
}

type visionPage struct {
	Blocks []visionBlock `json:"blocks"`
}

type visionAnnotation struct {
	Pages []visionPage `json:"pages"`
}

type visionError struct {
	Message string `json:"message"`
}

type visionSingle struct {
	FullTextAnnotation visionAnnotation `json:"fullTextAnnotation"`
	Error              visionError      `json:"error"`
}

type visionResponse struct {
	Responses []visionSingle `json:"responses"`
	Error     visionError    `json:"error"`
}

// blocks は Vision の入れ子構造を、C++ 側と同じ「単語＋座標」の並びに直す。
//
// 単語の単位にそろえる。Tesseract も単語で返すので、
// 抽出側（C++）が受け取る形が一致する。
// 段落や行で返すと、抽出側の「同じ行を連結する」処理が二重にかかる。
func (r visionSingle) blocks() []Block {
	var out []Block
	for _, pg := range r.FullTextAnnotation.Pages {
		for _, b := range pg.Blocks {
			for _, para := range b.Paragraphs {
				for _, w := range para.Words {
					var sb strings.Builder
					for _, s := range w.Symbols {
						sb.WriteString(s.Text)
					}
					x, y, ww, hh := w.BoundingBox.rect()
					out = append(out, Block{
						Text: sb.String(), Confidence: w.Confidence,
						X: x, Y: y, W: ww, H: hh,
					})
				}
			}
		}
	}
	// 【重要】ここで並べ替えない。Vision の元の順のまま渡す。
	//
	// 最初「上から下、同じ行なら左から右」に並べ替えていたが、
	// この比較（Yの差が小さければX比較）は推移律を満たさず、
	// 並び順が乱れることがある。実測で踏んだ:
	//
	//   del_0010  帳票の表記 (株)ハルタ物産512
	//     元の順で抽出       (株)ハルタ物産512   正しい
	//     並べ替えてから抽出  (株)ハルタ物産      512 が落ちる
	//
	// 落ちた結果が「ハルタ物産(株)」という実在の別会社と完全一致し、
	// スコア100で誤承認になった。閾値では止まらない。
	//
	// そもそも行まとめと並べ替えは C++ の抽出側（extract.cpp）に既にある。
	// ここでやるのは二重で、しかも壊し得るだけだった。
	return out
}
