package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v86"
)

// Stripe と同じ形の署名を作る。
// 正しい署名でだけ通ることを確かめるために、こちら側でも作れる必要がある。
func sign(payload []byte, secret string, ts time.Time) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", ts.Unix())
	mac.Write(payload)
	return fmt.Sprintf("t=%d,v1=%s", ts.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

const testSecret = "whsec_test_0123456789abcdefghijklmnopqrstuv"

func subPayload(id, status string, cancelAtEnd bool, orgID int64, periodEnd int64) []byte {
	// api_version は実際の Stripe が必ず載せてくる。
	// 無いと版の検証で弾かれるので、試験でも本物と同じ形にする。
	return []byte(fmt.Sprintf(`{
      "id": "evt_1", "object": "event",
      "api_version": "`+stripe.APIVersion+`",
      "type": "customer.subscription.updated",
      "data": {"object": {
        "id": "%s",
        "object": "subscription",
        "status": "%s",
        "cancel_at_period_end": %t,
        "customer": "cus_TEST",
        "metadata": {"organization_id": "%d"},
        "items": {"object":"list","data": [
          {"id":"si_1","object":"subscription_item",
           "current_period_end": %d,
           "price": {"id": "price_TEST", "object":"price"}}
        ]}
      }}
    }`, id, status, cancelAtEnd, orgID, periodEnd))
}

// ここが破られると、誰でも他社の契約状態を書き換えられる。
// 「検証しているつもりで通ってしまう」形の不具合は、
// 正常系のテストだけでは絶対に見つからない。
func TestParseWebhook_署名の検証(t *testing.T) {
	s := &Stripe{WebhookSecret: testSecret}
	now := time.Now()
	body := subPayload("sub_1", StatusActive, false, 7, now.Add(30*24*time.Hour).Unix())

	t.Run("正しい署名なら通る", func(t *testing.T) {
		ev, err := s.ParseWebhook(body, sign(body, testSecret, now))
		if err != nil {
			t.Fatalf("通るはずが失敗した: %v", err)
		}
		if ev.SubscriptionID != "sub_1" || ev.Status != StatusActive {
			t.Errorf("読み取りが違う: %+v", ev)
		}
		if ev.OrganizationID != 7 {
			t.Errorf("組織IDが取れていない: %d", ev.OrganizationID)
		}
		if ev.PriceID != "price_TEST" {
			t.Errorf("価格IDが取れていない: %q", ev.PriceID)
		}
		if ev.CurrentPeriodEnd.IsZero() {
			t.Error("期末が取れていない")
		}
	})

	t.Run("署名が無ければ弾く", func(t *testing.T) {
		if _, err := s.ParseWebhook(body, ""); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("弾くはずが: %v", err)
		}
	})

	t.Run("別の鍵で署名されていたら弾く", func(t *testing.T) {
		bad := sign(body, "whsec_someone_elses_secret_key_value_x", now)
		if _, err := s.ParseWebhook(body, bad); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("弾くはずが: %v", err)
		}
	})

	t.Run("中身を1文字でも書き換えたら弾く", func(t *testing.T) {
		sig := sign(body, testSecret, now)
		// 署名を作った後に本文を差し替える。中間で改竄された状況。
		tampered := subPayload("sub_1", StatusActive, false, 999, now.Unix())
		if _, err := s.ParseWebhook(tampered, sig); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("弾くはずが: %v", err)
		}
	})

	t.Run("古い署名は弾く（再送攻撃）", func(t *testing.T) {
		old := sign(body, testSecret, now.Add(-2*time.Hour))
		if _, err := s.ParseWebhook(body, old); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("弾くはずが: %v", err)
		}
	})
}

func TestParseWebhook_削除は解約として扱う(t *testing.T) {
	s := &Stripe{WebhookSecret: testSecret}
	now := time.Now()
	body := []byte(`{"id":"evt_2","object":"event","api_version":"` + stripe.APIVersion + `","type":"customer.subscription.deleted",
	  "data":{"object":{"id":"sub_9","object":"subscription","status":"active",
	  "customer":"cus_X","metadata":{"organization_id":"3"},
	  "items":{"object":"list","data":[]}}}}`)

	ev, err := s.ParseWebhook(body, sign(body, testSecret, now))
	if err != nil {
		t.Fatal(err)
	}
	// 届いた status は active だが、削除の知らせなので解約として扱う。
	// そのまま信じると、解約済みの事務所が使い続けられる。
	if ev.Status != StatusCanceled {
		t.Errorf("解約として扱うべき: %q", ev.Status)
	}
}

func TestParseWebhook_支払いの成否(t *testing.T) {
	s := &Stripe{WebhookSecret: testSecret}
	now := time.Now()

	for _, c := range []struct {
		typ           string
		wantFailed    bool
		wantSucceeded bool
	}{
		{"invoice.payment_failed", true, false},
		{"invoice.payment_succeeded", false, true},
		{"invoice.paid", false, true},
	} {
		body := []byte(fmt.Sprintf(`{"id":"evt_3","object":"event","api_version":"`+stripe.APIVersion+`","type":"%s","data":{"object":{
		  "id":"in_1","object":"invoice","customer":"cus_Y",
		  "parent":{"subscription_details":{"subscription":"sub_5"}},
		  "lines":{"object":"list","data":[]}}}}`, c.typ))
		ev, err := s.ParseWebhook(body, sign(body, testSecret, now))
		if err != nil {
			t.Fatalf("%s: %v", c.typ, err)
		}
		if ev.PaymentFailed != c.wantFailed || ev.PaymentSucceeded != c.wantSucceeded {
			t.Errorf("%s: failed=%v succeeded=%v", c.typ, ev.PaymentFailed, ev.PaymentSucceeded)
		}
		if ev.SubscriptionID != "sub_5" {
			t.Errorf("%s: 契約IDが取れていない: %q", c.typ, ev.SubscriptionID)
		}
	}
}
