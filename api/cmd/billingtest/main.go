// billingtest は契約の一生を通しで確かめる。
//
// 何を確かめるか
//
//	契約なし        → アップロードできない
//	契約した        → できる
//	支払い失敗      → 猶予の内はできる（③）
//	猶予を過ぎた    → できない（①）
//	支払いが通った  → 猶予が解除され、またできる
//	解約した        → できない
//
// Webhook は Stripe から届くものだが、開発中は localhost に届かない。
// こちらで正しい署名を作って投げる。署名の作り方は Stripe と同じなので、
// 受け取り側の検証・反映・判定を、実物と同じ経路で確かめられる。
//
//	go run ./cmd/billingtest -api http://localhost:58080 -client 3 -org 1
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe/stripe-go/v86"
)

var (
	api        = flag.String("api", "http://localhost:58080", "APIのアドレス")
	client     = flag.Int64("client", 3, "顧問先ID")
	org        = flag.Int64("org", 1, "組織ID")
	image      = flag.String("image", "", "アップロードに使う画像")
	dsn        = flag.String("dsn", os.Getenv("DATABASE_URL"), "DB（猶予の時刻を動かすのに使う）")
	whsec      = flag.String("whsec", os.Getenv("STRIPE_WEBHOOK_SECRET"), "Webhook署名の鍵")
	subID      = "sub_billingtest_1"
	custID     = "cus_billingtest_1"
	pass, fail int
)

func main() {
	flag.Parse()
	if *whsec == "" {
		die("STRIPE_WEBHOOK_SECRET が要ります")
	}
	if *image == "" {
		die("-image に画像を指定してください")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		die("DBに繋がりません: %v", err)
	}
	defer pool.Close()

	cleanup(ctx, pool)
	defer cleanup(ctx, pool)

	fmt.Println()
	step("契約が無い状態")
	check("アップロードできない", !canUpload())

	step("契約した（active）")
	sendSub(StatusActive, false, time.Now().AddDate(0, 1, 0))
	check("アップロードできる", canUpload())
	check("状態が active", statusIs("active"))

	step("支払いが失敗した")
	sendInvoice("invoice.payment_failed")
	check("猶予が始まっている", graceSet(ctx, pool))
	check("猶予の内はアップロードできる（③）", canUpload())

	step("猶予を過ぎた（時刻を巻き戻して再現）")
	expireGrace(ctx, pool)
	check("アップロードできない（①）", !canUpload())
	check("過去の伝票は見られる", canRead())

	step("支払いが通った")
	sendInvoice("invoice.payment_succeeded")
	check("猶予が解除された", !graceSet(ctx, pool))
	check("またアップロードできる", canUpload())

	step("解約した")
	sendSub(StatusCanceled, false, time.Now())
	check("アップロードできない", !canUpload())
	check("過去の伝票は見られる", canRead())

	step("同じ知らせが2回届いた")
	before := auditCount(ctx, pool)
	// 【注意】イベントIDが他と重ならないようにする。
	// 期末の秒から作ると、前の手順とたまたま同じ秒になって
	// 「処理済み」で弾かれ、試験のほうが誤って落ちる。
	ev := dupEvent()
	post(ev)
	post(ev) // 同じイベントIDで2回
	check("監査ログが1行だけ増える（べき等）", auditCount(ctx, pool)-before == 1)

	step("署名が正しくない知らせ")
	code := postRaw(subEvent(StatusActive, false, time.Now()), "t=1,v1=deadbeef")
	check("400で弾かれる", code == 400)

	fmt.Printf("\n  合格 %d / 不合格 %d\n", pass, fail)
	if fail > 0 {
		os.Exit(1)
	}
}

const StatusActive, StatusCanceled = "active", "canceled"

// べき等の確認用。IDを1つに固定して2回送る。
func dupEvent() []byte {
	return []byte(fmt.Sprintf(`{
	  "id": "evt_billingtest_dup_%d", "object": "event", "api_version": "%s",
	  "type": "customer.subscription.updated",
	  "data": {"object": {"id":"%s","object":"subscription","status":"active",
	    "cancel_at_period_end": false, "customer":"%s",
	    "metadata":{"organization_id":"%d"},
	    "items":{"object":"list","data":[]}}}}`,
		time.Now().UnixNano(), stripe.APIVersion, subID, custID, *org))
}

// ── 確認 ──

func step(s string) { fmt.Printf("\n── %s ──\n", s) }

func check(name string, ok bool) {
	if ok {
		pass++
		fmt.Printf("  ✅ %s\n", name)
	} else {
		fail++
		fmt.Printf("  ❌ %s\n", name)
	}
}

func canUpload() bool {
	f, err := os.Open(*image)
	if err != nil {
		die("画像を開けません: %v", err)
	}
	defer f.Close()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("client_id", fmt.Sprint(*client))
	fw, _ := mw.CreateFormFile("file", filepath.Base(*image))
	_, _ = io.Copy(fw, f)
	mw.Close()

	req, _ := http.NewRequest("POST", *api+"/v1/documents", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		die("%v", err)
	}
	defer res.Body.Close()
	return res.StatusCode == http.StatusAccepted
}

// 契約が切れても過去の伝票は見られる（①の要）。
func canRead() bool {
	res, err := http.Get(fmt.Sprintf("%s/v1/documents/1?candidates=0", *api))
	if err != nil {
		return false
	}
	defer res.Body.Close()
	// 404 は「その伝票が無い」であって、契約による遮断ではない。
	return res.StatusCode == http.StatusOK || res.StatusCode == http.StatusNotFound
}

func statusIs(want string) bool {
	res, err := http.Get(fmt.Sprintf("%s/v1/billing?organization_id=%d", *api, *org))
	if err != nil {
		return false
	}
	defer res.Body.Close()
	var v map[string]any
	_ = json.NewDecoder(res.Body).Decode(&v)
	return v["status"] == want
}

// ── Webhook を送る ──

func subEvent(status string, cancelAtEnd bool, periodEnd time.Time) []byte {
	return []byte(fmt.Sprintf(`{
	  "id": "evt_billingtest_%s_%d", "object": "event",
	  "api_version": "%s",
	  "type": "customer.subscription.updated",
	  "data": {"object": {
	    "id": "%s", "object": "subscription", "status": "%s",
	    "cancel_at_period_end": %t, "customer": "%s",
	    "metadata": {"organization_id": "%d"},
	    "items": {"object":"list","data":[{"id":"si_1","object":"subscription_item",
	      "current_period_end": %d, "price": {"id":"price_test","object":"price"}}]}
	  }}}`, status, periodEnd.Unix(), stripe.APIVersion,
		subID, status, cancelAtEnd, custID, *org, periodEnd.Unix()))
}

func sendSub(status string, cancelAtEnd bool, periodEnd time.Time) {
	if c := post(subEvent(status, cancelAtEnd, periodEnd)); c != 200 {
		die("契約の知らせが通りません: HTTP %d", c)
	}
}

func sendInvoice(typ string) {
	body := []byte(fmt.Sprintf(`{
	  "id": "evt_billingtest_%s_%d", "object": "event", "api_version": "%s", "type": "%s",
	  "data": {"object": {"id":"in_1","object":"invoice","customer":"%s",
	    "parent":{"subscription_details":{"subscription":"%s"}},
	    "lines":{"object":"list","data":[]}}}}`,
		typ, time.Now().UnixNano(), stripe.APIVersion, typ, custID, subID))
	if c := post(body); c != 200 {
		die("請求の知らせが通りません（%s）: HTTP %d", typ, c)
	}
}

func post(body []byte) int { return postRaw(body, sign(body, *whsec, time.Now())) }

func postRaw(body []byte, sig string) int {
	req, _ := http.NewRequest("POST", *api+"/v1/billing/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", sig)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		die("%v", err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode
}

// Stripe と同じ形の署名を作る。
func sign(payload []byte, secret string, ts time.Time) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", ts.Unix())
	mac.Write(payload)
	return fmt.Sprintf("t=%d,v1=%s", ts.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

// ── DB を直接見る／動かす ──

func graceSet(ctx context.Context, p *pgxpool.Pool) bool {
	var n int
	_ = p.QueryRow(ctx, `SELECT count(*) FROM subscriptions
	  WHERE stripe_subscription_id=$1 AND grace_until IS NOT NULL`, subID).Scan(&n)
	return n > 0
}

// 14日待つわけにいかないので、猶予の終わりを過去にする。
// 時計を進めるのと同じ意味になる。
func expireGrace(ctx context.Context, p *pgxpool.Pool) {
	_, err := p.Exec(ctx, `UPDATE subscriptions SET grace_until = now() - interval '1 second'
	  WHERE stripe_subscription_id=$1`, subID)
	if err != nil {
		die("猶予を動かせません: %v", err)
	}
}

func auditCount(ctx context.Context, p *pgxpool.Pool) int {
	var n int
	_ = p.QueryRow(ctx, `SELECT count(*) FROM audit_logs
	  WHERE target_table='subscriptions' AND organization_id=$1`, *org).Scan(&n)
	return n
}

func cleanup(ctx context.Context, p *pgxpool.Pool) {
	_, _ = p.Exec(ctx, `DELETE FROM subscriptions WHERE stripe_subscription_id=$1`, subID)
	_, _ = p.Exec(ctx, `DELETE FROM stripe_events WHERE id LIKE 'evt_billingtest%'`)
	// アップロードできた分の伝票を片付ける
	_, _ = p.Exec(ctx, `DELETE FROM documents WHERE client_id=$1 AND uploaded_at > now() - interval '10 minutes'`, *client)
}

func die(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "  "+f+"\n", a...)
	os.Exit(1)
}
