#include "ocr.hpp"

#include <tesseract/baseapi.h>
#include <leptonica/allheaders.h>

#include <chrono>
#include <cstring>
#include <memory>

namespace dm {
namespace {

class TesseractEngine : public OcrEngine {
public:
    explicit TesseractEngine(std::string lang) : lang_(std::move(lang)) {}

    ~TesseractEngine() override {
        if (api_) { api_->End(); delete api_; }
    }

    bool init(std::string& why) {
        api_ = new tesseract::TessBaseAPI();
        if (api_->Init(nullptr, lang_.c_str())) {
            why = "Tesseract を初期化できません（言語データ " + lang_ + " が無い）";
            delete api_; api_ = nullptr;
            return false;
        }
        // 帳票は段組みではなく、表と行の集合。自動段組み検出は誤作動しやすい。
        api_->SetPageSegMode(tesseract::PSM_AUTO);
        return true;
    }

    std::string name() const override { return "tesseract"; }

    // ローカル完結なので費用はゼロ。
    // これが「外部にデータを送らない構成も選べます」という商談上の武器になる。
    double cost_per_page_yen() const override { return 0.0; }

    bool recognize(const std::string& path, OcrResult& out,
                   std::string& why) override {
        if (!api_) { why = "初期化されていません"; return false; }

        const auto t0 = std::chrono::steady_clock::now();
        Pix* img = pixRead(path.c_str());
        if (!img) { why = "画像を読めません: " + path; return false; }

        api_->SetImage(img);
        if (api_->Recognize(nullptr) != 0) {
            pixDestroy(&img);
            why = "認識に失敗しました";
            return false;
        }

        out.blocks.clear();
        out.engine = name();
        out.cost_yen = 0.0;

        // 単語単位で取る。行単位だと座標が粗すぎて、承認画面で
        // 「この項目はここ」と指せない。
        std::unique_ptr<tesseract::ResultIterator> it(api_->GetIterator());
        const auto level = tesseract::RIL_WORD;
        if (it) {
            do {
                const char* w = it->GetUTF8Text(level);
                if (!w) continue;
                const std::string text = w;
                delete[] w;
                if (text.find_first_not_of(" \t\r\n") == std::string::npos) continue;

                TextBlock b;
                b.text = text;
                b.confidence = it->Confidence(level) / 100.0;
                int x1, y1, x2, y2;
                it->BoundingBox(level, &x1, &y1, &x2, &y2);
                b.x = x1; b.y = y1; b.w = x2 - x1; b.h = y2 - y1;
                out.blocks.push_back(std::move(b));
            } while (it->Next(level));
        }

        pixDestroy(&img);
        out.ms = static_cast<int>(std::chrono::duration_cast<std::chrono::milliseconds>(
            std::chrono::steady_clock::now() - t0).count());
        return true;
    }

private:
    std::string lang_;
    tesseract::TessBaseAPI* api_ = nullptr;
};

}  // namespace

std::unique_ptr<OcrEngine> make_tesseract(const std::string& lang) {
    auto e = std::make_unique<TesseractEngine>(lang);
    std::string why;
    if (!e->init(why)) {
        // 静かに nullptr を返さない。呼び出し側が原因を知れないと、
        // 「OCRが動かない」としか分からず調べようがない。
        fprintf(stderr, "  Tesseract の初期化に失敗: %s\n", why.c_str());
        return nullptr;
    }
    return e;
}

}  // namespace dm
