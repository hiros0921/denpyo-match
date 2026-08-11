#include "normalize.hpp"

#include <algorithm>
#include <array>
#include <cstdint>
#include <map>

namespace dm {
namespace {

// ── UTF-8 の入出力 ──
// ICU も libiconv も使わない。依存を増やさず、必要な範囲だけ自前で扱う。
// 帳票の取引先名に出る文字は限られる（英数・かな・カナ・漢字・記号）。

std::vector<uint32_t> to_codepoints(const std::string& s) {
    std::vector<uint32_t> out;
    out.reserve(s.size());
    for (size_t i = 0; i < s.size();) {
        const unsigned char c = s[i];
        uint32_t cp; int len;
        if      (c < 0x80)        { cp = c;          len = 1; }
        else if ((c & 0xE0) == 0xC0) { cp = c & 0x1F; len = 2; }
        else if ((c & 0xF0) == 0xE0) { cp = c & 0x0F; len = 3; }
        else if ((c & 0xF8) == 0xF0) { cp = c & 0x07; len = 4; }
        else { ++i; continue; }                       // 不正なバイトは捨てる
        if (i + len > s.size()) break;
        for (int k = 1; k < len; ++k) cp = (cp << 6) | (s[i + k] & 0x3F);
        out.push_back(cp);
        i += len;
    }
    return out;
}

std::string to_utf8(const std::vector<uint32_t>& cps) {
    std::string out;
    out.reserve(cps.size() * 3);
    for (uint32_t cp : cps) {
        if (cp < 0x80) out += static_cast<char>(cp);
        else if (cp < 0x800) {
            out += static_cast<char>(0xC0 | (cp >> 6));
            out += static_cast<char>(0x80 | (cp & 0x3F));
        } else if (cp < 0x10000) {
            out += static_cast<char>(0xE0 | (cp >> 12));
            out += static_cast<char>(0x80 | ((cp >> 6) & 0x3F));
            out += static_cast<char>(0x80 | (cp & 0x3F));
        } else {
            out += static_cast<char>(0xF0 | (cp >> 18));
            out += static_cast<char>(0x80 | ((cp >> 12) & 0x3F));
            out += static_cast<char>(0x80 | ((cp >> 6) & 0x3F));
            out += static_cast<char>(0x80 | (cp & 0x3F));
        }
    }
    return out;
}

// ── 半角カナ → 全角カナ ──
// 濁点・半濁点は独立した文字として来るので、前の文字と合成する。
// これをやらないと「ﾊﾞ」が「ハ」＋「゛」の2文字のまま残り、照合がずれる。
const std::map<uint32_t, uint32_t> kHankakuKana = {
    {0xFF66,0x30F2},{0xFF67,0x30A1},{0xFF68,0x30A3},{0xFF69,0x30A5},
    {0xFF6A,0x30A7},{0xFF6B,0x30A9},{0xFF6C,0x30E3},{0xFF6D,0x30E5},
    {0xFF6E,0x30E7},{0xFF6F,0x30C3},{0xFF70,0x30FC},{0xFF71,0x30A2},
    {0xFF72,0x30A4},{0xFF73,0x30A6},{0xFF74,0x30A8},{0xFF75,0x30AA},
    {0xFF76,0x30AB},{0xFF77,0x30AD},{0xFF78,0x30AF},{0xFF79,0x30B1},
    {0xFF7A,0x30B3},{0xFF7B,0x30B5},{0xFF7C,0x30B7},{0xFF7D,0x30B9},
    {0xFF7E,0x30BB},{0xFF7F,0x30BD},{0xFF80,0x30BF},{0xFF81,0x30C1},
    {0xFF82,0x30C4},{0xFF83,0x30C6},{0xFF84,0x30C8},{0xFF85,0x30CA},
    {0xFF86,0x30CB},{0xFF87,0x30CC},{0xFF88,0x30CD},{0xFF89,0x30CE},
    {0xFF8A,0x30CF},{0xFF8B,0x30D2},{0xFF8C,0x30D5},{0xFF8D,0x30D8},
    {0xFF8E,0x30DB},{0xFF8F,0x30DE},{0xFF90,0x30DF},{0xFF91,0x30E0},
    {0xFF92,0x30E1},{0xFF93,0x30E2},{0xFF94,0x30E4},{0xFF95,0x30E6},
    {0xFF96,0x30E8},{0xFF97,0x30E9},{0xFF98,0x30EA},{0xFF99,0x30EB},
    {0xFF9A,0x30EC},{0xFF9B,0x30ED},{0xFF9C,0x30EF},{0xFF9D,0x30F3},
};

// 旧字体・異体字。帳票の企業名でよく出るものに絞る。
// 網羅は狙わない。増やすときは実データで出たものだけを足す。
//
// コードポイントは手で書かず、必ず実際の文字から取ること。
// 手書きしたところ 櫻(0x6AFB を 0x6AAB) と 濵(0x6FF5 を 0x6FF1) の2箇所を
// 間違えていた。自己テストで櫻が引っかかって発覚した。
const std::map<uint32_t, uint32_t> kOldKanji = {
    {0x9AD9, 0x9AD8},  // 髙 → 高
    {0xFA11, 0x5D0E},  // 﨑 → 崎
    {0x6FF5, 0x6D5C},  // 濵 → 浜
    {0x5D8B, 0x5CF6},  // 嶋 → 島
    {0x5D8C, 0x5CF6},  // 嶌 → 島
    {0x908A, 0x8FBA},  // 邊 → 辺
    {0x9089, 0x8FBA},  // 邉 → 辺
    {0x9F8D, 0x7ADC},  // 龍 → 竜
    {0x6AFB, 0x685C},  // 櫻 → 桜
    {0x570B, 0x56FD},  // 國 → 国
    {0x5B78, 0x5B66},  // 學 → 学
    {0x6FA4, 0x6CA2},  // 澤 → 沢
    {0x9F4B, 0x658E},  // 齋 → 斎
    {0x85DD, 0x82B8},  // 藝 → 芸
    {0x5713, 0x5186},  // 圓 → 円
    {0x9A5B, 0x99C5},  // 驛 → 駅
};

// 除去する記号。企業名の表記揺れで揺れやすいものだけ。
bool is_strip_symbol(uint32_t cp) {
    static const std::array<uint32_t, 14> kSyms = {
        0x30FB,  // ・ 中点
        0x002E, 0xFF0E,          // . ．
        0x002D, 0xFF0D, 0x2010, 0x2212,  // - － ‐ −
        0x0027, 0x2019,          // ' ’
        0x0022, 0x201D,          // " ”
        0x0026, 0xFF06,          // & ＆
        0x00B7,                  // ·
    };
    return std::find(kSyms.begin(), kSyms.end(), cp) != kSyms.end();
}

bool is_space(uint32_t cp) {
    return cp == 0x20 || cp == 0x3000 || cp == 0x09 || cp == 0x0A || cp == 0x0D;
}

// 法人格の表記。先頭・末尾のどちらに付いていても除去する。
struct CorpForm { const char* text; CorpKind kind; };
const std::vector<CorpForm> kCorpForms = {
    {"株式会社", CorpKind::Kabushiki}, {"(株)", CorpKind::Kabushiki},
    {"（株）", CorpKind::Kabushiki},   {"㈱", CorpKind::Kabushiki},
    {"有限会社", CorpKind::Yugen},     {"(有)", CorpKind::Yugen},
    {"（有）", CorpKind::Yugen},       {"㈲", CorpKind::Yugen},
    {"合同会社", CorpKind::Godo},      {"(同)", CorpKind::Godo},
    {"合資会社", CorpKind::Other},     {"合名会社", CorpKind::Other},
    {"一般社団法人", CorpKind::Other}, {"公益社団法人", CorpKind::Other},
    {"一般財団法人", CorpKind::Other}, {"公益財団法人", CorpKind::Other},
    {"医療法人", CorpKind::Other},     {"学校法人", CorpKind::Other},
    {"社会福祉法人", CorpKind::Other}, {"宗教法人", CorpKind::Other},
    {"特定非営利活動法人", CorpKind::Other}, {"NPO法人", CorpKind::Other},
};

}  // namespace

CorpKind detect_corp(const std::string& s) {
    // 長いものから先に見る。「一般社団法人」を「社団法人」で切らないため。
    auto sorted = kCorpForms;
    std::sort(sorted.begin(), sorted.end(),
              [](const CorpForm& a, const CorpForm& b) {
                  return std::string(a.text).size() > std::string(b.text).size();
              });
    for (const auto& f : sorted) {
        if (s.find(f.text) != std::string::npos) return f.kind;
    }
    return CorpKind::None;
}

const char* corp_name(CorpKind k) {
    switch (k) {
        case CorpKind::Kabushiki: return "株式会社";
        case CorpKind::Yugen:     return "有限会社";
        case CorpKind::Godo:      return "合同会社";
        case CorpKind::Other:     return "その他";
        default:                  return "なし";
    }
}

std::string normalize(const std::string& s, const NormOptions& opt) {
    std::string work = s;

    // ── 法人格の除去 ──
    // コードポイント処理より先に、文字列として消す。
    // 長いものから消さないと「一般社団法人」が「社団法人」の残骸を残す。
    if (opt.strip_corp) {
        auto sorted = kCorpForms;
        std::sort(sorted.begin(), sorted.end(),
                  [](const CorpForm& a, const CorpForm& b) {
                      return std::string(a.text).size() > std::string(b.text).size();
                  });
        for (const auto& f : sorted) {
            const std::string t = f.text;
            size_t pos;
            while ((pos = work.find(t)) != std::string::npos) {
                work.erase(pos, t.size());
            }
        }
    }

    auto cps = to_codepoints(work);
    std::vector<uint32_t> out;
    out.reserve(cps.size());

    for (size_t i = 0; i < cps.size(); ++i) {
        uint32_t cp = cps[i];

        if (opt.nfkc) {
            // 全角英数 → 半角
            if (cp >= 0xFF01 && cp <= 0xFF5E) cp -= 0xFEE0;
            // 半角カナ → 全角カナ。濁点・半濁点は次の文字を見て合成する
            auto it = kHankakuKana.find(cp);
            if (it != kHankakuKana.end()) {
                cp = it->second;
                if (i + 1 < cps.size()) {
                    if (cps[i + 1] == 0xFF9E) { cp += 1; ++i; }       // ゛
                    else if (cps[i + 1] == 0xFF9F) { cp += 2; ++i; }  // ゜
                }
            }
        }

        if (opt.strip_space && is_space(cp)) continue;
        if (opt.strip_symbol && is_strip_symbol(cp)) continue;

        // ひらがな → カタカナ。同じ音を同じ文字に寄せる
        if (opt.to_katakana && cp >= 0x3041 && cp <= 0x3096) cp += 0x60;

        if (opt.old_kanji) {
            auto it = kOldKanji.find(cp);
            if (it != kOldKanji.end()) cp = it->second;
        }

        if (opt.normalize_choon) {
            if (cp == 0x30FC) continue;                  // ー を落とす
            if (cp == 0x30C3) cp = 0x30C4;               // ッ → ツ
        }

        // 英字は大文字に寄せる。ABC商事 と abc商事 を同一とみなす
        if (cp >= 'a' && cp <= 'z') cp -= 32;

        out.push_back(cp);
    }
    return to_utf8(out);
}

std::vector<std::string> ngrams(const std::string& normalized, int n) {
    const auto cps = to_codepoints(normalized);
    std::vector<std::string> out;
    if (static_cast<int>(cps.size()) < n) {
        if (!cps.empty()) out.push_back(normalized);
        return out;
    }
    out.reserve(cps.size() - n + 1);
    for (size_t i = 0; i + n <= cps.size(); ++i) {
        out.push_back(to_utf8({cps.begin() + i, cps.begin() + i + n}));
    }
    return out;
}

}  // namespace dm
