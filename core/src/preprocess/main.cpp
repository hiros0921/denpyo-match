// 前処理のCLI。単体で動く（仕様書 第3段階の要件）。
//
//   dm_preprocess in.png -o out.png
//   dm_preprocess in.png -o out.png --sheet steps.png    処理前後を並べた図も出す
//   dm_preprocess in.png --json                          数値だけをJSONで出す
//
// --json は Go 側から呼ぶときに使う。標準出力にJSONだけを出し、
// 進捗やエラーは標準エラーに出す。混ぜると呼び出し側で切り分けられない。
#include "pdf.hpp"
#include "preprocess.hpp"
#include "split.hpp"

#include <opencv2/imgcodecs.hpp>

#include <filesystem>
#include <iostream>
#include <string>
#include <vector>

namespace {

void usage() {
    std::cerr <<
        "使い方: dm_preprocess <入力画像> [オプション]\n"
        "  -o <path>       補正済み画像の出力先\n"
        "  --sheet <path>  処理前後を並べた確認用の画像\n"
        "  --json          数値だけをJSONで標準出力に出す\n"
        "  --split -o <dir> 1枚に複数の伝票があれば切り分けて <dir> に出す\n"
        "  --block <n>     適応的二値化のブロックサイズ（既定31・奇数）\n"
        "  --denoise <f>   ノイズ除去の強さ（既定7.0）\n";
}

}  // namespace

int main(int argc, char** argv) {
    if (argc < 2) { usage(); return 2; }

    std::string in = argv[1], out_path, sheet_path;
    // 向きだけを直して保存する。二値化も罫線除去もしない。
    bool upright_only = false;
    // 1枚の紙に複数の伝票があるとき、伝票ごとに切り出す。
    bool split_mode = false;
    bool as_json = false;
    dm::Options opt;

    for (int i = 2; i < argc; ++i) {
        const std::string a = argv[i];
        auto next = [&](const char* name) -> std::string {
            if (i + 1 >= argc) {
                std::cerr << name << " に値がありません\n";
                std::exit(2);
            }
            return argv[++i];
        };
        if      (a == "--upright") upright_only = true;
        else if (a == "--split")   split_mode = true;
        else if (a == "-o")        out_path   = next("-o");
        else if (a == "--sheet")   sheet_path = next("--sheet");
        else if (a == "--json")    as_json    = true;
        else if (a == "--block")   opt.block_size = std::stoi(next("--block"));
        else if (a == "--denoise") opt.denoise_h  = std::stof(next("--denoise"));
        else { std::cerr << "不明なオプション: " << a << "\n"; usage(); return 2; }
    }

    // ── PDF なら先に画像へ変換する ──
    //
    // OpenCV は PDF を読めない。poppler（pdftoppm）でページごとの画像にする。
    // 複数ページの PDF は1ページ目だけを使う。
    // 【注意】--split 以外の入口では1ページ目だけを使う。
    // ここで勝手に全ページを処理すると、1つの伝票に複数ページの内容が混ざる。
    // 複数ページを何件の伝票にするかを決めるのは --split の役目。
    std::string pdf_why;
    std::string pdf_tmp;
    std::vector<std::string> pdf_pages;
    if (dm::is_pdf(in)) {
        pdf_tmp = (out_path.empty() ? std::string("/tmp") : out_path) + "_pdfpages";
        pdf_pages = dm::pdf_to_images(in, pdf_tmp, split_mode ? 50 : 1, pdf_why);
        if (pdf_pages.empty()) {
            std::cerr << "PDFを画像にできません: " << pdf_why << "\n";
            return 1;
        }
        in = pdf_pages[0];
    }
    if (pdf_pages.empty()) pdf_pages.push_back(in);

    // ── 伝票ごとに切り分ける ──
    //
    // アップロードの受付で1回だけ呼ぶ。ここで「1つのファイル」を
    // 「N件の伝票」に展開してしまえば、そのあとの工程は
    // これまでどおり「1件＝1画像」で動く。
    //
    // 分けるかどうかを後段（ワーカー）で決めると、
    // 既に伝票の番号が振られたあとに枚数が変わることになり、
    // 監査ログとの対応が壊れる。
    if (split_mode) {
        if (out_path.empty()) { std::cerr << "--split には -o <dir> が要ります\n"; return 2; }
        std::error_code ec;
        std::filesystem::create_directories(out_path, ec);

        std::cout << "{\"parts\":[";
        bool first = true;
        for (size_t pi = 0; pi < pdf_pages.size(); ++pi) {
            cv::Mat img = cv::imread(pdf_pages[pi], cv::IMREAD_COLOR);
            if (img.empty()) continue;
            cv::Mat gray;
            cv::cvtColor(img, gray, cv::COLOR_BGR2GRAY);
            const auto regions = dm::split_documents(gray);

            for (size_t ri = 0; ri < regions.size(); ++ri) {
                const auto& g = regions[ri];
                // 切り口ぎりぎりで切らない。文字の端が欠けると読めなくなる。
                const int m = std::max(4, std::min(img.cols, img.rows) / 100);
                cv::Rect r(std::max(0, g.x - m), std::max(0, g.y - m), 0, 0);
                r.width  = std::min(img.cols - r.x, g.w + m * 2);
                r.height = std::min(img.rows - r.y, g.h + m * 2);
                if (r.width <= 0 || r.height <= 0) continue;

                const std::string f = out_path + "/p" + std::to_string(pi + 1) +
                                      "_r" + std::to_string(ri + 1) + ".png";
                if (!cv::imwrite(f, img(r))) {
                    std::cerr << "書き出せません: " << f << "\n";
                    return 1;
                }
                if (!first) std::cout << ",";
                first = false;
                std::cout << "{\"file\":\"" << f << "\",\"page\":" << (pi + 1)
                          << ",\"region\":" << (ri + 1)
                          << ",\"x\":" << r.x << ",\"y\":" << r.y
                          << ",\"w\":" << r.width << ",\"h\":" << r.height << "}";
            }
        }
        std::cout << "]}" << std::endl;
        return 0;
    }

    // ── 向きだけを直す ──
    //
    // 【重要】OCR には元画像を渡す（第9段階で前処理が精度を下げると実測した）が、
    // 「元画像」には EXIF の回転情報が付いていることがある。
    // スマートフォンで撮った写真は、横に倒した状態の画素と
    // 「表示するときは90度回せ」という指示の組み合わせで保存される。
    //
    // cv::imread は EXIF を適用して読むので、前処理側では正しく立っていた。
    // 一方 OCR には元のファイルをそのまま渡していたため、
    // Google Cloud Vision は倒れた画素を受け取っていた。
    //
    // 文字は正しく読める（Visionは向きを理解する）が、
    // 座標は倒れたままの画素座標で返る。実測:
    //   [2024/10/30]  幅62 高さ341   ← 10文字なのに縦長
    // 抽出は横書きを前提に「同じ行」「上部左」「御中の左」を見ているので、
    // 全部外れる。実物の請求書で 取引先名・伝票番号・金額 がすべて誤りになった。
    //
    // 向きを直すだけの工程を分けて持つ。二値化も罫線除去もしない。
    if (upright_only) {
        cv::Mat img = cv::imread(in, cv::IMREAD_COLOR);
        if (img.empty()) { std::cerr << "画像を読めません: " << in << "\n"; return 1; }
        if (out_path.empty()) { std::cerr << "--upright には -o が要ります\n"; return 2; }
        if (!cv::imwrite(out_path, img)) {
            std::cerr << "書き出せません: " << out_path << "\n"; return 1;
        }
        std::cout << "{\"width\":" << img.cols << ",\"height\":" << img.rows << "}"
                  << std::endl;
        return 0;
    }

    cv::Mat src = cv::imread(in, cv::IMREAD_COLOR);
    if (src.empty()) {
        std::cerr << "画像を読めません: " << in << "\n";
        return 1;
    }

    dm::Result r;
    std::string why;
    if (!dm::preprocess(src, opt, r, why)) {
        std::cerr << "前処理に失敗: " << why << "\n";
        return 1;
    }

    if (!out_path.empty() && !cv::imwrite(out_path, r.cleaned)) {
        std::cerr << "書き出せません: " << out_path << "\n";
        return 1;
    }
    if (!sheet_path.empty()) {
        cv::imwrite(sheet_path, dm::make_contact_sheet(r, src));
    }

    if (as_json) {
        // 呼び出し側が読む。余計なものを混ぜない。
        std::cout << "{"
                  << "\"angle_deg\":"     << r.angle_deg      << ","
                  << "\"angle_samples\":" << r.angle_samples  << ","
                  << "\"h_lines\":"       << r.lines.horizontal.size() << ","
                  << "\"v_lines\":"       << r.lines.vertical.size()   << ","
                  << "\"cells\":"         << r.cells.size()   << ","
                  << "\"width\":"         << src.cols         << ","
                  << "\"height\":"        << src.rows
                  << "}" << std::endl;
    } else {
        std::cerr << "  傾き        " << r.angle_deg << " 度"
                  << "（罫線 " << r.angle_samples << " 本から推定）\n"
                  << "  罫線        横 " << r.lines.horizontal.size()
                  << " / 縦 " << r.lines.vertical.size() << "\n"
                  << "  セル        " << r.cells.size() << "\n";
    }
    return 0;
}
