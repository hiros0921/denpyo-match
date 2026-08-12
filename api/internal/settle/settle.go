// Package settle は伝票と入出金の突合を判定する。
//
// 3点で採点する。名前・金額・日付。
//
//	名前   C++ の照合スコア（摘要の正規化名 vs 取引先名＋学習した別名）
//	金額   完全一致が基本。銀行振込だけ、手数料の差し引きを許す
//	日付   支払いは請求より後ろに来る。窓の中での近さ
//
// なぜ名前の重みを最大にしないか
//
//	実測（実在形式のマスタ8件）:
//	  摘要がカナだけの店名        名前スコア 100
//	  カナ読み vs 漢字の名前      20〜50。1位を外すこともある
//	  人が1回承認して別名を学習   100
//
//	つまり名前は「初回は弱く、2回目から強い」。初回の突合は
//	金額と日付が支え、人が確認して別名を覚えたら名前が効き始める。
//	名前だけに重みを置くと、初回が全部「相手なし」になる。
//
// この計算は DB にも C++ にも触らない。名前スコアは呼び出し側が
// C++ で出して渡す。こうしておくと、この判定だけで単体テストが書ける。
package settle

import (
	"fmt"
	"time"

	"github.com/hiros0921/denpo-match/api/internal/decide"
	"github.com/hiros0921/denpo-match/api/internal/ledger"
)

// Status は突合の結論。DB の settlements.status と同じ値。変えない。
type Status int16

const (
	StatusAuto        Status = 1 // 自動突合
	StatusConfirmed   Status = 2 // 人が確定
	StatusReview      Status = 3 // 要確認
	StatusNone        Status = 4 // 突合相手なし（現金の可能性）
	StatusNoneFixed   Status = 5 // 人が「相手なし」と確定
)

func (s Status) String() string {
	switch s {
	case StatusAuto:
		return "自動突合"
	case StatusConfirmed:
		return "人が確定"
	case StatusReview:
		return "要確認"
	case StatusNone:
		return "相手なし"
	case StatusNoneFixed:
		return "相手なし（人が確定）"
	}
	return "不明"
}

// 3点の重み。合計1.0。
//
// 根拠は上の実測。金額＋日付だけで 60 点になるので、
// 名前がまったく効かない初回でも、金額完全一致＋日付近接なら
// 要確認（下限55以上）に届く。人が見て確定すれば別名が学習され、
// 次からは名前も 100 になって自動突合（上限95以上）に届く。
const (
	wName   = 0.40
	wAmount = 0.35
	wDate   = 0.25
)

// Tx は突合候補になる1件の入出金。
type Tx struct {
	ID     int64
	Date   time.Time
	Amount int64
	Source ledger.SourceType
	// C++（dm_match）で出した名前スコア。0〜100。
	NameScore float64
}

// Doc は突合される側の伝票。
type Doc struct {
	Total int64
	// 読めていなければゼロ値。日付は中立（50点）として扱う。
	IssueDate time.Time
}

// Scored は採点済みの候補。
type Scored struct {
	Tx
	Score       float64
	AmountScore float64
	DateScore   float64
	Why         string
	// 摘要が複数の取引先に一致する（＝一意に定まらない）と判定されたとき設定される。
	// nil なら曖昧ではない。
	Ambiguity *Ambiguity
}

// Score は1候補を採点する。
func Score(d Doc, tx Tx) Scored {
	out := Scored{Tx: tx}

	// ── 金額 ──
	diff := d.Total - tx.Amount
	switch {
	case diff == 0:
		out.AmountScore = 100
	case tx.Source == ledger.Bank && diff > 0 && diff <= 880:
		// 振込手数料の先方負担。実務では 110〜880円 が差し引かれる。
		// カードにはこの慣習が無いので、銀行だけに許す。
		out.AmountScore = 70
		out.Why += fmt.Sprintf("金額差%d円は振込手数料の可能性。", diff)
	default:
		// 候補生成の段階で弾かれているはずの組。0点で返す。
		out.AmountScore = 0
		out.Why += "金額が合わない。"
	}

	// ── 日付 ──
	if d.IssueDate.IsZero() {
		// 読めなかった伝票を弾かない。日付は中立にして金額と名前で決める。
		out.DateScore = 50
		out.Why += "伝票の日付が読めていない。"
	} else {
		days := int(tx.Date.Sub(d.IssueDate).Hours() / 24)
		switch {
		case tx.Source == ledger.Card:
			// カードの利用日は購入日とほぼ同じ。数日の処理ずれだけ許す。
			switch {
			case days == 0:
				out.DateScore = 100
			case days >= 1 && days <= 3:
				out.DateScore = 80
			default:
				out.DateScore = 30
			}
		default:
			// 銀行振込。月末締め翌月末払いが多いので、窓は広い。
			switch {
			case days >= 0 && days <= 45:
				out.DateScore = 100
			case days >= 46 && days <= 75:
				out.DateScore = 80
			case days >= -5 && days < 0:
				out.DateScore = 60
				out.Why += "支払いが伝票の日付より前。前払いの可能性。"
			default:
				out.DateScore = 30
			}
		}
	}

	out.Score = wName*tx.NameScore + wAmount*out.AmountScore + wDate*out.DateScore
	return out
}

// Result は伝票1枚に対する結論。
type Result struct {
	Status Status
	// StatusAuto / StatusReview のとき、最有力の候補。
	Best *Scored
	Why  string
}

// Decide は採点済みの候補から結論を出す。
//
// 閾値は取引先照合と同じもの（既定 95/55）を使う。
// 別の閾値を持つ理由が出てきたら分ける。今は根拠になる実測が無い。
func Decide(scored []Scored, th decide.Threshold) Result {
	if len(scored) == 0 {
		return Result{Status: StatusNone,
			Why: "金額と日付の範囲に、対応する入出金がありません。" +
				"現金払いか、明細をまだ取り込んでいない期間の可能性があります"}
	}
	best := scored[0]
	for _, s := range scored[1:] {
		if s.Score > best.Score {
			best = s
		}
	}

	// 曖昧で頭打ちでは守れない設定（CapFor が ok=false を返した）のときは、
	// 名前スコアを触っていないのでスコアがそのまま上限を超えうる。
	// スコアの比較より先に、必ず要確認へ落とす。
	if best.Ambiguity != nil && best.Ambiguity.Forced {
		return Result{Status: StatusReview, Best: &best,
			Why: fmt.Sprintf("スコア%.1f（名前%.0f 金額%.0f 日付%.0f）。%s",
				best.Score, best.NameScore, best.AmountScore, best.DateScore,
				best.Ambiguity.Why())}
	}

	switch {
	case best.Score >= th.Upper:
		why := fmt.Sprintf("スコア%.1f（名前%.0f 金額%.0f 日付%.0f）が上限%.0f以上。%s",
			best.Score, best.NameScore, best.AmountScore, best.DateScore,
			th.Upper, best.Why)
		return Result{Status: StatusAuto, Best: &best, Why: why}
	case best.Score >= th.Lower:
		why := fmt.Sprintf("スコア%.1f（名前%.0f 金額%.0f 日付%.0f）。人の確認が要ります。%s",
			best.Score, best.NameScore, best.AmountScore, best.DateScore, best.Why)
		if best.Ambiguity != nil {
			// 頭打ちで守れた場合。要確認になった理由に曖昧さを併記する。
			why += " " + best.Ambiguity.Why()
		}
		return Result{Status: StatusReview, Best: &best, Why: why}
	default:
		// 最有力でも下限未満。候補は保存されるので、画面では見られる。
		return Result{Status: StatusNone,
			Why: fmt.Sprintf("候補はあるが最大スコア%.1fで下限%.0f未満。"+
				"対応する入出金ではない可能性が高い", best.Score, th.Lower)}
	}
}
