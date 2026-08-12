#!/usr/bin/env python3
"""銀行明細の摘要を、桁切れも含めて生成する。

    python3 testdata/generator/make_bank_desc.py

なぜ作るか
----------
銀行明細の摘要欄は桁数制限で途中が切れる。切れた摘要が照合スコアに
どう効くかは、実データを待たずに測れる。実際の銀行は半角カナで
十数桁のことが多い。

【要点】先頭が同じで後半だけ違う取引先を必ず含める。
短く切れたとき区別できなくなるのはその組み合わせで、
それが何桁で起きるかを知るのがこの生成の目的。

出力
    samples/bank_desc.json
      partners  取引先（正式名・表記揺れ・カナ読み・紛らわしい相手）
      descs     摘要（取引先・切り詰め桁数・変化のさせ方・文字列）
"""
import argparse
import json
import unicodedata
from pathlib import Path

BASE = Path(__file__).resolve().parent.parent
OUT = BASE / "samples"

# 全角カナ → 半角カナ。
# 【重要】濁点は独立した1文字になる。銀行の桁数はこれを1桁と数えるので、
# 「ﾀﾞ」は2桁。切れ目が濁点を割ることが実際に起きる。
Z2H = {
    "ア": "ｱ", "イ": "ｲ", "ウ": "ｳ", "エ": "ｴ", "オ": "ｵ",
    "カ": "ｶ", "キ": "ｷ", "ク": "ｸ", "ケ": "ｹ", "コ": "ｺ",
    "サ": "ｻ", "シ": "ｼ", "ス": "ｽ", "セ": "ｾ", "ソ": "ｿ",
    "タ": "ﾀ", "チ": "ﾁ", "ツ": "ﾂ", "テ": "ﾃ", "ト": "ﾄ",
    "ナ": "ﾅ", "ニ": "ﾆ", "ヌ": "ﾇ", "ネ": "ﾈ", "ノ": "ﾉ",
    "ハ": "ﾊ", "ヒ": "ﾋ", "フ": "ﾌ", "ヘ": "ﾍ", "ホ": "ﾎ",
    "マ": "ﾏ", "ミ": "ﾐ", "ム": "ﾑ", "メ": "ﾒ", "モ": "ﾓ",
    "ヤ": "ﾔ", "ユ": "ﾕ", "ヨ": "ﾖ",
    "ラ": "ﾗ", "リ": "ﾘ", "ル": "ﾙ", "レ": "ﾚ", "ロ": "ﾛ",
    "ワ": "ﾜ", "ヲ": "ｦ", "ン": "ﾝ",
    "ァ": "ｧ", "ィ": "ｨ", "ゥ": "ｩ", "ェ": "ｪ", "ォ": "ｫ",
    "ッ": "ｯ", "ャ": "ｬ", "ュ": "ｭ", "ョ": "ｮ",
    "ー": "ｰ", "・": "･", "゙": "ﾞ", "゚": "ﾟ",
}


def to_hankaku(s: str) -> str:
    return "".join(Z2H.get(c, c) for c in unicodedata.normalize("NFD", s))


# ── 取引先 ──
#
# 先頭が同じで後半だけ違う組を8組。これが測定の本体。
# 残りは、紛らわしくない対照として混ぜる。
#
# 実在企業に由来しない。組み合わせで作った架空の名前。
PAIRS = [
    # (正式名, カナ読み) を2つで1組
    (("株式会社ミライハイソウサービス", "ミライハイソウサービス"),
     ("ミライハイソウ運輸株式会社", "ミライハイソウウンユ")),
    (("サンプル商事株式会社", "サンプルショウジ"),
     ("サンプル商会", "サンプルショウカイ")),
    (("見本印刷株式会社", "ミホンインサツ"),
     ("見本印刷工業株式会社", "ミホンインサツコウギョウ")),
    (("カエデ製作所", "カエデセイサクショ"),
     ("カエデ精機株式会社", "カエデセイキ")),
    (("トウカイ電機株式会社", "トウカイデンキ"),
     ("トウカイ電気工事", "トウカイデンキコウジ")),
    (("アカネ物流センター", "アカネブツリュウセンター"),
     ("アカネ物流サービス", "アカネブツリュウサービス")),
    (("スミレフーズ株式会社", "スミレフーズ"),
     ("スミレフードサービス", "スミレフードサービス")),
    (("有限会社ハルタ工機", "ハルタコウキ"),
     ("ハルタ工業株式会社", "ハルタコウギョウ")),
]

# 紛らわしくない対照。実物のレシートに出た形に寄せてある。
SINGLES = [
    ("サンプルマート", "サンプルマート"),
    ("見本石油サービス", "ミホンセキユサービス"),
    ("居酒屋テスト亭", "イザカヤテストテイ"),
    ("モデルタクシー株式会社", "モデルタクシー"),
    ("家電テストランド", "カデンテストランド"),
    ("ドラッグ見本", "ドラッグミホン"),
    ("サンプル行政書士事務所", "サンプルギョウセイショシジムショ"),
]

# 切り詰める桁数（半角カナ）。0 は無切断＝対照群。
TRUNCS = [8, 12, 16, 20, 24, 0]


def variants_of(kana: str) -> list:
    """実際の銀行明細で起きる形にする。切り詰める前の全長。"""
    h = to_hankaku(kana)
    return [
        ("plain",   h),                    # 名前だけ
        ("prefix",  "ｶ)" + h),             # 全銀協の略号が前に付く
        ("suffix",  h + "(ｶ"),             # 略号が後ろに付く
        ("furikomi", "ﾌﾘｺﾐ " + h),          # 取引種別の語が前に付く
        ("furikomi_nospace", "ﾌﾘｺﾐ" + h),  # 空白が詰められている
    ]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default=str(OUT / "bank_desc.json"))
    args = ap.parse_args()

    partners, descs = [], []
    pid = 0

    def add(name, kana, pair_with=None):
        nonlocal pid
        pid += 1
        partners.append({
            "id": pid, "canonical": name, "kana": kana,
            # 学習前のマスタ側。法人格の書き方違いだけを持たせる。
            # カナ読みはここに入れない（人が承認して初めて覚える）。
            "variants": [name],
            "pair_with": pair_with,
        })
        return pid

    for a, b in PAIRS:
        ia = add(a[0], a[1])
        ib = add(b[0], b[1], pair_with=ia)
        partners[ia - 1]["pair_with"] = ib

    for name, kana in SINGLES:
        add(name, kana)

    for p in partners:
        for vname, full in variants_of(p["kana"]):
            for n in TRUNCS:
                text = full if n == 0 else full[:n]
                if not text:
                    continue
                descs.append({
                    "partner_id": p["id"],
                    "trunc": n,
                    "variant": vname,
                    "text": text,
                    "full_len": len(full),
                    # 実際に切られたか。全長より短い指定でだけ切れる。
                    "cut": n != 0 and len(full) > n,
                })

    out = {"partners": partners, "descs": descs}
    Path(args.out).write_text(
        json.dumps(out, ensure_ascii=False, indent=1), encoding="utf-8")
    print(f"取引先 {len(partners)}件（うち紛らわしい組 {len(PAIRS)}組）")
    print(f"摘要 {len(descs)}件 → {args.out}")


if __name__ == "__main__":
    main()
