-- 013: 契約状態の管理（①追加だけ止める ③14日の猶予）
--
-- 決めたこと
--
--   ① 契約が切れたら、新しい伝票をアップロードできない。
--      過去の伝票と監査ログは見られる。
--      会計事務所は顧問先の帳簿を預かる仕事なので、解約後に税務調査が来たときに
--      何も出せない状態は現実的でない。
--
--   ③ 支払いが失敗しても14日は今までどおり動く。
--      カードの期限切れは普通に起きる。その日のうちに止まると、
--      事務所の業務が朝いちで止まる。
--
-- 設計の要点
--
--   猶予の終わりを行に持つ（grace_until）。
--   「status が past_due になってから14日」を毎回その場で計算すると、
--   状態が変わった時刻をどこかに持つ必要がある。updated_at は他の理由でも動くので使えない。
--   支払いが失敗した時点で終わりの時刻を決めて書く。
--
--   契約が無い組織はアップロードできない。これが既定。
--   開発や社内利用のために billing_exempt を用意するが、既定は false。
--   「うっかり全社無料」が起きない側に倒す。

BEGIN;

-- Checkout を始める時点では、まだ契約は存在しないが顧客は必要になる。
-- 2回目以降のために、顧客IDは組織に持たせる。
ALTER TABLE organizations
  ADD COLUMN stripe_customer_id text UNIQUE,
  -- 課金の免除。社内利用・無償提供先のため。既定は false（課金が要る）。
  ADD COLUMN billing_exempt boolean NOT NULL DEFAULT false;

ALTER TABLE subscriptions
  -- 支払いが失敗した時点で決まる「ここまでは動かす」時刻。
  -- 支払いが通ったら NULL に戻す。
  ADD COLUMN grace_until timestamptz,
  -- 期末で解約予定か。画面に「◯月◯日で終了します」と出すために要る。
  ADD COLUMN cancel_at_period_end boolean NOT NULL DEFAULT false,
  ADD COLUMN stripe_price_id text;

-- 1つの組織に、生きている契約を2つ作らせない。
--
-- 終わった状態（canceled / incomplete_expired）は履歴として複数残ってよい。
-- 生きている契約が2つあると、どちらで判定するかが決まらない。
-- Webhook は順番どおりに届くとは限らないので、アプリ側の注意では防げない。
CREATE UNIQUE INDEX subscriptions_one_active_per_org
  ON subscriptions (organization_id)
  WHERE status NOT IN ('canceled', 'incomplete_expired');

COMMENT ON COLUMN subscriptions.status IS
  'Stripe の値をそのまま入れる: trialing/active/past_due/unpaid/canceled/incomplete/incomplete_expired/paused';
COMMENT ON COLUMN subscriptions.grace_until IS
  '支払い失敗後、ここまでは今までどおり動かす。支払いが通ったら NULL に戻す';

COMMIT;
