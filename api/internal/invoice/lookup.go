package invoice

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Lookup は国税庁の適格請求書発行事業者公表システムに問い合わせる。
//
// なぜ形式検査と分けるか
//
//	形式検査（Evaluate）は通信しないので、必ず動く。
//	公表システムは、利用申請して発行されたアプリケーションIDが要る。
//	申請が通るまでの間も、桁数と検査数字の検査だけで
//	「明らかに誤っている番号」は見つかる。片方が使えないと
//	もう片方も動かない作りにしない。
//
// 【重要】この経路は、実際のアプリケーションIDでまだ動かしていない。
//
//	IDの発行には国税庁への利用申請が必要で、こちらでは取得できない。
//	要求の組み立て（下の url.Values）と応答の読み取りは、公表されている
//	仕様に沿って書いてあるが、実物と突き合わせて確かめてはいない。
//	IDが手に入った時点で、まず1件を手で叩いて応答の形を確認すること。
//	確認するまで、この経路を「照合済み」として顧客に説明してはいけない。
//
// 失敗したときに形式検査の結果を壊さない
//
//	通信が失敗したら、その伝票の状態は「形式は正しい（実在は未確認）」の
//	ままにする。「登録が見つからない」にしてはいけない。
//	通信の失敗と、登録が無いことは、まったく違う意味を持つ。
//	前者は再実行すればよく、後者は先方に問い合わせる必要がある。
type Lookup struct {
	// 利用申請で発行されるアプリケーションID。空なら無効。
	AppID string
	// 差し替えられるようにしておく。試験で本物に当てないため。
	BaseURL string
	Client  *http.Client
}

func NewLookup(appID string) *Lookup {
	return &Lookup{
		AppID: appID,
		// 国税庁 適格請求書発行事業者公表システム Web-API（番号による検索）
		BaseURL: "https://web-api.invoice-kohyo.nta.go.jp/1/num",
		Client:  &http.Client{Timeout: 20 * time.Second},
	}
}

func (l *Lookup) Configured() bool { return l != nil && l.AppID != "" }

// Entry は公表システムが返す1件。
type Entry struct {
	// T を含まない13桁。
	RegNo string
	// 法人なら商号または名称、個人なら氏名。
	Name string
	// 所在地。個人事業者は公表していないことがあるので空になりうる。
	Addr string
	// 登録が失効している場合、その年月日。空なら有効。
	RevokedOn string
}

// Fetch は番号をまとめて引く。
//
// 1回に渡せる件数には上限があるので、呼ぶ側で分ける必要はない。
// ここで区切って複数回に分ける。会計事務所は月末に数百件をまとめて回す。
func (l *Lookup) Fetch(ctx context.Context, regNos []string) (map[string]Entry, error) {
	if !l.Configured() {
		return nil, fmt.Errorf("国税庁の公表システムを使う設定がありません")
	}
	out := map[string]Entry{}
	const batch = 10 // 公表システムの1回あたりの上限
	for i := 0; i < len(regNos); i += batch {
		j := i + batch
		if j > len(regNos) {
			j = len(regNos)
		}
		part, err := l.fetchOne(ctx, regNos[i:j])
		if err != nil {
			// 途中まで引けたものは返す。全部捨てると、
			// 500件のうち1件の通信失敗で全部やり直しになる。
			return out, err
		}
		for k, v := range part {
			out[k] = v
		}
	}
	return out, nil
}

func (l *Lookup) fetchOne(ctx context.Context, regNos []string) (map[string]Entry, error) {
	// 番号は T を外した13桁で渡す。
	nums := make([]string, 0, len(regNos))
	for _, s := range regNos {
		n := Normalize(s)
		nums = append(nums, strings.TrimPrefix(n, "T"))
	}

	q := url.Values{}
	q.Set("id", l.AppID)
	q.Set("number", strings.Join(nums, ","))
	q.Set("type", "21") // 21:JSON
	q.Set("history", "0")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		l.BaseURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	res, err := l.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("公表システムに繋がりません: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("公表システムが %d を返しました", res.StatusCode)
	}

	var body struct {
		Count        any `json:"count"`
		Announcement []struct {
			RegistratedNumber string `json:"registratedNumber"`
			Name              string `json:"name"`
			Address           string `json:"address"`
			DisposalDate      string `json:"disposalDate"`
		} `json:"announcement"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("公表システムの応答を読めません: %w", err)
	}

	out := map[string]Entry{}
	for _, a := range body.Announcement {
		out["T"+a.RegistratedNumber] = Entry{
			RegNo:     a.RegistratedNumber,
			Name:      a.Name,
			Addr:      a.Address,
			RevokedOn: a.DisposalDate,
		}
	}
	return out, nil
}

// Apply は問い合わせ結果を状態に反映する。
//
// 【重要】見つからなかったことと、通信できなかったことを分ける。
// この関数は「引けた」ことが前提。引けなかったときは呼ばない。
func Apply(cur Result, e Entry, found bool) Result {
	if !found {
		cur.Status = StatusNotFound
		cur.Why = "国税庁の公表システムに、この番号の登録がありません。" +
			"記載の誤りか、登録前の番号である可能性があります"
		return cur
	}
	if e.RevokedOn != "" {
		cur.Status = StatusNotFound
		cur.Why = "登録が " + e.RevokedOn + " に失効しています"
		return cur
	}
	cur.Status = StatusRegistered
	cur.Why = "国税庁の公表システムで登録を確認しました（" + e.Name + "）"
	return cur
}
