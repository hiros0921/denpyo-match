package settle

// Runner は突合を実際に回す。DB と C++ をつなぐ。
//
// 採点（Score / Decide）は同じパッケージの純粋な関数で、
// こちらは配線だけを持つ。テストは純粋な側に書く。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hiros0921/denpo-match/api/internal/core"
	"github.com/hiros0921/denpo-match/api/internal/decide"
	"github.com/hiros0921/denpo-match/api/internal/ledger"
	"github.com/hiros0921/denpo-match/api/internal/store"
)

type Runner struct {
	St   *store.Store
	Core *core.Runner
	// 一時ファイル置き場。C++ は Docker 越しに動くことがあるので、
	// このプロセスから見たパスと、C++ から見たパスの両方が要る。
	WorkDirHost      string
	WorkDirContainer string
}

// Stats は1回の突合の集計。
type Stats struct {
	Docs   int `json:"documents"` // 対象の伝票
	Auto   int `json:"auto"`      // 自動突合
	Review int `json:"review"`    // 要確認
	None   int `json:"none"`      // 相手なし
	Kept   int `json:"kept"`      // 前回と同じ結論なので書き直さなかった
}

// SettleClient は顧問先の受領伝票をまとめて突合する。
//
// 明細を取り込んだ直後と、手動の再実行で呼ばれる。
// 人が確定した伝票（status 2,5）は対象から外れる（SQL側で除外）。
func (r *Runner) SettleClient(ctx context.Context, orgID, clientID int64) (Stats, error) {
	var st Stats

	th := decide.Default
	if t, err := r.St.CurrentThreshold(ctx, orgID, clientID, 0); err == nil {
		th = decide.Threshold{ID: t.ID, Upper: t.Upper, Lower: t.Lower}
	}

	docs, err := r.St.SettleTargets(ctx, clientID)
	if err != nil {
		return st, err
	}
	st.Docs = len(docs)

	for _, d := range docs {
		res, cands, err := r.settleOne(ctx, clientID, d, th)
		if err != nil {
			return st, fmt.Errorf("伝票%dの突合に失敗: %w", d.DocumentID, err)
		}

		// 自動突合が確定済みの入出金を取り合わないよう、保存の前に見る。
		// 保存層にも同じ検査があるが、そちらは競合時の最後の砦。
		// ここで見ないと集計が実態とずれ、再実行のたびに降格の監査ログが重複する。
		if res.Status == StatusAuto && res.Best != nil {
			claimed, err := r.St.IsTxClaimed(ctx, res.Best.ID, d.DocumentID)
			if err != nil {
				return st, err
			}
			if claimed {
				res.Status = StatusReview
				res.Why = "同じ入出金に別の伝票が既に紐づいています。" +
					"合算振込か、偶然の同額です。人の確認が要ります。" + res.Why
			}
		}

		// 前回と同じ結論なら書き直さない。
		// 取り込みのたびに全伝票を書き直すと、監査ログが
		// 「settle_none の繰り返し」で埋まり、読む価値を失う。
		// 説明だけが変わったとき（範囲に無い→候補はあるが弱い）は、
		// 判断は同じなので監査ログ無しで説明だけ直す。
		if prev, ok, _ := r.St.GetSettlement(ctx, d.DocumentID); ok {
			sameTx := (prev.TransactionID == nil && res.Best == nil) ||
				(prev.TransactionID != nil && res.Best != nil &&
					*prev.TransactionID == res.Best.ID)
			if prev.Status == int16(res.Status) && sameTx {
				if prev.Why != res.Why {
					if err := r.St.RefreshSettlementWhy(ctx, d.DocumentID, res.Why); err != nil {
						return st, err
					}
				}
				st.Kept++
				continue
			}
		}

		save := store.SaveSettlement{
			OrgID:      orgID,
			DocumentID: d.DocumentID,
			Status:     int16(res.Status),
			Why:        res.Why,
		}
		switch res.Status {
		case StatusAuto:
			save.AuditAction = "settle_auto"
			st.Auto++
		case StatusReview:
			save.AuditAction = "settle_review"
			st.Review++
		default:
			save.AuditAction = "settle_none"
			st.None++
		}
		if res.Best != nil && res.Status != StatusNone {
			id := res.Best.ID
			sc := res.Best.Score
			save.TransactionID = &id
			save.Score = &sc
		}
		for _, c := range cands {
			save.Candidates = append(save.Candidates, store.SaveSettleCandidate{
				TransactionID: c.ID, Score: c.Score, NameScore: c.NameScore,
				AmountScore: c.AmountScore, DateScore: c.DateScore, Why: c.Why,
			})
		}
		if err := r.St.SaveSettlement(ctx, save); err != nil {
			return st, err
		}
	}
	return st, nil
}

func (r *Runner) settleOne(ctx context.Context, clientID int64, d store.SettleDoc,
	th decide.Threshold) (Result, []Scored, error) {

	txs, err := r.St.FindTxCandidates(ctx, clientID, d.Total, d.IssueDate)
	if err != nil {
		return Result{}, nil, err
	}
	if len(txs) == 0 {
		return Decide(nil, th), nil, nil
	}

	// 名前の採点。伝票側の名前（正式名＋別名＋帳票から読んだ発行元）を
	// マスタとして1件書き、摘要の正規化名を照会する。
	// 照合の実装は dm_match の1つだけ。ここで編集距離を書き直さない。
	canonical := d.PartnerName
	if canonical == "" {
		canonical = d.IssuerText
	}
	variants := append([]string{}, d.AliasNorms...)
	if d.IssuerText != "" && d.IssuerText != canonical {
		variants = append(variants, d.IssuerText)
	}

	scored := make([]Scored, 0, len(txs))
	if canonical == "" {
		// 名前が何も無い伝票。名前スコア0で金額と日付だけで採点する。
		for _, t := range txs {
			scored = append(scored, Score(docOf(d), txOf(t, 0)))
		}
		return Decide(scored, th), scored, nil
	}

	mastersHost, mastersCont, err := r.writeMasters(d.DocumentID, canonical, variants)
	if err != nil {
		return Result{}, nil, err
	}
	defer os.Remove(mastersHost)

	for _, t := range txs {
		name := 0.0
		if t.Norm != "" {
			m, err := r.Core.MatchWith(ctx, mastersCont, t.Norm, 1)
			if err != nil {
				return Result{}, nil, err
			}
			if len(m.Results) > 0 {
				name = m.Results[0].Score
			}
		}
		scored = append(scored, Score(docOf(d), txOf(t, name)))
	}
	return Decide(scored, th), scored, nil
}

func docOf(d store.SettleDoc) Doc {
	return Doc{Total: d.Total, IssueDate: d.IssueDate}
}

func txOf(t store.TxCandidate, nameScore float64) Tx {
	return Tx{ID: t.ID, Date: t.Date, Amount: t.Amount,
		Source: ledger.SourceType(t.Source), NameScore: nameScore}
}

func (r *Runner) writeMasters(docID int64, canonical string,
	variants []string) (host, cont string, err error) {

	body, err := json.Marshal([]map[string]any{{
		"id": 1, "canonical": canonical, "variants": variants,
	}})
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(r.WorkDirHost, 0o755); err != nil {
		return "", "", err
	}
	name := fmt.Sprintf("settle_%d.json", docID)
	host = filepath.Join(r.WorkDirHost, name)
	cont = filepath.Join(r.WorkDirContainer, name)
	if err := os.WriteFile(host, body, 0o644); err != nil {
		return "", "", err
	}
	return host, cont, nil
}
