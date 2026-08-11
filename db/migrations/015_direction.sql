-- 帳票の向き。受け取ったものか、出したものか。
--
-- なぜ要るか
--
--   照合すべき相手が、向きによって逆になる。
--
--     発行（自社が出した請求書）  相手＝宛先。「○○商事 御中」の側
--     受領（他社から来た請求書）  相手＝発行元。宛先は自社なので、
--                                 そこを照合しても毎回自分に当たるだけ
--
--   これは画像を見ても判別できない。どちらの様式にも発行元と宛先が
--   両方書いてあるので、「どちらが自社か」を知っている側でしか決められない。
--   OCR で当てにいくのではなく、設定として持つ。
--
--   実物のPDF5枚（受領側）で確認したところ、宛先と発行元が左右に並び、
--   宛先側を見ていたために5枚とも取引先名を誤っていた。
--
-- 既定値を「受領」にする理由
--
--   枚数では受領側が圧倒的に多い。出す請求書は月に数十枚でも、
--   受け取る請求書・領収書・レシートは数百枚になる。
--   既定は多いほうに合わせ、少ないほうを明示的に選ばせる。
--
-- 既存行を「発行」で埋める理由
--
--   これまでの100枚の検証データは自社が発行した請求書として作ってある
--   （発行元がすべて「テスト商事株式会社」、宛先が得意先）。
--   既定値でまとめて上書きすると、過去の実測と再現できなくなる。
BEGIN;

ALTER TABLE documents
  ADD COLUMN direction smallint NOT NULL DEFAULT 1;   -- 1:受領 2:発行

-- 既存はすべて発行側の検証データ。既定値ではなく実態で埋める。
UPDATE documents SET direction = 2;

ALTER TABLE documents
  ADD CONSTRAINT documents_direction_check CHECK (direction IN (1, 2));

COMMENT ON COLUMN documents.direction IS '1:受領（相手は発行元） 2:発行（相手は宛先）';

-- 顧問先ごとの既定。アップロードのたびに選ばせると必ず選び間違える。
-- 受領専門の顧問先が大半なので、そこは既定を固定できるようにする。
ALTER TABLE clients
  ADD COLUMN default_direction smallint NOT NULL DEFAULT 1;

ALTER TABLE clients
  ADD CONSTRAINT clients_default_direction_check CHECK (default_direction IN (1, 2));

COMMIT;
