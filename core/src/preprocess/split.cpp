#include "split.hpp"

#include <opencv2/imgproc.hpp>

#include <algorithm>
#include <vector>

namespace dm {
namespace {

// ── しきい値 ──
//
// すべて画像の大きさに対する割合で持つ。画素で書くと、
// 150dpi と 300dpi で挙動が変わる。実測は 150dpi の A4（1240×1755）で取った。

// 縦の帯（左右に分ける）。レシート2枚並びの実測は 2.4%。
// 請求書1枚では中央付近に縦の帯が1つも出なかったので、余裕を持って低めにできる。
constexpr double kMinGapW = 0.015;

// 横の帯（上下に分ける）。今は使わない。
//
// 【重要】0 は「切らない」を意味する。値を上げる前に必ず実測すること。
//
// 1枚の伝票の中にも横の空きは普通にある。中央付近の空きの実測:
//   実物の請求書                     3.2%
//   生成した検証データ               4.0%
//   レシート1枚の中（破線の上下）    4.3%
//   行数の少ないレシート             6.9%   ← これで1枚が2つに割れた
//
// 一方、上下に積まれた伝票の正例は、まだ1枚も手元に無い。
// 誤って切る例だけがあって、正しく切る例が無い状態でしきい値を決めると、
// 決めた値には何の根拠も無い。
//
// 切りすぎは切らないより悪い。1枚の請求書を2つに割ると、
// 両方とも金額も取引先も欠けた伝票になり、2件とも人が見ることになる。
// しかも元が1枚だったことは画面から分からない。
//
// 上下に並べてスキャンした実物が手に入ったら、そのときに測って決める。
constexpr double kMinGapH = 0.0;

// 端に寄った帯では切らない。余白は必ず端に出るので、
// これが無いと「余白と中身」に分けてしまう。
// 生成データで中身の右端 96.6% の位置に 3.4% の帯が出ていた。
constexpr double kEdge = 0.12;

// 分けたあと、片側の中身が薄すぎるなら切らない。
// 罫線1本だけの側を伝票として扱わないため。
constexpr double kMinInkShare = 0.15;

// 切りすぎの歯止め。1枚の紙に載る伝票の数には常識的な上限がある。
constexpr int kMaxDepth = 3;
constexpr int kMaxRegions = 8;

// インクとみなす画素を数えた投影。tol 以下を「空白」とする。
// 完全な0にしないのは、枠線やスキャンの汚れが1〜2画素残るため。
// 実測: レシートの境目は 0〜2 だった（枠線の横棒がわずかに入る）。
std::vector<int> profile(const cv::Mat& bin, bool by_column) {
    std::vector<int> p(by_column ? bin.cols : bin.rows, 0);
    for (int y = 0; y < bin.rows; ++y) {
        const uchar* row = bin.ptr<uchar>(y);
        for (int x = 0; x < bin.cols; ++x) {
            if (row[x] == 0) continue;
            if (by_column) p[x]++; else p[y]++;
        }
    }
    return p;
}

struct Gap { int start = 0, len = 0; };

std::vector<Gap> find_gaps(const std::vector<int>& p, int tol) {
    std::vector<Gap> out;
    int s = -1;
    for (size_t i = 0; i < p.size(); ++i) {
        if (p[i] <= tol) {
            if (s < 0) s = static_cast<int>(i);
        } else if (s >= 0) {
            out.push_back({s, static_cast<int>(i) - s});
            s = -1;
        }
    }
    if (s >= 0) out.push_back({s, static_cast<int>(p.size()) - s});
    return out;
}

// 中身のある範囲まで詰める。四方の余白を落とす。
cv::Rect trim(const cv::Mat& bin, cv::Rect r) {
    const cv::Mat sub = bin(r);
    const auto col = profile(sub, true);
    const auto row = profile(sub, false);
    const int tolc = std::max(1, static_cast<int>(sub.rows * 0.004));
    const int tolr = std::max(1, static_cast<int>(sub.cols * 0.004));

    int x0 = 0, x1 = static_cast<int>(col.size());
    while (x0 < x1 && col[x0] <= tolc) ++x0;
    while (x1 > x0 && col[x1 - 1] <= tolc) --x1;
    int y0 = 0, y1 = static_cast<int>(row.size());
    while (y0 < y1 && row[y0] <= tolr) ++y0;
    while (y1 > y0 && row[y1 - 1] <= tolr) --y1;

    if (x1 <= x0 || y1 <= y0) return r;   // 真っ白。詰めない
    return cv::Rect(r.x + x0, r.y + y0, x1 - x0, y1 - y0);
}

long long ink_of(const cv::Mat& bin, cv::Rect r) {
    return cv::countNonZero(bin(r));
}

// 一番広い帯を1つ選ぶ。条件を満たすものが無ければ len=0 を返す。
Gap best_gap(const std::vector<int>& p, int tol, double min_frac) {
    const int n = static_cast<int>(p.size());
    const int need = std::max(6, static_cast<int>(n * min_frac));
    Gap best;
    for (const auto& g : find_gaps(p, tol)) {
        if (g.len < need) continue;
        // 帯の中心が端に寄っていたら使わない
        const double c = (g.start + g.len / 2.0) / n;
        if (c < kEdge || c > 1.0 - kEdge) continue;
        if (g.len > best.len) best = g;
    }
    return best;
}

void split_rec(const cv::Mat& bin, cv::Rect r, int depth,
               long long total_ink, std::vector<cv::Rect>& out) {
    if (static_cast<int>(out.size()) >= kMaxRegions) { out.push_back(r); return; }
    if (depth >= kMaxDepth) { out.push_back(r); return; }

    const cv::Mat sub = bin(r);
    const int tolc = std::max(1, static_cast<int>(sub.rows * 0.004));
    const int tolr = std::max(1, static_cast<int>(sub.cols * 0.004));

    const Gap gv = best_gap(profile(sub, true), tolc, kMinGapW);
    Gap gh;
    if (kMinGapH > 0.0) gh = best_gap(profile(sub, false), tolr, kMinGapH);

    // 縦を優先する。
    //
    // 【重要】「広いほう」で選ばない。横の帯は1枚の伝票の中にも普通にあり、
    // 割合で見ると縦の帯より広くなる。広いほうを採ると、
    // 1枚の請求書を上下に割る。縦の帯は1枚の伝票の中にはまず現れない。
    bool did = false;
    if (gv.len > 0) {
        const int cut = gv.start + gv.len / 2;
        cv::Rect a(r.x, r.y, cut, r.height);
        cv::Rect b(r.x + cut, r.y, r.width - cut, r.height);
        a = trim(bin, a); b = trim(bin, b);
        if (ink_of(bin, a) >= total_ink * kMinInkShare &&
            ink_of(bin, b) >= total_ink * kMinInkShare) {
            split_rec(bin, a, depth + 1, total_ink, out);
            split_rec(bin, b, depth + 1, total_ink, out);
            did = true;
        }
    }
    if (!did && gh.len > 0) {
        const int cut = gh.start + gh.len / 2;
        cv::Rect a(r.x, r.y, r.width, cut);
        cv::Rect b(r.x, r.y + cut, r.width, r.height - cut);
        a = trim(bin, a); b = trim(bin, b);
        if (ink_of(bin, a) >= total_ink * kMinInkShare &&
            ink_of(bin, b) >= total_ink * kMinInkShare) {
            split_rec(bin, a, depth + 1, total_ink, out);
            split_rec(bin, b, depth + 1, total_ink, out);
            did = true;
        }
    }
    if (!did) out.push_back(r);
}

}  // namespace

std::vector<Region> split_documents(const cv::Mat& gray) {
    std::vector<Region> res;
    if (gray.empty()) return res;

    // 【重要】ここでぼかさない。
    //
    // 粒ノイズを落とすつもりで medianBlur(3) を入れたところ、
    // レシートの中の細い破線（1〜2画素）まで消えた。
    // 破線の上下にある 39px ずつの空白がつながって 80px の帯になり、
    // 1枚のレシートを上下に割った（10件のはずが16件）。
    //
    // 粒ノイズは投影のしきい値（tol）で吸収する。
    // 1個の粒が列に足すのは1〜2画素で、tol は高さの0.4%（150dpiのA4で7画素）。
    cv::Mat bin;
    cv::threshold(gray, bin, 0, 255,
                  cv::THRESH_BINARY_INV | cv::THRESH_OTSU);

    // 【重要】二値化が失敗したときは分けない。
    // 影の濃い写真では大津の方法が紙全体をインクだと判断することがある。
    // その状態で投影を見ても意味が無く、でたらめな位置で切ってしまう。
    // 「分けられない」は正常な結果なので、ここで諦めてよい。
    const long long total = cv::countNonZero(bin);
    const long long area = static_cast<long long>(bin.rows) * bin.cols;
    if (total == 0 || total > area * 4 / 10) {
        res.push_back({0, 0, gray.cols, gray.rows});
        return res;
    }

    std::vector<cv::Rect> rects;
    split_rec(bin, trim(bin, cv::Rect(0, 0, bin.cols, bin.rows)), 0, total, rects);

    // 読む順に並べる。上から、同じ高さなら左から。
    std::sort(rects.begin(), rects.end(), [](const cv::Rect& a, const cv::Rect& b) {
        // 縦のずれが小さければ同じ段とみなす
        if (std::abs(a.y - b.y) < std::max(a.height, b.height) / 2) return a.x < b.x;
        return a.y < b.y;
    });

    for (const auto& r : rects) res.push_back({r.x, r.y, r.width, r.height});
    return res;
}

}  // namespace dm
