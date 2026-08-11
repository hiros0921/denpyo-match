// 項目抽出。OCRの出力から、取引先名・日付・金額・伝票番号を取り出す。
//
// 帳票の様式ごとに項目の位置は違う。座標で決め打ちすると様式が増えるたびに
// 破綻するので、「手がかりの語 + 位置関係 + 値の形」の3つで当てにいく。
//
//   取引先名   「御中」「様」の直前にある / 上部にある / 法人格を含む
//   日付       年月日の形をしている / 「日付」「発行日」の近く
//   金額       「合計」「金額」の近く / ¥ や , を含む数字 / 最大の数字
//   伝票番号   「No.」「番号」の近く / 英数字の連なり
//   登録番号   「T」＋数字13桁（インボイス制度の適格請求書発行事業者登録番号）
//
// どれも単独では外れる。複数の手がかりに点を付けて合計し、最も高いものを採る。
// そして必ず信頼度を返す。0か1かで答えると、後段の三分岐が機能しない。
#pragma once

#include "ocr.hpp"

#include <string>
#include <vector>

namespace dm {

// 名前は3つ返す。
//
//   IssuerName     発行元。その帳票を作った側
//   RecipientName  宛先。「御中」「様」が付いている側
//   PartnerName    照合に使う側。RecipientName と同じ値を入れる
//
// なぜ分けるか。
//
//   自社が請求書を「出す」ときは、相手＝宛先。得意先マスタと照合する。
//   自社が請求書を「受け取る」ときは、相手＝発行元。宛先は自社なので、
//   そこを照合しても毎回自分に当たるだけで意味がない。
//
//   実物のPDF5枚で確かめたところ、宛先と発行元が左右に並んでいた。
//     株式会社 サンプル商事          ← 宛先（自社）
//     代表 見本 太郎 様
//                       株式会社 みらい配送サービス   ← 発行元
//   「御中／様の左」だけを見ていたので、5枚とも宛先側の、しかも
//   部署名や役職（代表／経理部／総務課）を取引先名として返していた。
//
//   どちらを使うかは帳票の向き（受領か発行か）で決まり、
//   それは画像を見てもわからない。ここでは両方返し、選ぶのは Go 側にする。
//
// PartnerName を残すのは、既存の呼び出し側と100枚の実測を動かさないため。
enum class FieldKey { PartnerName, IssuerName, RecipientName, IssueDate, Total,
                      DocNo, RegNo };
const char* field_key_name(FieldKey k);

struct Field {
    FieldKey    key;
    std::string value;        // 取り出した文字列（正規化前）
    double      confidence = 0.0;   // 0〜1。OCRの信頼度と手がかりの強さの積
    int x = 0, y = 0, w = 0, h = 0; // 承認画面で光らせる位置
    std::string why;          // どの手がかりで選んだか。人が読んで確かめられる
};

struct ExtractResult {
    std::vector<Field> fields;
    const Field* find(FieldKey k) const;
};

// OCRの出力から項目を抽出する。
ExtractResult extract(const OcrResult& ocr, int page_width, int page_height);

// ── 個別の判定。単体でテストできるように外に出す ──

// 「2026年8月10日」「2026/8/10」「R8.8.10」などを ISO 形式に直す。
// 直せなければ空を返す。
std::string parse_date(const std::string& s);

// 「¥1,234,567-」「1,234,567円」などから数値を取り出す。負なら失敗。
long long parse_amount(const std::string& s);

// 伝票番号らしいか。英数字とハイフンだけで、4文字以上。
bool looks_like_doc_no(const std::string& s);

// インボイス制度の登録番号「T」＋13桁を取り出す。無ければ空。
//
// 桁の間の空白とハイフンは飛ばす。OCRは "T9234567890123" を
// "T9234 567890 123" のように分けて返すことがあり、
// そのまま探すと1件も見つからない。
std::string find_reg_no(const std::string& s);

// 「登録番号」と書いてある行から、桁が足りなくても取り出す。
//
// 13桁ちょうどしか拾わないと、12桁しか印字されていない請求書が
// 「記載なし」になる。「番号が無い」と「番号が誤っている」は、
// 現場の対応がまったく違う。前者は免税事業者かもしれないが、
// 後者は必ず先方に問い合わせる。区別できないと機能として成立しない。
std::string find_reg_no_loose(const std::string& s);

}  // namespace dm
