-- 005: 非同期ジョブ待ち行列
--
-- なぜ documents.status を直接使わないか
--
--   documents.status を待ち行列として使うと、次の3つが置けない。
--     ① 失敗の理由        … 現場が自分で原因を見られないと問い合わせになる
--     ② 試行回数          … 何回まで自動で再試行するか
--     ③ 処理の期限        … ワーカーが落ちたとき、その伝票が永久に「処理中」で止まる
--   ③ が特に重い。伝票が1枚だけ何日も処理中のまま残り、
--   誰も気付かない状態は、業務で使う道具として成立しない。
--   期限（locked_until）を持たせて、過ぎたら別のワーカーが拾い直す。
--
-- 取り合いをどう防ぐか
--
--   SELECT ... FOR UPDATE SKIP LOCKED を使う。
--   ワーカーを3台に増やしても、同じ伝票を2台が処理することはない。
--   SKIP LOCKED が無いと、後続のワーカーは先頭の行が空くまで待つ。
--   待ち行列としては、待つのではなく次の行へ進んでほしい。

BEGIN;

CREATE TABLE jobs (
  id           bigserial PRIMARY KEY,
  document_id  bigint      NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  status       smallint    NOT NULL DEFAULT 1,
    -- 1:待機 2:処理中 3:完了 4:失敗（打ち切り）
  attempts     int         NOT NULL DEFAULT 0,
  max_attempts int         NOT NULL DEFAULT 3,

  -- 進捗。画面が「今どこを処理しているか」を出すために使う。
  -- 工程名をそのまま持つ。数字だけだと、後で工程が増えたときに意味が変わる。
  stage        text,        -- 'preprocess' | 'ocr' | 'match' | 'decide'
  progress     smallint    NOT NULL DEFAULT 0,  -- 0〜100

  run_after    timestamptz NOT NULL DEFAULT now(),  -- 再試行の待ち時間に使う
  locked_by    text,        -- どのワーカーが持っているか（ホスト名＋起動ID）
  locked_until timestamptz, -- この時刻を過ぎたら、落ちたとみなして拾い直す
  last_error   text,

  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT jobs_status_check   CHECK (status BETWEEN 1 AND 4),
  CONSTRAINT jobs_progress_check CHECK (progress BETWEEN 0 AND 100)
);

-- ワーカーが拾うとき専用。待機と処理中だけを対象にする部分インデックス。
-- 完了した行が何百万件貯まっても、この索引は太らない。
CREATE INDEX jobs_pickup ON jobs (run_after)
  WHERE status IN (1, 2);

CREATE INDEX ON jobs (document_id);

-- 1つの伝票に処理中のジョブを2つ作らせない。
-- 二重投入は現場で普通に起きる（画面を二度押す、再送する）。
-- アプリ側の確認だけに頼らず、DBで弾く。
CREATE UNIQUE INDEX jobs_one_active_per_document ON jobs (document_id)
  WHERE status IN (1, 2);

COMMIT;
