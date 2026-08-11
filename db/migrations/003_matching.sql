-- 003: 照合候補・確定結果・閾値
--
-- 設計の要点（このプロダクトの核心）
--
--   match_candidates を捨てない。
--   閾値シミュレーションは「保存済みのスコアを集計するだけ」で成立させる。
--   スライダーを動かすたびに照合をやり直す設計にしてはいけない。
--   実測: 50万件（伝票1万件 × 候補50件）の集計が 79.4ms。目標1秒に対し12倍の余裕。
--
--   thresholds を上書きしない。
--   「この伝票はどの閾値設定で自動承認されたか」を後から辿る要件がある。
--   設定を上書きすると追跡できなくなるため、valid_from/valid_to で履歴を積む。
--   match_results.threshold_id が、その時点の特定の行を指す。

BEGIN;

-- 閾値設定。上書きせず、履歴として積む。
-- 適用の優先順位は partner > client > organization、doc_type 一致を優先。
CREATE TABLE thresholds (
  id              bigserial PRIMARY KEY,
  organization_id bigint      NOT NULL REFERENCES organizations(id),
  client_id       bigint      REFERENCES clients(id),   -- NULL は組織全体
  partner_id      bigint      REFERENCES partners(id),  -- NULL は取引先を問わない
  doc_type        smallint,                             -- NULL は全種別
  upper           numeric(5,2) NOT NULL,  -- これ以上で自動承認
  lower           numeric(5,2) NOT NULL,  -- これ未満で却下
  valid_from      timestamptz NOT NULL DEFAULT now(),
  valid_to        timestamptz,            -- NULL は現行
  created_by      bigint      REFERENCES users(id),
  created_at      timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT thresholds_range_check CHECK (lower <= upper),
  CONSTRAINT thresholds_bounds_check
    CHECK (upper BETWEEN 0 AND 100 AND lower BETWEEN 0 AND 100)
);
CREATE INDEX ON thresholds (organization_id, client_id, partner_id, doc_type);
-- 現行の設定を引くとき用
CREATE INDEX ON thresholds (organization_id) WHERE valid_to IS NULL;

-- 照合候補とスコア。★閾値シミュレーションの元データ。消さない。
CREATE TABLE match_candidates (
  id           bigserial PRIMARY KEY,
  document_id  bigint      NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  partner_id   bigint      NOT NULL REFERENCES partners(id),
  score        numeric(5,2) NOT NULL,   -- 0〜100
  score_detail jsonb,                   -- 編集距離・n-gram類似度・先頭一致の内訳
  rank         smallint    NOT NULL,    -- 1〜50
  computed_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT match_candidates_score_check CHECK (score BETWEEN 0 AND 100)
);
CREATE INDEX ON match_candidates (document_id, score DESC);
-- シミュレーションの集計用。件数を数えるだけなのでインデックスだけで足りる。
CREATE INDEX ON match_candidates (score);

-- 確定した突合結果。どの閾値で判定されたかを必ず持つ。
CREATE TABLE match_results (
  id           bigserial PRIMARY KEY,
  document_id  bigint      NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  partner_id   bigint      REFERENCES partners(id),   -- 却下時は NULL
  score        numeric(5,2),
  decision     smallint    NOT NULL,
    -- 1:自動承認 2:人が承認 3:人が修正 4:却下
  threshold_id bigint      REFERENCES thresholds(id), -- ★どの設定で判定したか
  decided_by   bigint      REFERENCES users(id),      -- 自動承認時は NULL
  decided_at   timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT match_results_decision_check CHECK (decision BETWEEN 1 AND 4),
  -- 自動承認なら threshold_id が必須。追跡できない自動承認を作らせない。
  CONSTRAINT match_results_auto_needs_threshold
    CHECK (decision <> 1 OR threshold_id IS NOT NULL)
);
CREATE UNIQUE INDEX ON match_results (document_id);
CREATE INDEX ON match_results (partner_id, decided_at);
CREATE INDEX ON match_results (decision, decided_at);

COMMIT;
