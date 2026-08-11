-- 008: users に Devise が使う列を足す
--
-- 方針
--
--   スキーマの正は db/migrations の SQL であって、Rails のマイグレーションではない。
--   同じテーブルを Go と Rails の両方から触るので、片方のフレームワークに
--   構造の管理を任せると、もう片方から見て「勝手に変わる」ことになる。
--   Rails 側は `schema_format = :sql` にし、db/migrate は空のままにする。
--
--   したがって Devise の列もここで足す。
--   `rails generate devise User` は実行しない（マイグレーションを作られるため）。
--
-- 既存の users には既に email があるので、Devise の必須列だけを足す。

BEGIN;

ALTER TABLE users
  ADD COLUMN encrypted_password     varchar     NOT NULL DEFAULT '',
  ADD COLUMN reset_password_token   varchar,
  ADD COLUMN reset_password_sent_at timestamptz,
  ADD COLUMN remember_created_at    timestamptz,
  -- 何回入って何回失敗したか。事務所の共有端末で使われる想定なので、
  -- 「誰がいつ入ったか」を後から言えるようにしておく。
  ADD COLUMN sign_in_count          integer     NOT NULL DEFAULT 0,
  ADD COLUMN current_sign_in_at     timestamptz,
  ADD COLUMN last_sign_in_at        timestamptz,
  ADD COLUMN current_sign_in_ip     inet,
  ADD COLUMN last_sign_in_ip        inet;

CREATE UNIQUE INDEX users_reset_password_token_key
  ON users (reset_password_token);

COMMIT;
