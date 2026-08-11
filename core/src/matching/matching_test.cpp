// 照合エンジンの自己テスト。
//
//   ./build/dm_match --self-test
//
// 性能より先に「正しく当たるか」を確かめる。
// 速いが間違っている実装は、遅いが正しい実装より悪い。
#include "matching.hpp"
#include "normalize.hpp"

#include <iostream>
#include <vector>

namespace {

int g_pass = 0, g_fail = 0;

void ok(bool cond, const std::string& note) {
    if (cond) { ++g_pass; return; }
    ++g_fail;
    std::cout << "  ❌ " << note << "\n";
}

void near(double got, double want, double tol, const std::string& note) {
    if (std::abs(got - want) <= tol) { ++g_pass; return; }
    ++g_fail;
    std::cout << "  ❌ " << note << "  得た値 " << got << " / 期待 " << want << "\n";
}

dm::Partner mk(int64_t id, const std::string& name) {
    dm::Partner p;
    p.id = id;
    p.name = name;
    p.norm = dm::normalize(name);
    p.grams = dm::ngrams(p.norm, 2);
    return p;
}

}  // namespace

int run_match_self_test() {
    std::cout << "照合エンジンの自己テスト\n\n";

    // ── 個別の指標 ──
    near(dm::levenshtein_sim("ヤマト商事", "ヤマト商事"), 1.0, 1e-9, "完全一致");
    near(dm::levenshtein_sim("ヤマト商事", "ヤマダ商事"), 0.8, 1e-9,
         "5文字中1文字違い（バイトでなく文字で数えているか）");
    near(dm::levenshtein_sim("", ""), 1.0, 1e-9, "空文字どうし");
    near(dm::levenshtein_sim("ヤマト", ""), 0.0, 1e-9, "片方が空");

    near(dm::prefix_sim("ヤマト運輸", "ヤマト建設"), 0.6, 1e-9, "先頭3文字が一致");
    near(dm::prefix_sim("ヤマト", "サクラ"), 0.0, 1e-9, "先頭が違う");

    {
        const auto a = dm::ngrams("ヤマト商事", 2);
        near(dm::jaccard_sim(a, a), 1.0, 1e-9, "同じn-gram集合");
        near(dm::jaccard_sim(a, dm::ngrams("サクラ物産", 2)), 0.0, 1e-9,
             "共通のn-gramが無い");
    }

    // ── 索引と検索 ──
    dm::Index idx;
    idx.build({
        mk(1, "株式会社ヤマト商事"),
        mk(2, "ヤマト運輸株式会社"),
        mk(3, "株式会社ヤマダ電機"),
        mk(4, "有限会社サクラ工業"),
        mk(5, "大和証券株式会社"),
    });
    ok(idx.size() == 5, "マスタ5件が入る");

    // 表記揺れが1位で当たること
    for (const char* q : {"ヤマト商事", "(株)ヤマト商事", "㈱ヤマト商事",
                          "ヤマト商事株式会社", "ﾔﾏﾄ商事"}) {
        const auto r = idx.search(dm::normalize(q), 5);
        ok(!r.empty() && r[0].id == 1,
           std::string("表記揺れ [") + q + "] が ヤマト商事 に当たる");
    }

    // 別会社が1位を奪わないこと
    {
        const auto r = idx.search(dm::normalize("ヤマト運輸"), 5);
        ok(!r.empty() && r[0].id == 2, "ヤマト運輸 が ヤマト商事 より上に来る");
    }
    {
        const auto r = idx.search(dm::normalize("ヤマダ電機"), 5);
        ok(!r.empty() && r[0].id == 3, "ヤマダ電機 が ヤマト系より上に来る");
    }

    // 完全一致は満点に近いこと
    {
        const auto r = idx.search(dm::normalize("株式会社ヤマト商事"), 5);
        ok(!r.empty() && r[0].score > 99.0,
           "完全一致のスコアが99を超える");
    }

    // 無関係な名前で高スコアが出ないこと（誤承認の防止）
    {
        const auto r = idx.search(dm::normalize("北海道漁業協同組合"), 5);
        ok(r.empty() || r[0].score < 50.0,
           "無関係な名前のスコアが50未満に収まる");
    }

    // 空のクエリで落ちないこと
    {
        const auto r = idx.search("", 5);
        ok(r.empty(), "空のクエリで候補ゼロ");
    }

    std::cout << "\n  合格 " << g_pass << " / 不合格 " << g_fail << "\n";
    return g_fail == 0 ? 0 : 1;
}
