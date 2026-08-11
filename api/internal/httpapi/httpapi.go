// Package httpapi は REST の口を提供する。
//
// 方針
//
//	① アップロードは受け付けて即返す。処理の完了を待たない。
//	   OCRに1枚2秒かかるので、100枚まとめて投げられたら待たせられない。
//	   受け付けたことだけ返し、進み具合は照会で見せる。
//	② 照会はワーカーに聞きに行かず、DBの jobs を読む。
//	   ワーカーが何台でも、途中で落ちても、同じ1本の SQL で答えられる。
//	③ 承認は必ず「誰が」を残す。人の判断を記録として残せない口は作らない。
//
// 認証はここでは扱わない。第7段階の Rails（Devise）が前段に立ち、
// 利用者IDを渡してくる。この API は事務所の内側からしか呼ばれない。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hiros0921/denpo-match/api/internal/billing"
	"github.com/hiros0921/denpo-match/api/internal/core"
	"github.com/hiros0921/denpo-match/api/internal/store"
)

type Server struct {
	St  *store.Store
	Log *slog.Logger

	// 決済。設定されていなければ nil。
	// 【重要】nil のときアップロードを止めない。決済を後から入れる導入先や、
	// 開発環境で、既存の業務が動かなくなるのを避ける。
	// 契約が「無い」ことによる停止は billing.Evaluate が判断する。
	Stripe *billing.Stripe
	// 画面のURL。Stripe から戻ってくる先に使う。
	AppBaseURL string

	// 正規化。承認画面で学習させる別名を DB に入れる前に通す。
	// Go 側で書き直さず、C++ の実装を呼ぶ。
	// 保存時と照会時でルールがずれると、候補生成が静かに当たらなくなる。
	Norm *core.Runner

	// アップロードされた画像を置く場所。本番は R2 に上げてキーだけ持つ。
	ImageRoot string
	// 同じ場所を C++ から見たパス。開発では Docker 越しに呼ぶので別になる。
	// 空なら ImageRoot と同じ。
	ImageRootContainer string
	// 1ファイルの上限。既定 20MB。
	// スキャン画像は大きくても数MBなので、これを超えるのは誤りか攻撃。
	MaxUpload int64
}

func New(st *store.Store, norm *core.Runner, imageRoot string) *Server {
	return &Server{St: st, Norm: norm, Log: slog.Default(), ImageRoot: imageRoot,
		MaxUpload: 20 << 20, AppBaseURL: "http://localhost:53000"}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("POST /v1/documents", s.upload)
	mux.HandleFunc("GET /v1/documents/{id}", s.get)
	mux.HandleFunc("POST /v1/documents/{id}/decision", s.decision)
	mux.HandleFunc("GET /v1/billing", s.billingStatus)
	mux.HandleFunc("POST /v1/billing/checkout", s.billingCheckout)
	mux.HandleFunc("POST /v1/billing/portal", s.billingPortal)
	mux.HandleFunc("POST /v1/billing/webhook", s.billingWebhook)
	mux.HandleFunc("GET /v1/aliases", s.aliases)
	mux.HandleFunc("DELETE /v1/aliases/{id}", s.forgetAlias)
	return s.withLog(mux)
}

// ── 応答の形 ──

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// エラーは日本語で返す。この API を呼ぶのは事務所の画面であり、
// そこに出る文言が英語だと、現場が自分で原因を判断できない。
func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) withLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := time.Now()
		next.ServeHTTP(w, r)
		s.Log.Info("req", "method", r.Method, "path", r.URL.Path,
			"ms", time.Since(t).Milliseconds())
	})
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if err := s.St.Pool.Ping(r.Context()); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "DBに繋がりません")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ── アップロード ──

// POST /v1/documents
//
//	multipart/form-data
//	  file       画像（必須）
//	  client_id  顧問先（必須）
//	  doc_type   1:請求書 2:納品書 3:領収書（既定 1）
//	  direction  1:受領 2:発行（省略時は顧問先の既定）
//
// 受け付けたら 202 を返す。処理はワーカーが後から行う。
// 「受け付けた」と「終わった」を区別して返すのが要点。
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.MaxUpload)
	if err := r.ParseMultipartForm(s.MaxUpload); err != nil {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("ファイルを受け取れません（上限 %dMB）", s.MaxUpload>>20))
		return
	}
	clientID, err := strconv.ParseInt(r.FormValue("client_id"), 10, 64)
	if err != nil || clientID <= 0 {
		writeErr(w, http.StatusBadRequest, "client_id を指定してください")
		return
	}
	docType := int16(1)
	if v := r.FormValue("doc_type"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 3 {
			writeErr(w, http.StatusBadRequest,
				"doc_type は 1:請求書 2:納品書 3:領収書 のいずれかです")
			return
		}
		docType = int16(n)
	}

	// 帳票の向き。省略できる。省略したときは顧問先の既定を使う。
	//
	// 【重要】ここで既定値を決めない。決めると設定が2か所になる。
	// 受領側の顧問先が「発行」で処理される事故は、記録上は
	// 「スコアが低い」としか見えないので、原因に辿り着くのが遅い。
	direction := int16(0)
	if v := r.FormValue("direction"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || (n != 1 && n != 2) {
			writeErr(w, http.StatusBadRequest,
				"direction は 1:受領 2:発行 のいずれかです")
			return
		}
		direction = int16(n)
	}

	// 【重要】契約を確かめてから受け取る。画面の表示だけに頼らない。
	// APIを直接叩けば画面を通らないので、表示では何も守れない。
	if ok, msg := s.checkCanUpload(r, clientID); !ok {
		// 402 Payment Required。支払いが要ることを、状態番号でも示す。
		writeErr(w, http.StatusPaymentRequired, msg)
		return
	}

	f, fh, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "file が付いていません")
		return
	}
	defer f.Close()

	key, err := s.saveUpload(f, fh.Filename)
	if err != nil {
		s.Log.Error("保存に失敗", "err", err)
		writeErr(w, http.StatusInternalServerError, "ファイルを保存できませんでした")
		return
	}

	// ── 1枚の紙に複数の伝票があれば、ここで分ける ──
	//
	// 【重要】分けるのは受付。ワーカーではない。
	// ワーカーで分けると、伝票の番号が振られたあとに件数が変わる。
	// 監査ログは伝票ごとに連鎖しているので、対応が壊れる。
	docs := s.expand(r.Context(), key, fh.Filename, clientID, docType, direction)

	docIDs, jobIDs, err := s.St.CreateDocuments(r.Context(), docs)
	if err != nil {
		s.Log.Error("受付に失敗", "err", err)
		writeErr(w, http.StatusInternalServerError, "受け付けられませんでした")
		return
	}
	docID, jobID := docIDs[0], jobIDs[0]

	writeJSON(w, http.StatusAccepted, map[string]any{
		"document_id":  docID,
		"document_ids": docIDs,
		"count":        len(docIDs),
		"job_id":       jobID,
		"status":       "受付",
		"poll":         fmt.Sprintf("/v1/documents/%d", docID),
	})
}

// expand は1つのファイルを、伝票ごとの受付情報に展開する。
//
// レシートは1枚の紙に何枚も貼られていたり、まとめてスキャンされて
// 複数ページの PDF になっていたりする。1件として扱うと、
// 1つの伝票の中で項目が混ざる。実測（レシート2枚並び）:
//
//	取引先名 サンプルマート（左）／ 金額 ¥5,395（右）
//
// 正解は左が ¥1,258、右が見本石油サービス。
// どちらの項目にも「読めなかった」印は付かないので、
// スコアは高いまま出る。閾値では止まらない種類の誤りで、最もたちが悪い。
//
// 【重要】分けられなくても受付は通す。
// 分割は精度を上げるための工程であって、受付の条件ではない。
// ここで失敗して 500 を返すと、1枚しか無い普通の請求書まで入らなくなる。
func (s *Server) expand(ctx context.Context, key, origName string,
	clientID int64, docType, direction int16) []store.NewDocument {

	one := []store.NewDocument{{
		ClientID: clientID, DocType: docType, Direction: direction,
		R2Key: key, SourceName: origName, SourcePage: 1, SourceRegion: 1,
	}}
	if s.Norm == nil {
		return one
	}

	root := s.ImageRootContainer
	if root == "" {
		root = s.ImageRoot
	}
	parts, err := s.Norm.Split(ctx, root+"/"+key, root+"/"+key+"_parts")
	if err != nil {
		s.Log.Warn("切り分けできません。1件として受け付けます",
			"file", origName, "err", err)
		return one
	}
	if len(parts) == 0 {
		return one
	}

	out := make([]store.NewDocument, 0, len(parts))
	for _, p := range parts {
		box, _ := json.Marshal(map[string]int{"x": p.X, "y": p.Y, "w": p.W, "h": p.H})
		out = append(out, store.NewDocument{
			ClientID: clientID, DocType: docType, Direction: direction,
			// C++ は絶対パスを返す。保存はキー（ImageRoot からの相対）で持つ。
			R2Key:      key + "_parts/" + filepath.Base(p.File),
			SourceName: origName, SourcePage: p.Page, SourceRegion: p.Region,
			SourceBox: box,
		})
	}
	if len(out) > 1 {
		s.Log.Info("1枚から複数の伝票に分けました",
			"file", origName, "件数", len(out))
	}
	return out
}

// saveUpload はファイル名を作り直して保存する。
//
// アップロードされた名前をそのまま使わない。/ や .. が入っていれば
// 別の場所へ書き込める。拡張子だけを引き継ぎ、名前はこちらで決める。
func (s *Server) saveUpload(src io.Reader, orig string) (string, error) {
	ext := strings.ToLower(filepath.Ext(orig))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".tif", ".tiff":
	case ".pdf":
		// PDF は C++ 側（poppler / pdftoppm）で 150dpi の画像に変換してから
		// 前処理と OCR に入る。
		//
		// 一度ここで断っていた。OpenCV が PDF を読めず、受け付けたあと
		// 3回再試行して打ち切られていたため。現場から見ると
		// 「入れたのに、しばらくしてエラーになった」で原因が分からない。
		// 処理できるようになったので入口を開ける。
		//
		// 複数ページの PDF と、1ページに複数枚のレシートは、
		// 受付で expand() が伝票ごとに切り分ける。
	default:
		return "", fmt.Errorf("扱えない形式です: %q", ext)
	}
	if err := os.MkdirAll(s.ImageRoot, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(s.ImageRoot, "up_*"+ext)
	if err != nil {
		return "", err
	}
	defer tmp.Close()
	if _, err := io.Copy(tmp, src); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return filepath.Base(tmp.Name()), nil
}

// ── 状態照会 ──

// GET /v1/documents/{id}?candidates=5
func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "伝票IDが正しくありません")
		return
	}
	n := 5
	if v := r.URL.Query().Get("candidates"); v != "" {
		if k, err := strconv.Atoi(v); err == nil && k >= 0 && k <= 50 {
			n = k
		}
	}
	v, err := s.St.GetDocument(r.Context(), id, n)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "その伝票はありません")
		return
	}
	if err != nil {
		s.Log.Error("照会に失敗", "id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "照会できませんでした")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// ── 人の判断 ──

type decisionReq struct {
	OrgID     int64  `json:"organization_id"`
	ActorID   int64  `json:"actor_id"`
	Decision  int16  `json:"decision"`   // 2:承認 3:修正 4:却下
	PartnerID *int64 `json:"partner_id"` // 修正のとき、選び直した取引先
	// 修正のとき、この伝票に書かれていた表記。別名として学習する。
	// 空なら学習しない。誤った表記を貯めると次から精度が下がるため、
	// 学習させるかどうかは画面側が明示的に決める。
	LearnAlias string `json:"learn_alias"`
}

// POST /v1/documents/{id}/decision
func (s *Server) decision(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "伝票IDが正しくありません")
		return
	}
	var req decisionReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "本文を読めません")
		return
	}
	switch {
	case req.OrgID <= 0:
		writeErr(w, http.StatusBadRequest, "organization_id を指定してください")
		return
	case req.ActorID <= 0:
		// 誰が承認したか分からない記録を残さない。
		// 監査ログの意味が無くなる。
		writeErr(w, http.StatusBadRequest, "actor_id を指定してください")
		return
	case req.Decision < 2 || req.Decision > 4:
		writeErr(w, http.StatusBadRequest, "decision は 2:承認 3:修正 4:却下 です")
		return
	case req.Decision == 3 && req.PartnerID == nil:
		writeErr(w, http.StatusBadRequest, "修正のときは partner_id が必要です")
		return
	}

	// 別名の正規形は、DBに入れる前に必ず C++ の正規化を通す。
	//
	// ここで生の文字列をそのまま入れると、partners.norm を作ったときの
	// ルールと合わなくなる。候補生成は norm どうしを比べるので、
	// 揺れを覚えさせたつもりが二度と当たらない別名になる。
	// エラーは出ないので、気付くのは「学習させたのに効かない」という
	// 分かりにくい形になる。
	aliasNorm := ""
	if req.LearnAlias != "" {
		ns, nerr := s.Norm.Normalize(r.Context(), []string{req.LearnAlias})
		if nerr != nil || len(ns) == 0 {
			s.Log.Error("別名の正規化に失敗", "alias", req.LearnAlias, "err", nerr)
			writeErr(w, http.StatusInternalServerError,
				"表記の正規化に失敗しました。学習させずにもう一度お試しください")
			return
		}
		aliasNorm = ns[0]
	}

	err = s.St.Approve(r.Context(), req.OrgID, id, req.ActorID,
		req.Decision, req.PartnerID, req.LearnAlias, aliasNorm)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "その伝票には照合結果がありません")
		return
	}
	if err != nil {
		s.Log.Error("判断の記録に失敗", "id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "記録できませんでした")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"document_id": id, "ok": true})
}

// ── 覚えた表記 ──
//
// 承認画面で人が覚えさせた表記（source=2）を見せ、取り消せるようにする。
//
// この仕組みは強力な反面、誤って覚えると同じ強さで悪化する。
// 第7段階の検証で実際にそうなった。押し間違いは現場で必ず起きるので、
// 「覚えっぱなしで直せない」状態にはしない。

// GET /v1/aliases?organization_id=1&limit=100
func (s *Server) aliases(w http.ResponseWriter, r *http.Request) {
	orgID, err := strconv.ParseInt(r.URL.Query().Get("organization_id"), 10, 64)
	if err != nil || orgID <= 0 {
		writeErr(w, http.StatusBadRequest, "organization_id を指定してください")
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	list, err := s.St.LearnedAliases(r.Context(), orgID, limit)
	if err != nil {
		s.Log.Error("覚えた表記を読めません", "err", err)
		writeErr(w, http.StatusInternalServerError, "読み込めませんでした")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"aliases": list})
}

// DELETE /v1/aliases/{id}?organization_id=1&actor_id=1
func (s *Server) forgetAlias(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "IDが正しくありません")
		return
	}
	orgID, _ := strconv.ParseInt(r.URL.Query().Get("organization_id"), 10, 64)
	actorID, _ := strconv.ParseInt(r.URL.Query().Get("actor_id"), 10, 64)
	if orgID <= 0 || actorID <= 0 {
		// 誰が取り消したか分からない記録を残さない。
		writeErr(w, http.StatusBadRequest, "organization_id と actor_id を指定してください")
		return
	}
	err = s.St.ForgetAlias(r.Context(), orgID, id, actorID)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "その表記はありません")
		return
	}
	if err != nil {
		s.Log.Error("取り消しに失敗", "id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "取り消せませんでした")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "ok": true})
}
