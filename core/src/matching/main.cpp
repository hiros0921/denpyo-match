// 照合エンジンのCLI。単体で動く（仕様書 第4段階の要件）。
//
//   dm_match --self-test
//   dm_match --masters masters.json --query "株式会社ヤマト商事"
//   dm_match --masters masters.json --bench 10000
//
// --bench は仕様書の「1万件×1万件で処理時間を計測して報告すること」に対応する。
// 総当たりではなく、1枚の伝票につき1万件のマスタから正解を探す、を1万回。
#include "matching.hpp"
#include "normalize.hpp"

#include <cctype>
#include <chrono>
#include <fstream>
#include <iomanip>
#include <iostream>
#include <map>
#include <random>
#include <sstream>
#include <string>
#include <vector>

int run_match_self_test();

namespace {

// masters.json を読む。外部のJSONライブラリは入れない。
// 必要なのは "canonical" と "variants" だけなので、その2つを拾う。
std::string json_unescape(const std::string& s) {
    std::string o;
    for (size_t i = 0; i < s.size(); ++i) {
        if (s[i] == '\\' && i + 1 < s.size()) {
            switch (s[++i]) {
                case 'n': o += '\n'; break;
                case 't': o += '\t'; break;
                case '"': o += '"';  break;
                case '\\': o += '\\'; break;
                default: o += s[i];
            }
        } else o += s[i];
    }
    return o;
}

// "key": "値" を拾う。ネストは扱わない（この用途では不要）。
std::vector<std::string> extract_strings(const std::string& body, const std::string& key) {
    std::vector<std::string> out;
    const std::string pat = "\"" + key + "\"";
    size_t pos = 0;
    while ((pos = body.find(pat, pos)) != std::string::npos) {
        pos = body.find(':', pos + pat.size());
        if (pos == std::string::npos) break;
        // 配列なら中の文字列を全部、単一なら1つだけ
        size_t p = body.find_first_not_of(" \t\r\n", pos + 1);
        if (p == std::string::npos) break;
        if (body[p] == '[') {
            const size_t end = body.find(']', p);
            size_t q = p;
            while (true) {
                q = body.find('"', q + 1);
                if (q == std::string::npos || q > end) break;
                const size_t r = body.find('"', q + 1);
                if (r == std::string::npos || r > end) break;
                out.push_back(json_unescape(body.substr(q + 1, r - q - 1)));
                q = r;
            }
            pos = end;
        } else if (body[p] == '"') {
            const size_t r = body.find('"', p + 1);
            if (r == std::string::npos) break;
            out.push_back(json_unescape(body.substr(p + 1, r - p - 1)));
            pos = r;
        } else pos = p;
    }
    return out;
}

// "key": 123 を拾う。見つからなければ dflt。
long long extract_int(const std::string& body, const std::string& key, long long dflt) {
    const std::string pat = "\"" + key + "\"";
    const size_t k = body.find(pat);
    if (k == std::string::npos) return dflt;
    const size_t colon = body.find(':', k + pat.size());
    if (colon == std::string::npos) return dflt;
    const size_t p = body.find_first_not_of(" \t\r\n", colon + 1);
    if (p == std::string::npos) return dflt;
    if (body[p] != '-' && !std::isdigit(static_cast<unsigned char>(body[p]))) return dflt;
    try { return std::stoll(body.substr(p, 24)); } catch (...) { return dflt; }
}

// トップレベル配列の要素を1つずつ切り出す。
//
// 【重要】以前は "canonical" を目印にして前後で切っていた。それだと
// canonical より前に書かれた項目（"id" など）が、1つ前の要素の切れ端に入る。
//   { "id": 1, "canonical": "A" }, { "id": 2, "canonical": "B" }
//   → 目印で切ると  [id:1, canonical:A, id:2]  となり、A の id が 1 と 2 の両方に見える。
// 波括弧の対応を数えて切る。文字列の中の括弧は数えない。
std::vector<std::string> split_objects(const std::string& body) {
    std::vector<std::string> out;
    int depth = 0;
    size_t start = 0;
    bool in_str = false, esc = false;
    for (size_t i = 0; i < body.size(); ++i) {
        const char c = body[i];
        if (in_str) {
            if (esc)            esc = false;
            else if (c == '\\') esc = true;
            else if (c == '"')  in_str = false;
            continue;
        }
        if (c == '"')      { in_str = true; }
        else if (c == '{') { if (depth == 0) start = i; ++depth; }
        else if (c == '}') { if (depth > 0 && --depth == 0)
                                 out.push_back(body.substr(start, i - start + 1)); }
    }
    return out;
}

struct Master {
    long long   id = 0;   // 0 なら「指定なし」。呼び出し側が採番する。
    std::string canonical;
    std::vector<std::string> variants;
};

std::vector<Master> load_masters(const std::string& path) {
    std::ifstream f(path);
    if (!f) return {};
    std::stringstream ss; ss << f.rdbuf();
    const std::string body = ss.str();

    std::vector<Master> out;
    for (const auto& chunk : split_objects(body)) {
        Master m;
        const auto c = extract_strings(chunk, "canonical");
        if (c.empty()) continue;
        m.canonical = c[0];
        m.variants  = extract_strings(chunk, "variants");
        m.id        = extract_int(chunk, "id", 0);
        out.push_back(std::move(m));
    }
    return out;
}

double ms_since(std::chrono::steady_clock::time_point t0) {
    using namespace std::chrono;
    return duration<double, std::milli>(steady_clock::now() - t0).count();
}

}  // namespace

// 標準入力から1行1件を読み、正規化して返す。
//
// Go から呼ぶための口。正規化の実装を2つ持ってはいけない。
// partners.norm は登録時に計算して保存する設計なので、その計算を
// Go 側で書き直すと、いつか必ず C++ 側とずれる。ずれた瞬間、
// 候補生成が静かに当たらなくなり、原因を探すのが極めて難しくなる。
// 実装は1つに保ち、必要な側から呼ぶ。
//
// 1件ずつプロセスを起動すると、1万件で1万回の起動になる。
// まとめて渡せるようにしてあるのはそのため。
int run_normalize(bool as_json, bool bank) {
    dm::NormOptions opt;
    opt.bank = bank;
    std::vector<std::string> in;
    std::string line;
    while (std::getline(std::cin, line)) {
        if (!line.empty() && line.back() == '\r') line.pop_back();
        in.push_back(line);
    }
    if (as_json) {
        std::cout << "{\"norms\":[";
        for (size_t i = 0; i < in.size(); ++i) {
            if (i) std::cout << ",";
            const std::string n = dm::normalize(in[i], opt);
            std::cout << "\"";
            for (char c : n) {   // JSON として壊れない形にする
                if (c == '"' || c == '\\') std::cout << '\\' << c;
                else if (static_cast<unsigned char>(c) < 0x20) std::cout << ' ';
                else std::cout << c;
            }
            std::cout << "\"";
        }
        std::cout << "]}" << std::endl;
    } else {
        for (const auto& s : in) std::cout << dm::normalize(s, opt) << "\n";
    }
    return 0;
}

int main(int argc, char** argv) {
    std::string masters_path, query;
    int bench = 0, topk = 50;
    double noise_rate = 0.0;   // OCRの誤読を模す割合
    bool as_json = false;      // 機械が読む形で出す（Go から呼ぶとき）
    bool normalize_mode = false;
    // 銀行明細の摘要として正規化する（全銀協略号・取引種別語の除去）
    bool bank_mode = false;
    dm::Weights w;

    for (int i = 1; i < argc; ++i) {
        const std::string a = argv[i];
        auto next = [&]() -> std::string {
            if (i + 1 >= argc) { std::cerr << a << " に値がありません\n"; std::exit(2); }
            return argv[++i];
        };
        if      (a == "--self-test") return run_match_self_test();
        else if (a == "--normalize") normalize_mode = true;
        else if (a == "--bank")      bank_mode = true;
        else if (a == "--masters")   masters_path = next();
        else if (a == "--query")     query = next();
        else if (a == "--bench")     bench = std::stoi(next());
        else if (a == "--topk")      topk = std::stoi(next());
        else if (a == "--w-lev")     w.levenshtein = std::stod(next());
        else if (a == "--w-jac")     w.jaccard = std::stod(next());
        else if (a == "--w-pre")     w.prefix = std::stod(next());
        else if (a == "--w-suf")     w.suffix = std::stod(next());
        else if (a == "--noise")      noise_rate = std::stod(next());
        else if (a == "--json")       as_json = true;
        else { std::cerr << "不明なオプション: " << a << "\n"; return 2; }
    }

    if (normalize_mode) return run_normalize(as_json, bank_mode);

    if (masters_path.empty()) {
        std::cerr << "使い方: dm_match --masters <json> [--query <名前> | --bench <件数>]\n"
                     "        dm_match --self-test\n"
                     "        dm_match --normalize [--bank] [--json]   … 標準入力から1行1件\n";
        return 2;
    }

    auto t0 = std::chrono::steady_clock::now();
    const auto masters = load_masters(masters_path);
    if (masters.empty()) {
        std::cerr << "マスタを読めません: " << masters_path << "\n";
        return 1;
    }
    const double load_ms = ms_since(t0);

    // マスタを索引に載せる。norm と grams はここで一度だけ計算する。
    t0 = std::chrono::steady_clock::now();
    // id は JSON にあればそれを使う。無ければ並び順で採番する。
    //
    // Go から呼ぶときは、DBの partner_id をそのまま入れて渡す。
    // 並び順に依存した対応付けを Go 側でやると、片方の並べ替えを直した
    // 瞬間に、別の取引先へ静かに紐づく。返ってきた id をそのまま
    // DBの主キーとして使えるようにしておく。
    // 表記揺れ（variants）も索引に載せ、最も近い表記のスコアを採る。
    //
    // なぜ必要か（実測で分かったこと）
    //
    //   人が承認画面で「これは株式会社コウヨウ工業だ」と直すと、
    //   そのとき伝票に書かれていた表記が別名として貯まる。
    //   別名があると候補生成（pg_trgm）は正しい取引先を1位に上げる。
    //     コウヨウエ業 で照会 → 株式会社コウヨウ工業 1.000 / 他 0.400
    //
    //   ところが採点を正式名称だけで行うと、そこが変わらない。
    //     コウヨウエ業 vs コウヨウ工業（正式名称）  67.86
    //     コウヨウエ業 vs コウヨウ産業（別会社）    67.86  ← 同点のまま
    //   人が教えたのに次も同じところで止まる。学習した意味が無い。
    //
    // by_id_ は id をキーにした表なので、同じ id で複数登録できない。
    // 内部用の連番を振って索引に入れ、返ってきたら本来の id へ戻す。
    //
    // 【注意】別名は人が明示的に承認したものだけが入る（source=2）。
    // 自動では貯めない。誤った表記を覚えると、そのまま自信を持って
    // 誤承認するようになる。ここは人の判断を経たものに限る。
    struct Entry {
        int64_t     real_id;
        std::string display;   // 正式名称。画面に出すのはこちら
        std::string matched;   // 実際に一致した表記
    };
    std::vector<Entry> entries;
    std::vector<dm::Partner> ps;
    for (size_t i = 0; i < masters.size(); ++i) {
        const int64_t rid = masters[i].id > 0 ? masters[i].id
                                              : static_cast<int64_t>(i + 1);
        // 正式名称を先に入れる。同点なら正式名称が勝つようにするため。
        std::vector<std::string> forms{masters[i].canonical};
        for (const auto& v : masters[i].variants) {
            if (v != masters[i].canonical) forms.push_back(v);
        }
        for (const auto& f : forms) {
            dm::Partner p;
            p.id = static_cast<int64_t>(entries.size() + 1);   // 内部の連番
            p.name = f;
            p.norm = dm::normalize(f);
            p.grams = dm::ngrams(p.norm, 2);
            ps.push_back(std::move(p));
            entries.push_back({rid, masters[i].canonical, f});
        }
    }

    // --bench で正解を突き合わせるのに使う。マスタ1件につき先頭の内部IDを控える。
    std::vector<int64_t> ps_ids;
    ps_ids.reserve(masters.size());
    {
        int64_t last = 0;
        for (size_t e = 0; e < entries.size(); ++e) {
            if (entries[e].real_id != last) {
                ps_ids.push_back(entries[e].real_id);
                last = entries[e].real_id;
            }
        }
    }

    dm::Index idx;
    idx.build(std::move(ps));
    const double build_ms = ms_since(t0);

    std::cerr << "  マスタ " << masters.size() << "件"
              << "  読込 " << std::fixed << std::setprecision(1) << load_ms << "ms"
              << " / 索引 " << build_ms << "ms\n";

    // 内部IDで返ってきた結果を、本来の取引先IDへまとめ直す。
    // 同じ取引先が正式名称と別名の両方で当たるので、高いほうだけを残す。
    auto fold = [&](const std::vector<dm::Candidate>& in, size_t want) {
        struct Row { dm::Candidate c; const Entry* e; };
        std::vector<Row> out;
        std::map<int64_t, size_t> seen;   // 本来のid → out の位置
        for (const auto& c : in) {
            const size_t ix = static_cast<size_t>(c.id) - 1;
            if (ix >= entries.size()) continue;
            const Entry& e = entries[ix];
            auto it = seen.find(e.real_id);
            if (it == seen.end()) {
                seen[e.real_id] = out.size();
                out.push_back({c, &e});
            } else if (c.score > out[it->second].c.score) {
                out[it->second] = {c, &e};
            }
        }
        if (out.size() > want) out.resize(want);
        return out;
    };

    if (!query.empty()) {
        const std::string qn = dm::normalize(query);
        t0 = std::chrono::steady_clock::now();
        // 別名のぶん候補が水増しされるので、多めに取ってから畳む。
        // topk ちょうどを取ると、1つの取引先が別名で枠を埋めてしまい、
        // 別の取引先が押し出される。
        const auto raw = idx.search(qn, topk * 4, w);
        const auto res = fold(raw, static_cast<size_t>(topk));
        const double ms = ms_since(t0);
        std::cerr << "  照会 [" << query << "] → 正規形 [" << qn << "]"
                  << "  " << std::setprecision(3) << ms << "ms\n\n";
        if (as_json) {
            // 人が読む表を機械で解析しようとすると必ず壊れる。
            // 呼び出し側にはこちらを使わせる。
            std::cout << "{\"query\":\"" << query << "\",\"norm\":\"" << qn
                      << "\",\"ms\":" << ms << ",\"results\":[";
            for (size_t i = 0; i < res.size(); ++i) {
                if (i) std::cout << ",";
                const auto& c = res[i].c;
                std::cout << "{\"id\":" << res[i].e->real_id
                          << ",\"name\":\"" << res[i].e->display << "\""
                          // どの表記で当たったか。正式名称と違えば別名で当たっている。
                          // 画面で「別名で一致」と出せるようにする。
                          << ",\"matched\":\"" << res[i].e->matched << "\""
                          << ",\"score\":" << c.score
                          << ",\"lev\":" << c.levenshtein
                          << ",\"jac\":" << c.jaccard
                          << ",\"pre\":" << c.prefix
                          << ",\"suf\":" << c.suffix << "}";
            }
            std::cout << "]}" << std::endl;
            return 0;
        }
        // std::fixed を付ける。付けないと setprecision が有効桁数の意味になり、
        // 67.86 が "7e+01" と表示される。
        std::cout << std::fixed;
        std::cout << std::setw(7) << "score" << std::setw(8) << "lev"
                  << std::setw(8) << "jac" << std::setw(8) << "pre" << "  name\n";
        for (size_t i = 0; i < std::min<size_t>(10, res.size()); ++i) {
            const auto& c = res[i].c;
            std::cout << std::setw(7) << std::setprecision(1) << c.score
                      << std::setw(8) << std::setprecision(3) << c.levenshtein
                      << std::setw(8) << c.jaccard
                      << std::setw(8) << c.prefix
                      << "  " << res[i].e->display;
            if (res[i].e->matched != res[i].e->display)
                std::cout << "  [" << res[i].e->matched << " で一致]";
            std::cout << "\n";
        }
        return 0;
    }

    if (bench > 0) {
        // 伝票側の名前を作る。マスタの表記揺れから選ぶ。
        // 実際の帳票にはマスタの正式名称ではなく揺れた表記が書かれるため。
        std::mt19937 rng(20260810);
        std::uniform_int_distribution<size_t> pick(0, masters.size() - 1);
        std::vector<std::pair<std::string, int64_t>> queries;   // 正規形, 正解のid
        queries.reserve(bench);
        for (int i = 0; i < bench; ++i) {
            const size_t mi = pick(rng);
            const auto& vs = masters[mi].variants;
            std::string raw = vs.empty() ? masters[mi].canonical
                : vs[rng() % vs.size()];
            // OCRの誤読を模す。生成データは綺麗すぎて、実運用の難しさを表さない。
            // 1文字を別の文字に置き換える／落とす、を確率的に行う。
            if (noise_rate > 0.0) {
                std::uniform_real_distribution<double> ur(0.0, 1.0);
                if (ur(rng) < noise_rate) {
                    // UTF-8 の文字境界を壊さないよう、先頭バイトの位置を集める
                    std::vector<size_t> starts;
                    for (size_t b = 0; b < raw.size(); ++b)
                        if ((raw[b] & 0xC0) != 0x80) starts.push_back(b);
                    if (starts.size() > 2) {
                        const size_t si = 1 + rng() % (starts.size() - 1);
                        const size_t b = starts[si];
                        const size_t e = (si + 1 < starts.size()) ? starts[si + 1]
                                                                  : raw.size();
                        raw.erase(b, e - b);   // 1文字落とす
                    }
                }
            }
            // 正解は索引に載せた id。並び順ではなく id で照合する。
            queries.emplace_back(dm::normalize(raw), ps_ids[mi]);
        }

        size_t top1 = 0, top5 = 0;
        double worst = 0.0;
        t0 = std::chrono::steady_clock::now();
        for (const auto& [qn, want] : queries) {
            const auto t1 = std::chrono::steady_clock::now();
            const auto res = fold(idx.search(qn, topk * 4, w),
                                  static_cast<size_t>(topk));
            worst = std::max(worst, ms_since(t1));
            for (size_t r = 0; r < res.size(); ++r) {
                if (res[r].e->real_id == want) {
                    if (r == 0) ++top1;
                    if (r < 5)  ++top5;
                    break;
                }
            }
        }
        const double total = ms_since(t0);

        std::cout << std::fixed;
        std::cout << "\n  ── バッチ " << bench << "件 × マスタ "
                  << masters.size() << "件 ──\n"
                  << "  合計          " << std::setprecision(0) << total << " ms"
                  << "（目標 30,000 ms）"
                  << (total <= 30000 ? "  ✅" : "  ❌") << "\n"
                  << "  1件あたり平均  " << std::setprecision(3) << total / bench << " ms"
                  << "（目標 50 ms）"
                  << (total / bench <= 50 ? "  ✅" : "  ❌") << "\n"
                  << "  1件あたり最悪  " << worst << " ms\n"
                  << "  正解が1位     " << top1 << " / " << bench
                  << "（" << std::setprecision(2) << 100.0 * top1 / bench << "%）\n"
                  << "  正解が5位以内  " << top5 << " / " << bench
                  << "（" << 100.0 * top5 / bench << "%）\n";
        return 0;
    }

    std::cerr << "--query か --bench を指定してください\n";
    return 2;
}
