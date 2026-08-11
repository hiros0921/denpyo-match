-- インボイス制度の登録番号（T＋13桁）の検査結果。
--
-- なぜ表を分けるか
--
--   extracted_fields には読み取った文字列が既に入っている。
--   それとは別に、判定の結果をここに持つ。理由は2つ。
--
--   ① 一覧が速い。「登録番号に問題がある伝票」を出す画面は、
--      会計事務所が毎月まとめて見る。数千件から絞るのに、
--      その都度 T+13桁を取り出して検査数字を計算するのでは間に合わない。
--
--   ② 国税庁への問い合わせ結果を残せる。公表システムは
--      「いつ時点で登録があったか」に意味がある。登録は取り消される
--      ことがあり、あとから遡って確かめられないと、
--      「当時は有効だった」を示せない。
--
-- なぜ「無効」という状態を持たないか
--
--   検査数字が合わないことは、無効の証明にならない。
--   登録番号は2種類ある:
--     法人番号を持つ課税事業者   T + 法人番号13桁
--     それ以外（個人事業者など） T + 新たに付番された13桁
--   後者は法人番号ではないので、法人番号の検査数字に従うとは限らない。
--   ここで「無効」と記録すると、仕入税額控除の判断を誤らせる。
--   状態は「検査数字が合わない（要確認）」までにとどめる。
BEGIN;

CREATE TABLE invoice_reg_checks (
  document_id bigint      PRIMARY KEY REFERENCES documents(id) ON DELETE CASCADE,
  -- 読み取った番号を正規化したもの。T＋13桁。読めなければ NULL。
  reg_no      text,
  status      smallint    NOT NULL,
    -- 1:記載なし 2:形式が違う 3:検査数字が合わない
    -- 4:形式は正しい（実在は未確認） 5:登録あり 6:登録が見つからない
  -- なぜその判定になったか。画面にそのまま出す。
  -- 「無効です」だけでは、現場は先方に何を問い合わせればよいか分からない。
  why         text        NOT NULL,
  -- 国税庁の公表システムが返した名称・所在地。照合前は NULL。
  -- 名称が請求書の発行元と食い違っていたら、番号の使い回しを疑う。
  registered_name text,
  registered_addr text,
  -- 公表システムに問い合わせた時刻。していなければ NULL。
  looked_up_at timestamptz,
  checked_at  timestamptz NOT NULL DEFAULT now()
);

-- 「見るべきもの」を出す画面のための索引。
-- 状態4・5（問題なし）を除いた行だけを持つ。
-- 全件に索引を張ると、正常な数千件まで舐めることになる。
CREATE INDEX invoice_reg_checks_attention_idx
  ON invoice_reg_checks (status, checked_at DESC)
  WHERE status NOT IN (4, 5);

-- 同じ番号が何件の伝票に出てくるかを数える。
-- 1つの登録番号が複数の取引先名で出てきたら、記載の誤りを疑う。
CREATE INDEX invoice_reg_checks_reg_no_idx
  ON invoice_reg_checks (reg_no) WHERE reg_no IS NOT NULL;

COMMENT ON TABLE invoice_reg_checks IS
  'インボイス登録番号の検査結果。1伝票1行';

COMMIT;
