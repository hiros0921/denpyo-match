#!/usr/bin/env python3
"""生成した帳票を、実際のスキャンに近づけて劣化させる。

    python3 testdata/generator/degrade.py                  # clean/ を全部劣化させる
    python3 testdata/generator/degrade.py --preset light   # 弱い劣化だけ

なぜ必要か。
きれいなPNGに傾き補正をかけても、補正すべき傾きが無いので効果が測れない。
第3段階（傾き補正・ノイズ除去・二値化・罫線除去）を検証するには、
「わざと壊した画像」と「壊す前の正解」の両方が要る。

出力
    samples/scanned/inv_0001.png    劣化後
    samples/scanned/inv_0001.json   かけた劣化の内容（角度・強さ）＋元の正解

第3段階では、この JSON の angle と、前処理が検出した角度を比べる。
「−2.4度に傾けたものを、−2.38度と検出できたか」が数字で出る。

注意: これでも実際のスキャンの汚れ方は再現しきれない。
第9段階の通し検証で出る精度は、実運用より良い値が出ると考えること。
商談で「精度○%」と言う根拠にはできない。
"""
import argparse
import json
import math
import random
from pathlib import Path

import numpy as np
from PIL import Image, ImageDraw, ImageFilter

BASE = Path(__file__).resolve().parent.parent
CLEAN = BASE / "samples" / "clean"
OUT = BASE / "samples" / "scanned"

# 劣化の強さ。実際のスキャン品質の幅を3段階で表す。
PRESETS = {
    "light":  dict(angle=1.0, noise=4,  jpeg=88, shadow=0.06, bleed=0.03,
                   fold=0.3, fade=0.15, blur=0.3),
    "normal": dict(angle=2.5, noise=9,  jpeg=72, shadow=0.14, bleed=0.07,
                   fold=0.6, fade=0.30, blur=0.6),
    "heavy":  dict(angle=4.0, noise=16, jpeg=55, shadow=0.24, bleed=0.13,
                   fold=0.9, fade=0.50, blur=1.0),
}


def rotate(im, deg):
    """傾ける。余白は紙の白で埋める（黒にすると罫線検出が誤作動する）。"""
    return im.rotate(deg, resample=Image.BICUBIC, expand=False, fillcolor=(255, 255, 255))


def add_noise(im, sigma, rnd):
    a = np.asarray(im, dtype=np.int16)
    n = np.asarray(rnd.normalvariate, dtype=object)  # 使わない。下でnumpy乱数を使う
    g = np.random.default_rng(rnd.randint(0, 2**31)).normal(0, sigma, a.shape)
    return Image.fromarray(np.clip(a + g, 0, 255).astype(np.uint8))


def add_shadow(im, strength, rnd):
    """スキャナの蓋が浮いたときの影。片側が暗くなる勾配。"""
    w, h = im.size
    grad = np.zeros((h, w), dtype=np.float32)
    side = rnd.choice(["left", "right", "top", "bottom"])
    ramp = np.linspace(1.0, 0.0, w if side in ("left", "right") else h) ** 1.6
    if side == "left":
        grad = np.tile(ramp, (h, 1))
    elif side == "right":
        grad = np.tile(ramp[::-1], (h, 1))
    elif side == "top":
        grad = np.tile(ramp[:, None], (1, w))
    else:
        grad = np.tile(ramp[::-1, None], (1, w))
    a = np.asarray(im, dtype=np.float32)
    return Image.fromarray(
        np.clip(a * (1.0 - strength * grad[:, :, None]), 0, 255).astype(np.uint8))


def add_bleed(im, other, strength):
    """裏写り。裏面の帳票が薄く透ける。左右反転して重ねる。"""
    b = other.resize(im.size).transpose(Image.FLIP_LEFT_RIGHT)
    a = np.asarray(im, dtype=np.float32)
    c = np.asarray(b, dtype=np.float32)
    # 裏面の「黒い部分」だけが薄く出る
    return Image.fromarray(
        np.clip(a - (255 - c) * strength, 0, 255).astype(np.uint8))


def add_fold(im, strength, rnd):
    """折り目。紙を折った線が影として残る。"""
    w, h = im.size
    d = ImageDraw.Draw(im, "RGBA")
    if rnd.random() < 0.5:
        y = rnd.randint(int(h * 0.3), int(h * 0.7))
        for dy, al in ((-1, 0.35), (0, 1.0), (1, 0.35)):
            d.line([(0, y + dy), (w, y + dy)],
                   fill=(0, 0, 0, int(60 * strength * al)), width=2)
    else:
        x = rnd.randint(int(w * 0.3), int(w * 0.7))
        for dx, al in ((-1, 0.35), (0, 1.0), (1, 0.35)):
            d.line([(x + dx, 0), (x + dx, h)],
                   fill=(0, 0, 0, int(60 * strength * al)), width=2)
    return im


def add_fade(im, strength, rnd):
    """かすれ。トナー切れやインク不足で部分的に薄くなる。"""
    w, h = im.size
    mask = Image.new("L", (w // 8, h // 8), 0)
    md = ImageDraw.Draw(mask)
    for _ in range(rnd.randint(2, 5)):
        cx, cy = rnd.randint(0, w // 8), rnd.randint(0, h // 8)
        r = rnd.randint(w // 40, w // 12)
        md.ellipse([cx - r, cy - r, cx + r, cy + r], fill=int(255 * strength))
    mask = mask.resize((w, h)).filter(ImageFilter.GaussianBlur(30))
    a = np.asarray(im, dtype=np.float32)
    m = np.asarray(mask, dtype=np.float32)[:, :, None] / 255.0
    # 白を乗せる＝薄くする
    return Image.fromarray(np.clip(a + (255 - a) * m, 0, 255).astype(np.uint8))


def degrade_one(path: Path, others: list, preset: str, rnd: random.Random) -> dict:
    p = PRESETS[preset]
    im = Image.open(path).convert("RGB")
    applied = {"preset": preset}

    angle = round(rnd.uniform(-p["angle"], p["angle"]), 3)
    im = rotate(im, angle)
    applied["angle"] = angle          # ★第3段階はこの値を当てにいく

    if p["blur"] > 0:
        r = round(rnd.uniform(0, p["blur"]), 2)
        im = im.filter(ImageFilter.GaussianBlur(r))
        applied["blur"] = r

    if p["shadow"] > 0:
        s = round(rnd.uniform(p["shadow"] * 0.4, p["shadow"]), 3)
        im = add_shadow(im, s, rnd)
        applied["shadow"] = s

    if p["bleed"] > 0 and others:
        s = round(rnd.uniform(p["bleed"] * 0.4, p["bleed"]), 3)
        im = add_bleed(im, Image.open(rnd.choice(others)).convert("RGB"), s)
        applied["bleed"] = s

    if rnd.random() < p["fold"]:
        im = add_fold(im, p["fold"], rnd)
        applied["fold"] = True

    if p["fade"] > 0 and rnd.random() < 0.6:
        s = round(rnd.uniform(p["fade"] * 0.4, p["fade"]), 3)
        im = add_fade(im, s, rnd)
        applied["fade"] = s

    if p["noise"] > 0:
        im = add_noise(im, p["noise"], rnd)
        applied["noise"] = p["noise"]

    applied["jpeg_quality"] = p["jpeg"]
    return im, applied


def main() -> int:
    ap = argparse.ArgumentParser(description="帳票をスキャン品質に劣化させる")
    ap.add_argument("--preset", choices=[*PRESETS, "mixed"], default="mixed",
                    help="mixed は light/normal/heavy を混ぜる")
    ap.add_argument("--seed", type=int, default=20260810)
    a = ap.parse_args()

    srcs = sorted(CLEAN.glob("*.png"))
    if not srcs:
        print(f"  ❌ {CLEAN} に画像がありません。先に make_forms.py を実行してください")
        return 1
    OUT.mkdir(parents=True, exist_ok=True)
    rnd = random.Random(a.seed)

    counts = {}
    for i, src in enumerate(srcs, 1):
        preset = rnd.choice(list(PRESETS)) if a.preset == "mixed" else a.preset
        counts[preset] = counts.get(preset, 0) + 1
        others = [s for s in srcs if s != src]
        im, applied = degrade_one(src, others, preset, rnd)

        # JPEG圧縮を最後に通す。スキャナの出力はたいてい非可逆圧縮を経る。
        tmp = OUT / f"{src.stem}.jpg"
        im.save(tmp, quality=applied["jpeg_quality"])
        Image.open(tmp).save(OUT / f"{src.stem}.png")
        tmp.unlink()

        truth = json.loads((CLEAN / f"{src.stem}.json").read_text(encoding="utf-8"))
        truth["degradation"] = applied
        (OUT / f"{src.stem}.json").write_text(
            json.dumps(truth, ensure_ascii=False, indent=1), encoding="utf-8")
        if i % 10 == 0 or i == len(srcs):
            print(f"  劣化 {i}/{len(srcs)}", flush=True)

    print(f"\n  内訳: " + " / ".join(f"{k} {v}枚" for k, v in sorted(counts.items())))
    print(f"  ✅ {OUT} に {len(srcs)}枚（画像＋劣化内容つき正解JSON）")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
