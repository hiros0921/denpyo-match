# 確定した突合結果。どの閾値で判定されたかを必ず持つ。
class MatchResult < ApplicationRecord
  belongs_to :document
  belongs_to :partner, optional: true
  belongs_to :threshold, optional: true
  belongs_to :decided_by, class_name: "User", optional: true

  AUTO_APPROVE = 1
  HUMAN_APPROVE = 2
  HUMAN_UPDATE = 3
  REJECT = 4
  NEEDS_REVIEW = 5

  DECISION = {
    1 => "自動承認", 2 => "承認", 3 => "修正", 4 => "却下", 5 => "要確認"
  }.freeze

  def decision_ja = DECISION[decision] || "不明"

  # 人がまだ見ていないもの。承認キューに出す対象。
  scope :pending, -> { where(decision: [NEEDS_REVIEW, REJECT]) }
  scope :auto,    -> { where(decision: AUTO_APPROVE) }
end
