package decide

import "testing"

// 境界の扱いを固定する。ここを間違えると、承認すべきでないものを
// 自動承認したり、その逆が起きる。
func TestDecide(t *testing.T) {
	th := Threshold{ID: 42, Upper: 95, Lower: 70}
	cases := []struct {
		score float64
		want  Decision
		note  string
	}{
		{100.0, AutoApprove, "完全一致"},
		{95.0, AutoApprove, "上限ちょうどは自動承認に含む"},
		{94.9, NeedsReview, "上限を下回れば要確認"},
		{70.0, NeedsReview, "下限ちょうどは要確認に含む"},
		{69.9, Reject, "下限を下回れば却下"},
		{0.0, Reject, "スコア0"},
	}
	for _, c := range cases {
		got := Decide(c.score, true, th)
		if got.Decision != c.want {
			t.Errorf("%s: score=%.1f → %v（期待 %v）", c.note, c.score, got.Decision, c.want)
		}
		if got.ThresholdID != th.ID {
			t.Errorf("%s: どの閾値で判定したかが残っていない", c.note)
		}
		if got.Why == "" {
			t.Errorf("%s: 判定の理由が空", c.note)
		}
	}
}

// 候補が無いのに自動承認されないこと。
// 呼び出し側がスコア0を渡しても、ここで止める。
func TestDecideNoCandidate(t *testing.T) {
	th := Threshold{ID: 1, Upper: 0, Lower: 0} // 極端な設定でも
	got := Decide(100, false, th)
	if got.Decision != Reject {
		t.Errorf("候補が無いのに %v になった", got.Decision)
	}
}

// 閾値の検査。設定画面から不正な値が来ても弾く。
func TestThresholdValid(t *testing.T) {
	ok := []Threshold{{Upper: 95, Lower: 70}, {Upper: 100, Lower: 0}, {Upper: 80, Lower: 80}}
	for _, th := range ok {
		if err := th.Valid(); err != nil {
			t.Errorf("正しい閾値が弾かれた %v: %v", th, err)
		}
	}
	ng := []Threshold{{Upper: 50, Lower: 90}, {Upper: 101, Lower: 0}, {Upper: 95, Lower: -1}}
	for _, th := range ng {
		if err := th.Valid(); err == nil {
			t.Errorf("不正な閾値が通ってしまった %v", th)
		}
	}
}

// 実測にもとづく確認。第5段階で「照合スコア70を境に分離した」ことを
// 回帰テストとして固定する。既定値を変えたらここが落ちる。
func TestDefaultMatchesMeasurement(t *testing.T) {
	if Default.Lower != 70 {
		t.Errorf("既定の下限が70でない（%v）。第5段階の実測と食い違う", Default.Lower)
	}
	// 実測で正解だったスコア
	for _, s := range []float64{100.0, 91.4, 88.2, 82.3, 79.3, 76.3} {
		if Decide(s, true, Default).Decision == Reject {
			t.Errorf("正解だったスコア %.1f が却下された", s)
		}
	}
	// 実測で不正解だったスコア
	for _, s := range []float64{60.9, 53.9, 40.0, 12.0, 0.0} {
		if Decide(s, true, Default).Decision == AutoApprove {
			t.Errorf("不正解だったスコア %.1f が自動承認された", s)
		}
	}
}
