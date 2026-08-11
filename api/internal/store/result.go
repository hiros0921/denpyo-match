package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// SaveInput は1枚分の処理結果。ワーカーがこれを組み立ててから1回で書く。
type SaveInput struct {
	OrgID      int64
	JobID      int64
	DocumentID int64
	// 書き込む直前に「まだ自分が持っているか」を確かめるのに使う。
	// 持っていなければ何も書かない（ErrLostJob）。
	WorkerID string

	Engine    string          // 'vision' | 'tesseract'
	OcrRaw    json.RawMessage // OCRの生出力。様式が増えても定義を変えずに済む
	CostYen   float64
	R2KeyProc string // 前処理後の画像
	Width     int
	Height    int

	Fields     []SaveField
	Candidates []SaveCandidate

	Decision    int16 // 1:自動承認 2:人が承認 3:人が修正 4:却下 5:要確認
	PartnerID   *int64
	Score       *float64
	ThresholdID *int64
	Why         string

	// インボイス登録番号の検査結果。0 なら検査していない（書かない）。
	RegNo     string
	RegStatus int16
	RegWhy    string
}

type SaveField struct {
	Key        string
	Value      string
	Norm       string
	Confidence float64
	BBox       []int // x, y, w, h
}

type SaveCandidate struct {
	PartnerID int64
	Score     float64
	Rank      int16
	Detail    map[string]float64 // 編集距離・n-gram・先頭一致の内訳
	// 実際に一致した表記。正式名称と違えば、覚えた表記で当たっている。
	MatchedForm string
}

// SaveResult は1枚分の結果をまとめて書く。
//
// なぜ1つのトランザクションなのか
//
//	照合結果だけ入って監査ログが入っていない、という状態を作らないため。
//	自動承認は「人の確認を省いた」という判断なので、その記録が欠けると
//	後から説明できない。片方だけ成功する余地を残さない。
//
// 【重要】documents.status の更新もここに含める。別に書くと、
// 「結果はあるのに status が受付のまま」の伝票ができる。
// 画面には出ないが処理は終わっている、という一番気付きにくい壊れ方になる。
func (s *Store) SaveResult(ctx context.Context, in SaveInput) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// 【重要】最初に持ち分を確かめ、この取引の間そのジョブ行を押さえる。
	//
	// 確認を最後に置くと、2台が同時にここを通ったとき、
	// 両方が「自分のものだ」と判断してから片方が負ける。
	// 負けた側のトランザクションは捨てられるが、その前に
	// CopyFrom で50件を書くなど無駄な仕事をしている。
	// 先に行を押さえれば、負ける側はここで待たされ、すぐ弾かれる。
	var owner *string
	if err = tx.QueryRow(ctx,
		`SELECT locked_by FROM jobs WHERE id = $1 FOR UPDATE`,
		in.JobID).Scan(&owner); err != nil {
		return fmt.Errorf("ジョブを確認できません: %w", err)
	}
	if owner == nil || *owner != in.WorkerID {
		return ErrLostJob
	}

	// ── 前処理後の画像とサイズ ──
	var pageID int64
	err = tx.QueryRow(ctx, `
		UPDATE document_pages
		   SET r2_key_processed = $2, width = $3, height = $4
		 WHERE document_id = $1 AND page_no = 1
		RETURNING id`, in.DocumentID, in.R2KeyProc, in.Width, in.Height).Scan(&pageID)
	if err != nil {
		return fmt.Errorf("ページを更新できません: %w", err)
	}

	// ── OCRの生出力 ──
	if _, err = tx.Exec(ctx, `
		INSERT INTO ocr_results (document_page_id, engine, raw, cost_yen)
		VALUES ($1, $2, $3, $4)`,
		pageID, in.Engine, in.OcrRaw, in.CostYen); err != nil {
		return fmt.Errorf("OCR結果を書けません: %w", err)
	}

	// ── 抽出項目 ──
	//
	// 再処理でも積み増しにならないよう、先に消してから入れる。
	// extracted_fields は監査ログではないので、消してよい。
	// 「今の抽出結果」を表す表であり、履歴を持つ表ではない。
	if _, err = tx.Exec(ctx,
		`DELETE FROM extracted_fields WHERE document_id = $1`, in.DocumentID); err != nil {
		return err
	}
	for _, f := range in.Fields {
		var bbox any
		if len(f.BBox) == 4 {
			b, _ := json.Marshal(map[string]int{
				"page": 1, "x": f.BBox[0], "y": f.BBox[1],
				"w": f.BBox[2], "h": f.BBox[3]})
			bbox = b
		}
		if _, err = tx.Exec(ctx, `
			INSERT INTO extracted_fields
			  (document_id, field_key, value_text, value_norm, confidence, bbox)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			in.DocumentID, f.Key, f.Value, nullStr(f.Norm),
			f.Confidence*100, bbox); err != nil {
			return fmt.Errorf("抽出項目を書けません(%s): %w", f.Key, err)
		}
	}

	// ── 登録番号（インボイス制度）の検査結果 ──
	//
	// 再処理でも積み増しにならないよう、上書きにする。
	// 【重要】国税庁に問い合わせた記録（looked_up_at 以下）は消さない。
	// 登録は取り消されることがあり、「いつ時点で登録があったか」に
	// 意味がある。読み直すたびに消すと、それを示せなくなる。
	if in.RegStatus > 0 {
		if _, err = tx.Exec(ctx, `
			INSERT INTO invoice_reg_checks (document_id, reg_no, status, why)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (document_id) DO UPDATE
			   SET reg_no = EXCLUDED.reg_no,
			       status = EXCLUDED.status,
			       why    = EXCLUDED.why,
			       checked_at = now()`,
			in.DocumentID, nullStr(in.RegNo), in.RegStatus, in.RegWhy); err != nil {
			return fmt.Errorf("登録番号の検査結果を書けません: %w", err)
		}
	}

	// ── 照合候補 ──
	//
	// ★ここを消してはいけない。閾値シミュレーションはこの表を集計するだけで
	//   成立する設計になっている。捨てると、スライダーを動かすたびに
	//   1万件を照合し直すことになり、1秒では終わらない。
	if _, err = tx.Exec(ctx,
		`DELETE FROM match_candidates WHERE document_id = $1`, in.DocumentID); err != nil {
		return err
	}
	if len(in.Candidates) > 0 {
		rows := make([][]any, 0, len(in.Candidates))
		for _, c := range in.Candidates {
			// 内訳と一緒に「どの表記で当たったか」も残す。
			// score_detail は jsonb なので、列を増やさずに足せる。
			d := map[string]any{}
			for k, v := range c.Detail {
				d[k] = v
			}
			if c.MatchedForm != "" {
				d["matched"] = c.MatchedForm
			}
			b, _ := json.Marshal(d)
			rows = append(rows, []any{in.DocumentID, c.PartnerID, c.Score, b, c.Rank})
		}
		// 50件を1件ずつ INSERT すると往復が50回になる。CopyFrom は1回で済む。
		if _, err = tx.CopyFrom(ctx,
			pgx.Identifier{"match_candidates"},
			[]string{"document_id", "partner_id", "score", "score_detail", "rank"},
			pgx.CopyFromRows(rows)); err != nil {
			return fmt.Errorf("照合候補を書けません: %w", err)
		}
	}

	// ── 確定結果 ──
	//
	// 再処理に備えて上書きにする。match_results は伝票につき1行
	// （003 で UNIQUE 制約）。
	if _, err = tx.Exec(ctx, `
		INSERT INTO match_results
		  (document_id, partner_id, score, decision, threshold_id)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (document_id) DO UPDATE
		   SET partner_id = EXCLUDED.partner_id,
		       score = EXCLUDED.score,
		       decision = EXCLUDED.decision,
		       threshold_id = EXCLUDED.threshold_id,
		       decided_at = now()`,
		in.DocumentID, in.PartnerID, in.Score, in.Decision, in.ThresholdID); err != nil {
		return fmt.Errorf("照合結果を書けません: %w", err)
	}

	// ── 伝票の状態 ──
	// 4:照合済（人の確認待ち・却下） 5:確定（自動承認）
	status := int16(4)
	if in.Decision == 1 {
		status = 5
	}
	if _, err = tx.Exec(ctx,
		`UPDATE documents SET status = $2, updated_at = now() WHERE id = $1`,
		in.DocumentID, status); err != nil {
		return err
	}

	// ── 監査ログ ──
	//
	// 自動承認は必ず threshold_id とともに残す。
	// 「なぜ人の確認を飛ばしたのか」を後から説明できない状態にしない。
	action, err := auditAction(in.Decision)
	if err != nil {
		return err
	}
	after, _ := json.Marshal(map[string]any{
		"partner_id": in.PartnerID, "score": in.Score, "why": in.Why,
	})
	if _, err = tx.Exec(ctx, `
		INSERT INTO audit_logs
		  (organization_id, actor_id, target_table, target_id, action, after, threshold_id)
		VALUES ($1, NULL, 'match_results', $2, $3, $4, $5)`,
		in.OrgID, in.DocumentID, action, after, in.ThresholdID); err != nil {
		return fmt.Errorf("監査ログを書けません: %w", err)
	}

	if err = s.finishJob(ctx, tx, in.JobID, in.WorkerID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// MarkDocumentError は打ち切った伝票に印を付ける。
// 9:エラー。画面に出して人が対処できるようにする。
func (s *Store) MarkDocumentError(ctx context.Context, docID int64) error {
	_, err := s.Pool.Exec(ctx,
		`UPDATE documents SET status = 9, updated_at = now() WHERE id = $1`, docID)
	return err
}

// auditAction は decision を監査ログの action へ移す。
//
// 対応表を直接引かず、関数にして未知の値でエラーにする。
//
// 【重要】以前は map を直接引いていた。三分岐に「要確認」（5）を足したとき
// 対応表を直し忘れ、map が空文字を返した。action は NOT NULL だが
// 空文字は通るので、そのまま入った。20枚のうち3件が
// 「何が起きたか書いていない監査ログ」になった。エラーは一切出ていない。
// 監査ログの目的は後から説明することなので、この壊れ方が一番まずい。
// 007 でDB側にも制約を入れたが、ここでも止める。
func auditAction(decision int16) (string, error) {
	switch decision {
	case 1:
		return "auto_approve", nil
	case 2:
		return "approve", nil
	case 3:
		return "update", nil
	case 4:
		return "reject", nil
	case 5:
		return "needs_review", nil
	}
	return "", fmt.Errorf("監査ログに書けない判定です: decision=%d", decision)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
