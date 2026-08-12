package settle

import (
	"testing"
	"time"

	"github.com/hiros0921/denpo-match/api/internal/decide"
	"github.com/hiros0921/denpo-match/api/internal/ledger"
)

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

// 実測した名前スコアをそのまま使う。
//   カナだけの店名           100
//   カナ読み vs 漢字（初回）  36.4（ミホンセキユサービス → 見本石油サービス）
//   別名を学習した後         100
const (
	nameKanaOnly  = 100.0
	nameFirstTime = 36.4
	nameLearned   = 100.0
)

func TestScore_初回はカナ読みでも要確認に届く(t *testing.T) {
	// このプロダクトの初回の生命線。
	// 名前が弱くても、金額完全一致＋日付近接なら人の目に届くこと。
	d := Doc{Total: 44352, IssueDate: day("2026-07-31")}
	tx := Tx{Date: day("2026-08-25"), Amount: 44352,
		Source: ledger.Bank, NameScore: nameFirstTime}
	got := Score(d, tx)
	// 0.4*36.4 + 0.35*100 + 0.25*100 = 74.56
	if got.Score < decide.Default.Lower {
		t.Errorf("初回のカナ読みが要確認に届かない: %.1f", got.Score)
	}
	if got.Score >= decide.Default.Upper {
		t.Errorf("初回のカナ読みが自動突合されてしまう: %.1f", got.Score)
	}
}

func TestScore_別名学習後は自動突合に届く(t *testing.T) {
	d := Doc{Total: 44352, IssueDate: day("2026-07-31")}
	tx := Tx{Date: day("2026-08-25"), Amount: 44352,
		Source: ledger.Bank, NameScore: nameLearned}
	got := Score(d, tx)
	// 0.4*100 + 0.35*100 + 0.25*100 = 100
	if got.Score < decide.Default.Upper {
		t.Errorf("学習後が自動突合に届かない: %.1f", got.Score)
	}
}

func TestScore_手数料差引は自動突合しない(t *testing.T) {
	// 名前が完全でも、金額がずれているなら人が見る。
	// 手数料の差し引きは「可能性」であって確認された事実ではない。
	d := Doc{Total: 175010, IssueDate: day("2026-07-31")}
	tx := Tx{Date: day("2026-08-25"), Amount: 175010 - 440,
		Source: ledger.Bank, NameScore: nameLearned}
	got := Score(d, tx)
	// 0.4*100 + 0.35*70 + 0.25*100 = 89.5
	if got.Score >= decide.Default.Upper {
		t.Errorf("手数料差引が自動突合されてしまう: %.1f", got.Score)
	}
	if got.Score < decide.Default.Lower {
		t.Errorf("手数料差引が要確認に届かない: %.1f", got.Score)
	}
	if got.AmountScore != 70 {
		t.Errorf("金額スコア: %.0f", got.AmountScore)
	}
}

func TestScore_カードに手数料の許しは無い(t *testing.T) {
	d := Doc{Total: 1258, IssueDate: day("2026-07-15")}
	tx := Tx{Date: day("2026-07-15"), Amount: 1258 - 220,
		Source: ledger.Card, NameScore: nameKanaOnly}
	got := Score(d, tx)
	if got.AmountScore != 0 {
		t.Errorf("カードの金額ずれに手数料の解釈を適用した: %.0f", got.AmountScore)
	}
}

func TestScore_カードは同日100点(t *testing.T) {
	d := Doc{Total: 1258, IssueDate: day("2026-07-15")}
	same := Score(d, Tx{Date: day("2026-07-15"), Amount: 1258,
		Source: ledger.Card, NameScore: nameKanaOnly})
	lag := Score(d, Tx{Date: day("2026-07-17"), Amount: 1258,
		Source: ledger.Card, NameScore: nameKanaOnly})
	far := Score(d, Tx{Date: day("2026-08-20"), Amount: 1258,
		Source: ledger.Card, NameScore: nameKanaOnly})
	if same.DateScore != 100 || lag.DateScore != 80 || far.DateScore != 30 {
		t.Errorf("カードの日付: %v %v %v", same.DateScore, lag.DateScore, far.DateScore)
	}
	// カナだけの店名なら、同日・同額で初回から自動突合に届く
	if same.Score < decide.Default.Upper {
		t.Errorf("レシート×カードの理想形が自動突合に届かない: %.1f", same.Score)
	}
}

func TestScore_日付が読めない伝票は中立(t *testing.T) {
	d := Doc{Total: 5000} // IssueDate ゼロ値
	tx := Tx{Date: day("2026-07-15"), Amount: 5000,
		Source: ledger.Bank, NameScore: nameLearned}
	got := Score(d, tx)
	if got.DateScore != 50 {
		t.Errorf("日付なしは中立50のはず: %.0f", got.DateScore)
	}
	// 0.4*100 + 0.35*100 + 0.25*50 = 87.5 → 要確認。
	// 日付の裏取りが無いのに自動で確定してはいけない。
	if got.Score >= decide.Default.Upper {
		t.Errorf("日付の裏取りなしで自動突合された: %.1f", got.Score)
	}
}

func TestScore_金額と日付だけの偶然一致は自動突合されない(t *testing.T) {
	// 同額の無関係な支払いが同じ窓にあることは普通に起きる。
	// 名前が無関係（低スコア）なら、自動では紐づけない。
	d := Doc{Total: 10000, IssueDate: day("2026-07-01")}
	tx := Tx{Date: day("2026-07-20"), Amount: 10000,
		Source: ledger.Bank, NameScore: 10}
	got := Score(d, tx)
	// 0.4*10 + 0.35*100 + 0.25*100 = 64
	if got.Score >= decide.Default.Upper {
		t.Errorf("名前が無関係なのに自動突合された: %.1f", got.Score)
	}
}

func TestDecide(t *testing.T) {
	th := decide.Default
	// 候補なし → 相手なし（現金の可能性）
	r := Decide(nil, th)
	if r.Status != StatusNone {
		t.Errorf("候補なし: %v", r.Status)
	}
	// 最高スコアを選ぶ
	r = Decide([]Scored{
		{Tx: Tx{ID: 1}, Score: 74.6},
		{Tx: Tx{ID: 2}, Score: 100.0},
		{Tx: Tx{ID: 3}, Score: 64.0},
	}, th)
	if r.Status != StatusAuto || r.Best.ID != 2 {
		t.Errorf("最高スコア: %+v", r)
	}
	// 全候補が下限未満 → 相手なし
	r = Decide([]Scored{{Tx: Tx{ID: 1}, Score: 40.0}}, th)
	if r.Status != StatusNone {
		t.Errorf("下限未満: %v", r.Status)
	}
	if r.Why == "" {
		t.Error("理由が空")
	}
}
