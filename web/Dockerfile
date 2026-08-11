# 開発用の Rails 実行環境。
#
# 本番用は Dockerfile.production（rails new が生成したもの）。
# 分けるのは目的が違うため。開発はソースをマウントして即反映させたい。
# 本番はソースを焼き込み、多段ビルドで小さくしたい。
# 1つのファイルに ARG で両方を詰め込むと、どちらの都合か分からなくなる。
#
# Mac の Ruby は 2.6（OS 同梱）で Rails 8 が動かない。
# C++ も Go も DB もコンテナで動かしているので、ここだけ Mac に依存させない。
#
# Node は入れない。Rails 8 の既定（propshaft + importmap）は Node を必要としない。
FROM ruby:3.3-slim

ENV DEBIAN_FRONTEND=noninteractive \
    BUNDLE_PATH=/usr/local/bundle \
    RAILS_ENV=development

# libpq-dev は pg gem のビルドに要る。
# postgresql-client は psql を使うため（構造をSQLで持つ設定にしているので
# rails の db:* 系が psql を呼ぶ）。
RUN apt-get update && apt-get install -y --no-install-recommends \
      build-essential \
      libpq-dev \
      postgresql-client \
      libyaml-dev \
      git \
      curl \
    && rm -rf /var/lib/apt/lists/*

RUN gem install rails -N

WORKDIR /work

# bash -lc（ログインシェル）にしない。PATH が初期化されて
# /usr/local/bundle/bin が消え、rails が見つからなくなる。
CMD ["bash", "-c", "bin/rails server -b 0.0.0.0 -p 3000"]
