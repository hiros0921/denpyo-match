// Package worker は待ち行列から伝票を取り出し、最後まで通してDBへ書く。
//
// 設計の要点
//
//	① 1台で完結させない。ワーカーは何台でも立てられる形にする。
//	   取り合いの防止は DB の SKIP LOCKED に任せ、Go 側で調停しない。
//	② 落ちることを前提にする。処理中に落ちたジョブは locked_until が
//	   切れた時点で別の台が拾う。人が気付いて手で戻す運用にしない。
//	③ 途中経過を DB に書く。API は jobs を読むだけで進捗を返せる。
//	   ワーカーに問い合わせに行く作りにすると、台数が増えた瞬間に破綻する。
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/hiros0921/denpo-match/api/internal/decide"
	"github.com/hiros0921/denpo-match/api/internal/invoice"
	"github.com/hiros0921/denpo-match/api/internal/pipeline"
	"github.com/hiros0921/denpo-match/api/internal/settle"
	"github.com/hiros0921/denpo-match/api/internal/store"
)

type Worker struct {
	// 入出金の突合。nil なら行わない。
	// 伝票の処理が終わった直後に、その1枚ぶんだけ回す。
	// 明細CSVが先に取り込まれていて、伝票が後から来る順序のため。
	Settle *settle.Runner
	ID     string
	St     *store.Store
	Pipe   *pipeline.Pipeline
	Log    *slog.Logger
	OrgID  int64

	// 画像の実体がある場所。開発中はローカルのフォルダ。
	// 本番は R2 から落として渡すが、その差はここだけに閉じる。
	ImageRoot          string
	ImageRootContainer string

	// 持ち分の期限。処理が続く限り Heartbeat で延ばす。
	// 1枚の実測が約2秒なので、30秒あれば十分に余裕がある。
	Lease time.Duration
	// 待ち行列が空のときに次を見に行くまでの間隔。
	IdleWait time.Duration
	// 候補生成で残す件数。第1段でここまで絞り、第2段（C++）で精密に採点する。
	TopK int
}

// 同じプロセス内で何台目かを数える。
//
// 【重要】ホスト名とプロセスIDだけでは、1つのプロセスで複数台動かしたとき
// 全部が同じ名前になる。名前で持ち分を判定しているので、
// 同じ名前だと「自分が持っている」と全台が答えてしまい、
// 二重処理を止める仕組みが働かない。実際にそうなっていた。
var seq atomic.Int64

func New(st *store.Store, p *pipeline.Pipeline, orgID int64,
	imageRoot, imageRootContainer string) *Worker {
	host, _ := os.Hostname()
	return &Worker{
		// どの台が持っているかを残す。台数を増やしたとき、
		// 特定の台だけ失敗する状況を切り分けられるようにする。
		ID:                 fmt.Sprintf("%s/%d/%d", host, os.Getpid(), seq.Add(1)),
		St:                 st,
		Pipe:               p,
		Log:                slog.Default(),
		OrgID:              orgID,
		ImageRoot:          imageRoot,
		ImageRootContainer: imageRootContainer,
		Lease:              30 * time.Second,
		IdleWait:           500 * time.Millisecond,
		TopK:               50,
	}
}

// Run は止められるまで回り続ける。
func (w *Worker) Run(ctx context.Context) {
	w.Log.Info("ワーカー開始", "id", w.ID)
	for {
		select {
		case <-ctx.Done():
			w.Log.Info("ワーカー終了", "id", w.ID)
			return
		default:
		}

		n, err := w.Once(ctx)
		switch {
		case err != nil:
			// 取り出しそのものが失敗した＝DBが落ちている可能性。
			// 詰まった状態で回し続けても意味がないので、間隔を空ける。
			w.Log.Error("ジョブの取り出しに失敗", "err", err)
			sleep(ctx, 2*time.Second)
		case n == 0:
			sleep(ctx, w.IdleWait)
		}
	}
}

// Once は1件だけ処理する。処理した件数を返す（0なら待ち行列が空）。
func (w *Worker) Once(ctx context.Context) (int, error) {
	job, err := w.St.Claim(ctx, w.ID, w.Lease)
	if err != nil {
		return 0, err
	}
	if job == nil {
		return 0, nil
	}

	if err := w.process(ctx, job); err != nil {
		// 持ち分を失っていた場合は失敗ではない。
		// 別の台が同じ伝票を最後まで処理しており、そちらの結果が残る。
		// ここで Fail を呼ぶと、他の台が正しく終えたジョブを
		// 待機に戻してしまい、同じ伝票を延々と処理し続けることになる。
		if errors.Is(err, store.ErrLostJob) {
			w.Log.Warn("持ち分を失ったため破棄しました",
				"job", job.ID, "doc", job.DocumentID, "worker", w.ID)
			return 1, nil
		}
		// 失敗しても、ここで落ちない。記録して次へ進む。
		// 1枚の壊れた画像で待ち行列全体が止まるのが最悪の壊れ方。
		givenUp, ferr := w.St.Fail(ctx, job.ID, err)
		if ferr != nil {
			w.Log.Error("失敗の記録に失敗", "job", job.ID, "err", ferr)
		}
		if givenUp {
			w.Log.Error("打ち切り", "job", job.ID, "doc", job.DocumentID,
				"attempts", job.Attempts, "err", err)
			if merr := w.St.MarkDocumentError(ctx, job.DocumentID); merr != nil {
				w.Log.Error("エラー状態にできません", "doc", job.DocumentID, "err", merr)
			}
		} else {
			w.Log.Warn("再試行します", "job", job.ID, "attempts", job.Attempts, "err", err)
		}
		return 1, nil
	}
	return 1, nil
}

func (w *Worker) process(ctx context.Context, job *store.Job) error {
	beat := func(stage string, pct int16) {
		if err := w.St.Heartbeat(ctx, job.ID, stage, pct, w.Lease); err != nil {
			w.Log.Warn("進捗を書けません", "job", job.ID, "err", err)
		}
	}

	// ── 心拍 ──
	//
	// 工程の切れ目だけで期限を延ばしていたら、1つの工程が期限より
	// 長引いたときに切れた。実測:
	//   リース30秒 / OCRの上限60秒。OCRの最中に期限が切れ、
	//   別のワーカーが同じ伝票を拾い、2台が最後まで処理して
	//   両方が結果を書いた（伝票10048・10064）。
	// 処理している間ずっと打ち続ける。
	hbCtx, stopHB := context.WithCancel(ctx)
	defer stopHB()
	go func() {
		t := time.NewTicker(w.Lease / 3)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				if err := w.St.Touch(hbCtx, job.ID, w.ID, w.Lease); err != nil {
					w.Log.Warn("期限を延ばせません", "job", job.ID, "err", err)
				}
			}
		}
	}()

	// 閾値は DB から引く。無ければ既定値。
	// コードに埋め込まないのがこのプロダクトの核心なので、
	// 「DBを見に行く」経路を必ず通す。
	th := decide.Default
	if t, err := w.St.CurrentThreshold(ctx, w.OrgID, job.ClientID, job.DocType); err == nil {
		th = decide.Threshold{ID: t.ID, Upper: t.Upper, Lower: t.Lower}
	} else if err != store.ErrNotFound {
		return fmt.Errorf("閾値を読めません: %w", err)
	}

	beat("preprocess", 10)
	img := w.ImageRootContainer + "/" + job.R2KeyOrig

	// 候補生成を DB に任せる。pipeline は「候補を出す関数」を受け取る。
	// こうしておくと、pipeline 自体は DB を知らないままでいられる。
	// 顧問先ごとのエンジンを使う。設定が空なら tesseract。
	engine := job.OcrEngine
	if engine == "" {
		engine = "tesseract"
	}
	dir := pipeline.Direction(job.Direction)
	res, err := w.Pipe.RunWith(ctx, job.DocumentID, img, engine, dir, th,
		func(ctx context.Context, name string) ([]pipeline.Candidate, error) {
			beat("match", 70)
			return w.candidates(ctx, job.ClientID, name)
		},
		func(stage pipeline.Stage) {
			switch stage {
			case pipeline.StageOcr:
				beat("ocr", 40)
			case pipeline.StageDecide:
				beat("decide", 90)
			}
		})
	if err != nil {
		return err
	}

	if err := w.save(ctx, job, res, th); err != nil {
		return err
	}

	// 受領伝票なら、入出金との突合も回す。
	// 失敗しても伝票の処理は成立しているので、記録して続ける。
	// 突合は取り込み時と手動でも再実行できる。
	if w.Settle != nil && job.Direction == 1 {
		if _, err := w.Settle.SettleClient(ctx, w.OrgID, job.ClientID); err != nil {
			w.Log.Warn("突合に失敗（伝票の処理は完了）", "doc", job.DocumentID, "err", err)
		}
	}
	return nil
}

// candidates は pg_trgm で第1段の絞り込みを行う。
//
// 正規化は C++ を呼ぶ。Go 側に書き直すと、partners.norm を作ったときの
// ルールと、照会時のルールがずれる。ずれると候補生成が静かに当たらなくなり、
// 「なぜかこの取引先だけ拾えない」という形で出てくる。原因を探すのが最も難しい類い。
func (w *Worker) candidates(ctx context.Context, clientID int64,
	name string) ([]pipeline.Candidate, error) {

	norms, err := w.Pipe.Runner.Normalize(ctx, []string{name})
	if err != nil || len(norms) == 0 {
		return nil, fmt.Errorf("正規化に失敗: %w", err)
	}
	ps, err := w.St.Candidates(ctx, clientID, norms[0], w.TopK)
	if err != nil {
		return nil, err
	}
	out := make([]pipeline.Candidate, 0, len(ps))
	for _, p := range ps {
		out = append(out, pipeline.Candidate{
			ID: p.ID, Name: p.Name, Variants: p.Aliases,
		})
	}
	return out, nil
}

func (w *Worker) save(ctx context.Context, job *store.Job,
	res *pipeline.Result, th decide.Threshold) error {

	in := store.SaveInput{
		OrgID:      w.OrgID,
		JobID:      job.ID,
		WorkerID:   w.ID,
		DocumentID: job.DocumentID,
		// 実際に動いたエンジンを入れる。res.Ocr があれば上書きされる。
		Engine:   "tesseract",
		OcrRaw:   json.RawMessage("{}"),
		Why:      res.Decision.Why,
		Decision: dbDecision(res.Decision.Decision),
	}
	if res.Ocr != nil {
		in.Engine = res.Ocr.Engine
		in.CostYen = res.Ocr.CostYen
		if b, err := json.Marshal(res.Ocr); err == nil {
			in.OcrRaw = b
		}
		for k, f := range res.Ocr.Fields {
			in.Fields = append(in.Fields, store.SaveField{
				Key: k, Value: f.Value, Confidence: f.Confidence,
				BBox: f.BBox[:],
			})
		}
	}
	// ── インボイス登録番号 ──
	//
	// OCRが動いたかどうかに関わらず必ず判定する。
	// 読めなかったときに行が無いと、一覧に出てこない。
	// 「番号が書かれていない請求書」は控除が取れない可能性があり、
	// 現場が一番見なければならないものなので、抜けてはいけない。
	{
		raw := ""
		if res.Ocr != nil {
			if f, ok := res.Ocr.Fields["reg_no"]; ok {
				raw = f.Value
			}
		}
		r := invoice.Evaluate(raw)
		in.RegNo, in.RegStatus, in.RegWhy = r.RegNo, int16(r.Status), r.Why
	}

	if res.Preprocess != nil {
		in.Width, in.Height = res.Preprocess.Width, res.Preprocess.Height
	}
	in.R2KeyProc = res.ProcessedKey

	if res.Match != nil {
		for i, c := range res.Match.Results {
			in.Candidates = append(in.Candidates, store.SaveCandidate{
				PartnerID: c.ID, Score: c.Score, Rank: int16(i + 1),
				Detail: map[string]float64{
					"levenshtein": c.Lev, "jaccard": c.Jac, "prefix": c.Pre,
				},
				MatchedForm: c.Matched,
			})
		}
	}

	// 自動承認のときだけ threshold_id が必須（DBの制約でも弾かれる）。
	// 既定値（ID=0）はDBに行が無いので、そのまま入れると外部キー違反になる。
	// 実運用では組織ごとに必ず1件は入っている前提だが、
	// 入っていないときに「制約違反で落ちる」のは正しい振る舞い。
	// ここで黙って NULL にすると、追跡できない自動承認ができてしまう。
	if th.ID > 0 {
		id := th.ID
		in.ThresholdID = &id
	}
	if res.Decision.Score > 0 {
		s := res.Decision.Score
		in.Score = &s
	}
	if res.Match != nil && len(res.Match.Results) > 0 && res.Decision.Decision != decide.Reject {
		id := res.Match.Results[0].ID
		in.PartnerID = &id
	}

	return w.St.SaveResult(ctx, in)
}

// dbDecision は三分岐の結果を match_results.decision の値へ移す。
//
// 番号が一致していないので、必ずここを通す。
//
//	decide            match_results.decision
//	1 AutoApprove  →  1 自動承認
//	2 NeedsReview  →  5 要確認      ← 2 は「人が承認」。番号が同じでも意味が違う
//	3 Reject       →  4 却下
//
// 【重要】ここを素通りさせると、要確認の伝票が「人が承認した」として
// 記録される。誰も見ていないのに承認済みの記録が残る、という
// このプロダクトで最もやってはいけない誤りになる。実際に一度そうなった。
func dbDecision(d decide.Decision) int16 {
	switch d {
	case decide.AutoApprove:
		return 1
	case decide.NeedsReview:
		return 5
	case decide.Reject:
		return 4
	}
	return 5 // 分からないものは人に回す。黙って承認しない
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
