# 表記揺れの登録。
#   source 1: 手動登録
#   source 2: 承認画面で人が修正したときに自動で貯まったもの
class PartnerAlias < ApplicationRecord
  self.table_name = "partner_aliases"
  belongs_to :partner

  scope :learned, -> { where(source: 2) }
end
