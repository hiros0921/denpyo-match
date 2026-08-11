// OCR。エンジンを差し替えられる構造にする。
//
// 実装は2つ用意する。1つしか無ければ「差し替え可能」を証明できない。
//   Tesseract   ローカル完結。外部にデータを送らない
//   Vision      Google Cloud Vision。精度優先（Go側で実装。第6段階）
//
// 会計事務所は機密性を強く気にする。通常は Vision、機密性を重視する顧問先だけ
// Tesseract、という選択肢を持てることが商談上の武器になる。
// 選択は顧問先（clients）単位。組織単位だと「この顧問先だけローカル完結」ができない。
#pragma once

#include <memory>
#include <string>
#include <vector>

namespace dm {

// OCRが読んだ1つの塊。単語・行・段落のどれでもありうる。
// 座標を必ず持つ。承認画面で「この項目はここに書いてある」と示すために要る。
struct TextBlock {
    std::string text;
    double confidence = 0.0;   // 0〜1
    int x = 0, y = 0, w = 0, h = 0;
};

struct OcrResult {
    std::vector<TextBlock> blocks;
    std::string engine;
    double cost_yen = 0.0;     // このページの実費。保守料金の根拠になる
    int    ms = 0;
};

// エンジンの共通インターフェース。
class OcrEngine {
public:
    virtual ~OcrEngine() = default;
    virtual std::string name() const = 0;

    // 画像ファイルを読んでテキストと座標を返す。
    // 失敗したら例外ではなく false。理由は why に入れる。
    virtual bool recognize(const std::string& image_path,
                           OcrResult& out, std::string& why) = 0;

    // 1ページあたりの費用。ローカル完結なら 0 を返す。
    // インターフェースに含めるのは、処理のたびに記録して
    // 保守料金の根拠を自動で貯めるため。
    virtual double cost_per_page_yen() const = 0;
};

// Tesseract の実装を作る。日本語データが入っていなければ nullptr。
std::unique_ptr<OcrEngine> make_tesseract(const std::string& lang = "jpn+eng");

}  // namespace dm
