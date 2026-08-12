# 事務所の職員。
#
# 【重要】Devise のマイグレーションは作らない。
# users テーブルは Go 側も読み書きするので、スキーマの正は
# db/migrations/*.sql に置いてある（Devise 用の列は 008_devise.sql）。
# `rails generate devise User` を実行するとマイグレーションが作られ、
# 正が2箇所になる。
class User < ApplicationRecord
  # ── :registerable を外している ──
  #
  # 自分でアカウントを作れないようにする。会計事務所向けなので、
  # 職員は必ずどこかの事務所（organization）に属している必要があり、
  # その紐付けを本人の自己申告に任せるわけにいかない。
  # 他人の顧問先の帳票が見える事故は、これで起きる。
  #
  # 実際 users.organization_id は NOT NULL で既定値も無いため、
  # Devise の標準登録画面（メールとパスワードしか送らない）では
  # 必ず失敗していた。リンクは見えるのに押すと失敗する状態だったので、
  # 入口ごと塞いだ。ルート側でも skip している（config/routes.rb）。
  #
  # 【重要】モデルとルートの両方を直すこと。片方だけだと、
  # gem 標準のビューが登録リンクを出したまま、存在しないルートを指す。
  #
  # 職員の追加は管理者が行う。手順は docs/PROGRESS.md に置いてある。
  devise :database_authenticatable,
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
