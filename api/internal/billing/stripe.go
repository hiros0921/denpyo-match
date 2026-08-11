package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v86"
	billingportal "github.com/stripe/stripe-go/v86/billingportal/session"
	checkout "github.com/stripe/stripe-go/v86/checkout/session"
	"github.com/stripe/stripe-go/v86/customer"
	"github.com/stripe/stripe-go/v86/webhook"
)

// Stripe は Stripe API を呼ぶ。
//
// 公式のライブラリを使う。とくに Webhook の署名検証は自前で書かない。
// 定数時間比較・許容する時刻のずれ・複数署名の扱いを間違えると、
// 「検証しているつもりで誰でも通せる」状態になる。
// そして通ってしまう以上、テストでは気付けない。
type Stripe struct {
	SecretKey      string
	PublishableKey string
	WebhookSecret  string
	PriceID        string
	// 試用期間。0なら試用なし。
	TrialDays int64
}

func NewStripe(secret, publishable, webhookSecret, priceID string, trialDays int64) *Stripe {
	stripe.Key = secret
	return &Stripe{
		SecretKey: secret, PublishableKey: publishable,
		WebhookSecret: webhookSecret, PriceID: priceID, TrialDays: trialDays,
	}
}

func (s *Stripe) Configured() bool { return s.SecretKey != "" && s.PriceID != "" }

// EnsureCustomer は Stripe の顧客を用意する。既にあればそれを返す。
//
// 顧客IDは組織に持たせる。契約が終わって作り直すとき、
// 顧客を作り直すと過去の請求履歴が別の顧客にぶら下がって追えなくなる。
func (s *Stripe) EnsureCustomer(ctx context.Context, existingID, orgName, email string,
	orgID int64) (string, error) {
	if existingID != "" {
		return existingID, nil
	}
	c, err := customer.New(&stripe.CustomerParams{
		Name:  stripe.String(orgName),
		Email: stripe.String(email),
		// 組織IDを Stripe 側にも持たせる。Webhook が届いたときに
		// どの組織か分からない、という状態を作らない。
		Metadata: map[string]string{"organization_id": fmt.Sprint(orgID)},
	})
	if err != nil {
		return "", fmt.Errorf("顧客を作れません: %w", err)
	}
	return c.ID, nil
}

// CheckoutURL は申し込み画面のURLを作る。
func (s *Stripe) CheckoutURL(ctx context.Context, customerID string, orgID int64,
	successURL, cancelURL string) (string, error) {
	p := &stripe.CheckoutSessionParams{
		Mode:       stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		Customer:   stripe.String(customerID),
		SuccessURL: stripe.String(successURL),
		CancelURL:  stripe.String(cancelURL),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{Price: stripe.String(s.PriceID), Quantity: stripe.Int64(1)},
		},
		// 【重要】組織IDを契約そのものにも持たせる。
		// Checkout のセッションにしか付けないと、後から届く
		// customer.subscription.updated には載っていない。
		SubscriptionData: &stripe.CheckoutSessionSubscriptionDataParams{
			Metadata: map[string]string{"organization_id": fmt.Sprint(orgID)},
		},
		Metadata: map[string]string{"organization_id": fmt.Sprint(orgID)},
	}
	if s.TrialDays > 0 {
		p.SubscriptionData.TrialPeriodDays = stripe.Int64(s.TrialDays)
	}
	sess, err := checkout.New(p)
	if err != nil {
		return "", fmt.Errorf("申し込み画面を作れません: %w", err)
	}
	return sess.URL, nil
}

// PortalURL は支払い方法の変更・解約を行う画面のURLを作る。
//
// カード番号の入力欄を自前で作らない。Stripe の画面へ送る。
// 自前で受けると、その瞬間からカード情報を扱う側になる。
func (s *Stripe) PortalURL(ctx context.Context, customerID, returnURL string) (string, error) {
	sess, err := billingportal.New(&stripe.BillingPortalSessionParams{
		Customer:  stripe.String(customerID),
		ReturnURL: stripe.String(returnURL),
	})
	if err != nil {
		return "", fmt.Errorf("契約の管理画面を作れません: %w", err)
	}
	return sess.URL, nil
}

// ── Webhook ──

var ErrBadSignature = errors.New("署名が正しくありません")

// ErrAPIVersion は Webhook の版がライブラリの想定と違う場合。
//
// 【重要】これを署名の誤りと同じ扱いにしてはいけない。
// 本番で起きたとき「署名が正しくありません」とだけ出ると、
// 鍵の設定を延々と疑うことになる。実際の原因は
// 「エンドポイントを登録したときの API 版が違う」で、直す場所がまったく別。
//
// ライブラリがこれを弾くのは正しい。版が違うと項目の形が変わり、
// 金額や状態を取り違えたまま「成功」してしまう危険があるため。
var ErrAPIVersion = errors.New("Webhook の API 版がライブラリの想定と違います")

// ExpectedAPIVersion は、エンドポイントを登録するときに指定すべき版。
// stripe-go を上げたらこの値も変わるので、登録し直す必要がある。
func ExpectedAPIVersion() string { return stripe.APIVersion }

// Event は Webhook から取り出した、こちらが必要とするものだけ。
type Event struct {
	// Stripe のイベントID。同じ知らせを2回処理しないために使う。
	ID                string
	Type              string
	OrganizationID    int64
	CustomerID        string
	SubscriptionID    string
	Status            string
	CurrentPeriodEnd  time.Time
	CancelAtPeriodEnd bool
	PriceID           string
	// 支払いが失敗した知らせかどうか。猶予を始める判断に使う。
	PaymentFailed bool
	// 支払いが通った知らせかどうか。猶予を解除する判断に使う。
	PaymentSucceeded bool
}

// ParseWebhook は署名を検証し、必要な項目を取り出す。
//
// 【重要】署名の検証を通らないものは、中身を一切見ない。
// 検証前にJSONを解析して「とりあえずログに出す」のもやらない。
// 誰でも投げ込める口なので、解析した時点で攻撃対象になる。
func (s *Stripe) ParseWebhook(payload []byte, sigHeader string) (*Event, error) {
	ev, err := webhook.ConstructEvent(payload, sigHeader, s.WebhookSecret)
	if err != nil {
		// 版の食い違いと署名の誤りを分ける。直す場所がまったく違う。
		if strings.Contains(err.Error(), "API version") {
			return nil, fmt.Errorf("%w（想定 %s）: %v",
				ErrAPIVersion, stripe.APIVersion, err)
		}
		return nil, fmt.Errorf("%w: %v", ErrBadSignature, err)
	}

	out := &Event{ID: ev.ID, Type: string(ev.Type)}

	switch ev.Type {
	case "customer.subscription.created",
		"customer.subscription.updated",
		"customer.subscription.deleted":
		var sub stripe.Subscription
		if err := json.Unmarshal(ev.Data.Raw, &sub); err != nil {
			return nil, fmt.Errorf("契約の内容を読めません: %w", err)
		}
		out.SubscriptionID = sub.ID
		out.Status = string(sub.Status)
		out.CancelAtPeriodEnd = sub.CancelAtPeriodEnd
		if sub.Customer != nil {
			out.CustomerID = sub.Customer.ID
		}
		out.OrganizationID = orgFromMetadata(sub.Metadata)
		if len(sub.Items.Data) > 0 && sub.Items.Data[0].Price != nil {
			out.PriceID = sub.Items.Data[0].Price.ID
			// 期末は契約明細側にある（v82 で契約直下から移動した）。
			if e := sub.Items.Data[0].CurrentPeriodEnd; e > 0 {
				out.CurrentPeriodEnd = time.Unix(e, 0).UTC()
			}
		}
		// deleted は状態が canceled で届かないことがあるので、こちらで決める。
		if ev.Type == "customer.subscription.deleted" {
			out.Status = StatusCanceled
		}

	case "invoice.payment_failed":
		var inv stripe.Invoice
		if err := json.Unmarshal(ev.Data.Raw, &inv); err != nil {
			return nil, fmt.Errorf("請求の内容を読めません: %w", err)
		}
		out.PaymentFailed = true
		if inv.Customer != nil {
			out.CustomerID = inv.Customer.ID
		}
		out.SubscriptionID = subscriptionIDFromInvoice(&inv)

	case "invoice.payment_succeeded", "invoice.paid":
		var inv stripe.Invoice
		if err := json.Unmarshal(ev.Data.Raw, &inv); err != nil {
			return nil, fmt.Errorf("請求の内容を読めません: %w", err)
		}
		out.PaymentSucceeded = true
		if inv.Customer != nil {
			out.CustomerID = inv.Customer.ID
		}
		out.SubscriptionID = subscriptionIDFromInvoice(&inv)
	}

	return out, nil
}

func orgFromMetadata(m map[string]string) int64 {
	if m == nil {
		return 0
	}
	var id int64
	fmt.Sscan(m["organization_id"], &id)
	return id
}

// 請求から契約IDを取り出す。
// v82 では請求明細の親側に入っているため、階層を辿る必要がある。
func subscriptionIDFromInvoice(inv *stripe.Invoice) string {
	if inv.Parent != nil && inv.Parent.SubscriptionDetails != nil &&
		inv.Parent.SubscriptionDetails.Subscription != nil {
		return inv.Parent.SubscriptionDetails.Subscription.ID
	}
	for _, li := range inv.Lines.Data {
		if li.Parent != nil && li.Parent.SubscriptionItemDetails != nil {
			if s := li.Parent.SubscriptionItemDetails.Subscription; s != "" {
				return s
			}
		}
	}
	return ""
}
