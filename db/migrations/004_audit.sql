-- 004: 監査ログ（追記のみ）と課金
--
-- 設計の要点
--
--   UPDATE と DELETE を、アプリの実装ではなくDBの側で拒否する。
--   アプリのバグや、開発者がうっかり書いたクエリでは消せない状態にする。
--   二重にかける:
--     ① ルール（DO INSTEAD NOTHING）… 所有者が実行しても無視される
--     ② 権限の剥奪              … アプリ用ロールからそもそも権限を外す
--
--   さらにハッシュ連鎖を持たせる。
--   ①②で防げるのは「アプリ経由の変更」まで。DBに直接繋がれた場合や、
--   スーパーユーザーで実行された場合は防げない。連鎖させておけば、
--   後から書き換えられたことを「検知」できる。防止ではなく検知。
--
-- 法対応について
--   このテーブルは「訂正・削除の履歴を追記のみのログとして保持する」ための
--   実装である。電子帳簿保存法への適合を主張するものではない。
--   適合判断は専門家の確認を経て別途行う。README等にも同様に記述すること。

BEGIN;

CREATE TABLE audit_logs (
  id              bigserial PRIMARY KEY,
  organization_id bigint      NOT NULL REFERENCES organizations(id),
  actor_id        bigint      REFERENCES users(id),   -- 自動処理は NULL
  target_table    text        NOT NULL,
  target_id       bigint      NOT NULL,
  action          text        NOT NULL,   -- 'create'|'update'|'approve'|'reject'|'auto_approve'
  before          jsonb,
  after           jsonb,
  threshold_id    bigint      REFERENCES thresholds(id),  -- 自動承認時にどの設定か
  prev_hash       text,       -- 直前行の row_hash
  row_hash        text        NOT NULL,   -- sha256(prev_hash || 本行の内容)
  occurred_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON audit_logs (organization_id, occurred_at);
CREATE INDEX ON audit_logs (target_table, target_id);

-- ① ルールで UPDATE / DELETE を無効化する。
--    エラーにはせず「何も起きない」ようにする。エラーにすると、
--    誤ったコードがそこで例外を投げて処理が止まり、原因が分かりにくい。
--    ここでは静かに無視し、②の権限剥奪で明示的に弾く。
CREATE RULE audit_logs_no_update AS ON UPDATE TO audit_logs DO INSTEAD NOTHING;
CREATE RULE audit_logs_no_delete AS ON DELETE TO audit_logs DO INSTEAD NOTHING;

-- ② アプリ用ロールを作り、追記と読み取りだけを許す。
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'dm_app') THEN
    CREATE ROLE dm_app;
  END IF;
END $$;

GRANT INSERT, SELECT ON audit_logs TO dm_app;
GRANT USAGE, SELECT ON SEQUENCE audit_logs_id_seq TO dm_app;
REVOKE UPDATE, DELETE, TRUNCATE ON audit_logs FROM dm_app, PUBLIC;

-- ハッシュ連鎖。INSERT のたびに直前行と繋ぐ。
CREATE OR REPLACE FUNCTION audit_logs_chain() RETURNS trigger AS $$
DECLARE
  prev text;
BEGIN
  SELECT row_hash INTO prev FROM audit_logs ORDER BY id DESC LIMIT 1;
  NEW.prev_hash := prev;
  NEW.row_hash := encode(digest(
      coalesce(prev, '') ||
      NEW.organization_id::text || coalesce(NEW.actor_id::text, '') ||
      NEW.target_table || NEW.target_id::text || NEW.action ||
      coalesce(NEW.before::text, '') || coalesce(NEW.after::text, '') ||
      coalesce(NEW.threshold_id::text, '') || NEW.occurred_at::text,
      'sha256'), 'hex');
  RETURN NEW;
END $$ LANGUAGE plpgsql;

-- digest() は pgcrypto。00_extensions.sql で有効化する。
CREATE TRIGGER audit_logs_chain_trigger
  BEFORE INSERT ON audit_logs
  FOR EACH ROW EXECUTE FUNCTION audit_logs_chain();

-- 連鎖の検証。改竄されていれば、そこから先が全部ずれる。
CREATE OR REPLACE FUNCTION verify_audit_chain()
  RETURNS TABLE(bad_id bigint, reason text) AS $$
  SELECT a.id, '直前行との連鎖が切れている'
  FROM audit_logs a
  LEFT JOIN audit_logs p ON p.id = (
    SELECT max(id) FROM audit_logs WHERE id < a.id
  )
  WHERE a.prev_hash IS DISTINCT FROM p.row_hash;
$$ LANGUAGE sql STABLE;

-- Stripe 連携
CREATE TABLE subscriptions (
  id                     bigserial PRIMARY KEY,
  organization_id        bigint      NOT NULL REFERENCES organizations(id),
  stripe_customer_id     text        NOT NULL,
  stripe_subscription_id text        NOT NULL UNIQUE,
  status                 text        NOT NULL,
  current_period_end     timestamptz,
  created_at             timestamptz NOT NULL DEFAULT now(),
  updated_at             timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON subscriptions (organization_id);

COMMIT;
