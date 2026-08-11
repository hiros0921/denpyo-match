// Package store は DB への読み書きをまとめる。
//
// 方針
//
//	① SQL はこの package の外に出さない。ハンドラやワーカーに SQL が散ると、
//	   インデックスを直したいときに影響範囲が分からなくなる。
//	② 1枚の伝票の処理結果は、1つのトランザクションで書く。
//	   照合結果だけ入って監査ログが入っていない、という状態を作らない。
//	③ 監査ログは dm_app ロールの権限で INSERT のみ。UPDATE/DELETE は
//	   DB側で拒否される（004_audit.sql）。ここで気をつけるのではなく、
//	   間違えても壊せない仕組みの上に乗る。
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ Pool *pgxpool.Pool }

func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("接続文字列を読めません: %w", err)
	}
	// ワーカー3台＋APIを見込んだ上限。Postgres の既定の接続上限は100。
	cfg.MaxConns = 10
	cfg.MaxConnLifetime = time.Hour

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("接続プールを作れません: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("DBに繋がりません: %w", err)
	}
	return &Store{Pool: pool}, nil
}

func (s *Store) Close() { s.Pool.Close() }

var ErrNotFound = errors.New("見つかりません")

// ── 伝票の受付 ──

type NewDocument struct {
	ClientID int64
	DocType  int16 // 1:請求書 2:納品書 3:領収書
	// 帳票の向き。1:受領 2:発行。0 なら顧問先の既定を使う。
	// 受領側では照合の相手が発行元になる。指定を忘れたときに
	// 「発行」で固定すると、受領専門の顧問先が黙って壊れるので、
	// 0 は「顧問先の設定に従う」であって「発行」ではない。
	Direction  int16
	UploadedBy *int64
	R2Key      string // 原本のオブジェクトキー

	// 元の紙のどこから切り出したか。
	// 1枚の紙に複数の伝票が写っていたとき、これが無いと
	// 画面に並んだ伝票と手元の紙を突き合わせられない。
	SourceName   string // アップロード時のファイル名
	SourcePage   int    // 何ページ目か（1から）
	SourceRegion int    // そのページの何番目か（1から）
	SourceBox    []byte // {x,y,w,h} のJSON。分けなかったときは nil
}

// CreateDocument は伝票・ページ・ジョブを1つのトランザクションで作る。
//
// 3つに分けて書くと、伝票だけできてジョブが無い伝票が生まれる。
// その伝票は永久に処理されず、画面には「受付」のまま残る。
// 現場から見ると「アップロードしたのに何も起きない」という最悪の状態になる。
func (s *Store) CreateDocument(ctx context.Context, d NewDocument) (docID, jobID int64, err error) {
	ids, jobs, err := s.CreateDocuments(ctx, []NewDocument{d})
	if err != nil {
		return 0, 0, err
	}
	return ids[0], jobs[0], nil
}

// CreateDocuments は複数の伝票をまとめて作る。
//
// 【重要】1件ずつ別のトランザクションにしない。
// レシート10枚まとめの PDF を入れて、途中で失敗したときに
// 「7件だけできた」状態になる。現場は10枚入れたつもりなので、
// 足りない3枚に気付くのは、月末に金額が合わなくなったときになる。
// 全部できるか、1件もできないか、のどちらかにする。
func (s *Store) CreateDocuments(ctx context.Context, ds []NewDocument) (
	docIDs, jobIDs []int64, err error) {
	if len(ds) == 0 {
		return nil, nil, fmt.Errorf("伝票がありません")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit 済みなら何も起きない

	for _, d := range ds {
		var docID, jobID int64

		// 向きの指定が無ければ顧問先の既定を引く。
		// COALESCE ではなく副問い合わせにするのは、既定を1箇所（clients）に
		// 置いておきたいため。アップロードの経路ごとに既定を書くと必ずずれる。
		err = tx.QueryRow(ctx, `
			INSERT INTO documents
			  (client_id, doc_type, direction, status, uploaded_by,
			   source_name, source_page, source_region, source_box)
			VALUES ($1, $2,
			        CASE WHEN $3::smallint IN (1,2) THEN $3::smallint
			             ELSE (SELECT default_direction FROM clients WHERE id = $1) END,
			        1, $4, $5, $6, $7, $8) RETURNING id`,
			d.ClientID, d.DocType, d.Direction, d.UploadedBy,
			nullStr(d.SourceName), max1(d.SourcePage), max1(d.SourceRegion),
			nullBytes(d.SourceBox)).Scan(&docID)
		if err != nil {
			return nil, nil, fmt.Errorf("伝票を作れません: %w", err)
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO document_pages (document_id, page_no, r2_key_original)
			VALUES ($1, 1, $2)`, docID, d.R2Key)
		if err != nil {
			return nil, nil, fmt.Errorf("ページを作れません: %w", err)
		}

		err = tx.QueryRow(ctx, `
			INSERT INTO jobs (document_id) VALUES ($1) RETURNING id`,
			docID).Scan(&jobID)
		if err != nil {
			return nil, nil, fmt.Errorf("ジョブを作れません: %w", err)
		}
		docIDs = append(docIDs, docID)
		jobIDs = append(jobIDs, jobID)
	}

	if err = tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return docIDs, jobIDs, nil
}

// 1ページ目・1件目を既定にする。0 を入れると CHECK に引っかかる。
func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// ── ジョブの待ち行列 ──

type Job struct {
	ID         int64
	DocumentID int64
	ClientID   int64
	DocType    int16
	// 顧問先ごとのエンジン。'vision' か 'tesseract'。
	// 【重要】これを取ってこないと、設定しても効かない。
	// 顧問先が「精度優先で Vision」と選んでいるのに Tesseract が動く、
	// という状態が、記録上は分からないまま続く。
	OcrEngine string
	// 帳票の向き。1:受領 2:発行。
	// 【重要】これを取ってこないと、受領側の顧問先で宛先（＝自社）を
	// 照合し続ける。毎回スコアが低く出るので「OCRが悪い」に見え、
	// 設定の問題だと気付くのが遅れる。
	Direction   int16
	Attempts    int
	MaxAttempts int
	R2KeyOrig   string
}

// Claim は処理する1件を取り出す。
//
//	FOR UPDATE SKIP LOCKED
//
// SKIP LOCKED が要るのは、ワーカーを増やしたときに先頭で詰まらせないため。
// 無いと、2台目は1台目が持っている行が空くまで待つ。待ち行列としては、
// 待つのではなく次の行へ進んでほしい。
//
// locked_until は「落ちたワーカーの持ち分を拾い直す」ための期限。
// 期限を過ぎた処理中の行も、この1本の SQL で一緒に拾う。
// 見張り役のプロセスを別に立てると、それ自体が落ちる。
func (s *Store) Claim(ctx context.Context, workerID string, lease time.Duration) (*Job, error) {
	var j Job
	err := s.Pool.QueryRow(ctx, `
		WITH picked AS (
		  SELECT id FROM jobs
		  WHERE run_after <= now()
		    AND (status = 1 OR (status = 2 AND locked_until < now()))
		  ORDER BY run_after
		  FOR UPDATE SKIP LOCKED
		  LIMIT 1
		)
		UPDATE jobs j
		   SET status = 2,
		       attempts = j.attempts + 1,
		       locked_by = $1,
		       locked_until = now() + $2::interval,
		       stage = NULL,
		       progress = 0,
		       updated_at = now()
		  FROM picked, documents d, document_pages p, clients c
		 WHERE j.id = picked.id
		   AND d.id = j.document_id
		   AND c.id = d.client_id
		   AND p.document_id = d.id AND p.page_no = 1
		RETURNING j.id, j.document_id, d.client_id, d.doc_type, d.direction,
		          j.attempts, j.max_attempts, p.r2_key_original, c.ocr_engine`,
		workerID, lease.String()).
		Scan(&j.ID, &j.DocumentID, &j.ClientID, &j.DocType, &j.Direction,
			&j.Attempts, &j.MaxAttempts, &j.R2KeyOrig, &j.OcrEngine)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // 待ち行列が空。エラーではない
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// Heartbeat は進捗を書き、同時に期限を延ばす。
//
// 進捗の更新と期限の延長を1本にするのは、忘れないため。
// 別々にすると、処理は進んでいるのに期限切れとみなされて
// 別のワーカーに二重に拾われる、という起きにくく直しにくい不具合になる。
func (s *Store) Heartbeat(ctx context.Context, jobID int64, stage string,
	progress int16, lease time.Duration) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE jobs SET stage = $2, progress = $3,
		       locked_until = now() + $4::interval, updated_at = now()
		 WHERE id = $1`, jobID, stage, progress, lease.String())
	return err
}

// Touch は期限だけを延ばす。工程が変わらない間も定期的に呼ぶ。
//
// 工程の切れ目でしか延ばさないと、1つの工程が期限より長引いたときに
// 期限切れになる。OCRは実測0.6秒だが上限は60秒あり、重い画像では
// その間ずっと無音になる。
func (s *Store) Touch(ctx context.Context, jobID int64, workerID string,
	lease time.Duration) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE jobs SET locked_until = now() + $3::interval, updated_at = now()
		 WHERE id = $1 AND locked_by = $2`, jobID, workerID, lease.String())
	return err
}

// Fail は失敗を記録する。試行回数が上限に達したら打ち切る。
//
// 待ち時間を回数に応じて延ばす（10秒 → 40秒 → 90秒）。
// 一定間隔で再試行すると、外部APIが落ちているときに叩き続けることになる。
func (s *Store) Fail(ctx context.Context, jobID int64, cause error) (givenUp bool, err error) {
	err = s.Pool.QueryRow(ctx, `
		UPDATE jobs
		   SET status = CASE WHEN attempts >= max_attempts THEN 4 ELSE 1 END,
		       last_error = $2,
		       run_after = now() + (attempts * attempts * 10 || ' seconds')::interval,
		       locked_by = NULL, locked_until = NULL, updated_at = now()
		 WHERE id = $1
		RETURNING status = 4`, jobID, cause.Error()).Scan(&givenUp)
	return givenUp, err
}

// ErrLostJob は、書き込もうとしたときに持ち分を失っていた場合。
var ErrLostJob = errors.New("このジョブは既に別のワーカーが持っています")

// finishJob は完了にする。ただし自分がまだ持っている場合だけ。
//
// 【重要】locked_by の一致を条件に入れる。
//
// 期限が切れると、処理中でも別のワーカーが同じジョブを拾う。
// 拾い直しは必要な仕組み（落ちた台の持ち分を放置しないため）だが、
// 元の台が生きていた場合、2台が同じ伝票を最後まで処理して
// 両方が結果を書く。実測で起きた:
//
//	伝票10048 の監査ログが 37.5秒差で2行。リースは30秒だった。
//	OCRの上限が60秒なので、OCRの最中に期限が切れていた。
//
// 心拍でリースを延ばす対策も入れたが、それだけでは足りない。
// 心拍が遅れる／DBが一時的に応答しない、といった理由で期限は切れうる。
// 「書く直前に自分の持ち分かを確かめ、違えば書かない」を最後の砦にする。
// 条件に合わなければ0行更新になるので、呼び出し側でトランザクションごと捨てる。
func (s *Store) finishJob(ctx context.Context, tx pgx.Tx, jobID int64,
	workerID string) error {
	tag, err := tx.Exec(ctx, `
		UPDATE jobs SET status = 3, stage = 'decide', progress = 100,
		       locked_by = NULL, locked_until = NULL, last_error = NULL,
		       updated_at = now()
		 WHERE id = $1 AND status = 2 AND locked_by = $2`, jobID, workerID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLostJob
	}
	return nil
}

// ── 取引先マスタ ──

type Partner struct {
	ID   int64
	Name string
	Norm string
	// 覚えた表記。C++ の採点はこれも見る。
	// 人が承認画面で直したときに貯まったもの（source=2）と、手動登録（source=1）。
	Aliases []string
}

// Candidates は pg_trgm で候補を絞る。第1段。
//
// ここで1万件を50件まで落とし、精密な採点は C++ に渡す。
// 全件を C++ に渡すと、1万件の編集距離計算になり目標の50msに入らない。
//
// 別名（partner_aliases）も対象にする。人が承認画面で修正したときに
// 貯まる表記なので、使うほど当たるようになる。
// 同じ取引先が本名と別名の両方で当たることがあるので、id で束ねる。
//
// 【注意】採点は正式名称に対して行う（この後 C++ に canonical だけを渡す）。
// 別名で引っ掛かったが正式名称とは字面が遠い、という取引先は
// スコアが伸びない。別名を採点にどう混ぜるかは、実際に別名が
// 貯まってから実測して決める。今きめても検証する材料が無い。
// 本名と別名の両方で当たったときは、高いほうの類似度で並べる（max）。
// 単純に UNION すると同じ取引先が2行出て、50件の枠を食い合う。
func (s *Store) candidatesBy(ctx context.Context, sql string,
	args ...any) ([]Partner, error) {
	rows, err := s.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("候補生成に失敗: %w", err)
	}
	defer rows.Close()

	var out []Partner
	for rows.Next() {
		var p Partner
		var sim float64
		if err := rows.Scan(&p.ID, &p.Name, &p.Norm, &sim); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, s.attachAliases(ctx, out)
}

// attachAliases は候補に覚えた表記を付ける。
//
// 【重要】これを渡さないと、人が承認画面で直しても次に効かない。実測:
//
//	コウヨウエ業（OCRが工をエと誤読）で照会
//	  別名なし  コウヨウ産業 67.9 / コウヨウ工業 67.9   同点で却下
//	  別名あり  コウヨウ工業 100.0 / コウヨウ産業 67.9  分離して自動承認
//
// 候補生成（pg_trgm）は別名で正解を1位に上げていたが、
// 採点が正式名称だけを見ていたため、そこで元に戻っていた。
//
// 1件ずつ引くと候補50件で50往復になる。まとめて1回で引く。
func (s *Store) attachAliases(ctx context.Context, ps []Partner) error {
	if len(ps) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(ps))
	for _, p := range ps {
		ids = append(ids, p.ID)
	}
	rows, err := s.Pool.Query(ctx,
		`SELECT partner_id, alias FROM partner_aliases WHERE partner_id = ANY($1)`, ids)
	if err != nil {
		return fmt.Errorf("別名を読めません: %w", err)
	}
	defer rows.Close()

	byID := map[int64][]string{}
	for rows.Next() {
		var id int64
		var a string
		if err := rows.Scan(&id, &a); err != nil {
			return err
		}
		byID[id] = append(byID[id], a)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range ps {
		ps[i].Aliases = byID[ps[i].ID]
	}
	return nil
}

const sqlCandidates = `
	WITH hit AS (
	  SELECT p.id, similarity(p.norm, $2) AS sim
	    FROM partners p
	   WHERE p.client_id = $1 AND p.norm % $2
	  UNION ALL
	  SELECT a.partner_id, similarity(a.norm, $2)
	    FROM partner_aliases a JOIN partners p ON p.id = a.partner_id
	   WHERE p.client_id = $1 AND a.norm % $2
	)
	SELECT p.id, p.name, p.norm, max(h.sim) AS sim
	  FROM hit h JOIN partners p ON p.id = h.id
	 GROUP BY p.id, p.name, p.norm
	 ORDER BY sim DESC, p.id
	 LIMIT $3`

func (s *Store) Candidates(ctx context.Context, clientID int64, norm string,
	limit int) ([]Partner, error) {
	return s.candidatesBy(ctx, sqlCandidates, clientID, norm, limit)
}

// UpsertPartner は取引先を登録する。norm は呼び出し側が C++ で計算して渡す。
// ここで Go の正規化を書かない（実装が2つになると必ずずれる）。
func (s *Store) UpsertPartner(ctx context.Context, clientID int64,
	name, norm string) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO partners (client_id, name, norm) VALUES ($1, $2, $3)
		RETURNING id`, clientID, name, norm).Scan(&id)
	return id, err
}

func (s *Store) AddAlias(ctx context.Context, partnerID int64,
	alias, norm string, source int16) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO partner_aliases (partner_id, alias, norm, source)
		VALUES ($1, $2, $3, $4)`, partnerID, alias, norm, source)
	return err
}

// ── 閾値 ──

type Threshold struct {
	ID    int64
	Upper float64
	Lower float64
}

// CurrentThreshold は適用すべき設定を1件返す。
//
// 優先順位は取引先 > 顧問先 > 組織、帳票種別の一致を優先。
// 「この顧問先のこの帳票種別だけ厳しくしたい」が現場から必ず出る。
// 該当が無ければ呼び出し側が既定値を使う。
func (s *Store) CurrentThreshold(ctx context.Context, orgID, clientID int64,
	docType int16) (*Threshold, error) {
	var t Threshold
	err := s.Pool.QueryRow(ctx, `
		SELECT id, upper, lower FROM thresholds
		 WHERE organization_id = $1
		   AND valid_to IS NULL
		   AND (client_id IS NULL OR client_id = $2)
		   AND (doc_type  IS NULL OR doc_type  = $3)
		 ORDER BY (partner_id IS NOT NULL) DESC,
		          (client_id  IS NOT NULL) DESC,
		          (doc_type   IS NOT NULL) DESC,
		          valid_from DESC
		 LIMIT 1`, orgID, clientID, docType).Scan(&t.ID, &t.Upper, &t.Lower)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// OrgIDForClient は顧問先から組織を引く。
// アップロードのたびに呼ばれるので、clients の主キーで1回引くだけにする。
func (s *Store) OrgIDForClient(ctx context.Context, clientID int64) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx,
		`SELECT organization_id FROM clients WHERE id = $1`, clientID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}
