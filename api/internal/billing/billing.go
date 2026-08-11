// Package billing は契約状態から「何ができるか」を決める。
//
// 決めたこと（諏訪さんの承認済み）
//
//	① 契約が切れたら、新しい伝票をアップロードできない。
//	   過去の伝票と監査ログは見られる。
//	   会計事務所は顧問先の帳簿を預かる仕事なので、
//	   解約後に税務調査が来たときに何も出せない状態は現実的でない。
//
//	③ 支払いが失敗しても14日は今までどおり動く。
//	   カードの期限切れは普通に起きる。その日のうちに止まると、
//	   事務所の業務が朝いちで止まる。
//
// この package は Stripe にも DB にも触らない。
// 状態を受け取って判定を返すだけにしてある。理由は2つ。
//
//	① ここが最も間違えてはいけない場所だから。外部に繋がずにテストできる形にする。
//	   「支払っているのに使えない」も「解約したのに使える」も、どちらも事故になる。
//	② Stripe の状態名が増えたとき、影響がこのファイルに閉じる。
package billing

import (
	"fmt"
	"time"
)

// GraceDays は支払い失敗から何日を猶予とするか。
//
// 14日の根拠: カードの期限切れに気付いて再登録するまでの時間。
// Stripe の既定の再試行（dunning）は約3週間かけて4回行うので、
// その途中で人が気付ける長さにしてある。
const GraceDays = 14

// Stripe の状態をそのまま使う。独自の名前に翻訳しない。
// 翻訳すると、Stripe の画面で見た値とDBの値が食い違い、調査のときに混乱する。
const (
	StatusTrialing          = "trialing"
	StatusActive            = "active"
	StatusPastDue           = "past_due"
	StatusUnpaid            = "unpaid"
	StatusCanceled          = "canceled"
	StatusIncomplete        = "incomplete"
	StatusIncompleteExpired = "incomplete_expired"
	StatusPaused            = "paused"
)

// Subscription は判定に必要な情報だけを持つ。
type Subscription struct {
	Status            string
	CurrentPeriodEnd  time.Time
	GraceUntil        *time.Time // 支払い失敗後、ここまでは動かす
	CancelAtPeriodEnd bool
}

// Org は組織側の事情。
type Org struct {
	// 課金の免除。社内利用・無償提供先のため。
	// 【重要】既定は false。「うっかり全社無料」が起きない側に倒してある。
	BillingExempt bool
}

// Decision は判定の結果。
//
// なぜ理由を持つのか
//
//	「アップロードできません」とだけ出す画面は、現場が次に何をすればよいか
//	分からない。カードが切れたのか、解約したのか、そもそも契約していないのかで
//	やることが違う。理由と、次にやることを一緒に返す。
type Decision struct {
	CanUpload bool
	// 画面にそのまま出す文言。
	Reason string
	// 次にやること。空なら何もしなくてよい。
	NextStep string
	// 猶予期間中か。画面に警告を出すのに使う。
	InGrace bool
	// 猶予の終わり。InGrace のときだけ意味がある。
	GraceUntil time.Time
}

// Evaluate は「今アップロードできるか」を決める。
//
// now を引数に取るのは、テストで時刻を動かすため。
// time.Now() を中で呼ぶと、猶予期間の境界をテストできない。
func Evaluate(org Org, sub *Subscription, now time.Time) Decision {
	if org.BillingExempt {
		return Decision{CanUpload: true, Reason: "課金の免除が設定されています"}
	}

	if sub == nil {
		return Decision{
			CanUpload: false,
			Reason:    "ご契約がありません",
			NextStep:  "契約の画面からお申し込みください",
		}
	}

	switch sub.Status {
	case StatusActive, StatusTrialing:
		d := Decision{CanUpload: true}
		if sub.CancelAtPeriodEnd {
			// 解約予定でも期末までは使える。ただし黙っていると、
			// ある朝いきなり止まって「何も知らされていない」ことになる。
			d.Reason = fmt.Sprintf("%s に解約予定です",
				sub.CurrentPeriodEnd.Format("2006年1月2日"))
			d.NextStep = "続けて使う場合は、契約の画面から解約を取り消してください"
		}
		return d

	case StatusPastDue, StatusUnpaid:
		// ③ 猶予期間。支払いが失敗しても、ここまでは今までどおり動かす。
		if sub.GraceUntil != nil && now.Before(*sub.GraceUntil) {
			return Decision{
				CanUpload:  true,
				InGrace:    true,
				GraceUntil: *sub.GraceUntil,
				Reason: fmt.Sprintf("お支払いを確認できていません。%s までは今までどおりご利用いただけます",
					sub.GraceUntil.Format("2006年1月2日")),
				NextStep: "契約の画面からお支払い方法をご確認ください",
			}
		}
		return Decision{
			CanUpload: false,
			Reason:    "お支払いを確認できず、猶予期間を過ぎました",
			NextStep:  "契約の画面からお支払い方法をご確認ください",
		}

	case StatusCanceled:
		return Decision{
			CanUpload: false,
			Reason:    "ご契約が終了しています",
			NextStep:  "続けて使う場合は、契約の画面からお申し込みください",
		}

	case StatusIncomplete, StatusIncompleteExpired:
		return Decision{
			CanUpload: false,
			Reason:    "お申し込みが完了していません",
			NextStep:  "契約の画面からお手続きをやり直してください",
		}

	case StatusPaused:
		return Decision{
			CanUpload: false,
			Reason:    "ご契約が一時停止されています",
			NextStep:  "契約の画面から再開してください",
		}
	}

	// 【重要】知らない状態は「使えない」ではなく「使える」に倒す。
	//
	// Stripe が新しい状態名を足したとき、こちらが対応するまでの間、
	// 支払っている事務所が使えなくなるほうが害が大きい。
	// 使えてしまう側の損失は数日分の利用料だが、
	// 使えない側の損失は「業務が止まる」で、取り返しがつかない。
	// 記録は残すので、後から気付ける。
	return Decision{
		CanUpload: true,
		Reason:    fmt.Sprintf("契約の状態を判断できませんでした（%s）", sub.Status),
		NextStep:  "そのままご利用いただけます。状況の確認をお待ちください",
	}
}

// StartGrace は支払いが失敗したときの猶予の終わりを返す。
func StartGrace(now time.Time) time.Time {
	return now.AddDate(0, 0, GraceDays)
}
