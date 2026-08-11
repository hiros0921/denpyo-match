#include "preprocess.hpp"

#include <algorithm>
#include <cmath>
#include <numeric>

namespace dm {
namespace {

// 角度の根拠として最低限必要な罫線の本数。これを下回ったら測定不能とする。
constexpr size_t MIN_ANGLE_SAMPLES = 3;

// 角度の代表値に中央値を使う。平均だと、1本の誤検出に引きずられる。
double median(std::vector<double>& v) {
    if (v.empty()) return 0.0;
    const size_t mid = v.size() / 2;
    std::nth_element(v.begin(), v.begin() + mid, v.end());
    return v[mid];
}

// 罫線から傾きを測る。文字からは測らない。
//
// 文字を使わない理由: 日本語は縦画・横画が混在し、明朝体では横画が細い。
// 「文字の並び」から角度を取る手法は、縦書きが混ざると破綻する。
// 帳票には必ず罫線があり、それは設計上まっすぐなので、罫線の方が確実。
//
// 【失敗の記録】形態素処理で罫線だけを抜き出してから Hough にかける、という
// 変更を試したが明確な改悪だった。20枚での合格が 16枚 → 2枚 に落ちた。
// 原因は二重の絞り込み。開いた後の線は元より短く途切れるのに、そこへさらに
// 「画像幅の25%以上」という長さ条件をかけていた。ほとんどの線が条件を満たさず、
// 16枚が測定不能になった。Canny に戻してある。
//
// Canny の弱点（文字の輪郭が短い辺として大量に出て投票が分散する）は残るが、
// 実測では 20枚中18枚 が誤差0.1度以内に収まる。残り2枚は角度が小さく
// （-0.417度 / -0.307度）、補正しなくても後段への影響が小さい。
double detect_angle(const cv::Mat& gray, const Options& opt, int& n_samples) {
    cv::Mat edges;
    cv::Canny(gray, edges, 50, 150, 3);

    const int min_len = static_cast<int>(gray.cols * opt.min_line_ratio);
    std::vector<cv::Vec4i> lines;
    cv::HoughLinesP(edges, lines, 1, CV_PI / 720.0, 100, min_len, 20);

    std::vector<double> angles;
    angles.reserve(lines.size());
    for (const auto& l : lines) {
        const double dx = l[2] - l[0];
        const double dy = l[3] - l[1];
        if (std::abs(dx) < 1e-6) continue;                 // 垂直線は使わない
        double deg = std::atan2(dy, dx) * 180.0 / CV_PI;
        // 水平に近い線だけを採用する。縦罫線を混ぜると分布が二山になる
        if (std::abs(deg) > opt.max_angle_deg) continue;
        angles.push_back(deg);
    }

    n_samples = static_cast<int>(angles.size());
    // 根拠が薄いときは回さない。0 を返すが、n_samples で判別できるようにする。
    // 「傾き0度」と「傾きを測れなかった」を呼び出し側が区別できないと、
    // 検証で「正解0.4度を0.0度と誤検出」なのか「測定不能」なのか分からない。
    if (angles.size() < MIN_ANGLE_SAMPLES) return 0.0;
    return median(angles);
}

}  // namespace

bool preprocess(const cv::Mat& src, const Options& opt, Result& out, std::string& why) {
    if (src.empty()) { why = "画像が空です"; return false; }

    // ── ① グレースケール化 ──
    if (src.channels() == 3) cv::cvtColor(src, out.gray, cv::COLOR_BGR2GRAY);
    else                     out.gray = src.clone();

    // ── ② ノイズ除去 ──
    // 二値化より先に行う。順序を逆にすると、ノイズが黒点として二値に焼き付き、
    // あとから消せなくなる。
    cv::fastNlMeansDenoising(out.gray, out.denoised, opt.denoise_h, 7, 21);

    // ── ③ 傾き検出・補正 ──
    // 二値化より先に行う。二値化してから回すと、回転の補間で中間色が生まれ、
    // せっかくの二値が二値でなくなる。
    out.angle_deg = detect_angle(out.denoised, opt, out.angle_samples);
    if (std::abs(out.angle_deg) > 0.05) {
        const cv::Point2f c(out.denoised.cols / 2.0f, out.denoised.rows / 2.0f);
        const cv::Mat m = cv::getRotationMatrix2D(c, out.angle_deg, 1.0);
        // 余白は白で埋める。黒にすると、紙の縁を罫線と誤検出する
        cv::warpAffine(out.denoised, out.deskewed, m, out.denoised.size(),
                       cv::INTER_CUBIC, cv::BORDER_CONSTANT, cv::Scalar(255));
    } else {
        out.deskewed = out.denoised.clone();
    }

    // ── ④ 適応的二値化 ──
    // 大域的な閾値（threshold + OTSU）は使わない。スキャナの影や裏写りで
    // 明るさが片側に偏るため、1つの閾値では必ずどちらかが潰れる。
    int bs = opt.block_size | 1;                 // 必ず奇数にする
    cv::adaptiveThreshold(out.deskewed, out.binary, 255,
                          cv::ADAPTIVE_THRESH_GAUSSIAN_C, cv::THRESH_BINARY,
                          bs, opt.c_offset);

    // ── ⑤ 罫線検出 ──
    // 横長・縦長のカーネルで別々に開く。1つのカーネルでは両方拾えない。
    cv::Mat inv;
    cv::bitwise_not(out.binary, inv);            // 罫線を白にして形態素処理する

    const int hlen = std::max(10, static_cast<int>(inv.cols * opt.line_kernel_ratio));
    const int vlen = std::max(10, static_cast<int>(inv.rows * opt.line_kernel_ratio));
    cv::Mat hk = cv::getStructuringElement(cv::MORPH_RECT, {hlen, 1});
    cv::Mat vk = cv::getStructuringElement(cv::MORPH_RECT, {1, vlen});

    cv::Mat hmask, vmask;
    cv::morphologyEx(inv, hmask, cv::MORPH_OPEN, hk);
    cv::morphologyEx(inv, vmask, cv::MORPH_OPEN, vk);

    cv::HoughLinesP(hmask, out.lines.horizontal, 1, CV_PI / 180, 80, hlen / 2, 20);
    cv::HoughLinesP(vmask, out.lines.vertical,   1, CV_PI / 180, 80, vlen / 2, 20);

    // ── ⑥ 罫線除去 ──
    // 座標は out.lines に残す。表の構造情報として⑦で使うため、消して終わりにしない。
    cv::Mat lines_mask;
    // 少し太らせてから消す。細いままだと罫線の縁が残る。
    //
    // 縦横で別々の長いカーネルを試したが、効果は無かった（0.866% → 0.865%）。
    // そもそも「白紙に残るノイズ」だと思っていたものの正体は、備考欄の文字だった。
    // 座標で「ここは白紙のはず」と決め打ちした評価が誤っていた。
    // 正解画像と画素単位で比べる評価に直したところ、余分な黒は平均0.159%しかなく、
    // 解こうとしていた問題が最初から存在しなかった。単純な実装に戻してある。
    cv::bitwise_or(hmask, vmask, lines_mask);
    cv::dilate(lines_mask, lines_mask,
               cv::getStructuringElement(cv::MORPH_RECT, {3, 3}));
    out.cleaned = out.binary.clone();
    out.cleaned.setTo(255, lines_mask);          // 罫線だった画素を紙の白に戻す

    // 孤立点の除去。
    // 裏写りと影を適応的二値化が拾い、白紙であるべき領域に黒点が散る。
    // 実測: 白紙の左下に黒画素が 2.88% 残っていた（列ごとの黒画素は
    // 最大52・中央値13 と散在しており、線ではなく点であることを確認済み）。
    // 文字を消さずに点だけ消すため、連結成分の面積で切る。
    // モルフォロジーのオープンは使わない。細い文字（ー、一、｜）まで消えるため。
    {
        cv::Mat inv_clean;
        cv::bitwise_not(out.cleaned, inv_clean);
        cv::Mat labels, stats, centroids;
        const int n = cv::connectedComponentsWithStats(inv_clean, labels, stats,
                                                       centroids, 8, CV_32S);
        cv::Mat specks = cv::Mat::zeros(inv_clean.size(), CV_8U);
        for (int i = 1; i < n; ++i) {
            const int area = stats.at<int>(i, cv::CC_STAT_AREA);
            const int ww   = stats.at<int>(i, cv::CC_STAT_WIDTH);
            const int hh   = stats.at<int>(i, cv::CC_STAT_HEIGHT);
            // 面積が小さく、かつ縦横とも短いものだけを点とみなす。
            // 面積だけで切ると、細く長い文字の一部を消してしまう。
            if (area <= opt.speck_area && ww <= opt.speck_size && hh <= opt.speck_size) {
                specks.setTo(255, labels == i);
            }
        }
        out.speck_removed = cv::countNonZero(specks);
        out.cleaned.setTo(255, specks);
    }

    // ── ⑦ セルの切り出し ──
    // 横罫線と縦罫線の交点からセルを構成する。
    cv::Mat grid;
    cv::bitwise_and(hmask, vmask, grid);         // 交点だけが残る
    cv::Mat cells_mask;
    cv::bitwise_or(hmask, vmask, cells_mask);
    cv::morphologyEx(cells_mask, cells_mask, cv::MORPH_CLOSE,
                     cv::getStructuringElement(cv::MORPH_RECT, {5, 5}));

    std::vector<std::vector<cv::Point>> contours;
    cv::findContours(cells_mask, contours, cv::RETR_LIST, cv::CHAIN_APPROX_SIMPLE);
    for (const auto& c : contours) {
        cv::Rect r = cv::boundingRect(c);
        // 極端に小さい・細長いものは文字の断片。セルとして扱わない
        if (r.width < 40 || r.height < 20) continue;
        if (r.width > out.binary.cols * 0.98 && r.height > out.binary.rows * 0.98) continue;
        out.cells.push_back(r);
    }
    std::sort(out.cells.begin(), out.cells.end(),
              [](const cv::Rect& a, const cv::Rect& b) {
                  if (std::abs(a.y - b.y) > 15) return a.y < b.y;
                  return a.x < b.x;
              });

    return true;
}

cv::Mat make_contact_sheet(const Result& r, const cv::Mat& src) {
    // 処理前後を目で比べられるように、6枚を横に並べる。
    auto to_bgr = [](const cv::Mat& m) {
        cv::Mat o;
        if (m.channels() == 1) cv::cvtColor(m, o, cv::COLOR_GRAY2BGR);
        else                   o = m.clone();
        return o;
    };
    const int h = 700;
    std::vector<std::pair<std::string, cv::Mat>> steps = {
        {"1 original", to_bgr(src)},
        {"2 denoised", to_bgr(r.denoised)},
        {"3 deskewed", to_bgr(r.deskewed)},
        {"4 binary",   to_bgr(r.binary)},
        {"5 cleaned",  to_bgr(r.cleaned)},
    };
    // 罫線とセルを重ねた図も足す
    cv::Mat overlay = to_bgr(r.deskewed);
    for (const auto& l : r.lines.horizontal)
        cv::line(overlay, {l[0], l[1]}, {l[2], l[3]}, {0, 0, 255}, 2);
    for (const auto& l : r.lines.vertical)
        cv::line(overlay, {l[0], l[1]}, {l[2], l[3]}, {255, 0, 0}, 2);
    for (const auto& c : r.cells)
        cv::rectangle(overlay, c, {0, 200, 0}, 2);
    steps.emplace_back("6 lines+cells", overlay);

    std::vector<cv::Mat> tiles;
    for (auto& [name, img] : steps) {
        cv::Mat t;
        const double s = static_cast<double>(h) / img.rows;
        cv::resize(img, t, {}, s, s, cv::INTER_AREA);
        cv::copyMakeBorder(t, t, 34, 8, 4, 4, cv::BORDER_CONSTANT, {40, 40, 40});
        cv::putText(t, name, {8, 24}, cv::FONT_HERSHEY_SIMPLEX, 0.6,
                    {255, 255, 255}, 1, cv::LINE_AA);
        tiles.push_back(t);
    }
    cv::Mat sheet;
    cv::hconcat(tiles, sheet);
    return sheet;
}

}  // namespace dm
