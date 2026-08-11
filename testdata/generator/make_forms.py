#!/usr/bin/env python3
"""テスト用の帳票画像を生成する。実在の伝票は一切使わない。

    python3 testdata/generator/make_forms.py --count 20
    python3 testdata/generator/make_forms.py --count 10000 --masters-only

生成するのは「きれいな帳票」まで。実際のスキャンに近づける劣化は
degrade.py が担当する。2段階に分ける理由は、劣化前の状態を
「正解」として保持し、前処理でどれだけ元に戻せたかを測るため。

出力
    samples/clean/inv_0001.png    画像
    samples/clean/inv_0001.json   正解（取引先名・日付・金額・伝票番号と、その座標）
    samples/masters.json          取引先マスタ（表記揺れ込み）

取引先名は架空のものを組み合わせで作る。実在企業と偶然一致する可能性は
あるが、データそのものは実在の伝票に由来しない。
"""
import argparse
import json
import random
from pathlib import Path

from PIL import Image, ImageDraw, ImageFont

BASE = Path(__file__).resolve().parent.parent
OUT = BASE / "samples"

FONT_REG = "/System/Library/Fonts/ヒラギノ角ゴシック W3.ttc"
FONT_BOLD = "/System/Library/Fonts/ヒラギノ角ゴシック W6.ttc"

W, H = 1654, 2339          # A4 200dpi
DOC_TYPES = {1: "請求書", 2: "納品書", 3: "領収書"}

# 架空の取引先を組み合わせで作る。実在企業に由来しない。
HEAD = ["ヤマト", "サクラ", "ミドリ", "アオイ", "コウヨウ", "シラカワ", "ハルタ",
        "トウカイ", "ホクト", "ナンセイ", "アカネ", "スミレ", "カエデ", "ツバキ"]
TAIL = ["商事", "物産", "工業", "産業", "運輸", "電機", "製作所", "販売",
        "サービス", "システム", "エンジニアリング", "商会"]
# 法人格の付き方。これが表記揺れの主因になる。
#
# 重要: 株式会社と有限会社を同じ社の「揺れ」として混ぜてはいけない。
# 実際には別法人であり、混ぜると名寄せエンジンに誤った正解を教えることになる。
# 揺れとして許されるのは「同じ法人格の書き方違い」と「法人格の省略」まで。
CORP_KK = ["株式会社{}", "{}株式会社", "(株){}", "{}(株)", "㈱{}", "{}㈱", "{}"]
CORP_YK = ["有限会社{}", "{}有限会社", "(有){}", "{}(有)", "㈲{}", "{}"]

ITEMS = ["事務用品一式", "梱包資材", "配送料", "保守作業費", "部品代",
         "検査費用", "設置工事費", "月額利用料", "印刷代", "運搬費"]


def font(path, size):
    return ImageFont.truetype(path, size)


def make_masters(n_partners: int, seed: int) -> list:
    """取引先マスタを作る。1社につき表記揺れを2〜4通り持たせる。"""
    rnd = random.Random(seed)
    masters, used = [], set()
    while len(masters) < n_partners:
        core = rnd.choice(HEAD) + rnd.choice(TAIL)
        if core in used:
            core += str(rnd.randint(1, 999))
            if core in used:
                continue
        used.add(core)
        # 1社につき法人格は1種類に固定する（株式会社なら株式会社だけ）
        forms_pool = CORP_KK if rnd.random() < 0.8 else CORP_YK
        forms = rnd.sample(forms_pool, rnd.randint(2, 4))
        masters.append({
            "id": len(masters) + 1,
            "core": core,                                   # 法人格を除いた核
            "canonical": forms[0].format(core),             # 正式名称
            "variants": [f.format(core) for f in forms],    # 表記揺れ
        })
    return masters


def draw_form(doc_type: int, data: dict) -> tuple:
    """帳票を1枚描く。画像と、項目の座標を返す。"""
    im = Image.new("RGB", (W, H), "white")
    d = ImageDraw.Draw(im)
    f_title = font(FONT_BOLD, 72)
    f_head = font(FONT_BOLD, 34)
    f_body = font(FONT_REG, 30)
    f_small = font(FONT_REG, 24)
    boxes = {}

    def put(key, xy, text, fnt):
        d.text(xy, text, fill="black", font=fnt)
        l, t, r, b = d.textbbox(xy, text, font=fnt)
        boxes[key] = {"x": l, "y": t, "w": r - l, "h": b - t}

    title = DOC_TYPES[doc_type]
    tw = d.textbbox((0, 0), title, font=f_title)[2]
    d.text(((W - tw) / 2, 150), title, fill="black", font=f_title)
    d.line([(W / 2 - tw / 2 - 20, 240), (W / 2 + tw / 2 + 20, 240)], fill="black", width=3)

    # 宛先（＝取引先名）。ここが名寄せの対象。
    put("partner_name", (140, 360), data["partner_name"], f_head)
    d.text((140 + d.textbbox((0, 0), data["partner_name"], font=f_head)[2] + 20, 368),
           "御中", fill="black", font=f_body)
    d.line([(140, 420), (900, 420)], fill="black", width=2)

    # 伝票番号・日付
    put("doc_no", (1120, 350), f"No. {data['doc_no']}", f_body)
    put("issue_date", (1120, 400), data["issue_date"], f_body)

    # 発行元（架空）
    d.text((1120, 470), "テスト商事株式会社", fill="black", font=f_body)
    d.text((1120, 512), "東京都千代田区0-0-0", fill="black", font=f_small)
    d.text((1120, 546), "TEL 03-0000-0000", fill="black", font=f_small)

    # 合計金額（税込）。下部の「合計」欄と必ず一致させる。
    # ここがずれていると、金額抽出の正解が2通りできてしまう。
    d.rectangle([140, 560, 900, 660], outline="black", width=3)
    d.text((160, 590), "合計金額", fill="black", font=f_head)
    amt = f"¥{data['total_with_tax']:,}-"
    aw = d.textbbox((0, 0), amt, font=f_head)[2]
    put("total", (880 - aw, 588), amt, f_head)

    # 明細表
    y = 740
    cols = [(140, "品目"), (900, "数量"), (1080, "単価"), (1340, "金額")]
    d.rectangle([140, y, 1514, y + 56], outline="black", width=2)
    for x, label in cols:
        d.text((x + 14, y + 12), label, fill="black", font=f_body)
    y += 56
    for row in data["rows"]:
        d.rectangle([140, y, 1514, y + 56], outline="black", width=1)
        d.text((154, y + 12), row["item"], fill="black", font=f_body)
        for x, v in ((900, str(row["qty"])), (1080, f"{row['unit']:,}"),
                     (1340, f"{row['amount']:,}")):
            vw = d.textbbox((0, 0), v, font=f_body)[2]
            d.text((x + 160 - vw, y + 12), v, fill="black", font=f_body)
        y += 56

    # 小計・消費税・合計。紙面の下部を埋め、傾き検出の手がかりを増やす
    sub = sum(r["amount"] for r in data["rows"])
    tax = int(sub * 0.1)
    y += 30
    for label, val in (("小計", sub), ("消費税(10%)", tax), ("合計", sub + tax)):
        d.rectangle([1080, y, 1514, y + 52], outline="black", width=1)
        d.text((1094, y + 10), label, fill="black", font=f_small)
        v = f"{val:,}"
        vw = d.textbbox((0, 0), v, font=f_body)[2]
        d.text((1500 - vw, y + 8), v, fill="black", font=f_body)
        y += 52

    # 振込先。罫線が増えるので、罫線検出・除去の検証材料にもなる
    y += 70
    d.rectangle([140, y, 900, y + 200], outline="black", width=2)
    d.text((160, y + 16), "お振込先", fill="black", font=f_head)
    for i, line in enumerate(("テスト銀行 千代田支店",
                              "普通 0000000",
                              "テストショウジ(カ")):
        d.text((160, y + 70 + i * 42), line, fill="black", font=f_body)

    d.text((140, y + 240), "備考：本書はテスト用に生成した架空の帳票です。",
           fill="black", font=f_small)
    d.text((140, y + 280), "実在の企業・取引とは一切関係ありません。",
           fill="black", font=f_small)
    return im, boxes


def main() -> int:
    ap = argparse.ArgumentParser(description="テスト帳票の生成")
    ap.add_argument("--count", type=int, default=20, help="生成する帳票の枚数")
    ap.add_argument("--partners", type=int, default=200, help="取引先マスタの件数")
    ap.add_argument("--seed", type=int, default=20260810)
    ap.add_argument("--masters-only", action="store_true",
                    help="マスタだけ作る（第4段階の1万件生成用）")
    a = ap.parse_args()

    OUT.mkdir(parents=True, exist_ok=True)
    masters = make_masters(a.partners, a.seed)
    (OUT / "masters.json").write_text(
        json.dumps(masters, ensure_ascii=False, indent=1), encoding="utf-8")
    print(f"  取引先マスタ {len(masters)}件  "
          f"（表記揺れ計 {sum(len(m['variants']) for m in masters)}通り）")
    if a.masters_only:
        return 0

    clean = OUT / "clean"
    clean.mkdir(exist_ok=True)
    rnd = random.Random(a.seed + 1)
    for i in range(1, a.count + 1):
        m = rnd.choice(masters)
        rows = []
        for _ in range(rnd.randint(6, 14)):
            qty, unit = rnd.randint(1, 30), rnd.choice([500, 1200, 3000, 8000, 15000])
            rows.append({"item": rnd.choice(ITEMS), "qty": qty,
                         "unit": unit, "amount": qty * unit})
        doc_type = rnd.choice([1, 1, 1, 2, 3])   # 請求書を多めにする
        data = {
            "doc_type": doc_type,
            # 帳票にはマスタの「表記揺れ」の方を書く。正解はマスタのid。
            "partner_name": rnd.choice(m["variants"]),
            "partner_id": m["id"],
            "partner_canonical": m["canonical"],
            "issue_date": f"2026年{rnd.randint(1,12)}月{rnd.randint(1,28)}日",
            "doc_no": f"{doc_type}{i:06d}",
            "rows": rows,
            "total": sum(r["amount"] for r in rows),          # 税抜
            "total_with_tax": sum(r["amount"] for r in rows)
                              + int(sum(r["amount"] for r in rows) * 0.1),
        }
        im, boxes = draw_form(doc_type, data)
        name = f"{['','inv','del','rec'][doc_type]}_{i:04d}"
        im.save(clean / f"{name}.png")
        data["boxes"] = boxes
        (clean / f"{name}.json").write_text(
            json.dumps(data, ensure_ascii=False, indent=1), encoding="utf-8")
        if i % 10 == 0 or i == a.count:
            print(f"  帳票 {i}/{a.count}", flush=True)
    print(f"\n  ✅ {clean} に {a.count}枚（画像＋正解JSON）")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
