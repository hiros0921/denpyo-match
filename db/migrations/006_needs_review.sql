-- 006: match_results.decision に「要確認」を足す
--
-- なぜ必要か
--
--   三分岐の出力は 自動承認 / 要確認 / 却下 の3つ。
--   ところが 003 の decision は 1:自動承認 2:人が承認 3:人が修正 4:却下 で、
--   「要確認」に当たる値が無かった。
--
--   そのまま動かすと、要確認の伝票が decision=2（人が承認）として記録される。
--   実際には誰も見ていないのに「人が承認した」という記録が残る。
--   このプロダクトで最もやってはいけない種類の誤りなので、値を足す。
--   第6段階の通し検証で実際に発生した。
--
--   1〜4 は動かさない。既存の行と、それを読む側の解釈を変えないため。

BEGIN;

ALTER TABLE match_results DROP CONSTRAINT match_results_decision_check;
ALTER TABLE match_results ADD CONSTRAINT match_results_decision_check
  CHECK (decision BETWEEN 1 AND 5);

COMMENT ON COLUMN match_results.decision IS
  '1:自動承認 2:人が承認 3:人が修正 4:却下 5:要確認（人の確認待ち）';

-- 要確認は「まだ誰も判断していない」状態。
-- decided_by が入っていたら、それは人が判断した後なので要確認ではない。
ALTER TABLE match_results ADD CONSTRAINT match_results_review_no_actor
  CHECK (decision <> 5 OR decided_by IS NULL);

COMMIT;
