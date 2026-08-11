package billing

import (
	"testing"
	"time"
)

var base = time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)

func ptr(t time.Time) *time.Time { return &t }

// 「支払っているのに使えない」と「解約したのに使える」は、どちらも事故になる。
// 状態ごとに1件ずつ、境界も含めて固定する。
func TestEvaluate(t *testing.T) {
	cases := []struct {
		name      string
		org       Org
		sub       *Subscription
		now       time.Time
		canUpload bool
		inGrace   bool
	}{
		{
			name: "契約中はアップロードできる",
			sub:  &Subscription{Status: StatusActive, CurrentPeriodEnd: base.AddDate(0, 1, 0)},
			now:  base, canUpload: true,
		},
		{
			name: "試用中もアップロードできる",
			sub:  &Subscription{Status: StatusTrialing, CurrentPeriodEnd: base.AddDate(0, 0, 14)},
			now:  base, canUpload: true,
		},
		{
			name: "解約予定でも期末まではアップロードできる",
			sub: &Subscription{Status: StatusActive, CancelAtPeriodEnd: true,
				CurrentPeriodEnd: base.AddDate(0, 0, 10)},
			now: base, canUpload: true,
		},
		{
			// ③ ここが猶予の本体。カードの期限切れで朝いちに止めない。
			name: "支払い失敗でも猶予の内はアップロードできる",
			sub: &Subscription{Status: StatusPastDue,
				GraceUntil: ptr(base.AddDate(0, 0, 14))},
			now: base.AddDate(0, 0, 13), canUpload: true, inGrace: true,
		},
		{
			name: "猶予の最終日でもまだ使える",
			sub: &Subscription{Status: StatusPastDue,
				GraceUntil: ptr(base.AddDate(0, 0, 14))},
			now: base.AddDate(0, 0, 14).Add(-time.Second), canUpload: true, inGrace: true,
		},
		{
			name: "猶予を1秒でも過ぎたら止まる",
			sub: &Subscription{Status: StatusPastDue,
				GraceUntil: ptr(base.AddDate(0, 0, 14))},
			now: base.AddDate(0, 0, 14), canUpload: false,
		},
		{
			name: "支払い失敗で猶予が設定されていなければ止まる",
			sub:  &Subscription{Status: StatusPastDue},
			now:  base, canUpload: false,
		},
		{
			name: "未払いも猶予の内は使える",
			sub: &Subscription{Status: StatusUnpaid,
				GraceUntil: ptr(base.AddDate(0, 0, 3))},
			now: base, canUpload: true, inGrace: true,
		},
		{
			name: "解約済みは止まる",
			sub:  &Subscription{Status: StatusCanceled},
			now:  base, canUpload: false,
		},
		{
			name: "申し込み未完了は止まる",
			sub:  &Subscription{Status: StatusIncomplete},
			now:  base, canUpload: false,
		},
		{
			name: "一時停止は止まる",
			sub:  &Subscription{Status: StatusPaused},
			now:  base, canUpload: false,
		},
		{
			name: "契約が無ければ止まる",
			sub:  nil,
			now:  base, canUpload: false,
		},
		{
			// 既定は課金必須。免除は明示したときだけ。
			name: "免除されていれば契約が無くても使える",
			org:  Org{BillingExempt: true},
			sub:  nil,
			now:  base, canUpload: true,
		},
		{
			// Stripe が状態名を足したときに、支払っている事務所を止めない。
			name: "知らない状態は使える側に倒す",
			sub:  &Subscription{Status: "some_future_status"},
			now:  base, canUpload: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Evaluate(c.org, c.sub, c.now)
			if got.CanUpload != c.canUpload {
				t.Errorf("CanUpload = %v, 期待 %v（理由: %s）",
					got.CanUpload, c.canUpload, got.Reason)
			}
			if got.InGrace != c.inGrace {
				t.Errorf("InGrace = %v, 期待 %v", got.InGrace, c.inGrace)
			}
			// 使えないときは、次にやることを必ず示す。
			// 「できません」だけの画面は、現場が動けない。
			if !got.CanUpload && got.NextStep == "" {
				t.Errorf("使えないのに次にやることが空: %s", got.Reason)
			}
		})
	}
}

func TestStartGrace(t *testing.T) {
	got := StartGrace(base)
	want := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("StartGrace = %v, 期待 %v", got, want)
	}
}

// 猶予日数を変えると、この試験が落ちる。
// 「なんとなく短くしてみた」が通らないようにしておく。
func TestGraceDaysIsFourteen(t *testing.T) {
	if GraceDays != 14 {
		t.Fatalf("猶予は14日と決めた。変えるなら根拠を残すこと（今: %d）", GraceDays)
	}
}
