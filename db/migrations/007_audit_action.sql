-- 007: 監査ログの action を決められた語だけに限る
--
-- なぜ必要か
--
--   action は text の NOT NULL だが、空文字は NOT NULL を通る。
--   アプリ側の対応表に無い値が来たとき、空文字が静かに入る。
--
--   実際に起きた。006 で三分岐に「要確認」を足したとき、Go 側の
--   対応表（decision → action）を直し忘れ、要確認の伝票が
--   action='' で記録された。エラーは出ず、20枚のうち3件がそうなった。
--
--   監査ログは「後から説明するため」の表なので、
--   何が起きたか書いていない行が入るのは、その目的を壊す。
--   アプリで気をつけるのではなく、DBが受け付けないようにする。
--
--   既存の3件は消せない（追記のみの表なので、消す手段が無いのが正しい）。
--   移行の記録として残し、以後は入らないようにする。

BEGIN;

ALTER TABLE audit_logs ADD CONSTRAINT audit_logs_action_check
  CHECK (action IN (
    'create',        -- 伝票の受付
    'auto_approve',  -- 閾値による自動承認（threshold_id が必須）
    'needs_review',  -- 人の確認待ちへ回した
    'approve',       -- 人が承認
    'update',        -- 人が修正
    'reject'         -- 却下
  ))
  NOT VALID;   -- 既存行は検査しない。追記のみの表なので直せないため

COMMIT;
