// 正規化の自己テスト。外部ライブラリは使わない。
//
//   ./build/dm_normalize --self-test
//
// 表記揺れが同じ文字列に落ちることを確認する。
// ここが崩れると照合が根本から狂うので、変更のたびに必ず通すこと。
#include "normalize.hpp"

#include <iomanip>
#include <iostream>
#include <string>
#include <vector>

namespace {

int g_pass = 0, g_fail = 0;

void eq(const std::string& got, const std::string& want, const std::string& note) {
    if (got == want) { ++g_pass; return; }
    ++g_fail;
    std::cout << "  ❌ " << note << "\n"
              << "       得た結果: [" << got << "]\n"
              << "       期待:     [" << want << "]\n";
}

// 同じグループの文字列が、すべて同じ正規形になることを確かめる
void same_group(const std::vector<std::string>& xs, const std::string& note) {
    if (xs.empty()) return;
    const std::string base = dm::normalize(xs[0]);
    for (size_t i = 1; i < xs.size(); ++i) {
        eq(dm::normalize(xs[i]), base, note + " : " + xs[i] + " と " + xs[0]);
    }
}

// 別物として残るべきものが、同じにならないことを確かめる
void differ(const std::string& a, const std::string& b, const std::string& note) {
    if (dm::normalize(a) != dm::normalize(b)) { ++g_pass; return; }
    ++g_fail;
    std::cout << "  ❌ " << note << "\n"
              << "       [" << a << "] と [" << b << "] が同じになってしまった\n"
              << "       どちらも [" << dm::normalize(a) << "]\n";
}

}  // namespace

int run_self_test() {
    std::cout << "正規化の自己テスト\n\n";

    // ── 法人格の書き方違いは同じに落ちる ──
    same_group({"株式会社ヤマト商事", "ヤマト商事株式会社", "(株)ヤマト商事",
                "ヤマト商事(株)", "㈱ヤマト商事", "ヤマト商事㈱", "ヤマト商事"},
               "株式会社の書き方違い");
    same_group({"有限会社サクラ工業", "サクラ工業有限会社", "(有)サクラ工業",
                "㈲サクラ工業", "サクラ工業"},
               "有限会社の書き方違い");

    // ── 全角半角・空白・記号 ──
    same_group({"ＡＢＣ商事", "ABC商事", "ａｂｃ商事", "abc商事"}, "全角半角と大小文字");
    same_group({"ヤマト 商事", "ヤマト　商事", "ヤマト商事"}, "空白の有無");
    same_group({"ヤマト・商事", "ヤマト商事", "ヤマト-商事", "ヤマト＆商事"},
               "記号の有無");

    // ── かな・カナ ──
    same_group({"やまと商事", "ヤマト商事"}, "ひらがなとカタカナ");
    same_group({"ﾔﾏﾄ商事", "ヤマト商事"}, "半角カナ");
    same_group({"ﾊﾞﾝﾀﾞｲ", "バンダイ"}, "半角カナの濁点合成");
    same_group({"ﾊﾟﾝ工房", "パン工房"}, "半角カナの半濁点合成");

    // ── 旧字体 ──
    same_group({"髙島商事", "高島商事"}, "髙→高");
    same_group({"山﨑物産", "山崎物産"}, "﨑→崎");
    same_group({"渡邊工業", "渡辺工業"}, "邊→辺");
    same_group({"櫻井商会", "桜井商会"}, "櫻→桜");

    // ── 別物は別物のまま残る（過剰統合の検出） ──
    differ("ヤマト運輸", "ヤマト建設", "別の会社");
    differ("ヤマト商事", "ヤマダ商事", "1文字違いの別会社");
    differ("東京商事", "京東商事", "並び替え");

    // ── 法人格の種類は覚えている ──
    // 株式会社と有限会社は別法人。正規化では同じになるが、種類で区別できる。
    {
        const auto a = dm::detect_corp("株式会社ヤマト");
        const auto b = dm::detect_corp("有限会社ヤマト");
        if (a == dm::CorpKind::Kabushiki && b == dm::CorpKind::Yugen) ++g_pass;
        else {
            ++g_fail;
            std::cout << "  ❌ 法人格の判別: 株[" << dm::corp_name(a)
                      << "] 有[" << dm::corp_name(b) << "]\n";
        }
        if (dm::normalize("株式会社ヤマト") == dm::normalize("有限会社ヤマト")) ++g_pass;
        else { ++g_fail; std::cout << "  ❌ 法人格を除いた核が一致しない\n"; }
    }

    // ── 長いものから消す（部分一致の事故） ──
    eq(dm::normalize("一般社団法人ヤマト"), dm::normalize("ヤマト"),
       "一般社団法人を丸ごと消す");
    eq(dm::normalize("特定非営利活動法人サクラ"), dm::normalize("サクラ"),
       "特定非営利活動法人を丸ごと消す");

    // ── n-gram ──
    {
        const auto g = dm::ngrams(dm::normalize("ヤマト商事"), 2);
        // ヤマ マト ト商 商事 の4つ
        if (g.size() == 4 && g[0] == "ヤマ" && g[3] == "商事") ++g_pass;
        else {
            ++g_fail;
            std::cout << "  ❌ 2-gram: " << g.size() << "個 [";
            for (const auto& x : g) std::cout << x << " ";
            std::cout << "]\n";
        }
    }

    std::cout << "\n  合格 " << g_pass << " / 不合格 " << g_fail << "\n";
    return g_fail == 0 ? 0 : 1;
}
