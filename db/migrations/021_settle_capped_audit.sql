-- 021: 監査ログの action に「曖昧さによる頭打ち」を足す
--
-- settle_review と分けて記録する理由:
--   要確認になった理由が「スコアが微妙」なのか「摘要が複数の取引先に
--   一致するため機械的に止めた」のかは、現場の対応がまったく違う。
--   前者は候補を見て選べばよいが、後者は明細そのものが曖昧なので、
--   摘要のどこまでが確定情報かを人が判断する必要がある。
--   action を分けておけば、この種類の停止だけを一覧で追える。
BEGIN;

ALTER TABLE audit_logs DROP CONSTRAINT audit_logs_action_check;
ALTER TABLE audit_logs ADD CONSTRAINT audit_logs_action_check
  CHECK (action IN (
    'create',          -- 伝票の受付
    'auto_approve',    -- 閾値による自動承認（threshold_id が必須）
    'needs_review',    -- 人の確認待ちへ回した
    'approve',         -- 人が承認
    'update',          -- 人が修正
    'reject',          -- 却下
    'learn_alias',     -- 表記を覚えさせた
    'forget_alias',    -- 覚えた表記を取り消した
    'settle_auto',     -- 入出金と自動で突合した
    'settle_review',   -- 突合を人の確認待ちへ回した
    'settle_confirm',  -- 人が突合を確定した
    'settle_none',     -- 突合相手なし（現金の可能性）と記録した
    'settle_none_confirm', -- 人が「相手なし」を確定した
    'settle_capped'    -- 摘要が複数の取引先に一致するため、名前スコアを頭打ちにした
  ))
  NOT VALID;   -- 既存行は検査しない。追記のみの表なので直せないため

COMMIT;
