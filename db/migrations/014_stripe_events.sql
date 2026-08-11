-- 014: 処理済みの Stripe イベントを覚えておく
--
-- Stripe は「同じイベントが複数回届くことがある」と明記している。
-- 受け取り側が200を返す前に通信が切れれば、Stripe は再送する。
--
-- 状態の書き込み自体は、2回来ても同じ結果になるように作ってある（べき等）。
-- ただし監査ログは追記なので、2回来れば2行残る。
-- 「いつ止まったのか」を後から説明するための記録が水増しされると、
-- 調べるときにノイズになる。入口で1回に絞る。
--
-- 保持期間について
--   Stripe の再送は最大3日程度。それを超えた行は消してよい。
--   ただし自動削除は入れない。消す仕組みを入れるより、
--   増え方を見てから決めるほうが確実（1事務所あたり月に数十件程度）。

BEGIN;

CREATE TABLE stripe_events (
  id           text        PRIMARY KEY,   -- Stripe の evt_...
  received_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ON stripe_events (received_at);

COMMIT;
