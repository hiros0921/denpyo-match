-- 010: 監査ログの検証を作り直す＋絞り込み用の索引
--
-- ============================================================
-- 【重大】現行の検証は中身の書き換えを検知できなかった
-- ============================================================
--
-- verify_audit_chain() は「prev_hash が直前行の row_hash と一致するか」しか
-- 見ていない。row_hash を中身から計算し直していないので、
-- 行の内容だけを書き換えても異常として出てこない。
--
-- 実測（ルールとトリガを一時的に外して「DBに直接繋がれた」状況を作った）:
--
--   書き換え前  id=100  action=auto_approve  after={"score":100,"partner_id":10,...}
--   書き換え後  id=100  action=auto_approve  after={"score":999}
--   verify_audit_chain() が見つけた異常  0件   ← 検知できていない
--
-- 連鎖を持たせた目的は「後から書き換えられたことを検知する」ことなので、
-- これでは仕組みとして成立していない。中身から計算し直して突き合わせる。
--
-- ============================================================
-- 【もう1つの問題】occurred_at::text は接続の時間帯設定で変わる
-- ============================================================
--
-- 元の式は occurred_at::text を材料に使っている。timestamptz を text にすると、
-- その接続の TimeZone 設定に従って描画される。
-- 書いたときと検証するときで設定が違えば、改竄が無くても不一致になる。
--
-- 時間帯に依存しない形（UTC に直して固定書式）に変える。
--
-- ============================================================
-- 式を変えると既存行のハッシュが合わなくなる
-- ============================================================
--
-- 監査ログは追記のみで、後から書き直す手段が無い（それが正しい）。
-- よって「式を差し替えて全行を再計算する」ができない。
-- hash_version を持たせ、行ごとにどの式で作られたかを記録する。
--
--   version 1  元の式。中身の検証ができない（繋がりだけ見る）
--   version 2  中身から計算し直せる。時間帯に依存しない
--
-- 既存行は 1 のまま残す。運用開始後に同じことが起きても、
-- この仕組みがあれば版を足すだけで対応できる。

BEGIN;

-- 既存行は 1。以後の行は 2。
-- ADD COLUMN の DEFAULT は既存行にも入るので、1 で入れてから既定値を 2 にする。
-- （audit_logs は UPDATE できないので、後から書き換える手段が無い）
ALTER TABLE audit_logs ADD COLUMN hash_version smallint NOT NULL DEFAULT 1;
ALTER TABLE audit_logs ALTER COLUMN hash_version SET DEFAULT 2;

-- ── 材料の作り方（版2）──
--
-- 1つの関数にまとめる理由は、トリガと検証で必ず同じ式を使うため。
-- 2箇所に書くと、片方だけ直したときに全行が「改竄」と判定される。
CREATE OR REPLACE FUNCTION audit_log_payload_v2(
  p_prev            text,
  p_organization_id bigint,
  p_actor_id        bigint,
  p_target_table    text,
  p_target_id       bigint,
  p_action          text,
  p_before          jsonb,
  p_after           jsonb,
  p_threshold_id    bigint,
  p_occurred_at     timestamptz
) RETURNS text AS $$
  -- 項目の区切りに \x1f（ASCII の Unit Separator）を挟む。
  -- 区切り無しで連結すると、隣り合う項目の境目をずらした別の組み合わせが
  -- 同じ材料になりうる。実データで起きにくくても、検知の仕組みとしては穴になる。
  SELECT concat_ws(E'\x1f',
    coalesce(p_prev, ''),
    p_organization_id::text,
    coalesce(p_actor_id::text, ''),
    p_target_table,
    p_target_id::text,
    p_action,
    coalesce(p_before::text, ''),
    coalesce(p_after::text, ''),
    coalesce(p_threshold_id::text, ''),
    -- 時間帯に依存しない形にする。接続の TimeZone 設定で変わらない。
    to_char(p_occurred_at AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS.US')
  );
$$ LANGUAGE sql IMMUTABLE;

-- ── 追記時のハッシュ計算（版2）──
CREATE OR REPLACE FUNCTION audit_logs_chain() RETURNS trigger AS $$
DECLARE
  prev text;
BEGIN
  SELECT row_hash INTO prev FROM audit_logs ORDER BY id DESC LIMIT 1;
  NEW.prev_hash := prev;
  NEW.hash_version := 2;
  NEW.row_hash := encode(digest(
      audit_log_payload_v2(prev, NEW.organization_id, NEW.actor_id,
        NEW.target_table, NEW.target_id, NEW.action,
        NEW.before, NEW.after, NEW.threshold_id, NEW.occurred_at),
      'sha256'), 'hex');
  RETURN NEW;
END $$ LANGUAGE plpgsql;

-- ── 検証 ──
--
-- 2つを別々に見る。
--   繋がり  prev_hash が直前行の row_hash と一致するか
--   中身    row_hash が中身から計算した値と一致するか   ← 版2のみ
--
-- 相関副問い合わせ（1行ごとに直前行を引く）をやめ、窓関数で1回走査にする。
-- 実測（5万件）: 217ms → 14.5ms。15倍。
--
-- 範囲を指定できるようにする。全件の検証は行数に比例して伸びるので、
-- 画面を開くたびに全件を走らせる作りにはできない。
-- 画面は表示中の範囲だけを検証し、全件は人が明示的に実行する。
-- 【重要】先に古い定義を落とす。
-- CREATE OR REPLACE は引数の並びが違うと「置き換え」ではなく「多重定義」になる。
-- 引数なしの古い版が残ったまま、既定値つきの新しい版を作ると、
-- verify_audit_chain() の呼び出しがどちらか決められずエラーになる。
-- 画面はこの形で呼んでいるので、そのまま気付かず壊れる。
DROP FUNCTION IF EXISTS verify_audit_chain();

CREATE OR REPLACE FUNCTION verify_audit_chain(
  p_from_id bigint DEFAULT NULL,
  p_to_id   bigint DEFAULT NULL
) RETURNS TABLE(bad_id bigint, reason text) AS $$
  WITH scope AS (
    -- 範囲の先頭行は、その1つ前の行と繋がっているかを見る必要がある。
    -- 1行だけ手前から含める。
    SELECT * FROM audit_logs
     WHERE (p_from_id IS NULL
            OR id >= coalesce((SELECT max(id) FROM audit_logs WHERE id < p_from_id),
                              p_from_id))
       AND (p_to_id IS NULL OR id <= p_to_id)
  ), chained AS (
    SELECT s.*,
           lag(s.row_hash) OVER (ORDER BY s.id) AS expect_prev,
           row_number()    OVER (ORDER BY s.id) AS rn
      FROM scope s
  )
  SELECT c.id,
         CASE
           WHEN c.prev_hash IS DISTINCT FROM c.expect_prev
             THEN '直前行との連鎖が切れている'
           ELSE '中身がハッシュと一致しない（書き換えられた可能性）'
         END
    FROM chained c
   WHERE
     -- 先頭行は、手前の行を含められなかった場合に繋がりを判定できない。
     -- 範囲を切った境目を「異常」と誤報しないため、繋がりの判定から外す。
     (c.rn > 1 AND c.prev_hash IS DISTINCT FROM c.expect_prev)
     OR (c.hash_version = 2
         AND c.row_hash IS DISTINCT FROM encode(digest(
               audit_log_payload_v2(c.prev_hash, c.organization_id, c.actor_id,
                 c.target_table, c.target_id, c.action,
                 c.before, c.after, c.threshold_id, c.occurred_at),
               'sha256'), 'hex'));
$$ LANGUAGE sql STABLE;

COMMENT ON FUNCTION verify_audit_chain(bigint, bigint) IS
  '監査ログの検証。範囲を省くと全件。版1の行は繋がりのみ、版2は中身も検証する';

-- ── 絞り込み用の索引 ──
--
-- 実測（5万件）: 該当が0件の操作で絞ると全件走査になり 23.8ms。
-- 行数に比例して伸びるので、100万件なら約0.5秒、1000万件なら約5秒。
-- 「探したが無かった」がいちばん遅い、という状態になる。
--
-- 並び順は id の降順で固定する。追記のみの表なので id が実際の順序であり、
-- 重複しないので次ページの起点にも使える。
CREATE INDEX audit_logs_org_id_desc      ON audit_logs (organization_id, id DESC);
CREATE INDEX audit_logs_org_action_id    ON audit_logs (organization_id, action, id DESC);
CREATE INDEX audit_logs_org_actor_id     ON audit_logs (organization_id, actor_id, id DESC);
CREATE INDEX audit_logs_org_occurred_id  ON audit_logs (organization_id, occurred_at DESC, id DESC);

COMMIT;
