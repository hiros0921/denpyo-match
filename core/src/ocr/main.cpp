// OCR＋項目抽出のCLI。単体で動く（仕様書 第5段階の要件）。
//
//   dm_ocr image.png                 人が読む形で出す
//   dm_ocr image.png --json          機械が読む形で出す（Go から呼ぶとき）
//   dm_ocr image.png --engine tesseract
//
// --json は標準出力にJSONだけを出す。進捗やエラーは標準エラーへ。
// 混ぜると呼び出し側で切り分けられない。
#include "extract.hpp"
#include "ocr.hpp"

#include <iostream>
#include <sstream>
#include <string>

namespace {

std::string esc(const std::string& s) {
    std::string o;
    for (char c : s) {
        switch (c) {
            case '"':  o += "\\\""; break;
            case '\\': o += "\\\\"; break;
            case '\n': o += "\\n";  break;
            case '\t': o += "\\t";  break;
            default:
                if (static_cast<unsigned char>(c) < 0x20) continue;
                o += c;
        }
    }
    return o;
}

}  // namespace

// 標準入力からブロックを読み、項目の抽出だけを行う。
//
// なぜ要るか
//
//   OCRのエンジンは2つある（Tesseract と Google Cloud Vision）。
//   Vision は Go 側から呼ぶが、読んだ結果から
//   「どれが取引先名か・日付か・金額か」を決める処理は同じであるべき。
//
//   ここを Go 側にもう1つ書くと、実装が2つになる。
//   取引先名の決め方には「御中の左」「上部左」「法人格を含む」といった
//   細かい規則が積み重なっていて、片方だけ直せば必ず食い違う。
//   食い違っても例外は出ない。エンジンを変えた顧問先だけ精度が落ちる、
//   という形でしか表面化しないので、気付くのが最も遅れる。
//
//   抽出は1つに保ち、ブロックを渡してもらう形にする。
//
//   入力（標準入力・JSON）
//     {"blocks":[{"text":"...","confidence":0.9,"x":1,"y":2,"w":3,"h":4}, ...]}
int run_extract_only() {
    std::stringstream ss;
    ss << std::cin.rdbuf();
    const std::string body = ss.str();

    dm::OcrResult ocr;
    ocr.engine = "external";

    // 外部のJSONライブラリは入れない方針なので、必要な項目だけ拾う。
    // ブロックは "text" ごとに1つ。数値は続く4項目を順に読む。
    size_t pos = 0;
    while ((pos = body.find("\"text\"", pos)) != std::string::npos) {
        const size_t q1 = body.find('"', body.find(':', pos) + 1);
        if (q1 == std::string::npos) break;
        // 終端の引用符を探す。エスケープされたものは飛ばす。
        size_t q2 = q1 + 1;
        std::string text;
        while (q2 < body.size()) {
            if (body[q2] == '\\' && q2 + 1 < body.size()) {
                const char c = body[q2 + 1];
                text += (c == 'n') ? '\n' : (c == 't') ? '\t' : c;
                q2 += 2;
                continue;
            }
            if (body[q2] == '"') break;
            text += body[q2++];
        }
        dm::TextBlock b;
        b.text = text;
        auto num = [&](const char* key, double dflt) {
            const size_t k = body.find(std::string("\"") + key + "\"", q2);
            // 次のブロックの手前までに無ければ既定値
            const size_t nextText = body.find("\"text\"", q2);
            if (k == std::string::npos || (nextText != std::string::npos && k > nextText))
                return dflt;
            const size_t c = body.find(':', k);
            if (c == std::string::npos) return dflt;
            try { return std::stod(body.substr(c + 1, 32)); } catch (...) { return dflt; }
        };
        b.confidence = num("confidence", 0.0);
        b.x = static_cast<int>(num("x", 0));
        b.y = static_cast<int>(num("y", 0));
        b.w = static_cast<int>(num("w", 0));
        b.h = static_cast<int>(num("h", 0));
        ocr.blocks.push_back(std::move(b));
        pos = q2;
    }

    if (ocr.blocks.empty()) {
        std::cerr << "ブロックが1件もありません\n";
        return 1;
    }

    int pw = 0, ph = 0;
    for (const auto& b : ocr.blocks) {
        pw = std::max(pw, b.x + b.w);
        ph = std::max(ph, b.y + b.h);
    }
    const auto ex = dm::extract(ocr, pw, ph);

    std::cout << "{\"blocks\":" << ocr.blocks.size() << ",\"fields\":{";
    bool first = true;
    for (const auto& f : ex.fields) {
        if (!first) std::cout << ",";
        first = false;
        std::cout << "\"" << dm::field_key_name(f.key) << "\":{"
                  << "\"value\":\"" << esc(f.value) << "\""
                  << ",\"confidence\":" << f.confidence
                  << ",\"why\":\"" << esc(f.why) << "\""
                  << ",\"bbox\":[" << f.x << "," << f.y << ","
                  << f.w << "," << f.h << "]}";
    }
    std::cout << "}}" << std::endl;
    return 0;
}

int main(int argc, char** argv) {
    if (argc < 2) {
        std::cerr << "使い方: dm_ocr <画像> [--json] [--engine tesseract]\n"
                     "        dm_ocr --extract   … 標準入力のブロックから項目だけ抽出\n";
        return 2;
    }
    // --extract は画像を取らない。標準入力からブロックを受ける。
    for (int i = 1; i < argc; ++i)
        if (std::string(argv[i]) == "--extract") return run_extract_only();

    std::string path = argv[1], engine = "tesseract";
    bool as_json = false;
    for (int i = 2; i < argc; ++i) {
        const std::string a = argv[i];
        if (a == "--json") as_json = true;
        else if (a == "--engine" && i + 1 < argc) engine = argv[++i];
        else { std::cerr << "不明なオプション: " << a << "\n"; return 2; }
    }

    // 【重要】受け取った engine を実際に見る。
    //
    // 以前は --engine を受け取るだけで、中身を一切見ていなかった。
    // vision を渡しても黙って Tesseract が動く。
    // 顧問先が「精度優先で Vision」と設定していても、実際は Tesseract。
    // 設定と動作が食い違ったまま、記録には tesseract と残る。
    // 気付く手がかりが無いのが最悪で、「設定したのに効かない」という
    // 形でしか表面化しない。
    //
    // このCLIは Tesseract 専用にする。Vision は Go 側から呼ぶ
    // （Google の SDK を C++ に持ち込むと、この実行ファイルが
    //  ネットワークと認証情報を扱うことになる。分けたほうが安全）。
    if (engine != "tesseract") {
        std::cerr << "このコマンドは tesseract のみ対応しています（指定: "
                  << engine << "）\n"
                     "Vision は API 側（Go）から呼びます。\n";
        return 2;
    }

    auto eng = dm::make_tesseract("jpn+eng");
    if (!eng) return 1;

    dm::OcrResult ocr;
    std::string why;
    if (!eng->recognize(path, ocr, why)) {
        std::cerr << "OCRに失敗: " << why << "\n";
        return 1;
    }

    // ページの大きさはブロックの外接から求める。画像を再度読み込まない。
    int pw = 0, ph = 0;
    for (const auto& b : ocr.blocks) {
        pw = std::max(pw, b.x + b.w);
        ph = std::max(ph, b.y + b.h);
    }
    const auto ex = dm::extract(ocr, pw, ph);

    if (as_json) {
        std::cout << "{\"engine\":\"" << esc(ocr.engine) << "\""
                  << ",\"ms\":" << ocr.ms
                  << ",\"cost_yen\":" << eng->cost_per_page_yen()
                  << ",\"blocks\":" << ocr.blocks.size()
                  << ",\"fields\":{";
        bool first = true;
        for (const auto& f : ex.fields) {
            if (!first) std::cout << ",";
            first = false;
            std::cout << "\"" << dm::field_key_name(f.key) << "\":{"
                      << "\"value\":\"" << esc(f.value) << "\""
                      << ",\"confidence\":" << f.confidence
                      << ",\"why\":\"" << esc(f.why) << "\""
                      << ",\"bbox\":[" << f.x << "," << f.y << ","
                      << f.w << "," << f.h << "]}";
        }
        std::cout << "}}" << std::endl;
    } else {
        std::cerr << "  エンジン " << ocr.engine << " / " << ocr.ms << "ms"
                  << " / 1ページ " << eng->cost_per_page_yen() << "円"
                  << " / ブロック " << ocr.blocks.size() << "\n\n";
        for (const auto& f : ex.fields) {
            printf("  %-14s %-28s 信頼度 %.2f   %s\n",
                   dm::field_key_name(f.key), f.value.c_str(),
                   f.confidence, f.why.c_str());
        }
        if (ex.fields.empty()) std::cout << "  項目を抽出できませんでした\n";
    }
    return 0;
}
