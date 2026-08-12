-- 019: 監査ログの action に入出金の突合を足す
--
-- 突合も「後から説明できること」が要る判断なので、監査ログに残す。
-- 特に自動突合は人が見ていないぶん、いつ・どの設定で・なぜ紐づけたかを
-- 記録しておかないと、誤りが見つかったときに範囲を追えない。
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
    'settle_none_confirm'  -- 人が「相手なし」を確定した
  ))
  NOT VALID;   -- 既存行は検査しない。追記のみの表なので直せないため

COMMIT;
