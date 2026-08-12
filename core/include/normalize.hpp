// 取引先名の正規化。
//
// この処理は2箇所で使われる。片方だけ直すと不整合が起きるので、
// 物理的に1箇所にまとめてある。
//   ① マスタ登録・更新時    partners.norm に保存する値を作る
//   ② 照合時                伝票から読んだ名前を正規化する
//
// 照合のたびにマスタ側を正規化してはいけない。1万件に毎回かけると
// それだけで目標の50msを使い切る。マスタ側は事前計算してDBに持つ。
#pragma once

#include <string>
#include <vector>

namespace dm {

// 正規化の各段階を個別に切れるようにする。
// どの段階が効いているかを実測で確かめるため。まとめて1つにしない。
struct NormOptions {
    bool nfkc          = true;   // 全角英数→半角、半角カナ→全角カナ、①→1
    bool strip_space   = true;   // 全角・半角の空白をすべて除去
    bool strip_corp    = true;   // 法人格を除去（前株・後株を問わない）
    bool strip_symbol  = true;   // ・．－ー~ などの記号を除去
    bool to_katakana   = true;   // ひらがな→カタカナ
    bool old_kanji     = true;   // 旧字体→新字体（髙→高 など）

    // 銀行明細の摘要向け。既定で無効。
    //
    //   全銀協の略号を除去する    カ)ミライハイソウ → ミライハイソウ
    //                             ミホンセキユ(カ   → ミホンセキユ
    //   取引種別の語を除去する    フリコミ／フリカエ／デビット
    //
    // 摘要は「ｶ)ﾐﾗｲﾊｲｿｳｻｰﾋﾞｽ」のような形で来る。半角カナは nfkc が
    // 全角へ寄せるので、ここでは寄せたあとの形だけを扱う。
    //
    // 取引先名の正規化と別の旗にするのは、通常の帳票で「カ」で始まる
    // 名前（カネコ商店など）を誤って削らないため。銀行摘要の文脈でだけ
    // カ) は株式会社の略号だと言える。
    bool bank = false;

    // 長音・促音の正規化。既定で無効。
    //
    // 「コーヒー」と「コヒヒ」を同一にしてしまうなど、過剰統合の危険がある。
    // 第4段階で生成データを使って効果を測り、根拠を数字で示してから
    // 有効にするか決める。それまでは切っておく。
    bool normalize_choon = false;
};

// 正規化する。UTF-8 で入れて UTF-8 で返す。
std::string normalize(const std::string& s, const NormOptions& opt = {});

// n-gram を作る。照合の第2段で Jaccard 係数を取るのに使う。
// 文字単位。日本語では単語分割が不安定なので、形態素解析には頼らない。
std::vector<std::string> ngrams(const std::string& normalized, int n = 2);

// 除去した法人格の種類。株式会社と有限会社は別法人なので、
// 「除去したが、種類は覚えておく」ことで、後段で区別できるようにする。
enum class CorpKind { None, Kabushiki, Yugen, Godo, Other };
CorpKind detect_corp(const std::string& s);
const char* corp_name(CorpKind k);

}  // namespace dm
