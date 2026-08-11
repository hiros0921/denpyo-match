-- コンテナの初回起動時に自動実行される。
--
-- pg_trgm はこのプロダクトの核心。取引先名の表記揺れを吸収する候補生成を
-- DBの機能だけで賄うために使う。GINインデックスと組み合わせる。
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- 監査ログのハッシュ連鎖で digest() を使う。
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- 候補生成のしきい値。0.3 未満は候補にも入れない。
-- 第4段階で生成データを使って調整し、根拠を数字で残すこと。
--
-- 【注意】この設定は pg_dump に含まれない。
-- DBを作り直して復元したときに、黙って既定値（0.3）へ戻る。
-- 今は既定値と同じなので実害は無いが、値を変えたら別途書き戻すこと。
ALTER DATABASE denpo_match SET pg_trgm.similarity_threshold = 0.3;

-- ── ロケールの確認 ──
--
-- pg_trgm は「文字かどうか」の判定に CTYPE を使い、文字でないものを
-- 捨ててから3文字組を作る。CTYPE が C だと日本語が全部捨てられ、
-- show_trgm('シラカワ商事') が {} になる。
--
-- エラーは出ない。候補生成が常に0件になるだけなので、
-- 「速いが何も当たらない」状態になり、速度だけ測ると正常に見える。
-- 第6段階で実際に踏んだ。ここで止める。
DO $$
BEGIN
  IF array_length(show_trgm('シラカワ商事'), 1) IS NULL THEN
    RAISE EXCEPTION
      'pg_trgm が日本語を扱えません。DBの LC_CTYPE が % になっています。'
      '初期化引数を --lc-collate=C --lc-ctype=C.utf8 にしてください',
      (SELECT datctype FROM pg_database WHERE datname = current_database());
  END IF;
END $$;
