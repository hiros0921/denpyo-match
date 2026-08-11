// PDF をページごとの画像に変換する。
//
// 会計事務所が受け取る請求書は PDF が多い。OpenCV は PDF を読めないので、
// poppler（pdftoppm）で画像に落としてから、いつもの経路に乗せる。
#pragma once

#include <string>
#include <vector>

namespace dm {

// 拡張子と中身の先頭（%PDF）の両方で判定する。
// 名前が .pdf でも中身が画像、という取り違えは現場で起きる。
bool is_pdf(const std::string& path);

// 出力した画像のパスを、ページ順に返す。失敗したら空。
// 理由は why に入れる（例外にしない。壊れたPDFは日常的に来る）。
std::vector<std::string> pdf_to_images(const std::string& pdf_path,
                                       const std::string& out_dir,
                                       int max_pages,
                                       std::string& why);

}  // namespace dm
