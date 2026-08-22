package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// DocumentView は状態照会が返すもの。
//
// 進捗を返すのに、処理中のワーカーへ問い合わせに行かない。
// ワーカーは jobs へ進捗を書き、API は jobs を読むだけにする。
// こうしておけば、ワーカーが何台に増えても、途中で落ちても、
// 照会の側は同じ1本の SQL で答えられる。
type DocumentView struct {
	ID       int64  `json:"id"`
	ClientID int64  `json:"client_id"`
	DocType  int16  `json:"doc_type"`
	Status   int16  `json:"status"`
	StatusJa string `json:"status_ja"`

	Job *JobView `json:"job,omitempty"`

	Fields []FieldView `json:"fields,omitempty"`
	Result *ResultView `json:"result,omitempty"`

	UploadedAt time.Time `json:"uploaded_at"`
}

type JobView struct {
	Status    int16  `json:"status"`
	StatusJa  string `json:"status_ja"`
	Stage     string `json:"stage,omitempty"`
	Progress  int16  `json:"progress"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"last_error,omitempty"`
	WaitingMs *int64 `json:"waiting_ms,omitempty"` // 待機中なら、待ち始めてからの時間
}

type FieldView struct {
	Key        string  `json:"key"`
	Value      string  `json:"value"`
	Confidence float64 `json:"confidence"`
}

type ResultView struct {
	Decision    int16    `json:"decision"`
	DecisionJa  string   `json:"decision_ja"`
	PartnerID   *int64   `json:"partner_id,omitempty"`
	PartnerName string   `json:"partner_name,omitempty"`
	Score       *float64 `json:"score,omitempty"`
	ThresholdID *int64   `json:"threshold_id,omitempty"`
	// 上位の候補。人が確認するとき、2位以下が見えないと判断できない。
	Candidates []CandidateView `json:"candidates,omitempty"`
}

type CandidateView struct {
	PartnerID int64              `json:"partner_id"`
	Name      string             `json:"name"`
	Score     float64            `json:"score"`
	Rank      int16              `json:"rank"`
	Detail    map[string]float64 `json:"detail,omitempty"`
}

var docStatusJa = map[int16]string{
	1: "受付", 2: "前処理済", 3: "OCR済", 4: "照合済", 5: "確定", 9: "エラー",
}
var jobStatusJa = map[int16]string{
	1: "待機", 2: "処理中", 3: "完了", 4: "失敗",
}
var decisionJa = map[int16]string{
	1: "自動承認", 2: "承認", 3: "修正", 4: "却下", 5: "要確認",
}

// GetDocument は1枚の現況を返す。候補の上限は topCandidates。
func (s *Store) GetDocument(ctx context.Context, docID int64,
	topCandidates int) (*DocumentView, error) {

	var v DocumentView
	var j JobView
	var jobStatus *int16
	var stage, lastErr *string
	var progress *int16
	var attempts *int
	var runAfter *time.Time

	err := s.Pool.QueryRow(ctx, `
		SELECT d.id, d.client_id, d.doc_type, d.status, d.uploaded_at,
		       j.status, j.stage, j.progress, j.attempts, j.last_error, j.run_after
		  FROM documents d
		  LEFT JOIN LATERAL (
		    SELECT * FROM jobs WHERE document_id = d.id ORDER BY id DESC LIMIT 1
		  ) j ON true
		 WHERE d.id = $1`, docID).
		Scan(&v.ID, &v.ClientID, &v.DocType, &v.Status, &v.UploadedAt,
			&jobStatus, &stage, &progress, &attempts, &lastErr, &runAfter)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	v.StatusJa = docStatusJa[v.Status]

	if jobStatus != nil {
		j.Status = *jobStatus
		j.StatusJa = jobStatusJa[j.Status]
		if stage != nil {
			j.Stage = *stage
		}
		if progress != nil {
			j.Progress = *progress
		}
		if attempts != nil {
			j.Attempts = *attempts
		}
		if lastErr != nil {
			j.LastError = *lastErr
		}
		// 待機中は「あとどれくらいか」より「どれだけ待たされているか」を出す。
		// 順番待ちの長さは、現場が最初に気にする数字。
		if j.Status == 1 && runAfter != nil {
			ms := time.Since(*runAfter).Milliseconds()
			if ms < 0 {
				ms = 0
			}
			j.WaitingMs = &ms
		}
		v.Job = &j
	}

	rows, err := s.Pool.Query(ctx, `
		SELECT field_key, coalesce(value_text,''), coalesce(confidence,0)
		  FROM extracted_fields WHERE document_id = $1 ORDER BY field_key`, docID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var f FieldView
		if err := rows.Scan(&f.Key, &f.Value, &f.Confidence); err != nil {
			rows.Close()
			return nil, err
		}
		v.Fields = append(v.Fields, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var r ResultView
	var pname *string
	err = s.Pool.QueryRow(ctx, `
		SELECT m.decision, m.partner_id, p.name, m.score, m.threshold_id
		  FROM match_results m
		  LEFT JOIN partners p ON p.id = m.partner_id
		 WHERE m.document_id = $1`, docID).
		Scan(&r.Decision, &r.PartnerID, &pname, &r.Score, &r.ThresholdID)
	if err == nil {
		r.DecisionJa = decisionJa[r.Decision]
		if pname != nil {
			r.PartnerName = *pname
		}
		cs, err := s.topCandidates(ctx, docID, topCandidates)
		if err != nil {
			return nil, err
		}
		r.Candidates = cs
		v.Result = &r
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	return &v, nil
}

func (s *Store) topCandidates(ctx context.Context, docID int64, n int) ([]CandidateView, error) {
	if n <= 0 {
		return nil, nil
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT c.partner_id, p.name, c.score, c.rank, c.score_detail
		  FROM match_candidates c JOIN partners p ON p.id = c.partner_id
		 WHERE c.document_id = $1 ORDER BY c.rank LIMIT $2`, docID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CandidateView
	for rows.Next() {
		var c CandidateView
		var raw []byte
		if err := rows.Scan(&c.PartnerID, &c.Name, &c.Score, &c.Rank, &raw); err != nil {
			return nil, err
		}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &c.Detail)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ── 人による承認・修正・却下 ──

// Approve は人の判断を記録する。
//
// decision は 2:承認 3:修正 4:却下。
// 修正のとき（人が別の取引先を選び直したとき）は、その表記を
// partner_aliases に貯める。次から同じ表記が来たときに候補へ上がる。
// 現場が使うほど当たるようになる、という仕組みの中心がここ。
func (s *Store) Approve(ctx context.Context, orgID, docID, actorID int64,
	decision int16, partnerID *int64, learnAlias, aliasNorm string) error {

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// 【重要】組織を条件に入れる。orgID を監査ログにしか使わないと、
	// 他事務所の伝票を承認できてしまい、しかも監査ログには
	// 「承認した側の事務所」が残るので、記録を見ても気付けない。
	// ForgetAlias が同じ形で JOIN しているのに、ここだけ抜けていた。
	//
	// ハンドラ側でも所有を確かめている（httpapi/auth.go の ownDocument）。
	// 二重に見えるが、確認とデータ変更のあいだに割り込む余地を消すのと、
	// この関数を別の口から呼んだときに素通りしないようにするため、両方置く。
	var before []byte
	err = tx.QueryRow(ctx, `
		SELECT to_jsonb(m)
		  FROM match_results m
		  JOIN documents d ON d.id = m.document_id
		  JOIN clients   c ON c.id = d.client_id
		 WHERE m.document_id = $1 AND c.organization_id = $2`,
		docID, orgID).Scan(&before)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	var after []byte
	err = tx.QueryRow(ctx, `
		UPDATE match_results
		   SET decision = $2, partner_id = $3, decided_by = $4, decided_at = now()
		 WHERE document_id = $1
		RETURNING to_jsonb(match_results)`,
		docID, decision, partnerID, actorID).Scan(&after)
	if err != nil {
		return err
	}

	status := int16(5) // 確定
	if decision == 4 {
		status = 9 // 却下は人が対処する対象として残す
	}
	if _, err = tx.Exec(ctx,
		`UPDATE documents SET status = $2, updated_at = now() WHERE id = $1`,
		docID, status); err != nil {
		return err
	}

	// 表記揺れの自動学習。source=2。
	//
	// 【重要】覚えたことも監査ログに残す。
	// この仕組みは強力な反面、誤って覚えると同じ強さで悪化する。
	// 第7段階の検証で実際にそうなった（誤った組み合わせを1件登録したところ、
	// 正しい取引先が同点で2位に落ちた）。押し間違いは現場で必ず起きるので、
	// 「誰が・いつ・何を覚えさせたか」を残し、後から取り消せるようにする。
	if decision == 3 && partnerID != nil && learnAlias != "" {
		var aliasID int64
		if err = tx.QueryRow(ctx, `
			INSERT INTO partner_aliases (partner_id, alias, norm, source)
			VALUES ($1, $2, $3, 2) RETURNING id`,
			*partnerID, learnAlias, aliasNorm).Scan(&aliasID); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]any{
			"partner_id": *partnerID, "alias": learnAlias,
			"norm": aliasNorm, "document_id": docID,
		})
		if _, err = tx.Exec(ctx, `
			INSERT INTO audit_logs
			  (organization_id, actor_id, target_table, target_id, action, after)
			VALUES ($1, $2, 'partner_aliases', $3, 'learn_alias', $4)`,
			orgID, actorID, aliasID, payload); err != nil {
			return err
		}
	}

	action := map[int16]string{2: "approve", 3: "update", 4: "reject"}[decision]
	if _, err = tx.Exec(ctx, `
		INSERT INTO audit_logs
		  (organization_id, actor_id, target_table, target_id, action, before, after)
		VALUES ($1, $2, 'match_results', $3, $4, $5, $6)`,
		orgID, actorID, docID, action, before, after); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// LearnedAlias は人が覚えさせた表記。取り消せるように id を持つ。
type LearnedAlias struct {
	ID        int64     `json:"id"`
	PartnerID int64     `json:"partner_id"`
	Partner   string    `json:"partner"`
	Alias     string    `json:"alias"`
	Norm      string    `json:"norm"`
	CreatedAt time.Time `json:"created_at"`
}

// LearnedAliases は覚えた表記を一覧する（source=2 のみ）。
//
// 手動登録（source=1）は最初から用意された正しい揺れなので混ぜない。
// 画面で消せるのは「人が承認時に覚えさせたもの」だけにする。
func (s *Store) LearnedAliases(ctx context.Context, orgID int64,
	limit int) ([]LearnedAlias, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT a.id, p.id, p.name, a.alias, a.norm, a.created_at
		  FROM partner_aliases a
		  JOIN partners p ON p.id = a.partner_id
		  JOIN clients  c ON c.id = p.client_id
		 WHERE a.source = 2 AND c.organization_id = $1
		 ORDER BY a.id DESC LIMIT $2`, orgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LearnedAlias
	for rows.Next() {
		var a LearnedAlias
		if err := rows.Scan(&a.ID, &a.PartnerID, &a.Partner,
			&a.Alias, &a.Norm, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ForgetAlias は覚えた表記を取り消す。
//
// 行は消えるが、覚えた事実と取り消した事実は監査ログに残る
// （監査ログは追記のみで、消す手段が無い）。
// 「無かったことにする」のではなく「取り消した」を記録する。
func (s *Store) ForgetAlias(ctx context.Context, orgID, aliasID, actorID int64) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// 組織をまたいで消せないようにする。
	var before []byte
	err = tx.QueryRow(ctx, `
		SELECT to_jsonb(a) FROM partner_aliases a
		  JOIN partners p ON p.id = a.partner_id
		  JOIN clients  c ON c.id = p.client_id
		 WHERE a.id = $1 AND a.source = 2 AND c.organization_id = $2`,
		aliasID, orgID).Scan(&before)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	if _, err = tx.Exec(ctx,
		`DELETE FROM partner_aliases WHERE id = $1`, aliasID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `
		INSERT INTO audit_logs
		  (organization_id, actor_id, target_table, target_id, action, before)
		VALUES ($1, $2, 'partner_aliases', $3, 'forget_alias', $4)`,
		orgID, actorID, aliasID, before); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
