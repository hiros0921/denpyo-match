// 呼び出し元の確認と、事務所の取り違え防止。
//
// ── なぜ要るのか ──
//
// この API は当初「事務所の内側からしか呼ばれない」前提で書いた（httpapi.go 冒頭）。
// 1事務所に1台ずつ置くなら、その前提で正しい。
//
// 複数の事務所で1台を共有した瞬間に、前提が崩れる。
// 具体的には、認証を入れる前は次が全部通っていた。
//
//	GET /v1/documents/1        伝票IDを1から順に叩けば、全事務所の伝票が読める
//	GET /v1/billing?organization_id=1   番号を書き換えれば他事務所の契約が読める
//	POST /v1/documents         client_id を他事務所のものにすれば、そこへ投げ込める
//
// ── 直し方の考え方 ──
//
// 「認証を足す」だけでは足りない。認証は「誰が呼んだか」しか答えないので、
// 呼んだ人が organization_id を自己申告する形が残ると、番号の書き換えは防げない。
//
// なので、事務所番号を「本文や問い合わせ文字列に書いてある値」から
// 「署名に含まれる値」に移す。署名を検証した時点で事務所が確定し、
// 以後どのハンドラも本文の organization_id を読まない。
//
//	【重要】事務所番号の出どころを1つに保つこと。
//	署名ヘッダと本文の両方から読める状態にすると、片方だけを見るハンドラが
//	必ず1つ紛れ込む。そして紛れ込んだことは、事故が起きるまで分からない。
//
// ── 方式 ──
//
// 呼ぶのは Rails だけで、ブラウザからは呼ばれない。
// 利用者のセッションではなく、共有鍵による署名にする。
//
//	X-DM-Org        事務所ID
//	X-DM-Timestamp  UNIX秒
//	X-DM-Signature  v1=<HMAC-SHA256 の16進>
//
// 署名する文字列（改行で連結）:
//
//	v1
//	POST
//	/v1/documents
//	<問い合わせ文字列（そのまま）>
//	<UNIX秒>
//	<事務所ID>
//	<本文の SHA-256（16進）>
//
// 鍵そのものは通信に乗らない。本文まで署名に含めるので、
// 途中で本文だけ差し替えることもできない。
//
//	【承知のうえの穴】同じ要求をそのまま録って投げ直す（リプレイ）は、
//	許容時間の 300 秒以内なら通る。防ぐには使用済み署名の記録が要るが、
//	そのためだけに状態を持つ台を増やす価値が、今の構成では釣り合わない。
//	記録を持つのは、この API を事務所の外へ出すときにする。
package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hiros0921/denpo-match/api/internal/store"
)

// 署名の許容時間。これを外れた要求は受け付けない。
//
// Rails と Go は同じ台の中で動くので時計はずれない。
// 短くしすぎると、時計が少し狂った台に置いたときに「たまに通らない」
// という最も調べにくい壊れ方をするので、5分取る。
const authSkew = 5 * time.Minute

// 署名の版。方式を変えるときはここを上げる。
// 版を署名の中身にも含めるのは、方式を変えたあとに古い署名を
// 黙って受け付けてしまわないようにするため。
const authVersion = "v1"

type ctxKey int

const orgKey ctxKey = 1

// Auth は呼び出し元の確認を行う。
//
// Secret が空のときは New で弾く。ここに来た時点では必ず入っている。
type Auth struct {
	Secret []byte

	// 確認を行わない口。
	//
	//	/healthz            compose の healthcheck が叩く。DBの生死しか答えない
	//	/v1/billing/webhook Stripe が叩く。あちらは Stripe-Signature で検証済み
	//
	// 【重要】ここに口を足すときは、その口が事務所を跨いで何も返さないことを
	// 確かめること。「認証が要らない」と「誰の情報も出さない」は別の話。
	Skip map[string]bool

	// 本文の上限。署名のために本文を全部読むので、ここで頭を押さえる。
	MaxBody int64

	// 検証を行わない。開発でだけ立てる。
	//
	// 【重要】この場合でも事務所IDは X-DM-Org から取る。
	// 「検証しない」と「事務所を区別しない」は別の話で、後者にすると
	// 開発中だけ全事務所が混ざり、事務所を跨ぐ不具合が手元で再現しなくなる。
	Off bool
}

// 鍵の最短の長さ。
//
// HMAC-SHA256 の鍵は短くても動くが、短ければ総当たりで割り出せる。
// 「動くから大丈夫」と見えてしまうのが厄介なので、起動時に弾く。
const minSecretLen = 32

// NewAuth は共有鍵から検証器を作る。
func NewAuth(secret []byte, maxBody int64) (*Auth, error) {
	if len(secret) < minSecretLen {
		return nil, errors.New("DM_API_SECRET が短すぎます（" +
			strconv.Itoa(len(secret)) + "文字）。" +
			strconv.Itoa(minSecretLen) + "文字以上にしてください")
	}
	return &Auth{Secret: secret, MaxBody: maxBody, Skip: defaultSkip()}, nil
}

// NoAuth は検証を行わない検証器を作る。開発専用。
func NoAuth() *Auth {
	return &Auth{Off: true, Skip: defaultSkip()}
}

func defaultSkip() map[string]bool {
	return map[string]bool{
		"/healthz":            true,
		"/v1/billing/webhook": true,
	}
}

// authError は呼び出し側に返す理由を持つ。
//
// 【重要】画面には出さない。ここでの失敗は現場の操作ミスではなく設定の誤りで、
// 詳しい理由を外に返すと、鍵を持たない相手に手がかりを渡すことになる。
// 記録には残し、応答は一律にする。
type authError struct{ detail string }

func (e authError) Error() string { return e.detail }

// orgFrom は署名を検証済みの事務所IDを取り出す。
//
// 【重要】ハンドラは必ずこれを使う。r.URL.Query() や本文から
// organization_id を読んではいけない。読めてしまう限り、いつか読む人が出る。
func orgFrom(ctx context.Context) (int64, bool) {
	v, ok := ctx.Value(orgKey).(int64)
	return v, ok && v > 0
}

// withAuth は署名を確かめ、事務所IDを文脈に載せる。
func (s *Server) withAuth(a *Auth, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Skip[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}

		if a.Off {
			// 検証はしないが、事務所は header から取る。
			orgID, err := strconv.ParseInt(r.Header.Get("X-DM-Org"), 10, 64)
			if err != nil || orgID <= 0 {
				writeErr(w, http.StatusBadRequest, "X-DM-Org を指定してください")
				return
			}
			next.ServeHTTP(w, r.WithContext(
				context.WithValue(r.Context(), orgKey, orgID)))
			return
		}

		orgID, body, err := a.verify(r)
		if err != nil {
			// 何が足りなかったかは記録にだけ残す。
			s.Log.Warn("要求を拒否", "path", r.URL.Path, "理由", err.Error(),
				"from", r.RemoteAddr)
			writeErr(w, http.StatusUnauthorized, "この要求は受け付けられません")
			return
		}

		// 本文は署名の計算で読み切っているので、ハンドラのために戻す。
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), orgKey, orgID)))
	})
}

func (a *Auth) verify(r *http.Request) (orgID int64, body []byte, err error) {
	orgID, err = strconv.ParseInt(r.Header.Get("X-DM-Org"), 10, 64)
	if err != nil || orgID <= 0 {
		return 0, nil, authError{"X-DM-Org がない、または正しくない"}
	}

	ts, err := strconv.ParseInt(r.Header.Get("X-DM-Timestamp"), 10, 64)
	if err != nil {
		return 0, nil, authError{"X-DM-Timestamp がない、または正しくない"}
	}
	if d := time.Since(time.Unix(ts, 0)); d > authSkew || d < -authSkew {
		return 0, nil, authError{"時刻が離れすぎている（" + d.Truncate(time.Second).String() + "）"}
	}

	got := r.Header.Get("X-DM-Signature")
	if !strings.HasPrefix(got, authVersion+"=") {
		return 0, nil, authError{"X-DM-Signature の版が違う"}
	}
	gotRaw, err := hex.DecodeString(strings.TrimPrefix(got, authVersion+"="))
	if err != nil {
		return 0, nil, authError{"X-DM-Signature が16進でない"}
	}

	body, err = io.ReadAll(http.MaxBytesReader(nil, r.Body, a.MaxBody))
	if err != nil {
		return 0, nil, authError{"本文を読めない（上限超過の可能性）"}
	}

	want := a.sign(r.Method, r.URL.Path, r.URL.RawQuery, ts, orgID, body)

	// 【重要】バイト列の比較に == や bytes.Equal を使わない。
	// 先頭から順に比べて違ったところで打ち切る比較は、掛かった時間から
	// 「どこまで合っていたか」が漏れる。hmac.Equal は最後まで比べる。
	if !hmac.Equal(gotRaw, want) {
		return 0, nil, authError{"署名が合わない"}
	}
	return orgID, body, nil
}

func (a *Auth) sign(method, path, query string, ts, orgID int64, body []byte) []byte {
	sum := sha256.Sum256(body)
	mac := hmac.New(sha256.New, a.Secret)
	// 改行で区切る。区切らずに連結すると、項目の切れ目が動いたときに
	// 別々の要求が同じ署名を持ちうる。
	io.WriteString(mac, strings.Join([]string{
		authVersion,
		method,
		path,
		query,
		strconv.FormatInt(ts, 10),
		strconv.FormatInt(orgID, 10),
		hex.EncodeToString(sum[:]),
	}, "\n"))
	return mac.Sum(nil)
}

// ── 所有の確認 ──
//
// 署名で「どの事務所か」は確定する。だが要求の中身が指す顧問先や伝票が、
// その事務所のものとは限らない。番号を1つずらすだけで隣の事務所に届く。
// 署名の検証と、この確認は、別の仕事である。

// ErrForbidden は、確かに存在するが、その事務所のものではないことを表す。
var errForbidden = errors.New("この事務所のものではありません")

// ownClient は顧問先がその事務所のものであることを確かめる。
//
// 【重要】「無い」と「他所のもの」を、応答では区別しない。
// 区別すると、番号を順に叩くだけで「どの番号が実在するか」が分かる。
// 記録には区別して残す。調べるときに必要になるのはこちらだけ。
func (s *Server) ownClient(r *http.Request, clientID int64) error {
	orgID, ok := orgFrom(r.Context())
	if !ok {
		return errForbidden
	}
	owner, err := s.St.OrgIDForClient(r.Context(), clientID)
	if errors.Is(err, store.ErrNotFound) {
		return errForbidden
	}
	if err != nil {
		return err
	}
	if owner != orgID {
		s.Log.Warn("他事務所の顧問先が指定された",
			"要求元", orgID, "顧問先", clientID, "持ち主", owner)
		return errForbidden
	}
	return nil
}

// ownDocument は伝票がその事務所のものであることを確かめる。
func (s *Server) ownDocument(r *http.Request, docID int64) error {
	orgID, ok := orgFrom(r.Context())
	if !ok {
		return errForbidden
	}
	owner, err := s.St.OrgIDForDocument(r.Context(), docID)
	if errors.Is(err, store.ErrNotFound) {
		return errForbidden
	}
	if err != nil {
		return err
	}
	if owner != orgID {
		s.Log.Warn("他事務所の伝票が指定された",
			"要求元", orgID, "伝票", docID, "持ち主", owner)
		return errForbidden
	}
	return nil
}

// ownActor は操作者がその事務所の職員であることを確かめる。
//
// 【重要】これを省くと、監査ログに他事務所の職員IDが残せてしまう。
// 「誰が承認したか」を後から示せることがこの仕組みの土台なので、
// そこに他所の人間を書き込める口があってはいけない。
func (s *Server) ownActor(r *http.Request, actorID int64) error {
	orgID, ok := orgFrom(r.Context())
	if !ok {
		return errForbidden
	}
	owner, err := s.St.OrgIDForUser(r.Context(), actorID)
	if errors.Is(err, store.ErrNotFound) {
		return errForbidden
	}
	if err != nil {
		return err
	}
	if owner != orgID {
		s.Log.Warn("他事務所の職員が操作者に指定された",
			"要求元", orgID, "職員", actorID, "所属", owner)
		return errForbidden
	}
	return nil
}

// denyOwn は所有の確認に落ちたときの応答を書く。
// 404 を返す。403 だと「あるが見せない」と分かってしまう。
func (s *Server) denyOwn(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errForbidden) {
		writeErr(w, http.StatusNotFound, "対象が見つかりません")
		return true
	}
	s.Log.Error("所有の確認に失敗", "err", err)
	writeErr(w, http.StatusInternalServerError, "確認できませんでした")
	return true
}
