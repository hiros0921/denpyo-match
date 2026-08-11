// 名寄せ・照合エンジン。
//
// 二段構え。総当たりはしない。
//   第1段  候補生成    n-gram の転置索引で、1万件から上位50件に絞る
//   第2段  精密採点    絞った候補にだけ、編集距離とn-gram類似度をかける
//
// 実比較は 1万件 × 50 = 50万回 で、1億回にはならない。
//
// 本番では第1段を PostgreSQL の pg_trgm に任せる選択肢もある（実測 1.123ms）。
// ここで C++ 側にも索引を持つ理由は2つ:
//   ① バッチ処理でマスタをメモリに載せたまま1万件を流すとき、
//      DBへの往復1万回が支配的になる
//   ② DBに依存しない CLI として単体で動かせる（仕様書の要件）
#pragma once

#include <cstdint>
#include <string>
#include <unordered_map>
#include <vector>

namespace dm {

// 取引先マスタの1件。norm と grams は登録時に計算して持っておく。
// 照合のたびに正規化してはいけない（1万件に毎回かけると目標を使い切る）。
struct Partner {
    int64_t     id = 0;
    std::string name;    // 表示用の正式名称
    std::string norm;    // 正規化済み
    std::vector<std::string> grams;   // 2-gram
};

struct Candidate {
    int64_t id = 0;
    double  score = 0.0;      // 0〜100
    double  levenshtein = 0.0;  // 内訳。どの指標が効いたかを後から追える
    double  jaccard = 0.0;
    double  prefix = 0.0;
    double  suffix = 0.0;
};

struct Weights {
    // 実測で決めた値。以下は 5,000件 × マスタ384件（同じ頭を持つ紛らわしい
    // 組を意図的に含む）・OCR誤読50% の条件で測った正解率。
    //
    //   lev / jac / pre        1位      5位以内
    //   0.5 / 0.3 / 0.2       98.00%    100.00%   ← 採用
    //   1.0 / 0.0 / 0.0       97.92%    100.00%
    //   0.5 / 0.4 / 0.1       98.04%    100.00%
    //   0.0 / 1.0 / 0.0       92.56%     96.82%
    //   0.0 / 0.0 / 1.0       59.76%     65.24%   ← 先頭一致だけでは使えない
    //
    // 上位の配分どうしの差は 0.12ポイント（5,000件中6件）で測定誤差の範囲。
    // 編集距離が主役で、n-gram が補助。この2つが効いている。
    //
    // 【設計根拠の訂正】
    // 当初「日本語の企業名は先頭が識別子として働く」と考えて先頭一致に
    // 重みを置いたが、実測すると逆だった。日本語の企業名で識別しているのは
    // 末尾（運輸／建設／商事／電機）で、先頭は同系列の会社が共有する。
    //
    //   ヤマト運輸 で照会したときの先頭一致
    //     ヤマト開発 0.6 / ヤマト電機 0.6 / ヤマト商会 0.6  ← 全部別会社
    //
    // 先頭一致は「同じ頭を持つ別会社を混同させる」方向に働く。だから単独では
    // 59.76% しか出ない。重みを 0.2 に留めているのはこのため。ゼロにしないのは、
    // score_detail に内訳を残して「なぜこの候補が上位に来たか」を
    // 承認画面で人が読めるようにするため。
    double levenshtein = 0.5;
    double jaccard     = 0.3;
    // 先頭一致と末尾一致を半々にする（第9段階で 0.2/0.0 から変更）。
    //
    // 【変更の根拠】100枚での実測
    //
    // ① 先頭一致だけだと、OCRが先頭を1文字壊しただけで 0.000 になる。
    //    重み0.2＝20点が丸ごと消える。
    //      バハルタ商事87 vs ハルタ商事87
    //        編集距離 0.875 / n-gram 0.857 / 先頭一致 0.000 → 合計 69.5
    //    下限70に0.5点届かず却下された。半々にすると届く。
    //
    // ② 第4段階で「日本語の企業名を識別しているのは末尾
    //    （運輸／建設／商事／電機）」と実測しながら、指標は先頭のままだった。
    //    重みを下げただけで中身を変えていなかった。
    //
    // ③ 別会社との差が広がることを確認した。
    //      ヤマト運輸株式会社 で照会（正解は100点のまま）
    //        先頭のみ    別会社 52.0 点（差 48点）
    //        半々        別会社 46.0 点（差 54点）
    //
    // ④ 100枚の総当たりでは、重みを変えても1位は変わらなかった（79件すべて正解）。
    //    変わるのは要確認と却下の分かれ目だけで、この配分が最も却下が少ない。
    //      0.2/0.0  要確認15 / 却下2
    //      0.1/0.1  要確認17 / 却下0
    double prefix      = 0.1;
    double suffix      = 0.1;
};
// 索引つきのマスタ。build に時間をかけ、検索を速くする。
class Index {
public:
    // マスタを取り込み、n-gram の転置索引を作る。
    void build(std::vector<Partner> partners);

    // 第1段：候補を絞る。共有する n-gram の数が多い順に上位 k 件。
    std::vector<int64_t> candidates(const std::string& query_norm, int k = 50) const;

    // 第1段＋第2段。スコア付きで返す。上位 k 件。
    std::vector<Candidate> search(const std::string& query_norm,
                                  int k = 50, const Weights& w = {}) const;

    size_t size() const { return partners_.size(); }
    const Partner& at(size_t i) const { return partners_[i]; }
    const Partner* find(int64_t id) const;

private:
    std::vector<Partner> partners_;
    std::unordered_map<int64_t, size_t> by_id_;
    // n-gram → その gram を含むマスタの添字
    std::unordered_map<std::string, std::vector<uint32_t>> inverted_;
};

// ── 個別の指標。単体でテストできるように外に出す ──

// 正規化レーベンシュタイン距離を 0〜1 の類似度にしたもの（1が完全一致）
double levenshtein_sim(const std::string& a, const std::string& b);

// 2-gram の Jaccard 係数
double jaccard_sim(const std::vector<std::string>& a,
                   const std::vector<std::string>& b);

// 先頭が何文字一致するか / 長い方の文字数
double prefix_sim(const std::string& a, const std::string& b);

// 末尾からの一致。
//
// なぜ要るか（第9段階の実測）
//   OCRは先頭を壊すことがある。「バハルタ商事87」のように1文字違うだけで、
//   先頭一致は 0.000 になり、重み0.2＝20点が丸ごと失われる。
//     バハルタ商事87 vs ハルタ商事87
//       編集距離 0.875 / n-gram 0.857 / 先頭一致 0.000 → 合計 69.5
//   下限70に0.5点届かず却下された。
//
//   第4段階で「日本語の企業名を識別しているのは末尾（運輸／建設／商事）」と
//   実測していたのに、指標は先頭のままだった。重みを下げただけで
//   中身を変えていなかった。
double suffix_sim(const std::string& a, const std::string& b);

// 3つを重み付き合成して 0〜100 にする
double combine(double lev, double jac, double pre, double suf, const Weights& w);

}  // namespace dm
