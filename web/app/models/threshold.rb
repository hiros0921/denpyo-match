# 閾値設定。上書きせず、履歴として積む。
#
# 「この伝票はどの閾値設定で自動承認されたか」を後から辿る要件がある。
# 上書きすると追跡できなくなるので、valid_from / valid_to で世代を作る。
class Threshold < ApplicationRecord
  belongs_to :organization
  belongs_to :client, optional: true
  belongs_to :partner, optional: true
  belongs_to :created_by, class_name: "User", optional: true

  scope :current, -> { where(valid_to: nil) }

  validates :upper, :lower, presence: true,
            numericality: { greater_than_or_equal_to: 0, less_than_or_equal_to: 100 }
  validate  :lower_not_above_upper

  def scope_ja
    return "取引先 #{partner_id}" if partner_id
    return "顧問先 #{client&.name}" if client_id
    "組織全体"
  end

  private

  def lower_not_above_upper
    return if lower.blank? || upper.blank?
    errors.add(:lower, "は上限を超えられません") if lower > upper
  end
end
