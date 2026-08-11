-- 002: 伝票・ページ・OCR出力・抽出項目
--
-- 設計の要点
--   画像本体はDBに入れない。Cloudflare R2（開発中はMinIO）に置き、
--   ここにはオブジェクトキーだけを持つ。DBが肥大すると
--   バックアップも復旧も現実的でなくなる。
--
--   ocr_results.raw を jsonb にする理由は、帳票様式ごとに
--   OCRの出力構造が変わるため。様式を1つ増やすたびにテーブル定義を
--   変えることになると、運用が回らない。

BEGIN;

-- 伝票1枚（複数ページを束ねる単位）
CREATE TABLE documents (
  id          bigserial PRIMARY KEY,
  client_id   bigint      NOT NULL REFERENCES clients(id),
  doc_type    smallint    NOT NULL,   -- 1:請求書 2:納品書 3:領収書
  status      smallint    NOT NULL DEFAULT 1,
    -- 1:受付 2:前処理済 3:OCR済 4:照合済 5:確定 9:エラー
  uploaded_by bigint      REFERENCES users(id),
  uploaded_at timestamptz NOT NULL DEFAULT now(),
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT documents_doc_type_check CHECK (doc_type BETWEEN 1 AND 3),
  CONSTRAINT documents_status_check   CHECK (status IN (1,2,3,4,5,9))
);
CREATE INDEX ON documents (client_id, status);
CREATE INDEX ON documents (status, uploaded_at);   -- ワーカーが拾う順

-- ページ。画像はR2にあり、ここにはキーだけ。
CREATE TABLE document_pages (
  id                bigserial PRIMARY KEY,
  document_id       bigint      NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  page_no           int         NOT NULL,
  r2_key_original   text        NOT NULL,   -- スキャン原本
  r2_key_processed  text,                   -- C++で補正した画像
  width             int,
  height            int,
  created_at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE (document_id, page_no)
);

-- OCRの生出力。エンジンを差し替えても、ここは同じ形で受ける。
CREATE TABLE ocr_results (
  id                bigserial PRIMARY KEY,
  document_page_id  bigint      NOT NULL REFERENCES document_pages(id) ON DELETE CASCADE,
  engine            text        NOT NULL,   -- 'vision' | 'tesseract'
  raw               jsonb       NOT NULL,   -- ページ・行・単語・座標・信頼度
  cost_yen          numeric(8,4),           -- このページの実費。保守料金の根拠になる
  processed_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON ocr_results (document_page_id);

-- 抽出した項目。bbox は承認UIで該当箇所を光らせるのに使う。
CREATE TABLE extracted_fields (
  id           bigserial PRIMARY KEY,
  document_id  bigint      NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  field_key    text        NOT NULL,
    -- 'partner_name' | 'issue_date' | 'total' | 'doc_no'
  value_text   text,
  value_norm   text,        -- 取引先名の場合、正規化した文字列
  confidence   numeric(5,2),
  bbox         jsonb,       -- {page:1, x:..., y:..., w:..., h:...}
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON extracted_fields (document_id, field_key);

COMMIT;
