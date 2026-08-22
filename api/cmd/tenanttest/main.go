// tenanttest は「他の事務所のデータに手が届かない」ことを、実際に叩いて確かめる。
//
//	go run ./cmd/tenanttest -api http://localhost:58080
//
// ── なぜこれを作るのか ──
//
// 会計事務所は顧問先の帳簿を預かる仕事なので、「別の事務所のデータが見えた」は
// 起きた時点で終わる。だから「見えません」と言うだけでは足りず、
// 見えないことを目の前で示せる必要がある。
//
// このツールは、事務所を2つ作り、片方の署名でもう片方のあらゆる口を叩く。
// 全部が拒否されること、そして拒否されたあとも相手のデータが1行も
// 変わっていないことを、DBを直接見て確かめる。
//
// ── この種のテストで最も多い間違い ──
//
//	「拒否された」だけを確かめると、APIが落ちていても通ってしまう。
//
// 何を送っても失敗するなら、跨ぎも当然失敗する。それは守れている証拠ではない。
// なので1つの口につき必ず2回叩く。
//
//	跨いで叩く   → 拒否されること
//	持ち主が叩く → 拒否されないこと
//
// 両方が揃って初めて「拒否しているのは所有の確認であって、故障ではない」と言える。
//
// 持ち主側は、わざと別の理由で落ちる要求を送る場合がある（ファイルを付けない等）。
// 見たいのは「所有の確認を通り抜けたか」だけなので、
// 404 でなければよい。実際に取り込みや突合を走らせて、検証のたびに
// データを増やす必要はない。
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	apiURL     = flag.String("api", "http://localhost:58080", "APIのアドレス")
	dsn        = flag.String("dsn", os.Getenv("DATABASE_URL"), "DBの接続文字列")
	secret     = flag.String("secret", os.Getenv("DM_API_SECRET"), "画面とAPIの共有鍵")
	pass, fail int
)

// 検証用に作る2つの事務所。名前で見分けが付くようにしておく。
// 本物の事務所と紛れないよう、接頭辞を付ける。
const prefix = "【検証】"

type office struct {
	OrgID    int64
	UserID   int64
	ClientID int64
	DocID    int64
	AliasID  int64
	Name     string
}

func main() {
	flag.Parse()
	if *dsn == "" {
		*dsn = "postgres://dm:dm_dev_only@localhost:55432/denpo_match?sslmode=disable"
	}
	if *secret == "" {
		die("DM_API_SECRET が要ります。-secret か環境変数で渡してください")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		die("DBに繋がりません: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		die("DBに繋がりません: %v", err)
	}

	a := seed(ctx, pool, "A")
	b := seed(ctx, pool, "B")

	fmt.Printf("\n事務所A  org=%d user=%d client=%d doc=%d alias=%d\n",
		a.OrgID, a.UserID, a.ClientID, a.DocID, a.AliasID)
	fmt.Printf("事務所B  org=%d user=%d client=%d doc=%d alias=%d\n",
		b.OrgID, b.UserID, b.ClientID, b.DocID, b.AliasID)
	fmt.Println("（事務所と職員は残る。監査ログが職員を参照していて、" +
		"その監査ログは規則で消せないため。伝票まわりは毎回作り直す）")

	section("① 署名そのものの検証")
	checkSignature(a)

	// Bの中身を控える。
	//
	// 【重要】控えるのは②の直前で、比べるのは②の直後。
	// ③はBが自分の伝票を正当に承認するので、そこまで含めて比べると
	// 「跨がれた」ではなく「正しく承認された」を検出してしまう。
	// 最初に書いたときは実際にそれで3件落ちた。
	before := snapshot(ctx, pool, b)

	section("② 事務所を跨げないこと（Aの署名で、Bのものを触る）")
	checkCrossOffice(a, b)

	section("③ 跨いだあと、Bのデータが1行も変わっていないこと")
	after := snapshot(ctx, pool, b)
	for _, k := range snapshotKeys {
		ok(before[k] == after[k],
			fmt.Sprintf("%s が変わっていない（%s → %s）", k, before[k], after[k]))
	}

	section("④ 持ち主なら通ること（同じ口を、Bの署名で叩く）")
	checkOwnerPasses(b)

	section("⑤ 残した記録が、後から書き換えも削除もできないこと")
	checkAuditImmutable(ctx, pool, b)

	fmt.Printf("\n───────────────────────────\n")
	fmt.Printf("  通った %d 件 / 落ちた %d 件\n", pass, fail)
	if fail > 0 {
		fmt.Printf("  ❌ 事務所を跨いで手が届く経路があります\n\n")
		os.Exit(1)
	}
	fmt.Printf("  ✅ 事務所をまたぐ経路は見つかりませんでした\n\n")
}

// ── ① 署名 ──

func checkSignature(a office) {
	// 署名なし
	res, _ := raw("GET", "/v1/documents/"+itoa(a.DocID), "", nil)
	ok(res == 401, fmt.Sprintf("署名なしは拒否される（%d）", res))

	// 鍵違い
	res, _ = signedWith("wrong-secret-wrong-secret-wrong!", a.OrgID,
		"GET", "/v1/documents/"+itoa(a.DocID), nil)
	ok(res == 401, fmt.Sprintf("鍵が違えば拒否される（%d）", res))

	// 時刻ずれ（10分前）
	res, _ = signedAt(time.Now().Add(-10*time.Minute).Unix(), a.OrgID,
		"GET", "/v1/documents/"+itoa(a.DocID), nil)
	ok(res == 401, fmt.Sprintf("古い署名は拒否される（%d）", res))

	// 本文の差し替え。署名を作ったあとで本文だけ変える。
	res = tamperedBody(a)
	ok(res == 401, fmt.Sprintf("本文を差し替えると拒否される（%d）", res))

	// 事務所番号の差し替え。署名は事務所1、ヘッダは事務所2。
	res = tamperedOrg(a)
	ok(res == 401, fmt.Sprintf("事務所番号を書き換えると拒否される（%d）", res))

	// 正しく署名すれば通ること。これが無いと上の全部が「単に壊れている」で説明できる。
	res, _ = signed(a.OrgID, "GET", "/v1/documents/"+itoa(a.DocID), nil)
	ok(res == 200, fmt.Sprintf("正しく署名すれば通る（%d）", res))
}

// ── ② 事務所を跨ぐ試み ──

func checkCrossOffice(a, b office) {
	type attempt struct {
		what   string
		method string
		path   string
		body   string
	}

	// すべて「Aの署名で、Bのもの」を指す。
	attempts := []attempt{
		{"Bの伝票を読む", "GET", "/v1/documents/" + itoa(b.DocID), ""},
		{"Bの伝票を承認する", "POST", "/v1/documents/" + itoa(b.DocID) + "/decision",
			`{"actor_id":` + itoa(a.UserID) + `,"decision":2}`},
		{"Bの伝票の突合を確定する", "POST", "/v1/documents/" + itoa(b.DocID) + "/settlement",
			`{"actor_id":` + itoa(a.UserID) + `,"none":true}`},
		{"Bの顧問先で突合を走らせる", "POST", "/v1/settlements/run",
			`{"client_id":` + itoa(b.ClientID) + `}`},
		{"Bの覚えた表記を消す", "DELETE",
			"/v1/aliases/" + itoa(b.AliasID) + "?actor_id=" + itoa(a.UserID), ""},
	}

	for _, at := range attempts {
		var body io.Reader
		if at.body != "" {
			body = strings.NewReader(at.body)
		}
		code, _ := signed(a.OrgID, at.method, at.path, body)
		ok(code == 404,
			fmt.Sprintf("%s → 拒否（%d。あるとも無いとも答えない404）", at.what, code))
	}

	// アップロードと取り込みは multipart なので別に組む。
	code := uploadAs(a.OrgID, b.ClientID)
	ok(code == 404, fmt.Sprintf("Bの顧問先へアップロードする → 拒否（%d）", code))

	code = importAs(a.OrgID, b.ClientID)
	ok(code == 404, fmt.Sprintf("Bの顧問先へ明細を取り込む → 拒否（%d）", code))

	// 職員の跨ぎ。Aの伝票を、Bの職員IDで承認しようとする。
	// 監査ログに他事務所の職員が残らないこと。
	code, _ = signed(a.OrgID, "POST", "/v1/documents/"+itoa(a.DocID)+"/decision",
		strings.NewReader(`{"actor_id":`+itoa(b.UserID)+`,"decision":2}`))
	ok(code == 404,
		fmt.Sprintf("自分の伝票を、Bの職員IDで承認する → 拒否（%d）", code))

	// 契約状態。以前は organization_id を問い合わせ文字列で受け取っていたので、
	// 番号を書き換えれば他事務所の契約が読めた。今は署名から決まる。
	//
	// 「Bの番号を足したときの応答」と「何も足さないときの応答」が
	// 同じであることを見る。応答に organization_id が入っているかを
	// 調べる形にすると、そもそも入っていないので常に通ってしまう。
	_, plain := signed(a.OrgID, "GET", "/v1/billing", nil)
	code, withB := signed(a.OrgID, "GET", "/v1/billing?organization_id="+itoa(b.OrgID), nil)
	ok(code == 200 && plain == withB,
		fmt.Sprintf("URLに他事務所の番号を足しても応答が変わらない（%d・%v）",
			code, plain == withB))
}

// ── ③ 持ち主なら通ること ──
//
// ②と同じ口を、持ち主の署名で叩く。ここが全部落ちるなら、
// ②の「拒否」は所有の確認ではなく、単なる故障で説明できてしまう。
func checkOwnerPasses(b office) {
	code, _ := signed(b.OrgID, "GET", "/v1/documents/"+itoa(b.DocID), nil)
	ok(code == 200, fmt.Sprintf("自分の伝票を読む → 通る（%d）", code))

	code, _ = signed(b.OrgID, "GET", "/v1/aliases?limit=5", nil)
	ok(code == 200, fmt.Sprintf("自分の覚えた表記を読む → 通る（%d）", code))

	code, _ = signed(b.OrgID, "GET", "/v1/billing", nil)
	ok(code == 200, fmt.Sprintf("自分の契約状態を読む → 通る（%d）", code))

	// 以下は、所有の確認を通り抜けたかどうかだけを見る。
	// 実際に走らせるとデータが増えるので、わざと別の理由で落ちる要求を送る。
	// 404 でなければ、所有の確認は通っている。

	code = uploadAs(b.OrgID, b.ClientID) // ファイルを付けていないので 400 になる
	ok(code != 404,
		fmt.Sprintf("自分の顧問先へのアップロードは、所有の確認で止まらない（%d）", code))

	code = importAs(b.OrgID, b.ClientID)
	ok(code != 404,
		fmt.Sprintf("自分の顧問先への取り込みは、所有の確認で止まらない（%d）", code))

	code, _ = signed(b.OrgID, "POST", "/v1/settlements/run",
		strings.NewReader(`{"client_id":`+itoa(b.ClientID)+`}`))
	ok(code != 404,
		fmt.Sprintf("自分の顧問先の突合は、所有の確認で止まらない（%d）", code))

	// 承認は実際に通す。Bの伝票なので、Bのデータだけが変わる。
	code, _ = signed(b.OrgID, "POST", "/v1/documents/"+itoa(b.DocID)+"/decision",
		strings.NewReader(`{"actor_id":`+itoa(b.UserID)+`,"decision":2}`))
	ok(code == 200, fmt.Sprintf("自分の伝票を、自分の職員で承認する → 通る（%d）", code))
}

// ── ⑤ 記録が動かせないこと ──
//
// 事務所を分けられていても、承認の記録を後から書き換えられるなら、
// 「誰が何を承認したか」を示せたことにならない。
// DBの持ち主（dm ユーザ）が直接叩いても効かないことを見る。
func checkAuditImmutable(ctx context.Context, pool *pgxpool.Pool, o office) {
	var id int64
	var action, hash string
	err := pool.QueryRow(ctx, `
		SELECT id, action, row_hash FROM audit_logs
		 WHERE organization_id = $1 ORDER BY id DESC LIMIT 1`, o.OrgID).Scan(&id, &action, &hash)
	if err != nil {
		ok(false, "④で残ったはずの監査ログが見つからない: "+err.Error())
		return
	}

	// 消してみる。DO INSTEAD NOTHING の規則が効いていれば、何も起きない。
	if _, err := pool.Exec(ctx, `DELETE FROM audit_logs WHERE id = $1`, id); err != nil {
		ok(false, "削除しようとしてエラーになった（規則ではなく別の理由で止まっている）")
		return
	}
	var n int
	pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE id = $1`, id).Scan(&n)
	ok(n == 1, fmt.Sprintf("DELETE を投げても記録は消えない（id=%d）", id))

	// 書き換えてみる。
	if _, err := pool.Exec(ctx,
		`UPDATE audit_logs SET action = 'reject' WHERE id = $1`, id); err != nil {
		ok(false, "更新しようとしてエラーになった")
		return
	}
	var got string
	pool.QueryRow(ctx, `SELECT action FROM audit_logs WHERE id = $1`, id).Scan(&got)
	ok(got == action, fmt.Sprintf("UPDATE を投げても中身は変わらない（%s のまま）", got))

	// 連鎖のハッシュが入っていること。1行だけ差し替えても、後ろが合わなくなる。
	ok(hash != "", "行ごとのハッシュが記録されている（改ざんすると後続と合わなくなる）")
}

// ── 署名して叩く ──

func sign(sec string, method, path, query string, ts, org int64, body []byte) string {
	sum := sha256.Sum256(body)
	mac := hmac.New(sha256.New, []byte(sec))
	io.WriteString(mac, strings.Join([]string{
		"v1", method, path, query, itoa(ts), itoa(org),
		hex.EncodeToString(sum[:]),
	}, "\n"))
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

func signed(org int64, method, path string, body io.Reader) (int, string) {
	return signedAt(time.Now().Unix(), org, method, path, body)
}

func signedAt(ts, org int64, method, path string, body io.Reader) (int, string) {
	return doSigned(*secret, ts, org, method, path, body, "", nil)
}

func signedWith(sec string, org int64, method, path string, body io.Reader) (int, string) {
	return doSigned(sec, time.Now().Unix(), org, method, path, body, "", nil)
}

func doSigned(sec string, ts, org int64, method, path string, body io.Reader,
	contentType string, over map[string]string) (int, string) {

	var raw []byte
	if body != nil {
		raw, _ = io.ReadAll(body)
	}
	p, q, _ := strings.Cut(path, "?")

	req, err := http.NewRequest(method, *apiURL+path, bytes.NewReader(raw))
	if err != nil {
		die("要求を作れません: %v", err)
	}
	if contentType == "" {
		contentType = "application/json"
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-DM-Org", itoa(org))
	req.Header.Set("X-DM-Timestamp", itoa(ts))
	req.Header.Set("X-DM-Signature", sign(sec, method, p, q, ts, org, raw))
	for k, v := range over {
		req.Header.Set(k, v)
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		die("APIに繋がりません（%s）: %v", *apiURL, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func raw(method, path, body string, hdr map[string]string) (int, string) {
	req, _ := http.NewRequest(method, *apiURL+path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		die("APIに繋がりません（%s）: %v", *apiURL, err)
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)
	return res.StatusCode, ""
}

// 署名を作ったあとで本文を差し替える。
func tamperedBody(a office) int {
	ts := time.Now().Unix()
	honest := []byte(`{"actor_id":` + itoa(a.UserID) + `,"decision":2}`)
	evil := []byte(`{"actor_id":` + itoa(a.UserID) + `,"decision":4}`)
	path := "/v1/documents/" + itoa(a.DocID) + "/decision"

	req, _ := http.NewRequest("POST", *apiURL+path, bytes.NewReader(evil))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-DM-Org", itoa(a.OrgID))
	req.Header.Set("X-DM-Timestamp", itoa(ts))
	req.Header.Set("X-DM-Signature", sign(*secret, "POST", path, "", ts, a.OrgID, honest))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		die("APIに繋がりません: %v", err)
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)
	return res.StatusCode
}

// 署名は自分の事務所、ヘッダだけ別の事務所にする。
func tamperedOrg(a office) int {
	ts := time.Now().Unix()
	path := "/v1/billing"
	req, _ := http.NewRequest("GET", *apiURL+path, nil)
	req.Header.Set("X-DM-Org", itoa(a.OrgID+1))
	req.Header.Set("X-DM-Timestamp", itoa(ts))
	req.Header.Set("X-DM-Signature", sign(*secret, "GET", path, "", ts, a.OrgID, nil))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		die("APIに繋がりません: %v", err)
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)
	return res.StatusCode
}

// multipart の口。ファイルは付けない。
// 見たいのは所有の確認を通り抜けたかどうかで、実際に取り込むことではない。
func multipartAs(org, clientID int64, path string, extra map[string]string) int {
	var buf bytes.Buffer
	const boundary = "dmtenanttest"
	fields := map[string]string{"client_id": itoa(clientID)}
	for k, v := range extra {
		fields[k] = v
	}
	for k, v := range fields {
		fmt.Fprintf(&buf, "--%s\r\nContent-Disposition: form-data; name=%q\r\n\r\n%s\r\n",
			boundary, k, v)
	}
	fmt.Fprintf(&buf, "--%s--\r\n", boundary)

	code, _ := doSigned(*secret, time.Now().Unix(), org, "POST", path,
		bytes.NewReader(buf.Bytes()),
		"multipart/form-data; boundary="+boundary, nil)
	return code
}

func uploadAs(org, clientID int64) int {
	return multipartAs(org, clientID, "/v1/documents", map[string]string{"doc_type": "1"})
}

func importAs(org, clientID int64) int {
	return multipartAs(org, clientID, "/v1/transactions/import",
		map[string]string{"source_type": "1"})
}

// ── 事務所を作る・消す ──

// seed は検証用の事務所を用意する。
//
// ── 事務所と職員は作り直さない ──
//
// audit_logs.actor_id が users を参照していて、その audit_logs は
//
//	CREATE RULE audit_logs_no_delete AS ON DELETE TO audit_logs DO INSTEAD NOTHING;
//
// で消せないようになっている（db/migrations/004_audit.sql）。
// つまり一度でも承認を記録した職員は、二度と消せない。
//
// これは不便ではなく、この仕組みの土台そのものである。
// 記録を後から消せるなら、記録がある意味が無い。
// 検証ツールの都合で消す道を作るほうが間違っているので、
// 事務所と職員は残したまま、伝票まわりだけを毎回作り直す。
func seed(ctx context.Context, pool *pgxpool.Pool, tag string) office {
	o := office{Name: prefix + tag + "事務所"}
	q := func(sql string, args ...any) int64 {
		var id int64
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			die("検証用のデータを作れません（%s）: %v", tag, err)
		}
		return id
	}
	// 既にあれば使い回す。無ければ作る。
	find := func(sql string, args ...any) int64 {
		var id int64
		err := pool.QueryRow(ctx, sql, args...).Scan(&id)
		if err != nil {
			return 0
		}
		return id
	}

	email := "tenanttest-" + strings.ToLower(tag) + "@example.invalid"

	o.OrgID = find(`SELECT id FROM organizations WHERE name = $1`, o.Name)
	if o.OrgID == 0 {
		o.OrgID = q(`INSERT INTO organizations (name) VALUES ($1) RETURNING id`, o.Name)
	}
	o.UserID = find(`SELECT id FROM users WHERE email = $1`, email)
	if o.UserID == 0 {
		o.UserID = q(`INSERT INTO users (organization_id, email, name, role)
		              VALUES ($1, $2, $3, 2) RETURNING id`, o.OrgID, email, tag+"職員")
	}
	o.ClientID = find(`SELECT id FROM clients WHERE organization_id = $1 AND name = $2`,
		o.OrgID, prefix+tag+"顧問先")
	if o.ClientID == 0 {
		o.ClientID = q(`INSERT INTO clients (organization_id, name, ocr_engine)
		                VALUES ($1, $2, 'tesseract') RETURNING id`,
			o.OrgID, prefix+tag+"顧問先")
	}

	// 伝票まわりは毎回まっさらにする。前回の承認済みの状態が残っていると、
	// 「承認できた」のか「もともと承認済みだった」のか区別が付かない。
	resetClient(ctx, pool, o.ClientID)

	o.DocID = q(`INSERT INTO documents (client_id, doc_type, status, uploaded_by)
	             VALUES ($1, 1, 4, $2) RETURNING id`, o.ClientID, o.UserID)

	// 承認できる状態にするため、照合結果を1件置く。
	partnerID := q(`INSERT INTO partners (client_id, name, norm)
	                VALUES ($1, $2, $3) RETURNING id`,
		o.ClientID, prefix+tag+"商事", "ケンショウ"+tag+"ショウジ")
	q(`INSERT INTO match_results (document_id, partner_id, score, decision)
	   VALUES ($1, $2, 88.00, 2) RETURNING id`, o.DocID, partnerID)

	// 取り消せる別名（source=2）を1件。跨いで消せないことを見るために使う。
	o.AliasID = q(`INSERT INTO partner_aliases (partner_id, alias, norm, source)
	               VALUES ($1, $2, $3, 2) RETURNING id`,
		partnerID, tag+"ｼｮｳｼﾞ", "ケンショウ"+tag+"ショウジ")

	return o
}

var snapshotKeys = []string{"伝票の状態", "照合結果", "覚えた表記", "監査ログ", "顧問先の数"}

func snapshot(ctx context.Context, pool *pgxpool.Pool, o office) map[string]string {
	s := map[string]string{}
	one := func(key, sql string, args ...any) {
		var v *string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
			die("控えを取れません（%s）: %v", key, err)
		}
		if v == nil {
			s[key] = "(なし)"
		} else {
			s[key] = *v
		}
	}
	one("伝票の状態", `SELECT status::text FROM documents WHERE id = $1`, o.DocID)
	one("照合結果", `SELECT (decision::text || '/' || coalesce(decided_by::text,'-'))
	                 FROM match_results WHERE document_id = $1`, o.DocID)
	one("覚えた表記", `SELECT count(*)::text FROM partner_aliases a
	                   JOIN partners p ON p.id = a.partner_id
	                   WHERE p.client_id = $1`, o.ClientID)
	one("監査ログ", `SELECT count(*)::text FROM audit_logs WHERE organization_id = $1`, o.OrgID)
	one("顧問先の数", `SELECT count(*)::text FROM clients WHERE organization_id = $1`, o.OrgID)
	return s
}

// resetClient は、その顧問先にぶら下がる伝票まわりを消す。
//
// 【重要】顧問先IDを1つだけ受け取り、その配下しか触らない。
// 名前で広く消す形にすると、接頭辞の付け方を間違えたときに
// 本物の顧問先まで巻き込む。呼ぶ側は seed が引いたIDしか渡さない。
//
// audit_logs は消さない（消せない）。organizations と users も残す。
func resetClient(ctx context.Context, pool *pgxpool.Pool, clientID int64) {
	// 参照している側から順に消す。順番を間違えると外部キーで落ちる。
	stmts := []string{
		`DELETE FROM settlements            WHERE document_id IN (SELECT id FROM documents WHERE client_id = $1)`,
		`DELETE FROM settlement_candidates  WHERE document_id IN (SELECT id FROM documents WHERE client_id = $1)`,
		`DELETE FROM invoice_reg_checks     WHERE document_id IN (SELECT id FROM documents WHERE client_id = $1)`,
		`DELETE FROM extracted_fields       WHERE document_id IN (SELECT id FROM documents WHERE client_id = $1)`,
		`DELETE FROM match_candidates       WHERE document_id IN (SELECT id FROM documents WHERE client_id = $1)`,
		`DELETE FROM match_results          WHERE document_id IN (SELECT id FROM documents WHERE client_id = $1)`,
		`DELETE FROM jobs                   WHERE document_id IN (SELECT id FROM documents WHERE client_id = $1)`,
		`DELETE FROM ocr_results            WHERE document_page_id IN (SELECT p.id FROM document_pages p
		                                                               JOIN documents d ON d.id = p.document_id
		                                                              WHERE d.client_id = $1)`,
		`DELETE FROM document_pages         WHERE document_id IN (SELECT id FROM documents WHERE client_id = $1)`,
		`DELETE FROM documents              WHERE client_id = $1`,
		`DELETE FROM transactions           WHERE client_id = $1`,
		`DELETE FROM import_batches         WHERE client_id = $1`,
		`DELETE FROM thresholds             WHERE client_id = $1`,
		`DELETE FROM partner_aliases        WHERE partner_id IN (SELECT id FROM partners WHERE client_id = $1)`,
		`DELETE FROM partners               WHERE client_id = $1`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s, clientID); err != nil {
			die("検証用データの作り直しに失敗しました: %v\n  %s", err, s)
		}
	}
}

// ── 出力 ──

func section(title string) { fmt.Printf("\n%s\n", title) }

func ok(cond bool, what string) {
	if cond {
		pass++
		fmt.Printf("  ✅ %s\n", what)
		return
	}
	fail++
	fmt.Printf("  ❌ %s\n", what)
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\n"+format+"\n\n", args...)
	os.Exit(1)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
