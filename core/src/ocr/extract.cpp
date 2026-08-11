#include "extract.hpp"

#include <algorithm>
#include <cctype>
#include <cstring>
#include <cmath>
#include <map>
#include <regex>

namespace dm {
namespace {

// 全角数字・全角記号を半角に寄せる。OCRは全角半角を取り違えやすい。
std::string ascii_fold(const std::string& s) {
    std::string o;
    for (size_t i = 0; i < s.size();) {
        const unsigned char c = s[i];
        if (c < 0x80) { o += static_cast<char>(c); ++i; continue; }
        if ((c & 0xF0) == 0xE0 && i + 2 < s.size()) {
            const uint32_t cp = ((c & 0x0F) << 12) | ((s[i+1] & 0x3F) << 6) | (s[i+2] & 0x3F);
            if (cp >= 0xFF01 && cp <= 0xFF5E) { o += static_cast<char>(cp - 0xFEE0); i += 3; continue; }
            // OCRがよく間違える文字を寄せる
            if (cp == 0x30FC || cp == 0x2212 || cp == 0x2010) { o += '-'; i += 3; continue; }
        }
        int len = ((c & 0xE0) == 0xC0) ? 2 : ((c & 0xF0) == 0xE0) ? 3 : 4;
        o += s.substr(i, len);
        i += len;
    }
    return o;
}

// 同じ行にあるとみなす縦のずれ（ブロックの高さに対する割合）
bool same_row(const TextBlock& a, const TextBlock& b, double tol = 0.6) {
    const double h = std::max(1, std::max(a.h, b.h));
    return std::abs((a.y + a.h / 2.0) - (b.y + b.h / 2.0)) < h * tol;
}

// 年月日を ISO 形式に組み立てる。
// snprintf の固定バッファは使わない。OCRが 9999年99月99日 のような値を
// 読んだときに桁があふれる（コンパイラの -Wformat-truncation が指摘した）。
// 範囲外の値はここで弾く。日付として成立しないものを後段に流さない。
std::string make_iso(int y, int mo, int d) {
    if (y < 1900 || y > 2200 || mo < 1 || mo > 12 || d < 1 || d > 31) return "";
    std::string s;
    s += std::to_string(y);
    s += '-';
    if (mo < 10) s += '0';
    s += std::to_string(mo);
    s += '-';
    if (d < 10) s += '0';
    s += std::to_string(d);
    return s;
}

bool contains_any(const std::string& s, std::initializer_list<const char*> keys) {
    for (const char* k : keys)
        if (s.find(k) != std::string::npos) return true;
    return false;
}

// 法人格を含むか。名前らしさの手がかりで、部署名や役職と分けるのに効く。
bool has_corporate(const std::string& s) {
    return contains_any(s, {"株式会社", "(株)", "㈱", "有限会社", "(有)", "㈲",
                            "合同会社", "合資会社", "合名会社"});
}

// 役職・部署の語だけでできているか。
//
// 実物のPDF5枚で、宛先として「代表」「代表取締役」「経理部」「総務課」
// 「管理本部」を返していた。宛名が
//     株式会社 サンプル商事
//     代表 見本 太郎 様
// の2行構成で、「様」と同じ行にあるのが役職＋個人名だったため。
// 法人格を含むものは除外しない（「株式会社○○ 経理部」を落とさない）。
bool looks_like_title(const std::string& s) {
    if (has_corporate(s)) return false;
    return contains_any(s, {"代表", "取締役", "社長", "専務", "常務", "部長",
                            "課長", "係長", "主任", "担当", "経理部", "総務部",
                            "総務課", "経理課", "管理本部", "営業部", "購買部",
                            "御担当"});
}

// 連絡先・支払先の行か。名前ではありえない語だけで判定する。
//
// 発行元を見分ける最も強い手がかり。請求書でもレシートでも、
// 発行元の名前の下には必ず住所・電話・振込先・登録番号が並ぶ。
// 位置（右上／中央上）は様式で変わるが、この並びは変わらない。
bool looks_like_contact(const std::string& s) {
    const std::string t = ascii_fold(s);
    return contains_any(t, {"〒", "TEL", "Tel", "tel", "FAX", "Fax", "fax",
                            "電話", "登録番号", "振込", "口座", "銀行"});
}

// 住所らしいか。
//
// 【重要】これを「名前ではない」判定に使ってはいけない。
// 市川商店・北区運送のように、地名を含む社名は普通にある。
// 「この下に住所が並んでいる」という位置の手がかりとしてだけ使う。
bool looks_like_address(const std::string& s) {
    return contains_any(s, {"都", "道", "府", "県", "市", "区", "町", "村",
                            "丁目", "番地"});
}

// 数字と記号だけでできているか。金額・日付・電話番号を名前から外す。
//
// 【重要】「数字を含む＝名前ではない」にしてはいけない。
// ハルタ商会284・ナンセイ工業945 のように、数字を含む社名がある。
// 数字を取り除いたあとに何も残らないときだけ、名前ではないと決める。
bool looks_numeric(const std::string& s) {
    const std::string t = ascii_fold(s);
    for (size_t i = 0; i < t.size();) {
        const unsigned char c = t[i];
        if (c < 0x80) {
            if (!std::isdigit(c) && c != ',' && c != '.' && c != '-' &&
                c != ' ' && c != '\\' && c != '/' && c != ':' && c != '(' && c != ')')
                return false;
            ++i;
            continue;
        }
        const int len = ((c & 0xE0) == 0xC0) ? 2 : ((c & 0xF0) == 0xE0) ? 3 : 4;
        const std::string ch = t.substr(i, len);
        // 金額・日付に付く文字だけは「残っていない」とみなす
        if (ch != "¥" && ch != "円" && ch != "年" && ch != "月" && ch != "日")
            return false;
        i += len;
    }
    return true;
}

// 帳票の表題や表の見出し。名前ではない。
bool looks_like_label(const std::string& s) {
    return contains_any(s, {"合計", "小計", "消費税", "請求書", "納品書", "領収書",
                            "品目", "数量", "単価", "金額", "備考", "振込",
                            "お振込先", "御請求", "上記", "レシート", "領収証",
                            "お預り", "お釣り", "対象", "内訳"});
}

// b の下に o があるか。横は重なっていればよい（レシートは中央揃えで
// 左端が揃わない）。距離は行の高さを基準にする。文字の大きさが
// 帳票ごとに違うので、画素の絶対値で書くと様式が変わるたびに外れる。
bool is_below(const TextBlock& b, const TextBlock& o) {
    if (o.y < b.y + b.h / 2) return false;
    if (o.y - (b.y + b.h) > b.h * 8) return false;
    const int l = std::max(b.x, o.x);
    const int r = std::min(b.x + b.w, o.x + o.w);
    return r > l;   // 横がどこかで重なる
}

// 手がかりの語を含むブロックを探す
const TextBlock* find_block(const std::vector<TextBlock>& bs,
                            const std::vector<std::string>& keys) {
    for (const auto& b : bs)
        for (const auto& k : keys)
            if (b.text.find(k) != std::string::npos) return &b;
    return nullptr;
}

}  // namespace

const char* field_key_name(FieldKey k) {
    switch (k) {
        case FieldKey::PartnerName:   return "partner_name";
        case FieldKey::IssuerName:    return "issuer_name";
        case FieldKey::RecipientName: return "recipient_name";
        case FieldKey::IssueDate:   return "issue_date";
        case FieldKey::Total:       return "total";
        case FieldKey::DocNo:       return "doc_no";
        case FieldKey::RegNo:       return "reg_no";
    }
    return "?";
}

const Field* ExtractResult::find(FieldKey k) const {
    for (const auto& f : fields) if (f.key == k) return &f;
    return nullptr;
}

std::string parse_date(const std::string& s) {
    // 【重要】std::regex はバイト単位で動く。マルチバイト文字を
    // 文字クラス [年/\-\.] に書いてはいけない。年 は3バイト（E5 B9 B4）なので、
    // 「そのいずれか1バイト」の意味になり、E5 に一致した後 B9 B4 が
    // 数字でないため失敗する。実測で日付が 0/20 になった原因がこれ。
    //
    // 先に区切り文字をASCIIの '-' へ置き換え、そのあと純ASCIIの正規表現をかける。
    std::string t = ascii_fold(s);
    for (const char* sep : {"年", "月", "日", "/", ".", "／", "．"}) {
        size_t p;
        while ((p = t.find(sep)) != std::string::npos) t.replace(p, strlen(sep), "-");
    }
    // 和暦の元号もASCIIの印に置き換える
    bool wareki = false;
    for (const char* era : {"令和", "R"}) {
        size_t p = t.find(era);
        if (p != std::string::npos) { t.replace(p, strlen(era), "W"); wareki = true; break; }
    }

    std::smatch m;
    if (wareki) {
        if (std::regex_search(t, m, std::regex(R"(W\s*(\d{1,2})\s*-\s*(\d{1,2})\s*-\s*(\d{1,2}))"))) {
            return make_iso(2018 + std::stoi(m[1]), std::stoi(m[2]), std::stoi(m[3]));
        }
    }
    if (std::regex_search(t, m, std::regex(R"((\d{4})\s*-\s*(\d{1,2})\s*-\s*(\d{1,2}))"))) {
        return make_iso(std::stoi(m[1]), std::stoi(m[2]), std::stoi(m[3]));
    }
    return "";
}

long long parse_amount(const std::string& s) {
    std::string t = ascii_fold(s);

    // 【重要】数字のあとに来る「-」で切る。
    //
    // 日本の帳票では ¥882,090- のように、末尾のハイフンで
    // 「これ以下の端数は無い」を表す。金額の一部ではなく終端記号。
    //
    // 切らないと、そのあとに続いた文字の数字まで金額に混ざる。
    // 100枚での実測（Vision）:
    //   Vision が 1語として返した内容        こちらの読み取り   正解
    //     882,090-00                          88209000         882090
    //     1,395,350-000                     1395350000        1395350
    //     376,750-8                            3767508         376750
    //     952,160-888                        952160888         952160
    //   金額の一致 66/94 → この1点だけで大きく変わる。
    //
    // 数字より前のハイフンは切らない。マイナス記号や、
    // 罫線を「-」と誤読したものが先頭に付くことがあるため。
    {
        bool seen_digit = false;
        for (size_t i = 0; i < t.size(); ++i) {
            if (std::isdigit(static_cast<unsigned char>(t[i]))) { seen_digit = true; continue; }
            if (t[i] == '-' && seen_digit) { t.erase(i); break; }
        }
    }

    // 通貨記号・カンマを落として数字だけにする。
    // OCRは ¥ を \ と読むことが多い（実測で確認）。
    std::string d;
    for (char c : t) if (std::isdigit(static_cast<unsigned char>(c))) d += c;
    if (d.empty() || d.size() > 12) return -1;
    // 数字が少なすぎるものは金額とみなさない（電話番号の断片など）
    if (d.size() < 3) return -1;
    try { return std::stoll(d); } catch (...) { return -1; }
}

bool looks_like_doc_no(const std::string& s) {
    const std::string t = ascii_fold(s);
    int alnum = 0;
    for (char c : t) {
        if (std::isalnum(static_cast<unsigned char>(c))) ++alnum;
        else if (c != '-' && c != '_' && c != ' ') return false;
    }
    return alnum >= 4;
}

namespace {

// T のあとに続く数字を集める。区切り（空白・ハイフン）は飛ばす。
std::string digits_after_t(const std::string& t, size_t i) {
    std::string d;
    for (size_t j = i + 1; j < t.size() && d.size() < 13; ++j) {
        const unsigned char c = static_cast<unsigned char>(t[j]);
        if (std::isdigit(c)) { d += static_cast<char>(c); continue; }
        // 【重要】ここで英字を飛ばしてはいけない。
        // "TEL 049-999-0001" の T から数字を拾い始めてしまう。
        if (c == '-' || c == ' ') continue;
        break;
    }
    return d;
}

}  // namespace

std::string find_reg_no(const std::string& s) {
    const std::string t = ascii_fold(s);
    for (size_t i = 0; i < t.size(); ++i) {
        // OCRは T を t と読むことがある。大文字小文字は問わない。
        if (t[i] != 'T' && t[i] != 't') continue;
        if (digits_after_t(t, i).size() == 13) return "T" + digits_after_t(t, i);
    }
    return "";
}

std::string find_reg_no_loose(const std::string& s) {
    // 「登録番号」と書いてある行でだけ使う。
    //
    // 桁が足りない番号を見つけるために要る。13桁ちょうどしか拾わないと、
    // 12桁しか印字されていない請求書が「記載なし」になる。
    // 「番号が無い」と「番号が誤っている」は、現場の対応がまったく違う。
    // 前者は免税事業者かもしれないが、後者は必ず先方に問い合わせる。
    const std::string t = ascii_fold(s);
    std::string best;
    for (size_t i = 0; i < t.size(); ++i) {
        if (t[i] != 'T' && t[i] != 't') continue;
        const std::string d = digits_after_t(t, i);
        // 数字が数個しか続かないものは番号ではない（TEL の断片など）。
        if (d.size() >= 8 && d.size() > best.size()) best = "T" + d;
    }
    if (!best.empty()) return best;

    // T が読めなかった場合。数字だけの長い連なりを拾う。
    // 「登録番号 9234567890123」のように T が落ちていることがある。
    std::string run, longest;
    for (size_t i = 0; i <= t.size(); ++i) {
        const bool d = i < t.size() &&
                       std::isdigit(static_cast<unsigned char>(t[i]));
        if (d) { run += t[i]; continue; }
        if (i < t.size() && (t[i] == '-' || t[i] == ' ') && !run.empty()) continue;
        if (run.size() > longest.size()) longest = run;
        run.clear();
    }
    if (longest.size() >= 10) return longest;
    return "";
}

ExtractResult extract(const OcrResult& ocr, int page_w, int page_h) {
    ExtractResult out;
    const auto& bs = ocr.blocks;
    if (bs.empty()) return out;

    // 行ごとにまとめてから、x順に連結する。
    //
    // 【重要】ブロックは重なる。日本語には単語の区切りが無いため、
    // Tesseract の単語矩形が隣どうしで食い込む。実測:
    //   x=121 w=208 [ハル]   ← x 121〜329 を占める
    //   x=178 w=41  [タ]     ← 前のブロックの内側から始まる
    // 「隙間が負なら別項目」と書くと、重なるブロックを全部捨てることになる。
    // 重なりは許し、離れすぎたときだけ切る。
    std::vector<std::vector<const TextBlock*>> rows;
    for (const auto& b : bs) {
        if (b.text.find_first_not_of(" \t\r\n") == std::string::npos) continue;
        bool placed = false;
        for (auto& r : rows) {
            if (same_row(*r.front(), b)) { r.push_back(&b); placed = true; break; }
        }
        if (!placed) rows.push_back({&b});
    }
    std::vector<TextBlock> merged;
    for (auto& r : rows) {
        std::sort(r.begin(), r.end(),
                  [](const TextBlock* a, const TextBlock* b) { return a->x < b->x; });
        TextBlock m = *r.front();
        for (size_t i = 1; i < r.size(); ++i) {
            const int gap = r[i]->x - (m.x + m.w);
            if (gap > m.h) {                    // 1文字分より離れたら別項目
                merged.push_back(m);
                m = *r[i];
                continue;
            }
            m.text += r[i]->text;
            m.w = std::max(m.x + m.w, r[i]->x + r[i]->w) - m.x;
            m.h = std::max(m.h, r[i]->h);
            m.confidence = std::min(m.confidence, r[i]->confidence);
        }
        merged.push_back(m);
    }

    // 以降はすべて merged を使う。連結しないと日本語が単語で割れる。
    // 日付「2026年5月25日」も複数ブロックに分かれるため、
    // 連結前のブロックで正規表現をかけても一致しない（実測 0/20）。

    // ── 名前（宛先と発行元） ──
    //
    // Tesseract は単語単位で返すので、日本語の企業名は複数ブロックに割れる。
    // 実測では「ハルタ物産512株式会社」が「ハル」「タ物産512」「株式会社」の
    // ように分かれた。ブロック単体で見ると「ハル」しか取れない。
    // 同じ行で横に近いブロックを先に連結してから判定する。
    //
    // 宛先と発行元を別々に採点する。どちらを取引先とみなすかは
    // 帳票の向き（受領か発行か）で決まり、それは画像からはわからない。
    // ここでは両方返し、選ぶのは呼び出し側の仕事にする。
    {
        // 名前の後始末。宛先・発行元の両方で同じ処理をする。
        auto tidy = [](std::string v) {
            // 「御中」は宛先を示す語であって名前ではない。そこから後ろを切る。
            //
            // 【重要】"御中" という2文字での一致では足りない。
            // 実際に多かったのは、御中の「中」側が壊れて別の文字になった形。
            //   ヤマト運輸335(株)御』of
            //   帆ナンセイサービス御Fnre
            //   (有)シラカワ運輸御』o
            // 2文字で探すと一致しないので、そのまま名前に残る。
            // 「御」1文字を見つけたら、そこから後ろを全部落とす。
            // 日本の会社名に「御」が現れることは、まず無い。
            for (const char* k : {"御", "様"}) {
                const size_t p = v.find(k);
                if (p != std::string::npos) v.erase(p);
            }
            // 末尾に残った英字を落とす。
            //
            // 御中を切った後も、その先の誤読が残ることがある（…(株)4a）。
            // 日本語を含む名前の末尾に英字が付くことは、この用途ではまず無い。
            // 【注意】先頭の英字は落とさない。ABC商事のような社名が実在する。
            bool has_jp = false;
            for (size_t i = 0; i < v.size(); ++i)
                if (static_cast<unsigned char>(v[i]) >= 0x80) { has_jp = true; break; }
            if (has_jp) {
                while (!v.empty()) {
                    const unsigned char c = v.back();
                    if ((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) v.pop_back();
                    else break;
                }
            }
            return v;
        };

        // 宛先に近いか。発行元の減点にも使うので関数にする。
        auto near_honorific = [&](const TextBlock& b) {
            for (const auto& o : merged) {
                if (&o == &b) continue;
                if (!contains_any(o.text, {"御中", "様"})) continue;
                if (same_row(b, o) && o.x > b.x && o.x - (b.x + b.w) < b.h * 6) return 1;
                if (is_below(b, o) && o.y - (b.y + b.h) < b.h * 3) return 2;
            }
            return 0;
        };

        // ── 発行元 ──
        //
        // 最も強い手がかりは「その下に連絡先が並んでいる」こと。
        // 請求書でもレシートでも、発行元の名前の下には住所・電話・
        // 振込先・登録番号が続く。位置（右上か中央上か）は様式で変わるが、
        // この並びは変わらない。
        Field iss; iss.key = FieldKey::IssuerName;
        double iss_best = 0.0;
        const TextBlock* iss_blk = nullptr;
        for (const auto& b : merged) {
            if (b.text.size() < 6) continue;
            if (looks_like_label(b.text)) continue;
            // 金額・日付をそのまま名前として返さない。
            if (looks_numeric(b.text) || !parse_date(b.text).empty()) continue;
            // 住所・電話の行そのものは名前ではない。
            // 【注意】ここで住所らしさ（市・区など）を見てはいけない。
            // 市川商店のような社名を落とす。
            if (looks_like_contact(b.text)) continue;

            double sc = 0.0;
            std::string why;

            int contacts = 0;
            for (const auto& o : merged) {
                if (&o == &b) continue;
                if (is_below(b, o) &&
                    (looks_like_contact(o.text) || looks_like_address(o.text))) ++contacts;
            }
            if (contacts >= 1) { sc += 0.35; why += "連絡先の上 "; }
            if (contacts >= 2) { sc += 0.15; why += "連絡先が続く "; }

            if (b.y < page_h / 3 && b.x >= page_w / 2) { sc += 0.25; why += "上部右 "; }
            // レシートは中央上に店名が来る。左右では当たらない。
            if (b.y < page_h / 8) { sc += 0.2; why += "最上部 "; }
            if (has_corporate(b.text)) { sc += 0.25; why += "法人格 "; }

            // 宛先側は発行元ではない。ここを引かないと、
            // 宛先の下にも住所と電話が並ぶ様式で取り違える。
            if (contains_any(b.text, {"御中", "様"}) || near_honorific(b) != 0) {
                sc -= 0.5; why += "宛先側 ";
            }
            if (looks_like_title(b.text)) { sc -= 0.5; why += "役職・部署 "; }

            if (sc > iss_best) {
                iss_best = sc;
                iss.value = b.text; iss.why = why;
                iss.x = b.x; iss.y = b.y; iss.w = b.w; iss.h = b.h;
                iss.confidence = sc * b.confidence;
                iss_blk = &b;
            }
        }
        iss.value = tidy(iss.value);

        // ── 宛先 ──
        //
        // 【重要】発行元として選んだブロックは、宛先の候補から外す。
        // 同じ帳票の発行元と宛先が同じ行になることはない。
        //
        // これが無いと、OCRが「御中」を読めなかったときに、
        // 手がかりが「法人格」だけ（0.2点）になり、
        // 自社名（発行元）を取引先として返す。100枚の実測で2枚。
        //   inv_0051  正解 (有)ヤマト運輸  → テスト商事株式会社
        //   inv_0083  正解 ハルタ産業㈱    → テスト商事株式会社
        // 自社名は必ずマスタに無いので、必ず低いスコアで別の会社に当たる。
        // 却下されるので実害は出ないが、却下の理由が「読めなかった」ではなく
        // 「自社名を照合していた」になり、現場が原因を追えない。
        //
        // 手がかりは4つ。単独では外れるので、点を付けて足す。
        //   ① 「御中」「様」を含む／その左にある（最も強い）
        //   ② 「様」の付いた行の上にある（宛名が2行に分かれる形）
        //   ③ 上から3分の1、左半分にある
        //   ④ 法人格を含む
        Field rcp; rcp.key = FieldKey::RecipientName;
        double rcp_best = 0.0;
        for (const auto& b : merged) {
            if (b.text.size() < 6) continue;   // UTF-8で2文字以上
            if (&b == iss_blk) continue;       // 発行元は宛先ではない
            // 金額・日付をそのまま名前として返さない。
            // 実測（inv_0051 / inv_0083）で ¥1,537,250- を取引先名にしていた。
            if (looks_numeric(b.text) || !parse_date(b.text).empty()) continue;
            // 帳票のラベルや表題は取引先名ではない。
            // 実測で「合計金額」「領収書」を取引先名として拾っていた。
            // 御中が読めなかったときに、弱い手がかりで無理に答えを出したため。
            if (looks_like_label(b.text)) continue;

            double sc = 0.0;
            std::string why;

            if (contains_any(b.text, {"御中", "様"})) {
                sc += 0.6; why += "御中を含む ";
            } else {
                const int rel = near_honorific(b);
                if (rel == 1) { sc += 0.6; why += "御中の左 "; }
                // 宛名が2行に分かれている形。会社名が上、役職＋個人名＋様が下。
                //   株式会社 サンプル商事
                //   代表 見本 太郎 様
                // 法人格を含むものに限る。そうしないと、宛名の上にある
                // 表題や日付まで拾ってしまう。
                else if (rel == 2 && has_corporate(b.text)) {
                    sc += 0.5; why += "宛名の上 ";
                }
            }

            if (b.y < page_h / 3 && b.x < page_w / 2) { sc += 0.2; why += "上部左 "; }
            if (has_corporate(b.text)) { sc += 0.2; why += "法人格 "; }
            // 役職・部署は名前ではない。
            // 実物のPDF5枚で「代表」「経理部」「総務課」を返していた原因。
            if (looks_like_title(b.text)) { sc -= 0.5; why += "役職・部署 "; }

            if (sc > rcp_best) {
                rcp_best = sc;
                rcp.value = b.text; rcp.why = why;
                rcp.x = b.x; rcp.y = b.y; rcp.w = b.w; rcp.h = b.h;
                rcp.confidence = sc * b.confidence;
            }
        }
        rcp.value = tidy(rcp.value);

        if (iss_best > 0.0 && !iss.value.empty()) out.fields.push_back(iss);
        if (rcp_best > 0.0 && !rcp.value.empty()) out.fields.push_back(rcp);

        // partner_name は宛先と同じ値を入れる。
        //
        // 受領側では発行元が正しいが、それを決めるのは向きの設定であって
        // 画像ではない。ここで勝手に発行元へ切り替えると、
        // 発行側で使っている既存の顧問先が黙って壊れる。
        // 既定は今までどおり宛先にし、切り替えは呼び出し側で行う。
        Field f = rcp;
        f.key = FieldKey::PartnerName;
        const double best = rcp_best;
        // 確信が低くても返す。抽出側で捨ててはいけない。
        //
        // 一度「確信が持てなければ返さない」（最低スコア0.4）を入れたが、
        // 照合の正解が 12/20 → 10/20 に落ちた。正しい抽出まで捨てていた。
        //
        // このプロダクトには「要確認」の仕組みがある。確信の低いものは
        // 低い信頼度を付けて返し、どこで人が確認するかは閾値設定に委ねる。
        // それが「どの精度なら人の確認を省いてよいかを現場が決める」という
        // 仕様書の思想に沿う。抽出の段階で線を引くと、現場が調整できなくなる。
        if (best > 0.0 && !f.value.empty()) out.fields.push_back(f);
    }

    // ── 登録番号（インボイス制度） ──
    //
    // 2023年10月から、受け取った請求書・領収書に
    // 「T＋13桁」の登録番号が正しく記載されているかを確かめる必要がある。
    // 番号が無効だと仕入税額控除が取れない。
    //
    // 受領側にしか発生せず、全件やらなければならず、純粋に機械的で、
    // 間違えると税額に直結する。人がやる意味がまったく無い作業。
    //
    // 【重要】ここでは「取り出す」だけ。正しいかどうかは判断しない。
    // 検査数字の計算も、国税庁の公表システムとの照合も、後段（Go）の仕事。
    // 抽出の段階で捨てると、「読めたが誤っている」と「読めなかった」を
    // 区別できなくなる。現場が確かめるべきなのは前者だけ。
    {
        Field f; f.key = FieldKey::RegNo;
        double best = 0.0;
        for (const auto& b : merged) {
            // 「登録番号」と書いてある行なら、桁が足りなくても拾う。
            // そうしないと 12桁しか無い請求書が「記載なし」になり、
            // 誤りとして一覧に出てこない。
            const bool labeled =
                contains_any(b.text, {"登録番号", "登録", "インボイス", "適格"});
            const std::string v = labeled ? find_reg_no_loose(b.text)
                                          : find_reg_no(b.text);
            if (v.empty()) continue;
            double sc = 0.6;
            std::string why = "T+13桁 ";
            if (labeled) { sc = 0.95; why = "登録番号の記載 "; }
            if (sc > best) {
                best = sc;
                f.value = v; f.why = why;
                f.x = b.x; f.y = b.y; f.w = b.w; f.h = b.h;
                f.confidence = sc * b.confidence;
            }
        }
        if (best > 0.0) out.fields.push_back(f);
    }

    // ── 日付 ──
    {
        Field f; f.key = FieldKey::IssueDate;
        double best = 0.0;
        for (const auto& b : merged) {
            const std::string iso = parse_date(b.text);
            if (iso.empty()) continue;
            double sc = 0.7;
            std::string why = "日付の形 ";
            // 上部にあるほど発行日らしい
            if (b.y < page_h / 3) { sc += 0.3; why += "上部 "; }
            if (sc > best) {
                best = sc; f.value = iso; f.why = why;
                f.x = b.x; f.y = b.y; f.w = b.w; f.h = b.h;
                f.confidence = sc * b.confidence;
            }
        }
        if (best > 0.0) out.fields.push_back(f);
    }

    // ── 合計金額 ──
    // 「合計」の右にある数字を最優先。無ければ最大の数字。
    {
        Field f; f.key = FieldKey::Total;
        double best = 0.0;
        const TextBlock* kw = find_block(merged, {"合計金額", "合計", "総額", "御請求"});
        for (const auto& b : merged) {
            // 【重要】日付は金額ではない。先に外す。
            //
            // 「2026年5月25日」から数字だけを抜くと 2026525 になり、
            // 7桁の数字として金額の候補に入ってしまう。
            // 手がかり（合計の右・通貨記号）が無い伝票では、
            // これが最高点になって金額として採用されていた。
            //   実測 inv_0004  読み 2026525 / 正解 273350
            if (!parse_date(b.text).empty()) continue;

            const long long v = parse_amount(b.text);
            if (v <= 0) continue;
            double sc = 0.2;
            std::string why = "数字 ";
            if (kw && same_row(b, *kw) && b.x > kw->x) { sc += 0.6; why += "合計の右 "; }
            // 通貨記号があれば金額らしい。OCRは ¥ を \ と読むことがある
            if (b.text.find("¥") != std::string::npos ||
                b.text.find("\\") != std::string::npos ||
                b.text.find("円") != std::string::npos) { sc += 0.2; why += "通貨記号 "; }
            if (sc > best) {
                best = sc; f.value = std::to_string(v); f.why = why;
                f.x = b.x; f.y = b.y; f.w = b.w; f.h = b.h;
                f.confidence = sc * b.confidence;
            }
        }
        if (best > 0.0) out.fields.push_back(f);
    }

    // ── 伝票番号 ──
    {
        Field f; f.key = FieldKey::DocNo;
        double best = 0.0;
        const TextBlock* kw = find_block(merged, {"No.", "No", "番号", "伝票"});
        for (const auto& b : merged) {
            std::string t = ascii_fold(b.text);
            // "No." が同じブロックに入っていることがあるので剥がす
            const size_t p = t.find("No");
            if (p != std::string::npos) {
                t = t.substr(p + 2);
                while (!t.empty() && (t[0] == '.' || t[0] == ' ' || t[0] == ':')) t.erase(0, 1);
            }
            if (!looks_like_doc_no(t)) continue;
            double sc = 0.2;
            std::string why = "英数字 ";
            if (p != std::string::npos) { sc += 0.6; why += "No.付き "; }
            else if (kw && same_row(b, *kw) && b.x > kw->x) { sc += 0.5; why += "No.の右 "; }
            if (b.y < page_h / 3) { sc += 0.1; why += "上部 "; }
            if (sc > best) {
                best = sc; f.value = t; f.why = why;
                f.x = b.x; f.y = b.y; f.w = b.w; f.h = b.h;
                f.confidence = sc * b.confidence;
            }
        }
        if (best > 0.0) out.fields.push_back(f);
    }

    return out;
}

}  // namespace dm
