# Go の API とワーカーの実行環境。
#
# ── なぜ golang イメージをそのまま使わないか ──
#
# この API は C++ の実行ファイル（dm_preprocess / dm_ocr / dm_match）を
# プロセスとして呼ぶ。呼ばれる側は core/Dockerfile（ubuntu:24.04）で
# ビルドされていて、そこの共有ライブラリに結びついている。実測:
#
#   dm_preprocess → libopencv_imgcodecs.so.406 / libopencv_core.so.406 ...
#   dm_ocr        → libtesseract.so.5 / liblept.so.5
#
# soname のバージョンまで一致している必要があるので、土台を core と
# 同じ ubuntu:24.04 に揃える。golang イメージ（Debian 系）にすると
# OpenCV のバージョンが違い、「実行ファイルはあるのに起動できない」
# という形で失敗する。エラーは C++ 側から出るので原因が分かりにくい。
#
# Go の道具一式だけを公式イメージから持ってくる。これなら
# 土台は core と同一、Go のバージョンは公式のもの、の両方を満たせる。
FROM golang:1.26 AS gotoolchain

FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

COPY --from=gotoolchain /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:/root/go/bin:$PATH

# ── C++ 側の実行に要るもの ──
#
# core/Dockerfile から build-essential / cmake / pkg-config を除いたもの。
# ここでは C++ をコンパイルしない（ビルドは core の仕事）。実行に要る
# 共有ライブラリと、Tesseract の日本語データ、PDF 変換の poppler だけ。
#
# 【注意】-dev パッケージを入れているのは、パッケージ名を推測しないため。
# Ubuntu 24.04 は 64bit time_t 移行でランタイム側の名前に t64 が付く
# ことがあり、当てずっぽうで書くと apt が黙って別物を入れる。
# core と同じ名前を使えば、soname が一致することが確実になる。
RUN apt-get update && apt-get install -y --no-install-recommends \
      libopencv-dev \
      libtesseract-dev \
      libleptonica-dev \
      tesseract-ocr \
      tesseract-ocr-jpn \
      poppler-utils \
      ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Tesseract の OpenMP を1スレッドに固定する。
#
# core/Dockerfile と同じ理由。ワーカーを増やすとここが効く。実測:
#   同時4本                        29,735 ms
#   同時4本 ＋ OMP_THREAD_LIMIT=1      948 ms   ← 31倍速い
#
# 【重要】OCR を起動するのはこのコンテナなので、core 側に書いてあっても
# 効かない。環境変数は子プロセスに引き継がれるため、親であるここに要る。
ENV OMP_THREAD_LIMIT=1

# cgo は使わない。C++ とはプロセス間通信でつないでいるので必要がなく、
# 切っておけば C コンパイラを入れずに済む（このイメージには gcc が無い）。
ENV CGO_ENABLED=0

WORKDIR /work

# ソースはマウントして使う（web/Dockerfile と同じ開発用の作り）。
# 直したら再起動するだけで反映される。
#
# パスはすべてコンテナ内で完結させる。Mac 側で go run していたときは
# 「Go から見たパス」と「C++ から見たパス」を別々に渡す必要があったが、
# 同じコンテナで動かすなら同一でよい。
CMD ["go", "run", "./cmd/dmapi", \
     "-http", ":8080", \
     "-bin", "/core/build", \
     "-masters", "/testdata/samples/masters.json", \
     "-images", "/data/images", "-images-container", "/data/images", \
     "-work", "/data/work", "-work-container", "/data/work"]
