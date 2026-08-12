-- 摘要の桁切れによる取り違えを検出するための索引。
--
-- なぜ要るか
--
--   銀行明細の摘要は桁数制限で切れる。長い社名が切れると、短い別会社の
--   名前と完全一致することがある。実測（12桁で切断）:
--
--     見本印刷工業 の摘要 → ﾐﾎﾝｲﾝｻﾂ
--       見本印刷（別会社）  100.0  ← 全成分1.00の完全一致
--       見本印刷工業（正解） 51.4
--
--   誤ったほうが満点になるので、照合スコアの重みでは原理的に勝てない
--   （prefix の重みを 0.0〜0.5 で振っても逆転件数は1件も動かなかった）。
--   対策は採点の外に置き、「その正規形で始まる取引先が2件以上あるか」を
--   マスタに聞く。2件以上なら一意に定まらない＝自動で確定させない。
--
-- 濁点を落とした形でも引く理由
--
--   半角カナでは濁点が独立した1桁を食う。切れ目が濁点を割ると
--   ｷﾞ が ｷ に化ける。実測:
--     ハルタ工業（ﾊﾙﾀｺｳｷﾞｮｳ）が8桁で切れる → ハルタコウキ
--       有限会社ハルタ工機  norm=ハルタコウキ      濁点あり ○
--       ハルタ工業株式会社  norm=ハルタコウギョウ  濁点あり ×  濁点なし ○
--   濁点ありだけだと正解自身が前方一致せず、一致1件＝一意と誤判定する。
BEGIN;

-- 濁点・半濁点を清音に落とす。
--
-- IMMUTABLE にしないと式索引に使えない。translate は入力だけで結果が
-- 決まるので条件を満たす。
--
-- 【注意】ここは「検出のための鍵」であって、正規化ルールではない。
-- partners.norm や照合そのものには一切影響しない。
CREATE OR REPLACE FUNCTION dm_strip_daku(t text) RETURNS text AS $$
  SELECT translate($1,
    'ガギグゲゴザジズゼゾダヂヅデドバビブベボパピプペポヴ',
    'カキクケコサシスセソタチツテトハヒフヘホハヒフヘホウ')
$$ LANGUAGE sql IMMUTABLE STRICT;

COMMENT ON FUNCTION dm_strip_daku(text) IS
  '濁点・半濁点を清音に落とす。桁切れの検出用。正規化ルールではない';

-- 前方一致の索引。
--
-- 【重要】text_pattern_ops が要る。
-- DBは LC_COLLATE=C で作ってあるので既定の演算子クラスでも
-- LIKE 'x%' は索引を使えるが、照合順序の設定に依存させたくない。
-- 明示しておけば、将来 COLLATE を変えても壊れない。
CREATE INDEX partners_norm_prefix_idx
  ON partners (norm text_pattern_ops);
CREATE INDEX partners_norm_daku_prefix_idx
  ON partners (dm_strip_daku(norm) text_pattern_ops);

CREATE INDEX partner_aliases_norm_prefix_idx
  ON partner_aliases (norm text_pattern_ops);
CREATE INDEX partner_aliases_norm_daku_prefix_idx
  ON partner_aliases (dm_strip_daku(norm) text_pattern_ops);

-- 桁切れの判定に使う「その取り込みで一番長い摘要」を速く出す。
--
-- 切れていない摘要まで曖昧扱いにすると、安全な明細の自動突合まで止まる。
-- 実測（無切断）: 桁の条件を付けないと 115件中15件の自動突合を失った。
-- 付けたら 115件のまま変わらなかった。
CREATE INDEX transactions_batch_len_idx
  ON transactions (batch_id, char_length(description) DESC);

COMMIT;
