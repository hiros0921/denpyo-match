-- 1つのアップロードから複数の伝票ができるようにする。
--
-- なぜ要るか
--
--   受領側で一番多いのはレシートで、現場は何枚もまとめてスキャンする。
--   実物の「レシートサンプル_10枚まとめ.pdf」は 5ページ × 2枚 = 10件だった。
--
--   これを1件として扱うと、1つの伝票の中で項目が混ざる。実測:
--     取引先名 サンプルマート（左のレシート）
--     金額     ¥5,395        （右のレシート）
--   正解は左が ¥1,258、右が見本石油サービス。
--   どちらの項目にも「読めなかった」印は付かないので、スコアは高いまま出る。
--   閾値では止まらない種類の誤りで、これが一番たちが悪い。
--
-- どこで分けるか
--
--   受付（アップロード）で分ける。ワーカーではない。
--   ワーカーで分けると、伝票の番号が振られたあとに件数が変わる。
--   監査ログは伝票ごとに連鎖しているので、対応が壊れる。
--
-- 何を記録するか
--
--   「この伝票は、どのファイルの何ページ目の、どの位置から切り出したか」。
--   これが無いと、現場が画面で見たときに元の紙と突き合わせられない。
--   レシート10枚が10件の伝票として並ぶだけでは、
--   どれがどれか分からず、確認できない。
BEGIN;

ALTER TABLE documents
  -- アップロードされたファイルの名前。同じ紙から出た伝票をまとめる手がかり。
  -- 保存キーではなく人が見る名前を入れる。画面で照らし合わせるのは人。
  ADD COLUMN source_name   text,
  -- 何ページ目か。1から。PDF でなければ 1。
  ADD COLUMN source_page   int  NOT NULL DEFAULT 1,
  -- そのページの中の何番目か。1から。分けなかったときも 1。
  ADD COLUMN source_region int  NOT NULL DEFAULT 1,
  -- 切り出した位置。元の紙のどこだったかを画面で示すのに使う。
  ADD COLUMN source_box    jsonb;

ALTER TABLE documents
  ADD CONSTRAINT documents_source_page_check   CHECK (source_page   >= 1),
  ADD CONSTRAINT documents_source_region_check CHECK (source_region >= 1);

-- 同じファイルから出た伝票を並べる。
-- 現場は「さっき入れたPDFの分」をまとめて見る。
CREATE INDEX documents_source_idx
  ON documents (client_id, source_name, source_page, source_region)
  WHERE source_name IS NOT NULL;

COMMENT ON COLUMN documents.source_name   IS 'アップロード時のファイル名';
COMMENT ON COLUMN documents.source_page   IS 'PDFの何ページ目か（1から）';
COMMENT ON COLUMN documents.source_region IS 'そのページの何番目の伝票か（1から）';
COMMENT ON COLUMN documents.source_box    IS '元のページ内での切り出し位置 {x,y,w,h}';

COMMIT;
