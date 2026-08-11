# C++ 画像前処理コアのビルド環境。
#
# OpenCV は apt の Ubuntu 提供版を使い、ソースビルドを避ける。
# ソースから入れると Apple Silicon で数時間かかることがある。
# 今回必要な処理（deskew / denoise / adaptiveThreshold / morphology）は
# 4.6系ですべて利用できるため、実害はない。
#
# レイヤを分けている理由:
#   依存のインストール（重い）と、自前コードのコンパイル（軽い）を分離する。
#   ソースを直しても apt の再取得は起きない。
FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

# ── レイヤ1: 依存。ここはソースを変えても再実行されない ──
RUN apt-get update && apt-get install -y --no-install-recommends \
      build-essential \
      cmake \
      pkg-config \
      libopencv-dev \
      libtesseract-dev \
      libleptonica-dev \
      tesseract-ocr \
      tesseract-ocr-jpn \
      poppler-utils \
    && rm -rf /var/lib/apt/lists/*

# Tesseract の OpenMP を1スレッドに固定する。
#
# 同時に複数枚を処理すると、これが無いと極端に遅くなる。実測（4コアのVM）:
#   同時1本  721 ms
#   同時4本  29,735 ms                    ← 1本あたり 7.4秒
#   同時4本 ＋ OMP_THREAD_LIMIT=1  948 ms  ← 31倍速い
#
# 純粋なCPU負荷は4並列でも劣化しない（1,065ms → 1,166ms）ことを確認済みなので、
# コア数の問題ではなく Tesseract の並列実装の問題。
#
# OMP_NUM_THREADS では直らない。Tesseract が内部で omp_set_num_threads() を
# 呼んで上書きするため。OMP_THREAD_LIMIT はプログラムから上書きできない上限。
#
# 1枚を速くするのではなく、複数枚を同時に捌けるようにする設定。
# 単体でも 721 → 653 ms とわずかに速い。
ENV OMP_THREAD_LIMIT=1

WORKDIR /work

# ── レイヤ2: ここから先はソースを直すたびに再実行される ──
# 実際のビルドは docker compose 経由でマウントして行うため、
# ここではコピーせず、ビルド用のコマンドだけ用意しておく。
CMD ["sleep", "infinity"]
