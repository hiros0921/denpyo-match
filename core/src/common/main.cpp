// 正規化のCLI。単体で動く。
//
//   dm_normalize --self-test              自己テストを走らせる
//   dm_normalize "株式会社ヤマト商事"       1件だけ正規化して表示
//   dm_normalize --stdin                  標準入力から1行ずつ（Go から呼ぶとき）
#include "normalize.hpp"

#include <iostream>
#include <string>

int run_self_test();

int main(int argc, char** argv) {
    if (argc < 2) {
        std::cerr << "使い方: dm_normalize [--self-test | --stdin | <文字列>]\n";
        return 2;
    }
    const std::string a1 = argv[1];

    if (a1 == "--self-test") return run_self_test();

    if (a1 == "--stdin") {
        std::string line;
        while (std::getline(std::cin, line)) {
            std::cout << dm::normalize(line) << "\n";
        }
        return 0;
    }

    const std::string norm = dm::normalize(a1);
    std::cout << "  入力    " << a1 << "\n"
              << "  正規形  " << norm << "\n"
              << "  法人格  " << dm::corp_name(dm::detect_corp(a1)) << "\n"
              << "  2-gram  ";
    for (const auto& g : dm::ngrams(norm, 2)) std::cout << g << " ";
    std::cout << "\n";
    return 0;
}
