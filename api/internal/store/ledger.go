package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hiros0921/denpo-match/api/internal/ledger"
)

// ErrDuplicateFile は同じファイルを二重に取り込もうとしたとき。
var ErrDuplicateFile = errors.New("このファイルは取り込み済みです")

// ImportRow は取り込む1行。正規化済みの名前を持つ。
//
// 正規化は呼び出し側が C++ でまとめて行う。ここでやらないのは、
// store が core（プロセス起動）に依存すると、テストのたびに
// C++ のビルドが要るようになるため。
type ImportRow struct {
	ledger.Row
	NormalizedName string
}

// ImportBatch は1ファイルぶんの取り込みを1トランザクションで行う。
//
// 行の重複（期間の重なった再取り込み）は指紋で静かに読み飛ばし、
// 件数として返す。ファイルの重複は ErrDuplicateFile で止める。
// 「1件も入らなかった」と「半分入った」を呼び出し側が区別できるよう、
// 入った件数と飛ばした件数の両方を返す。
func (s *Store) ImportBatch(ctx context.Context, clientID int64,
	src ledger.SourceType, filename, fileSHA256 string, rows []ImportRow,
	importedBy *int64) (batchID int64, inserted, skipped int, err error) {

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	err = tx.QueryRow(ctx, `
		INSERT INTO import_batches
		  (client_id, source_type, filename, file_sha256, imported_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (client_id, file_sha256) DO NOTHING
		RETURNING id`,
		clientID, src, filename, fileSHA256, importedBy).Scan(&batchID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, 0, ErrDuplicateFile
	}
	if err != nil {
		return 0, 0, 0, fmt.Errorf("取り込みの台帳を書けません: %w", err)
	}

	for _, r := range rows {
		raw, _ := json.Marshal(r.Raw)
		ct, err := tx.Exec(ctx, `
			INSERT INTO transactions
			  (client_id, batch_id, source_type, transaction_date, amount,
			   direction, description, normalized_name, raw_data, row_fingerprint)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (client_id, row_fingerprint) DO NOTHING`,
			clientID, batchID, src, r.Date, r.Amount, r.Direction,
			r.Description, r.NormalizedName, raw, r.Fingerprint(src))
		if err != nil {
			return 0, 0, 0, fmt.Errorf("入出金を書けません: %w", err)
		}
		if ct.RowsAffected() == 0 {
			skipped++
		} else {
			inserted++
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE import_batches SET row_count = $2, skipped_count = $3 WHERE id = $1`,
		batchID, inserted, skipped); err != nil {
		return 0, 0, 0, err
	}
	return batchID, inserted, skipped, tx.Commit(ctx)
}

// TxCandidate は突合候補になる入出金。
type TxCandidate struct {
	ID     int64
	Date   time.Time
	Amount int64
	Source int16
	Norm   string
	Desc   string
}

// FindTxCandidates は金額と日付の窓で候補を絞る。
//
// 名前では絞らない。摘要がカナ読みで、名前のスコアが低いのは
// 実測で分かっている（初回 20〜50）。名前で絞ると初回が全部落ちる。
// 金額で絞れば候補は数件になるので、名前の採点は全候補にかけられる。
func (s *Store) FindTxCandidates(ctx context.Context, clientID, total int64,
	issueDate time.Time) ([]TxCandidate, error) {

	// 日付が読めていない伝票は窓を掛けない。金額だけで絞る。
	var from, to any
	if !issueDate.IsZero() {
		from = issueDate.AddDate(0, 0, -5)
		to = issueDate.AddDate(0, 0, 75)
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, transaction_date, amount, source_type, normalized_name, description
		  FROM transactions
		 WHERE client_id = $1
		   AND direction = 2
		   AND (amount = $2
		        OR (source_type = 1 AND amount BETWEEN $2 - 880 AND $2))
		   AND ($3::date IS NULL OR transaction_date BETWEEN $3 AND $4)
		 ORDER BY transaction_date`,
		clientID, total, from, to)
	if err != nil {
		return nil, fmt.Errorf("突合候補を読めません: %w", err)
	}
	defer rows.Close()

	var out []TxCandidate
	for rows.Next() {
		var c TxCandidate
		if err := rows.Scan(&c.ID, &c.Date, &c.Amount, &c.Source,
			&c.Norm, &c.Desc); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SettleDoc は突合の対象になる伝票。
type SettleDoc struct {
	DocumentID  int64
	Total       int64
	IssueDate   time.Time // 読めていなければゼロ値
	PartnerID   *int64    // 取引先照合の結果。無ければ nil
	PartnerName string    // 取引先の正式名。無ければ空
	IssuerText  string    // 帳票から読んだ発行元（生の文字列）
	// 学習した別名の【正規化後】の文字列。生の別名ではない。
	//
	// 【重要】ここで partner_aliases.alias を渡してはいけない。
	// 銀行摘要の別名は生のまま「ｶ)ﾐﾗｲﾊｲｿｳｻｰﾋﾞｽ」で保存されており、
	// dm_match はマスタ側を既定の正規化（--bank なし）で処理するため
	// 「カ)ミライハイソウサービス」になる。一方、照会する摘要は
	// --bank 付きで「ミライハイソウサービス」に落ちている。
	// 「カ)」の分だけずれて、学習済みなのに名前スコアが 100 にならない。
	// 実測: 生の別名を渡すと 75.8、正規化後を渡すと 100。
	//
	// norm は学習時に --bank 付きで計算済みなので、それをそのまま渡す。
	// 再度正規化されても同じ文字列に落ちる（冪等）。
	AliasNorms []string
}

// SettleTargets は突合すべき伝票を返す。
//
// 受領（direction=1）だけ。発行側の消込（入金の突合）は別の絵になる。
// 人が確定した伝票（status 2,5）は上書きしない。
func (s *Store) SettleTargets(ctx context.Context, clientID int64) ([]SettleDoc, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT d.id,
		       (SELECT value_text FROM extracted_fields
		         WHERE document_id = d.id AND field_key = 'total' LIMIT 1),
		       (SELECT value_text FROM extracted_fields
		         WHERE document_id = d.id AND field_key = 'issue_date' LIMIT 1),
		       mr.partner_id, p.name,
		       coalesce((SELECT value_text FROM extracted_fields
		         WHERE document_id = d.id AND field_key = 'issuer_name' LIMIT 1), '')
		  FROM documents d
		  LEFT JOIN match_results mr ON mr.document_id = d.id
		  LEFT JOIN partners p ON p.id = mr.partner_id
		 WHERE d.client_id = $1
		   AND d.direction = 1
		   AND NOT EXISTS (SELECT 1 FROM settlements st
		                    WHERE st.document_id = d.id AND st.status IN (2,5))
		 ORDER BY d.id`, clientID)
	if err != nil {
		return nil, fmt.Errorf("突合対象を読めません: %w", err)
	}
	defer rows.Close()

	var out []SettleDoc
	for rows.Next() {
		var (
			doc         SettleDoc
			totalText   *string
			dateText    *string
			partnerName *string
		)
		if err := rows.Scan(&doc.DocumentID, &totalText, &dateText,
			&doc.PartnerID, &partnerName, &doc.IssuerText); err != nil {
			return nil, err
		}
		if totalText == nil || *totalText == "" {
			// 金額が読めていない伝票は突合できない。対象から外す。
			// 「読めていないから突合していない」ことは画面で見える
			// （settlements に行が無い＝未突合）。
			continue
		}
		fmt.Sscan(*totalText, &doc.Total)
		if doc.Total <= 0 {
			continue
		}
		if dateText != nil {
			if t, err := time.Parse("2006-01-02", *dateText); err == nil {
				doc.IssueDate = t
			}
		}
		if partnerName != nil {
			doc.PartnerName = *partnerName
		}
		out = append(out, doc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 別名をまとめて付ける
	ids := make([]int64, 0, len(out))
	for _, d := range out {
		if d.PartnerID != nil {
			ids = append(ids, *d.PartnerID)
		}
	}
	if len(ids) > 0 {
		ar, err := s.Pool.Query(ctx,
			`SELECT partner_id, norm FROM partner_aliases WHERE partner_id = ANY($1)`, ids)
		if err != nil {
			return nil, err
		}
		defer ar.Close()
		byID := map[int64][]string{}
		for ar.Next() {
			var id int64
			var a string
			if err := ar.Scan(&id, &a); err != nil {
				return nil, err
			}
			byID[id] = append(byID[id], a)
		}
		if err := ar.Err(); err != nil {
			return nil, err
		}
		for i := range out {
			if out[i].PartnerID != nil {
				out[i].AliasNorms = byID[*out[i].PartnerID]
			}
		}
	}
	return out, nil
}

// Settlement は今の結論。再実行時に「変わっていないなら書かない」ために読む。
type Settlement struct {
	Status        int16
	TransactionID *int64
	Why           string
}

// GetSettlement は伝票の突合の結論を返す。無ければ ok=false。
func (s *Store) GetSettlement(ctx context.Context, docID int64) (Settlement, bool, error) {
	var out Settlement
	err := s.Pool.QueryRow(ctx, `
		SELECT status, transaction_id, why FROM settlements WHERE document_id = $1`,
		docID).Scan(&out.Status, &out.TransactionID, &out.Why)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, false, nil
	}
	if err != nil {
		return out, false, err
	}
	return out, true, nil
}

// IsTxClaimed は入出金が既にどこかの伝票に紐づいているかを返す。
//
// 実行側が保存の前に見る。保存層にも同じ検査があるが（競合時の最後の砦）、
// 実行側で先に見ないと、集計が実態とずれ、再実行のたびに
// 降格の監査ログが重複して書かれる。
func (s *Store) IsTxClaimed(ctx context.Context, txID, excludeDocID int64) (bool, error) {
	var claimed bool
	err := s.Pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM settlements
		  WHERE transaction_id = $1 AND status IN (1,2) AND document_id <> $2)`,
		txID, excludeDocID).Scan(&claimed)
	return claimed, err
}

// RefreshSettlementWhy は結論を変えずに説明だけを直す。
//
// 「相手なし（範囲に入出金が無い）」で記録された伝票が、明細の取り込み後の
// 再計算で「候補はあるが弱い」に変わることがある。結論（相手なし）は同じでも、
// 現場が次に取る行動が違う（明細を足す／候補を目で見る）ので、説明は直す。
// 判断は変わっていないので、監査ログは書かない。
func (s *Store) RefreshSettlementWhy(ctx context.Context, docID int64, why string) error {
	_, err := s.Pool.Exec(ctx,
		`UPDATE settlements SET why = $2 WHERE document_id = $1`, docID, why)
	return err
}

// SaveSettlement は突合の結果を書く。候補と結論と監査ログを1トランザクションで。
type SaveSettlement struct {
	OrgID      int64
	DocumentID int64
	// 結論。settle.Status の値
	Status int16
	// 紐づけた入出金。相手なしのときは nil
	TransactionID *int64
	Score         *float64
	Why           string
	ActorID       *int64 // 自動なら nil
	// 候補（順位つき）。画面で「なぜこれが1位か」を見せる
	Candidates []SaveSettleCandidate
	// 監査ログの action
	AuditAction string
	// 選ばれた候補が曖昧（摘要が複数の取引先に一致）だったときの根拠。
	// nil なら曖昧ではない。「なぜ自動承認されなかったのか」を
	// 後から説明できるよう、監査ログの after にそのまま記録する。
	Ambiguity *AmbiguityAudit
}

// AmbiguityAudit は監査ログに残す曖昧さの根拠。
// 摘要の正規形・前方一致した取引先の一覧・適用した上限値。
type AmbiguityAudit struct {
	Norm    string   `json:"norm"`
	Matched []string `json:"matched"`
	Cap     float64  `json:"cap"`
	Forced  bool     `json:"forced"`
}

type SaveSettleCandidate struct {
	TransactionID int64
	Score         float64
	NameScore     float64
	AmountScore   float64
	DateScore     float64
	Why           string
}

func (s *Store) SaveSettlement(ctx context.Context, in SaveSettlement) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// 自動突合が、確定済みの入出金を取り合わないようにする。
	//
	// 1回の振込で複数の請求をまとめて払う「合算振込」があるので、
	// 人は同じ入出金に複数の伝票を紐づけられる（DBでは禁止しない）。
	// 自動だけが遠慮する。金額完全一致が前提の自動突合では、
	// 同じ入出金が2枚の伝票に合うのは偶然の同額であり、
	// どちらかは誤りだから。
	if in.Status == 1 && in.TransactionID != nil {
		var claimed bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM settlements
			  WHERE transaction_id = $1 AND status IN (1,2) AND document_id <> $2)`,
			*in.TransactionID, in.DocumentID).Scan(&claimed); err != nil {
			return err
		}
		if claimed {
			in.Status = 3 // 要確認へ落とす
			in.Why = "同じ入出金に別の伝票が既に紐づいています。" +
				"合算振込か、偶然の同額です。人の確認が要ります。" + in.Why
			in.AuditAction = "settle_review"
		}
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM settlement_candidates WHERE document_id = $1`,
		in.DocumentID); err != nil {
		return err
	}
	for i, c := range in.Candidates {
		if _, err := tx.Exec(ctx, `
			INSERT INTO settlement_candidates
			  (document_id, transaction_id, rank, score,
			   name_score, amount_score, date_score, why)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			in.DocumentID, c.TransactionID, i+1, c.Score,
			c.NameScore, c.AmountScore, c.DateScore, c.Why); err != nil {
			return fmt.Errorf("突合候補を書けません: %w", err)
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO settlements
		  (document_id, transaction_id, status, score, why, decided_by, decided_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())
		ON CONFLICT (document_id) DO UPDATE
		   SET transaction_id = EXCLUDED.transaction_id,
		       status = EXCLUDED.status,
		       score = EXCLUDED.score,
		       why = EXCLUDED.why,
		       decided_by = EXCLUDED.decided_by,
		       decided_at = now()`,
		in.DocumentID, in.TransactionID, in.Status, in.Score, in.Why,
		in.ActorID); err != nil {
		return fmt.Errorf("突合の結論を書けません: %w", err)
	}

	afterFields := map[string]any{
		"status": in.Status, "transaction_id": in.TransactionID,
		"score": in.Score, "why": in.Why,
	}
	if in.Ambiguity != nil {
		afterFields["ambiguity"] = in.Ambiguity
	}
	after, _ := json.Marshal(afterFields)
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs
		  (organization_id, actor_id, target_table, target_id, action, after)
		VALUES ($1, $2, 'settlements', $3, $4, $5)`,
		in.OrgID, in.ActorID, in.DocumentID, in.AuditAction, after); err != nil {
		return fmt.Errorf("監査ログを書けません: %w", err)
	}
	return tx.Commit(ctx)
}

// ── 摘要の桁切れによる曖昧さ ──

// Ambiguity は「この摘要は複数の取引先に一致する」という判定結果。
type Ambiguity struct {
	// 摘要の正規形。前方一致に使った文字列。
	Norm string
	// 前方一致した取引先の名前。2件以上なら曖昧。
	Matched []string
	// 桁が上限に達している＝切れている可能性がある。
	// これが false なら、何件一致しても曖昧とはみなさない。
	AtLimit bool
}

func (a Ambiguity) IsAmbiguous() bool { return a.AtLimit && len(a.Matched) >= 2 }

// CheckAmbiguity は摘要が複数の取引先に一致しないかを1クエリで調べる。
//
// ── 判定の順序 ──
//
//	① その取り込みで一番長い摘要の桁数 M を出す
//	② M に達している行が2行以上あるときだけ、M を「桁の上限」とみなす
//	   （切れていない取り込みでも最長の1行は必ずあるので、
//	    1行だけなら桁の上限ではなく、たまたま長い摘要とみなす）
//	③ この行が M に達していなければ、切れていない。曖昧ではない
//	④ 達していれば、正規形で始まる取引先を引く。2件以上なら曖昧
//
// ②③が要る理由は実測にある。この条件が無いと、短い社名が長い社名の
// 前方一致になる組（見本印刷／見本印刷工業）で、切れていない摘要まで
// 永久に曖昧扱いになり、無切断で115件中15件の自動突合を失った。
// 条件を付けたら115件のまま変わらなかった。
//
// 濁点を落とした形でも引くのは、半角カナで濁点が1桁を食うため。
// ｷﾞ が ｷ に化けると、濁点ありだけでは正解自身が前方一致しない。
func (s *Store) CheckAmbiguity(ctx context.Context, clientID, txID int64) (Ambiguity, error) {
	var out Ambiguity
	err := s.Pool.QueryRow(ctx, `
		WITH t AS (
		  SELECT batch_id, normalized_name AS norm, char_length(description) AS len
		    FROM transactions WHERE id = $2 AND client_id = $1
		),
		b AS (
		  SELECT max(char_length(x.description)) AS maxlen,
		         count(*) AS total
		    FROM transactions x, t WHERE x.batch_id = t.batch_id
		),
		atmax AS (
		  SELECT count(*) AS n
		    FROM transactions x, t, b
		   WHERE x.batch_id = t.batch_id
		     AND char_length(x.description) = b.maxlen
		),
		f AS (
		  SELECT t.norm,
		         (t.len >= b.maxlen AND atmax.n >= 2 AND t.norm <> '') AS at_limit
		    FROM t, b, atmax
		),
		-- LIKE のメタ文字を無効化する。正規形は原則カナと英数だが、
		-- % や _ が混ざると前方一致が別物になる。
		pat AS (
		  SELECT f.norm, f.at_limit,
		         replace(replace(replace(f.norm,'\','\\'),'%','\%'),'_','\_') || '%' AS p,
		         replace(replace(replace(dm_strip_daku(f.norm),'\','\\'),'%','\%'),'_','\_') || '%' AS pd
		    FROM f
		),
		m AS (
		  SELECT DISTINCT p.name
		    FROM partners p, pat
		   WHERE p.client_id = $1 AND pat.at_limit
		     AND (p.norm LIKE pat.p OR dm_strip_daku(p.norm) LIKE pat.pd)
		  UNION
		  SELECT DISTINCT p.name
		    FROM partner_aliases a
		    JOIN partners p ON p.id = a.partner_id, pat
		   WHERE p.client_id = $1 AND pat.at_limit
		     AND (a.norm LIKE pat.p OR dm_strip_daku(a.norm) LIKE pat.pd)
		)
		SELECT pat.norm, pat.at_limit,
		       coalesce((SELECT array_agg(name ORDER BY name) FROM m), '{}')
		  FROM pat`,
		clientID, txID).Scan(&out.Norm, &out.AtLimit, &out.Matched)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	return out, err
}
