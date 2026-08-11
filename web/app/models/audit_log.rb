# 監査ログ。読むだけ。
#
# UPDATE / DELETE は DB 側で拒否される（004_audit.sql のルールと権限剥奪）。
# Rails 側で readonly! を付けるのは、間違えたときに DB まで行かず
# ここで止めて理由が分かるようにするため。防止は DB が担う。
class AuditLog < ApplicationRecord
  belongs_to :organization
  belongs_to :actor, class_name: "User", optional: true

  ACTION = {
    "create" => "受付", "auto_approve" => "自動承認",
    "needs_review" => "要確認へ", "approve" => "承認",
    "update" => "修正", "reject" => "却下"
  }.freeze

  def action_ja = ACTION[action] || action
  def readonly? = true

  # 一覧に出す1行の要約。
  # 何が起きたかを開かずに追えないと、絞り込んだ意味が薄い。
  def summary
    case action
    when "learn_alias"  then "「#{after&.dig("alias")}」を覚えた"
    when "forget_alias" then "「#{before&.dig("alias")}」を取り消した"
    when "auto_approve", "needs_review", "reject"
      s = after&.dig("score")
      s ? "スコア #{s.to_f.round(1)}" : ""
    when "approve", "update"
      b = before&.dig("partner_id")
      a = after&.dig("partner_id")
      b == a ? "取引先 #{a}" : "取引先 #{b || "なし"} → #{a}"
    else ""
    end
  rescue StandardError
    ""
  end
end
