-- 入出金（銀行明細・カード明細）と、伝票との突合。
--
-- なぜ入出金か
--
--   受領側の照合相手は、本当はマスタではなく「お金が動いた事実」。
--   会計ソフトの仕訳は記帳が終わった後にしか存在しないが、
--   このシステムは記帳そのものを助けるものなので、順序が逆になる。
--   銀行明細は記帳前でも必ず存在し、実際にお金が動いた記録なので、
--   照合の基準として一番強い。
--
-- なぜ1つの表に寄せるか
--
--   取り込み口（銀行CSV・カード明細・将来の仕訳データ）はアダプタで分け、
--   中は transactions 1本に寄せる。こうしておけば、後から freee 連携を
--   足すときも、突合ロジックに手を入れずに済む。
--
-- 現金の割り切り
--
--   現金取引は銀行にもカードにも載らない。レシートの多くは現金払い。
--   突合相手が無いものは「証憑のみ」として扱う。ここは仕様として割り切る。
BEGIN;

-- 取り込みの台帳。同じファイルの二重取り込みをここで弾く。
--
-- 行単位ではなくファイル単位で守る理由:
--   同じ日に同額・同摘要の行は正当に存在する（同じ店で2回買う）。
--   行の中身だけで重複判定すると、正当な行を捨てる。
CREATE TABLE import_batches (
  id            bigserial   PRIMARY KEY,
  client_id     bigint      NOT NULL REFERENCES clients(id),
  source_type   smallint    NOT NULL,   -- 1:銀行 2:カード 3:仕訳（将来）
  filename      text        NOT NULL,
  file_sha256   text        NOT NULL,
  row_count     int         NOT NULL DEFAULT 0,
  skipped_count int         NOT NULL DEFAULT 0,
  imported_by   bigint      REFERENCES users(id),
  created_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT import_batches_source_check CHECK (source_type IN (1,2,3)),
  -- 同じ顧問先に同じファイルは1回だけ
  UNIQUE (client_id, file_sha256)
);

CREATE TABLE transactions (
  id               bigserial   PRIMARY KEY,
  client_id        bigint      NOT NULL REFERENCES clients(id),
  batch_id         bigint      NOT NULL REFERENCES import_batches(id) ON DELETE CASCADE,
  source_type      smallint    NOT NULL,   -- 1:銀行 2:カード 3:仕訳
  transaction_date date        NOT NULL,
  amount           bigint      NOT NULL,
  direction        smallint    NOT NULL,   -- 1:入金 2:出金
  -- 摘要。原文のまま。ｶ)ﾐﾗｲﾊｲｿｳｻｰﾋﾞｽ のような半角カナで来る。
  description      text        NOT NULL,
  -- C++（dm_match --normalize --bank）で正規化した相手名。
  -- 全銀協略号（カ)＝株式会社 等）と取引種別語（フリコミ等）を除いたもの。
  -- 【重要】Go や Ruby で正規化を書き直さない。実装は C++ の1箇所だけ。
  normalized_name  text        NOT NULL,
  -- 元の行をそのまま。列の解釈を後から直すときに、原本が要る。
  raw_data         jsonb       NOT NULL,
  -- 期間の重なった再取り込みを検出する行の指紋。
  -- 内容＋ファイル内での同一内容の出現順から作る。
  -- 同じ物理取引は別ファイルでも同じ指紋になり、正当な同額同日は
  -- 出現順で区別される。
  row_fingerprint  text        NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT transactions_source_check    CHECK (source_type IN (1,2,3)),
  CONSTRAINT transactions_direction_check CHECK (direction IN (1,2)),
  CONSTRAINT transactions_amount_check    CHECK (amount > 0),
  UNIQUE (client_id, row_fingerprint)
);

-- 突合の第一条件は金額。次に日付の窓。この並びで索引を張る。
CREATE INDEX transactions_match_idx
  ON transactions (client_id, direction, amount, transaction_date);
-- 摘要の名前で候補を出す経路（学習した別名との突き合わせ）
CREATE INDEX transactions_norm_trgm
  ON transactions USING gin (normalized_name gin_trgm_ops);

-- 伝票と入出金の突合候補。閾値を変えたときの再集計はこの表を読むだけ。
-- match_candidates（取引先の照合）と同じ思想。
CREATE TABLE settlement_candidates (
  id             bigserial   PRIMARY KEY,
  document_id    bigint      NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  transaction_id bigint      NOT NULL REFERENCES transactions(id) ON DELETE CASCADE,
  rank           smallint    NOT NULL,
  score          numeric(5,1) NOT NULL,
  -- 3点の内訳。合計だけだと「なぜこの順位か」を人に説明できない。
  name_score     numeric(5,1) NOT NULL,
  amount_score   numeric(5,1) NOT NULL,
  date_score     numeric(5,1) NOT NULL,
  why            text        NOT NULL,
  created_at     timestamptz NOT NULL DEFAULT now(),
  UNIQUE (document_id, rank),
  UNIQUE (document_id, transaction_id)
);

-- 突合の結論。1伝票につき1行。
CREATE TABLE settlements (
  document_id    bigint      PRIMARY KEY REFERENCES documents(id) ON DELETE CASCADE,
  transaction_id bigint      REFERENCES transactions(id),
  status         smallint    NOT NULL,
    -- 1:自動突合 2:人が確定 3:要確認 4:突合相手なし（現金の可能性） 5:人が「相手なし」と確定
  score          numeric(5,1),
  why            text        NOT NULL,
  decided_by     bigint      REFERENCES users(id),
  decided_at     timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT settlements_status_check CHECK (status IN (1,2,3,4,5))
);

-- 1つの入出金を2枚の伝票が取り合ったら、後から来たほうを要確認にする。
-- そのための「確定済みの取引」を探す索引。
--
-- 【注意】UNIQUE にはしない。1回の振込で複数の請求をまとめて払う
-- 「合算振込」が実務には普通にある。禁止すると正しい運用を弾く。
-- 自動突合だけが取り合いを避け、人はどちらも紐づけられる、が正しい形。
CREATE INDEX settlements_transaction_idx
  ON settlements (transaction_id)
  WHERE transaction_id IS NOT NULL AND status IN (1,2);

COMMENT ON TABLE transactions           IS '入出金。銀行・カード・仕訳を1本に寄せる';
COMMENT ON TABLE import_batches         IS '取り込みの台帳。ファイル単位の重複を弾く';
COMMENT ON TABLE settlement_candidates  IS '伝票×入出金の突合候補と3点スコア';
COMMENT ON TABLE settlements            IS '突合の結論。1伝票1行';

COMMIT;
