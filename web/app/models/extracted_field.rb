# 抽出した項目。bbox は承認画面で該当箇所を光らせるのに使う。
class ExtractedField < ApplicationRecord
  belongs_to :document

  LABEL = {
    "partner_name"   => "取引先名",
    "issuer_name"    => "発行元",
    "recipient_name" => "宛先",
    "issue_date"     => "日付",
    "total"          => "金額",
    "doc_no"         => "伝票番号"
  }.freeze

  def label = LABEL[field_key] || field_key

  # 信頼度は 0〜100 で入っている。
  # 【注意】この値では正誤を分けられない（第5段階の実測: 正解0.53 / 不正解0.49）。
  # 参考として出すだけで、判定には使わない。判定は照合スコアで行う。
  def confidence_pct = confidence.to_f
end
