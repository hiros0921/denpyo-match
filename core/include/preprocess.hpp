// 帳票画像の前処理。
//
// 処理の順序が結果を左右する。順序を変えてはいけない理由を各段階に書いた。
//
// この層は「画像を受け取り、補正済み画像と、その過程で得た情報を返す」
// だけを担う。OCRも照合もしない。CLIとして単体で動くこと（仕様書の要件）。
#pragma once

#include <opencv2/opencv.hpp>
#include <string>
#include <vector>

namespace dm {

// 検出した罫線。除去はするが座標は捨てない。
// 「金額欄はどのセルか」を決めるのに、表の構造情報として使う。
struct Lines {
    std::vector<cv::Vec4i> horizontal;
    std::vector<cv::Vec4i> vertical;
};

// 前処理の結果。画像だけでなく、判断に使った数値も返す。
// 数値を返さないと、うまくいったのか偶然なのかが分からない。
struct Result {
    cv::Mat gray;        // ① グレースケール
    cv::Mat denoised;    // ② ノイズ除去
    cv::Mat deskewed;    // ③ 傾き補正後
    cv::Mat binary;      // ④ 二値化
    cv::Mat cleaned;     // ⑥ 罫線除去後（OCRに渡すのはこれ）

    double  angle_deg = 0.0;   // ③で検出した傾き。正解と突き合わせて評価する
    int     angle_samples = 0; // 角度の根拠になった罫線の本数
    int     speck_removed = 0; // ⑥で消した孤立点の画素数
    Lines   lines;             // ⑤で検出した罫線
    std::vector<cv::Rect> cells;  // ⑦で切り出したセル
};

struct Options {
    // ② ノイズ除去の強さ。大きいほど消えるが文字も潰れる
    float denoise_h = 7.0f;

    // ③ 傾き検出に使う罫線の最小長（画像幅に対する割合）
    //    短い線を拾うと、文字の一部を罫線と誤認して角度が狂う
    double min_line_ratio = 0.25;
    double max_angle_deg  = 8.0;   // これを超える角度は検出誤りとみなす

    // ④ 適応的二値化。大域的な閾値は使わない（影・裏写りで片側が潰れる）
    int    block_size = 31;        // 奇数。小さすぎると文字が滲む
    double c_offset   = 12.0;

    // ⑤ 罫線検出のカーネル長（画像の辺に対する割合）
    double line_kernel_ratio = 0.04;

    // ⑥ 孤立点の除去。これ以下の面積かつ縦横ともこの大きさ以下なら点とみなす。
    //    モルフォロジーのオープンは使わない（細い文字まで消えるため）
    int    speck_area = 12;
    int    speck_size = 4;

    bool   dump_steps = false;     // 各段階を画像に書き出す（目視確認用）
};

// 前処理を通す。失敗したら例外ではなく false を返し、why に理由を入れる。
bool preprocess(const cv::Mat& src, const Options& opt, Result& out, std::string& why);

// 途中経過を並べた1枚の画像を作る。処理前後を目で比べるため。
cv::Mat make_contact_sheet(const Result& r, const cv::Mat& src);

}  // namespace dm
