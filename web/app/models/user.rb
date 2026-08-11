# 事務所の職員。
#
# 【重要】Devise のマイグレーションは作らない。
# users テーブルは Go 側も読み書きするので、スキーマの正は
# db/migrations/*.sql に置いてある（Devise 用の列は 008_devise.sql）。
# `rails generate devise User` を実行するとマイグレーションが作られ、
# 正が2箇所になる。
class User < ApplicationRecord
  devise :database_authenticatable, :registerable,
         :recoverable, :rememberable, :validatable, :trackable

  belongs_to :organization

  ROLE_MEMBER = 1
  ROLE_ADMIN  = 2

  def admin? = role == ROLE_ADMIN

  # 閾値を変えられるのは管理者だけ。
  # 「どの精度なら人の確認を省いてよいか」は事務所の責任の線引きであって、
  # 個々の担当者が自分の手元で下げられる設定ではない。
  def can_edit_threshold? = admin?

  # 契約の手続きができるのは管理者だけ。
  # 支払い方法の変更や解約は事務所の意思決定であって、
  # 担当者が自分の判断で行う操作ではない。
  def can_manage_billing? = admin?
end
