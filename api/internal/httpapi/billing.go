package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/hiros0921/denpo-match/api/internal/billing"
	"github.com/hiros0921/denpo-match/api/internal/store"
)

// ── 契約状態の照会 ──

// GET /v1/billing?organization_id=1
//
// 画面はこれを見て「アップロードできるか」「何を出すか」を決める。
// 判定を画面側にも書くと、2箇所で食い違う。判定はここ1つに集める。
func (s *Server) billingStatus(w http.ResponseWriter, r *http.Request) {
	orgID, err := strconv.ParseInt(r.URL.Query().Get("organization_id"), 10, 64)
	if err != nil || orgID <= 0 {
		writeErr(w, http.StatusBadRequest, "organization_id を指定してください")
		return
	}
	b, err := s.St.LoadBilling(r.Context(), orgID)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "その組織はありません")
		return
	}
	if err != nil {
		s.Log.Error("契約状態を読めません", "org", orgID, "err", err)
		writeErr(w, http.StatusInternalServerError, "読み込めませんでした")
		return
	}

	d := billing.Evaluate(billing.Org{BillingExempt: b.BillingExempt}, b.Sub, time.Now())
	out := map[string]any{
		"can_upload":   d.CanUpload,
		"reason":       d.Reason,
		"next_step":    d.NextStep,
		"in_grace":     d.InGrace,
		"has_contract": b.Sub != nil,
		"configured":   s.Stripe != nil && s.Stripe.Configured(),
	}
	if d.InGrace {
		out["grace_until"] = d.GraceUntil
	}
	if b.Sub != nil {
		out["status"] = b.Sub.Status
		out["cancel_at_period_end"] = b.Sub.CancelAtPeriodEnd
		if !b.Sub.CurrentPeriodEnd.IsZero() {
			out["current_period_end"] = b.Sub.CurrentPeriodEnd
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ── 申し込み・管理画面へのリンク ──

type checkoutReq struct {
	OrganizationID int64  `json:"organization_id"`
	ActorEmail     string `json:"actor_email"`
}

// POST /v1/billing/checkout
//
// カード番号の入力欄を自前で作らない。Stripe の画面へ送る。
// 自前で受けると、その瞬間からカード情報を扱う側になり、
// 求められる基準がまったく変わる。
func (s *Server) billingCheckout(w http.ResponseWriter, r *http.Request) {
	if s.Stripe == nil || !s.Stripe.Configured() {
		writeErr(w, http.StatusServiceUnavailable,
			"決済の設定がされていません（管理者にお問い合わせください）")
		return
	}
	var req checkoutReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil ||
		req.OrganizationID <= 0 {
		writeErr(w, http.StatusBadRequest, "organization_id を指定してください")
		return
	}

	b, err := s.St.LoadBilling(r.Context(), req.OrganizationID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "その組織はありません")
		return
	}

	cust, err := s.Stripe.EnsureCustomer(r.Context(), b.StripeCustomerID,
		b.OrgName, req.ActorEmail, b.OrganizationID)
	if err != nil {
		s.Log.Error("顧客を用意できません", "org", req.OrganizationID, "err", err)
		writeErr(w, http.StatusBadGateway, "決済サービスに繋がりません")
		return
	}
	if b.StripeCustomerID == "" {
		if err := s.St.SetStripeCustomer(r.Context(), b.OrganizationID, cust); err != nil {
			s.Log.Error("顧客IDを保存できません", "org", req.OrganizationID, "err", err)
		}
	}

	url, err := s.Stripe.CheckoutURL(r.Context(), cust, b.OrganizationID,
		s.AppBaseURL+"/billing?done=1", s.AppBaseURL+"/billing?canceled=1")
	if err != nil {
		s.Log.Error("申し込み画面を作れません", "org", req.OrganizationID, "err", err)
		writeErr(w, http.StatusBadGateway, "申し込み画面を開けませんでした")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": url})
}

// POST /v1/billing/portal
func (s *Server) billingPortal(w http.ResponseWriter, r *http.Request) {
	if s.Stripe == nil || !s.Stripe.Configured() {
		writeErr(w, http.StatusServiceUnavailable, "決済の設定がされていません")
		return
	}
	var req checkoutReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil ||
		req.OrganizationID <= 0 {
		writeErr(w, http.StatusBadRequest, "organization_id を指定してください")
		return
	}
	b, err := s.St.LoadBilling(r.Context(), req.OrganizationID)
	if err != nil || b.StripeCustomerID == "" {
		writeErr(w, http.StatusNotFound, "まだお申し込みがありません")
		return
	}
	url, err := s.Stripe.PortalURL(r.Context(), b.StripeCustomerID, s.AppBaseURL+"/billing")
	if err != nil {
		s.Log.Error("管理画面を作れません", "org", req.OrganizationID, "err", err)
		writeErr(w, http.StatusBadGateway, "管理画面を開けませんでした")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": url})
}

// ── Webhook ──

// POST /v1/billing/webhook
//
// 【重要】この口は誰でも叩ける。認証は署名だけ。
// 署名の検証を通らないものは、中身を一切見ない。
//
// 返す値にも意味がある。
//
//	200  受け取った。Stripe は再送しない
//	4xx  受け取らない。Stripe は再送しない（署名の誤りなど、再送しても直らない）
//	5xx  こちらの都合で処理できない。Stripe は再送する
//
// ここを取り違えて、処理に失敗したのに200を返すと、
// その知らせは永久に失われる。契約が止まらない／止まったままになる。
func (s *Server) billingWebhook(w http.ResponseWriter, r *http.Request) {
	if s.Stripe == nil || s.Stripe.WebhookSecret == "" {
		// 署名を検証できない状態で受けない。設定漏れを黙って通さない。
		writeErr(w, http.StatusServiceUnavailable, "Webhook の設定がされていません")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "本文を読めません")
		return
	}

	ev, err := s.Stripe.ParseWebhook(body, r.Header.Get("Stripe-Signature"))
	if err != nil {
		// 署名の誤りと API 版の食い違いを分けて記録する。直す場所が違う。
		if errors.Is(err, billing.ErrAPIVersion) {
			s.Log.Error("Webhook の API 版が合いません", "err", err,
				"想定", billing.ExpectedAPIVersion())
		} else {
			s.Log.Warn("Webhook の署名が正しくありません", "err", err)
		}
		// どちらも再送されても直らないので 400。
		writeErr(w, http.StatusBadRequest, "受け取れませんでした")
		return
	}

	// 同じ知らせを2回処理しない。
	seen, err := s.St.SeenEvent(r.Context(), ev.ID)
	if err != nil {
		s.Log.Error("イベントの記録に失敗", "event", ev.ID, "err", err)
		// こちらの都合なので5xx。Stripe が再送してくれる。
		writeErr(w, http.StatusInternalServerError, "あとでもう一度お送りください")
		return
	}
	if seen {
		writeJSON(w, http.StatusOK, map[string]string{"status": "処理済み"})
		return
	}

	// 契約に関係しない知らせは、受け取ったとだけ返す。
	if ev.SubscriptionID == "" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "対象外"})
		return
	}

	orgID := ev.OrganizationID
	if orgID == 0 {
		// 請求の知らせには組織IDが入っていない。顧客ID経由で辿る。
		orgID, err = s.St.OrgIDForCustomer(r.Context(), ev.CustomerID)
		if err != nil {
			// どの組織か分からないものは処理できない。
			// 再送されても分からないので200で受け取り、記録だけ残す。
			s.Log.Warn("組織を特定できない知らせ", "event", ev.ID,
				"type", ev.Type, "customer", ev.CustomerID)
			writeJSON(w, http.StatusOK, map[string]string{"status": "組織を特定できません"})
			return
		}
	}

	if err := s.St.ApplySubscription(r.Context(), orgID, ev, time.Now()); err != nil {
		s.Log.Error("契約を反映できません", "event", ev.ID, "org", orgID, "err", err)
		writeErr(w, http.StatusInternalServerError, "あとでもう一度お送りください")
		return
	}
	s.Log.Info("契約を反映", "event", ev.Type, "org", orgID,
		"subscription", ev.SubscriptionID, "status", ev.Status)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ── アップロードの可否 ──

// checkCanUpload は「今アップロードできるか」を返す。
//
// 【重要】画面側の表示だけに頼らない。ここで止める。
// 画面は親切のために出すもので、止めるのはこちら側の仕事。
// APIを直接叩けば画面を通らないので、表示だけでは何も守れない。
func (s *Server) checkCanUpload(r *http.Request, clientID int64) (ok bool, msg string) {
	orgID, err := s.St.OrgIDForClient(r.Context(), clientID)
	if err != nil {
		return false, "顧問先が見つかりません"
	}
	b, err := s.St.LoadBilling(r.Context(), orgID)
	if err != nil {
		s.Log.Error("契約状態を読めません", "org", orgID, "err", err)
		// 【重要】読めないときは通す。
		// DBの一時的な不調で、支払っている事務所の業務を止めない。
		// 「取り返しがつかないのは止めたほう」という方針に揃える。
		return true, ""
	}
	d := billing.Evaluate(billing.Org{BillingExempt: b.BillingExempt}, b.Sub, time.Now())
	if d.CanUpload {
		return true, ""
	}
	return false, fmt.Sprintf("%s。%s", d.Reason, d.NextStep)
}
