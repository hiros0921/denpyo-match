#include "matching.hpp"
#include "normalize.hpp"

#include <algorithm>
#include <cmath>
#include <numeric>
#include <unordered_set>

namespace dm {
namespace {

// UTF-8 を文字単位に切る。
//
// バイト単位で編集距離を取ってはいけない。日本語は1文字3バイトなので、
// 1文字違うだけで距離が3になり、長さも3倍に数えられる。
// 「ヤマト商事」と「ヤマダ商事」は1文字違いだが、バイトでは3違う。
std::vector<std::string> chars_of(const std::string& s) {
    std::vector<std::string> out;
    for (size_t i = 0; i < s.size();) {
        const unsigned char c = s[i];
        int len = (c < 0x80) ? 1 : ((c & 0xE0) == 0xC0) ? 2
                : ((c & 0xF0) == 0xE0) ? 3 : ((c & 0xF8) == 0xF0) ? 4 : 1;
        if (i + len > s.size()) len = 1;
        out.push_back(s.substr(i, len));
        i += len;
    }
    return out;
}

}  // namespace

double levenshtein_sim(const std::string& a, const std::string& b) {
    const auto A = chars_of(a), B = chars_of(b);
    if (A.empty() && B.empty()) return 1.0;
    if (A.empty() || B.empty()) return 0.0;

    // 行を2本だけ持つ。1万件×50回まわすので、行列全体は確保しない。
    std::vector<size_t> prev(B.size() + 1), cur(B.size() + 1);
    std::iota(prev.begin(), prev.end(), 0);

    for (size_t i = 1; i <= A.size(); ++i) {
        cur[0] = i;
        for (size_t j = 1; j <= B.size(); ++j) {
            const size_t cost = (A[i - 1] == B[j - 1]) ? 0 : 1;
            cur[j] = std::min({cur[j - 1] + 1, prev[j] + 1, prev[j - 1] + cost});
        }
        prev.swap(cur);
    }
    const double dist = static_cast<double>(prev[B.size()]);
    const double len  = static_cast<double>(std::max(A.size(), B.size()));
    return 1.0 - dist / len;
}

double jaccard_sim(const std::vector<std::string>& a,
                   const std::vector<std::string>& b) {
    if (a.empty() && b.empty()) return 1.0;
    if (a.empty() || b.empty()) return 0.0;
    std::unordered_set<std::string> sa(a.begin(), a.end());
    std::unordered_set<std::string> sb(b.begin(), b.end());
    size_t inter = 0;
    for (const auto& x : sa) if (sb.count(x)) ++inter;
    const size_t uni = sa.size() + sb.size() - inter;
    return uni ? static_cast<double>(inter) / static_cast<double>(uni) : 0.0;
}

// 末尾からの一致。数え方は先頭一致と対称にする。
double suffix_sim(const std::string& a, const std::string& b) {
    const auto A = chars_of(a), B = chars_of(b);
    if (A.empty() || B.empty()) return 0.0;
    size_t n = 0;
    while (n < A.size() && n < B.size() &&
           A[A.size() - 1 - n] == B[B.size() - 1 - n]) ++n;
    return static_cast<double>(n) / static_cast<double>(std::max(A.size(), B.size()));
}

double prefix_sim(const std::string& a, const std::string& b) {
    const auto A = chars_of(a), B = chars_of(b);
    if (A.empty() || B.empty()) return 0.0;
    size_t n = 0;
    while (n < A.size() && n < B.size() && A[n] == B[n]) ++n;
    return static_cast<double>(n) / static_cast<double>(std::max(A.size(), B.size()));
}

double combine(double lev, double jac, double pre, double suf, const Weights& w) {
    const double sum = w.levenshtein + w.jaccard + w.prefix + w.suffix;
    if (sum <= 0.0) return 0.0;
    const double s = (lev * w.levenshtein + jac * w.jaccard +
                      pre * w.prefix + suf * w.suffix) / sum;
    return std::clamp(s * 100.0, 0.0, 100.0);
}

void Index::build(std::vector<Partner> partners) {
    partners_ = std::move(partners);
    by_id_.clear();
    inverted_.clear();
    by_id_.reserve(partners_.size());
    inverted_.reserve(partners_.size() * 4);

    for (uint32_t i = 0; i < partners_.size(); ++i) {
        auto& p = partners_[i];
        if (p.grams.empty()) p.grams = ngrams(p.norm, 2);
        by_id_[p.id] = i;
        // 同じ gram が1件の中に複数回出ても、索引には1回だけ入れる
        std::unordered_set<std::string> uniq(p.grams.begin(), p.grams.end());
        for (const auto& g : uniq) inverted_[g].push_back(i);
    }
}

const Partner* Index::find(int64_t id) const {
    auto it = by_id_.find(id);
    return it == by_id_.end() ? nullptr : &partners_[it->second];
}

std::vector<int64_t> Index::candidates(const std::string& query_norm, int k) const {
    const auto qg = ngrams(query_norm, 2);
    if (qg.empty() || partners_.empty()) return {};

    // 共有 gram の数を数える。転置索引を引くだけなので、
    // 走査するのは「その gram を含むマスタ」だけで済む。
    std::unordered_map<uint32_t, uint32_t> hits;
    hits.reserve(1024);
    std::unordered_set<std::string> uniq(qg.begin(), qg.end());
    for (const auto& g : uniq) {
        auto it = inverted_.find(g);
        if (it == inverted_.end()) continue;
        for (uint32_t idx : it->second) ++hits[idx];
    }
    if (hits.empty()) return {};

    std::vector<std::pair<uint32_t, uint32_t>> v(hits.begin(), hits.end());
    const size_t kk = std::min<size_t>(k, v.size());
    // 全体を並べ替えない。上位 k 件だけ確定させれば足りる。
    std::partial_sort(v.begin(), v.begin() + kk, v.end(),
                      [](const auto& a, const auto& b) { return a.second > b.second; });
    std::vector<int64_t> out;
    out.reserve(kk);
    for (size_t i = 0; i < kk; ++i) out.push_back(partners_[v[i].first].id);
    return out;
}

std::vector<Candidate> Index::search(const std::string& query_norm,
                                     int k, const Weights& w) const {
    const auto qg = ngrams(query_norm, 2);
    // 第1段で少し多めに拾い、第2段で絞る。
    // 第1段は「共有 gram の数」しか見ないので、順位が粗い。
    // ここを k ぴったりにすると、正解が k+1 位に落ちたときに拾えない。
    const auto ids = candidates(query_norm, k * 3);

    std::vector<Candidate> out;
    out.reserve(ids.size());
    for (int64_t id : ids) {
        const Partner* p = find(id);
        if (!p) continue;
        Candidate c;
        c.id = id;
        c.levenshtein = levenshtein_sim(query_norm, p->norm);
        c.jaccard     = jaccard_sim(qg, p->grams);
        c.prefix      = prefix_sim(query_norm, p->norm);
        c.suffix      = suffix_sim(query_norm, p->norm);
        c.score       = combine(c.levenshtein, c.jaccard, c.prefix, c.suffix, w);
        out.push_back(c);
    }
    const size_t kk = std::min<size_t>(k, out.size());
    std::partial_sort(out.begin(), out.begin() + kk, out.end(),
                      [](const Candidate& a, const Candidate& b) {
                          return a.score > b.score;
                      });
    out.resize(kk);
    return out;
}

}  // namespace dm
