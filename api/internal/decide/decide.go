// Package decide は三分岐を担う。
//
// このプロダクトの価値は OCR の精度ではなく、
// 「どの精度なら人の確認を省いてよいか」の線引きを現場が調整できることにある。
// だから閾値をコードに埋め込まない。DBから読み、組織・顧問先・取引先・帳票種別
// ごとに切り替えられるようにする。
//
// 何で線を引くかは実測で決めた（第5段階・20枚）:
//
//	              抽出信頼度   照合スコア
//	正解の平均       0.53        88.2
//	不正解の平均      0.49        33.4
//
// 抽出信頼度では分けられない。照合スコアなら分かれる。実測では70を境に
// 完全に分離した。よって三分岐は照合スコアで引く。
package decide

import "fmt"

type Decision int

const (
	AutoApprove Decision = iota + 1 // 1: 人の確認なしで確定
	NeedsReview                     // 2: 人の確認待ちキューへ
	Reject                          // 3: 再スキャンまたは手入力
)

func (d Decision) String() string {
	switch d {
	case AutoApprove:
		return "自動承認"
	case NeedsReview:
		return "要確認"
	case Reject:
		return "却下"
	}
	return "不明"
}

// Threshold は DB の thresholds 行に対応する。
// 上書きせず履歴として積むので、ID を持って「どの設定で判定したか」を残す。
type Threshold struct {
	ID    int64
	Upper float64 // これ以上で自動承認
	Lower float64 // これ未満で却下
}

// 既定値。DBに設定が1件も無いときだけ使う。
//
// 根拠は100枚の通し検証（第9段階＋受領側対応後に測り直し）。
//
//	              1位が正解の分布              1位が不正解の最大
//	Tesseract    19.3〜（74.0 から密集）        26.0
//	Vision       62.9, 66.4, あとは全て100      50.9
//
// 上限95: 安全側の線。この100枚では75まで下げても誤承認ゼロ
// （Tesseractで自動承認 67→82）だが、それは生成データでの話。
// 既定は余裕を取り、下げる判断は閾値シミュレーションで
// 現場が自分のデータを見て行う。それがこのプロダクトの核心。
//
// 下限55: 不正解の最大（50.9）より上、Visionの正解の最小（62.9）より下。
// 70だと Vision の正解2枚（62.9 / 66.4）を却下していた。
// 55なら要確認に回り、人が候補を見て承認できる。
// どちらの側に外しても人が見ることに変わりは無い線なので、
// 上限ほどの余裕は要らない。
//
// 【注意】この2つの線は役割が違う。上限は「人を省いてよいか」の線で、
// 間違えると誤った確定が起きる。下限は「候補を見せるか」の線で、
// 間違えても人の手間が少し増えるだけ。余裕の取り方が違うのはそのため。
var Default = Threshold{ID: 0, Upper: 95, Lower: 55}

func (t Threshold) Valid() error {
	if t.Lower > t.Upper {
		return fmt.Errorf("下限 %.1f が上限 %.1f を超えています", t.Lower, t.Upper)
	}
	if t.Lower < 0 || t.Upper > 100 {
		return fmt.Errorf("閾値は0〜100の範囲で指定してください（下限 %.1f / 上限 %.1f）",
			t.Lower, t.Upper)
	}
	return nil
}

// Result は判定の結果。なぜその判定になったかを必ず持つ。
// 「自動承認された理由が説明できない」状態を作らない。
type Result struct {
	Decision    Decision
	Score       float64
	ThresholdID int64
	Why         string
}

// Decide は照合スコアから三分岐を決める。
//
// 候補が空のときは却下。スコア0で自動承認されることがないよう、
// 呼び出し側の実装ミスをここで止める。
func Decide(score float64, hasCandidate bool, t Threshold) Result {
	if !hasCandidate {
		return Result{
			Decision:    Reject,
			Score:       0,
			ThresholdID: t.ID,
			Why:         "照合の候補が1件も見つかりませんでした",
		}
	}
	switch {
	case score >= t.Upper:
		return Result{
			Decision:    AutoApprove,
			Score:       score,
			ThresholdID: t.ID,
			Why: fmt.Sprintf("照合スコア %.1f が上限 %.1f 以上のため自動承認",
				score, t.Upper),
		}
	case score >= t.Lower:
		return Result{
			Decision:    NeedsReview,
			Score:       score,
			ThresholdID: t.ID,
			Why: fmt.Sprintf("照合スコア %.1f が %.1f〜%.1f の間のため要確認",
				score, t.Lower, t.Upper),
		}
	default:
		return Result{
			Decision:    Reject,
			Score:       score,
			ThresholdID: t.ID,
			Why: fmt.Sprintf("照合スコア %.1f が下限 %.1f 未満のため却下",
				score, t.Lower),
		}
	}
}
