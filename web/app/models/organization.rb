# 契約単位。会計事務所・記帳代行業者そのもの。
class Organization < ApplicationRecord
  has_many :users
  has_many :clients
  has_many :thresholds
end
