-- 001: 契約単位・顧問先・利用者・取引先マスタ
--
-- 設計の要点
--   partners.norm と trigrams は「登録・更新時に計算して保存」する。
--   照合のたびに正規化してはいけない。1万件のマスタに対して毎回正規化を
--   かけると、それだけで目標の50msを使い切る。
--   計算は core/src/common の正規化ルールで行い、アプリから書き込む。

BEGIN;

-- 契約単位。会計事務所・記帳代行業者そのもの。
CREATE TABLE organizations (
  id          bigserial PRIMARY KEY,
  name        text        NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);

-- 利用者。事務所の職員。
CREATE TABLE users (
  id              bigserial PRIMARY KEY,
  organization_id bigint      NOT NULL REFERENCES organizations(id),
  email           text        NOT NULL UNIQUE,
  name            text        NOT NULL,
  role            smallint    NOT NULL DEFAULT 1,  -- 1:一般 2:管理者
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON users (organization_id);

-- 顧問先企業。伝票はこの単位で集まる。
-- OCRエンジンの選択もここで持つ。機密性を気にする顧問先だけ
-- ローカル完結（Tesseract）にできるようにするため、組織単位ではなく顧問先単位。
CREATE TABLE clients (
  id              bigserial PRIMARY KEY,
  organization_id bigint      NOT NULL REFERENCES organizations(id),
  name            text        NOT NULL,
  ocr_engine      text        NOT NULL DEFAULT 'vision',  -- 'vision' | 'tesseract'
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT clients_ocr_engine_check
    CHECK (ocr_engine IN ('vision', 'tesseract'))
);
CREATE INDEX ON clients (organization_id);

-- 取引先マスタ。名寄せの正解データ。
CREATE TABLE partners (
  id          bigserial PRIMARY KEY,
  client_id   bigint      NOT NULL REFERENCES clients(id),
  name        text        NOT NULL,   -- 表示用の正式名称
  norm        text        NOT NULL,   -- 正規化済み。登録・更新時に計算して保存
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON partners (client_id);
-- 候補生成の要。これが無いと1万件の走査になる。
CREATE INDEX partners_norm_trgm ON partners USING gin (norm gin_trgm_ops);

-- 表記揺れの登録。人が「これも同じ取引先だ」と教えた文字列。
-- source=2 は、承認UIで人が修正したときに自動で貯まるもの。
-- 使えば使うほど候補生成が当たるようになる。
CREATE TABLE partner_aliases (
  id          bigserial PRIMARY KEY,
  partner_id  bigint      NOT NULL REFERENCES partners(id) ON DELETE CASCADE,
  alias       text        NOT NULL,
  norm        text        NOT NULL,   -- こちらも事前計算
  source      smallint    NOT NULL DEFAULT 1,  -- 1:手動登録 2:承認時に自動学習
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON partner_aliases (partner_id);
CREATE INDEX partner_aliases_norm_trgm ON partner_aliases USING gin (norm gin_trgm_ops);

COMMIT;
