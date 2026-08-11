package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hiros0921/denpo-match/api/internal/billing"
)

// ── 契約状態の読み取り ──

type OrgBilling struct {
	OrganizationID   int64
	OrgName          string
	BillingExempt    bool
	StripeCustomerID string
	Sub              *billing.Subscription
}

// LoadBilling は判定に必要なものを1回で引く。
//
// アップロードのたびに呼ばれるので、往復を増やさない。
// 生きている契約は1件だけ（013 の部分一意索引で保証）。
func (s *Store) LoadBilling(ctx context.Context, orgID int64) (*OrgBilling, error) {
	var b OrgBilling
	var cust *string
	var status, priceID *string
	var periodEnd, graceUntil *time.Time
	var cancelAtEnd *bool

	err := s.Pool.QueryRow(ctx, `
		SELECT o.id, o.name, o.billing_exempt, o.stripe_customer_id,
		       s.status, s.current_period_end, s.grace_until,
		       s.cancel_at_period_end, s.stripe_price_id
		  FROM organizations o
		  LEFT JOIN LATERAL (
		    SELECT * FROM subscriptions
		     WHERE organization_id = o.id
		       AND status NOT IN ('canceled', 'incomplete_expired')
		     ORDER BY id DESC LIMIT 1
		  ) s ON true
		 WHERE o.id = $1`, orgID).
		Scan(&b.OrganizationID, &b.OrgName, &b.BillingExempt, &cust,
			&status, &periodEnd, &graceUntil, &cancelAtEnd, &priceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if cust != nil {
		b.StripeCustomerID = *cust
	}
	if status != nil {
		sub := &billing.Subscription{Status: *status}
		if periodEnd != nil {
			sub.CurrentPeriodEnd = *periodEnd
		}
		sub.GraceUntil = graceUntil
		if cancelAtEnd != nil {
			sub.CancelAtPeriodEnd = *cancelAtEnd
		}
		b.Sub = sub
	}
	return &b, nil
}

func (s *Store) SetStripeCustomer(ctx context.Context, orgID int64, customerID string) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE organizations SET stripe_customer_id = $2, updated_at = now()
		 WHERE id = $1 AND stripe_customer_id IS NULL`, orgID, customerID)
	return err
}

// ── Webhook の反映 ──

// ApplySubscription は契約の状態を書き込む。
//
// 【重要】Webhook は順番どおりに届くとは限らず、同じものが複数回届く。
// Stripe 自身がそう明記している。よって次の2つを満たす必要がある。
//
//	① 同じものが2回来ても結果が変わらない（べき等）
//	② 古い知らせが後から来ても、新しい状態を上書きしない
//
// ②のために「解約済みの契約を、古い active の知らせで生き返らせない」
// という条件を入れてある。これが無いと、解約した事務所が使い続けられる。
func (s *Store) ApplySubscription(ctx context.Context, orgID int64, ev *billing.Event,
	now time.Time) error {
	if ev.SubscriptionID == "" {
		return fmt.Errorf("契約IDがありません")
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var before []byte
	_ = tx.QueryRow(ctx, `SELECT to_jsonb(x) FROM subscriptions x
	  WHERE stripe_subscription_id = $1`, ev.SubscriptionID).Scan(&before)

	var periodEnd any
	if !ev.CurrentPeriodEnd.IsZero() {
		periodEnd = ev.CurrentPeriodEnd
	}

	// 支払いが通ったら猶予を解除する。
	// 解除し忘れると、次に支払いが失敗したとき古い猶予が残っていて、
	// 本来より短い（または長い）期間になる。
	//
	// 【重要】新規に入れるときと、既存を更新するときで式が違う。
	// 更新側は「既に猶予が始まっていれば延ばさない」ために既存の値を見るが、
	// 新規側にはまだ行が無い。同じ式を両方に使うと
	// invalid reference to FROM-clause entry で落ちる（実際に落ちた）。
	graceAt := billing.StartGrace(now).UTC()
	var insertGrace any    // 新規のとき入れる値
	var updateGrace string // 既存を更新するときの式
	switch {
	case ev.PaymentSucceeded || ev.Status == billing.StatusActive:
		insertGrace, updateGrace = nil, "NULL"
	case ev.PaymentFailed:
		insertGrace = graceAt
		// 支払いの再試行のたびに延びると、無期限に使えてしまう。
		// 既に始まっていれば、その終わりを動かさない。
		updateGrace = fmt.Sprintf("coalesce(subscriptions.grace_until, '%s'::timestamptz)",
			graceAt.Format(time.RFC3339Nano))
	default:
		insertGrace, updateGrace = nil, "subscriptions.grace_until"
	}

	sql := fmt.Sprintf(`
		INSERT INTO subscriptions
		  (organization_id, stripe_customer_id, stripe_subscription_id,
		   status, current_period_end, cancel_at_period_end, stripe_price_id, grace_until)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (stripe_subscription_id) DO UPDATE SET
		   status = CASE
		     -- 解約済みを、後から届いた古い知らせで生き返らせない。
		     WHEN subscriptions.status = 'canceled' AND EXCLUDED.status <> 'canceled'
		       THEN subscriptions.status
		     ELSE EXCLUDED.status END,
		   current_period_end = coalesce(EXCLUDED.current_period_end,
		                                 subscriptions.current_period_end),
		   cancel_at_period_end = EXCLUDED.cancel_at_period_end,
		   stripe_price_id = coalesce(EXCLUDED.stripe_price_id, subscriptions.stripe_price_id),
		   grace_until = %s,
		   updated_at = now()`, updateGrace)

	status := ev.Status
	if status == "" {
		// 請求の知らせには状態が入っていない。既存を保つ。
		status = billing.StatusActive
		if before != nil {
			var cur struct {
				Status string `json:"status"`
			}
			if json.Unmarshal(before, &cur) == nil && cur.Status != "" {
				status = cur.Status
			}
		}

		// 【重要】支払いが失敗したら、状態も past_due にする。
		//
		// 実際の Stripe は invoice.payment_failed と
		// customer.subscription.updated(past_due) の両方を送ってくるが、
		// 届く順番は保証されない。状態を active のままにしておくと、
		// 猶予の終わりだけが書かれた「active なのに猶予がある」行ができる。
		// billing.Evaluate は active を見て素通しするので、
		// 猶予が切れても止まらない。実測で踏んだ。
		//
		// 猶予を書くのなら、状態もそれに合わせる。片方だけ書かない。
		if ev.PaymentFailed &&
			(status == billing.StatusActive || status == billing.StatusTrialing) {
			status = billing.StatusPastDue
		}
		// 逆に、支払いが通ったのに past_due のままだと止まってしまう。
		if ev.PaymentSucceeded &&
			(status == billing.StatusPastDue || status == billing.StatusUnpaid) {
			status = billing.StatusActive
		}
	}

	if _, err = tx.Exec(ctx, sql, orgID, ev.CustomerID, ev.SubscriptionID,
		status, periodEnd, ev.CancelAtPeriodEnd, nullStr(ev.PriceID),
		insertGrace); err != nil {
		return fmt.Errorf("契約を書けません: %w", err)
	}

	// 契約の変化は監査ログに残す。
	// 「いつ止まったのか」「いつ猶予に入ったのか」を後から説明できるようにする。
	after, _ := json.Marshal(map[string]any{
		"stripe_subscription_id": ev.SubscriptionID,
		"status":                 status,
		"event":                  ev.Type,
		"payment_failed":         ev.PaymentFailed,
		"payment_succeeded":      ev.PaymentSucceeded,
	})
	if _, err = tx.Exec(ctx, `
		INSERT INTO audit_logs
		  (organization_id, actor_id, target_table, target_id, action, before, after)
		VALUES ($1, NULL, 'subscriptions', 0, 'update', $2, $3)`,
		orgID, before, after); err != nil {
		return fmt.Errorf("監査ログを書けません: %w", err)
	}

	return tx.Commit(ctx)
}

// OrgIDForCustomer は Stripe の顧客IDから組織を引く。
//
// 請求の知らせ（invoice.*）には組織IDが入っていないので、顧客ID経由で辿る。
func (s *Store) OrgIDForCustomer(ctx context.Context, customerID string) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx,
		`SELECT id FROM organizations WHERE stripe_customer_id = $1`, customerID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// 契約の表からも探す。顧客IDを組織に書く前に請求が届くことがある。
		err = s.Pool.QueryRow(ctx,
			`SELECT organization_id FROM subscriptions WHERE stripe_customer_id = $1
			  ORDER BY id DESC LIMIT 1`, customerID).Scan(&id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

// ── Webhook の重複を防ぐ ──

// SeenEvent は同じ知らせを2回処理しないための記録。
//
// Stripe は「同じイベントが複数回届くことがある」と明記している。
// 猶予の開始などは2回走らせても同じ結果になるよう作ってあるが、
// 監査ログは追記なので、2回来れば2行残る。記録が水増しされる。
func (s *Store) SeenEvent(ctx context.Context, eventID string) (bool, error) {
	var inserted bool
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO stripe_events (id) VALUES ($1)
		ON CONFLICT (id) DO NOTHING
		RETURNING true`, eventID).Scan(&inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil // 既にある＝処理済み
	}
	if err != nil {
		return false, err
	}
	return false, nil
}
