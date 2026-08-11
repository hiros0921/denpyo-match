// PDF をページごとの画像に変換する。
//
// なぜ要るか
//
//   会計事務所が受け取る請求書は PDF が多い。電子帳簿保存法の流れもあって、
//   紙より PDF のほうが増えている。
//   受け取れる形式に PDF を入れていたが、前処理（OpenCV）は PDF を読めず、
//   受け付けたあと3回再試行して打ち切られていた。
//
// なぜ poppler（外部コマンド）か
//
//   OpenCV は PDF を読めない。PDF の描画は独立した大きな問題で、
//   自前で書くものではない。poppler は Linux で標準的に使われている実装で、
//   apt で入る。ライブラリとしてリンクするのではなく pdftoppm を呼ぶのは、
//   壊れた PDF でコマンドが落ちても、こちらのプロセスが巻き込まれないため。
//
// 解像度について
//
//   150dpi にする。72dpi（PDFの既定）では文字が小さすぎて OCR が読めない。
//   300dpi にすると精度は変わらないのにファイルが4倍になり、
//   Vision へ送る時間とメモリが増える。
#include "pdf.hpp"

#include <algorithm>
#include <array>
#include <cctype>
#include <cstdio>
#include <cstdlib>
#include <filesystem>
#include <sstream>
#include <string>
#include <vector>

namespace fs = std::filesystem;

namespace dm {

namespace {

// シェルを経由せずに済むよう、引数は個別に渡す。
// PDF のパスにはファイル名がそのまま入るので、
// シェルに渡すと引用符や記号で壊れる（そして任意のコマンドが動きうる）。
int run(const std::vector<std::string>& args, std::string& why) {
    std::string cmd;
    for (const auto& a : args) {
        // 単引用符で囲み、中の単引用符だけを退避する。
        cmd += " '";
        for (char c : a) {
            if (c == '\'') cmd += "'\\''";
            else cmd += c;
        }
        cmd += "'";
    }
    cmd += " 2>&1";
    FILE* f = popen(cmd.c_str(), "r");
    if (!f) { why = "コマンドを起動できません"; return -1; }
    std::array<char, 512> buf{};
    std::string out;
    while (fgets(buf.data(), static_cast<int>(buf.size()), f)) out += buf.data();
    const int rc = pclose(f);
    if (rc != 0) why = out;
    return rc;
}

}  // namespace

bool is_pdf(const std::string& path) {
    if (path.size() < 4) return false;
    std::string ext = path.substr(path.size() - 4);
    for (auto& c : ext) c = static_cast<char>(::tolower(c));
    if (ext != ".pdf") return false;
    // 拡張子だけを信じない。中身の先頭も見る。
    // 名前が .pdf でも中身が画像、という取り違えは現場で起きる。
    FILE* f = fopen(path.c_str(), "rb");
    if (!f) return false;
    char head[5] = {0};
    const size_t n = fread(head, 1, 4, f);
    fclose(f);
    return n == 4 && std::string(head, 4) == "%PDF";
}

std::vector<std::string> pdf_to_images(const std::string& pdf_path,
                                       const std::string& out_dir,
                                       int max_pages,
                                       std::string& why) {
    std::vector<std::string> out;
    std::error_code ec;
    fs::create_directories(out_dir, ec);

    const std::string prefix = out_dir + "/page";

    std::vector<std::string> args{
        "pdftoppm", "-png",
        // 150dpi。72では文字が小さすぎて読めず、300では無駄に重い。
        "-r", "150",
        "-f", "1",
        "-l", std::to_string(max_pages),
        pdf_path, prefix};

    if (run(args, why) != 0) {
        if (why.empty()) why = "PDFを画像に変換できません";
        return out;
    }

    // pdftoppm は page-1.png / page-01.png のように、
    // 総ページ数に応じて桁数を変える。決め打ちせず、実際にできたものを拾う。
    std::vector<fs::path> found;
    for (const auto& e : fs::directory_iterator(out_dir, ec)) {
        if (!e.is_regular_file()) continue;
        const auto name = e.path().filename().string();
        if (name.rfind("page", 0) == 0 && e.path().extension() == ".png")
            found.push_back(e.path());
    }
    // 名前順に並べる。桁数が揃っているので文字列比較で正しい順になる。
    std::sort(found.begin(), found.end());
    for (const auto& p : found) out.push_back(p.string());

    if (out.empty()) why = "PDFからページを取り出せませんでした";
    return out;
}

}  // namespace dm
