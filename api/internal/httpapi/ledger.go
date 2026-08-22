package httpapi

// 入出金の取り込みと突合。
//
// 取り込み口はここ（multipart の CSV）。解釈は ledger パッケージ、
// 正規化は C++、保存は store、突合は settle。この関数は配線だけ。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/hiros0921/denpo-match/api/internal/ledger"
	"github.com/hiros0921/denpo-match/api/internal/settle"
	"github.com/hiros0921/denpo-match/api/internal/store"
)

// POST /v1/transactions/import
//
//	multipart/form-data
//	  file         CSV（必須）。Shift_JIS でも UTF-8 でもよい
//	  client_id    顧問先（必須）
//	  source_type  1:銀行 2:カード（必須）
//
// 取り込んだら、その顧問先の受領伝票をまとめて突合する。
// 応答に突合の集計も入れる。「取り込んだ。で、何件合った？」が
// 現場が次に必ず聞くことなので、同じ応答で返す。
func (s *Server) importTransactions(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.MaxUpload)
	if err := r.ParseMultipartForm(s.MaxUpload); err != nil {
		writeErr(w, http.StatusBadRequest, "ファイルを受け取れません")
		return
	}
	clientID, err := strconv.ParseInt(r.FormValue("client_id"), 10, 64)
	if err != nil || clientID <= 0 {
		writeErr(w, http.StatusBadRequest, "client_id を指定してください")
		return
	}
	// 他事務所の顧問先へ明細を取り込ませない。
	if s.denyOwn(w, s.ownClient(r, clientID)) {
		return
	}
	srcN, _ := strconv.Atoi(r.FormValue("source_type"))
	src := ledger.SourceType(srcN)
	if src != ledger.Bank && src != ledger.Card {
		writeErr(w, http.StatusBadRequest, "source_type は 1:銀行 2:カード のいずれかです")
		return
	}

	f, fh, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "file が付いていません")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "ファイルを読めません")
		return
	}

	rows, err := ledger.Parse(src, data)
	if err != nil {
		// 何行目が読めないかまで含めて返す。現場が直せるのは
		// 「ファイルのどこが悪いか」が分かるときだけ。
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// 摘要の正規化。C++ にまとめて渡す。
	descs := make([]string, len(rows))
	for i, row := range rows {
		descs[i] = row.Description
	}
	norms, err := s.Norm.NormalizeBank(r.Context(), descs)
	if err != nil {
		s.Log.Error("正規化に失敗", "err", err)
		writeErr(w, http.StatusInternalServerError, "摘要の正規化に失敗しました")
		return
	}
	irows := make([]store.ImportRow, len(rows))
	for i := range rows {
		irows[i] = store.ImportRow{Row: rows[i], NormalizedName: norms[i]}
	}

	sum := sha256.Sum256(data)
	batchID, inserted, skipped, err := s.St.ImportBatch(r.Context(), clientID,
		src, fh.Filename, hex.EncodeToString(sum[:]), irows, nil)
	if errors.Is(err, store.ErrDuplicateFile) {
		writeErr(w, http.StatusConflict,
			"このファイルは取り込み済みです。内容を変えた場合はファイル名ではなく中身で判定しています")
		return
	}
	if err != nil {
		s.Log.Error("取り込みに失敗", "err", err)
		writeErr(w, http.StatusInternalServerError, "取り込めませんでした")
		return
	}

	out := map[string]any{
		"batch_id": batchID,
		"rows":     inserted,
		"skipped":  skipped,
	}

	// 続けて突合。失敗しても取り込み自体は成立しているので、
	// エラーにせず「突合は失敗した」と応答に書く。
	if s.Settle != nil {
		orgID, err := s.St.OrgIDForClient(r.Context(), clientID)
		if err == nil {
			if stats, err := s.Settle.SettleClient(r.Context(), orgID, clientID); err == nil {
				out["settle"] = stats
			} else {
				s.Log.Error("突合に失敗", "err", err)
				out["settle_error"] = "取り込みは完了しましたが、突合に失敗しました。再実行できます"
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /v1/settlements/run
//
//	{ "client_id": 1 }
//
// 突合だけを再実行する。別名を覚えさせた後や、閾値を変えた後に使う。
func (s *Server) runSettlements(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ClientID int64 `json:"client_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.ClientID <= 0 {
		writeErr(w, http.StatusBadRequest, "client_id を指定してください")
		return
	}
	if s.Settle == nil {
		writeErr(w, http.StatusServiceUnavailable, "突合が設定されていません")
		return
	}
	if s.denyOwn(w, s.ownClient(r, in.ClientID)) {
		return
	}
	orgID, _ := orgFrom(r.Context())
	stats, err := s.Settle.SettleClient(r.Context(), orgID, in.ClientID)
	if err != nil {
		s.Log.Error("突合に失敗", "err", err)
		writeErr(w, http.StatusInternalServerError, "突合に失敗しました")
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// POST /v1/documents/{id}/settlement
//
//	{ "organization_id": 1, "actor_id": 2,
//	  "transaction_id": 123 }          … この入出金と確定する
//	{ ... "none": true }               … 「相手なし（現金など）」と確定する
//	{ ... "learn_alias": "ﾐﾎﾝｾｷﾕ..." } … 摘要の表記を取引先の別名として覚える
//
// 人の確定。以後の自動突合はこの伝票を上書きしない。
func (s *Server) confirmSettlement(w http.ResponseWriter, r *http.Request) {
	docID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "伝票の番号が不正です")
		return
	}
	// organization_id は受け取らない。署名から決まる（auth.go）。
	var in struct {
		ActorID       int64  `json:"actor_id"`
		TransactionID *int64 `json:"transaction_id"`
		None          bool   `json:"none"`
		LearnAlias    string `json:"learn_alias"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "本文を読めません")
		return
	}
	if in.ActorID <= 0 {
		writeErr(w, http.StatusBadRequest, "actor_id が要ります")
		return
	}
	orgID, _ := orgFrom(r.Context())
	if s.denyOwn(w, s.ownDocument(r, docID)) {
		return
	}
	if s.denyOwn(w, s.ownActor(r, in.ActorID)) {
		return
	}
	if (in.TransactionID == nil) == !in.None {
		writeErr(w, http.StatusBadRequest,
			"transaction_id か none のどちらか一方を指定してください")
		return
	}

	save := store.SaveSettlement{
		OrgID:      orgID,
		DocumentID: docID,
		ActorID:    &in.ActorID,
	}
	if in.None {
		save.Status = int16(settle.StatusNoneFixed)
		save.Why = "人が「対応する入出金なし（現金払いなど）」と確定"
		save.AuditAction = "settle_none_confirm"
	} else {
		save.Status = int16(settle.StatusConfirmed)
		save.TransactionID = in.TransactionID
		save.Why = "人が確定"
		save.AuditAction = "settle_confirm"
	}
	if err := s.St.SaveSettlement(r.Context(), save); err != nil {
		s.Log.Error("突合の確定に失敗", "err", err)
		writeErr(w, http.StatusInternalServerError, "記録できませんでした")
		return
	}

	// 摘要の表記を覚える。次からは名前スコアが100になり、
	// 同じ相手の伝票は自動突合に届くようになる。
	if in.LearnAlias != "" && in.TransactionID != nil {
		if err := s.learnSettleAlias(r, docID, orgID, in.ActorID,
			in.LearnAlias); err != nil {
			// 確定は済んでいる。覚えられなかったことだけ伝える。
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "alias_error": err.Error(),
			})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) learnSettleAlias(r *http.Request, docID, orgID, actorID int64,
	alias string) error {

	// 伝票に紐づいた取引先へ覚えさせる。取引先が未確定なら覚えられない。
	var partnerID *int64
	err := s.St.Pool.QueryRow(r.Context(),
		`SELECT partner_id FROM match_results WHERE document_id = $1`,
		docID).Scan(&partnerID)
	if err != nil || partnerID == nil {
		return fmt.Errorf("取引先が確定していない伝票には表記を覚えさせられません")
	}
	norms, err := s.Norm.NormalizeBank(r.Context(), []string{alias})
	if err != nil {
		return err
	}
	if err := s.St.AddAlias(r.Context(), *partnerID, alias, norms[0], 2); err != nil {
		return err
	}
	// 監査ログ。誰が何を覚えさせたか。取り消しの起点になる。
	after, _ := json.Marshal(map[string]any{
		"partner_id": *partnerID, "alias": alias,
		"norm": norms[0], "document_id": docID, "source": "settlement",
	})
	_, err = s.St.Pool.Exec(r.Context(), `
		INSERT INTO audit_logs
		  (organization_id, actor_id, target_table, target_id, action, after)
		VALUES ($1, $2, 'partner_aliases', $3, 'learn_alias', $4)`,
		orgID, actorID, docID, after)
	return err
}
