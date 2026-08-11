# 取引先マスタ。名寄せの正解データ。
class Partner < ApplicationRecord
  belongs_to :client
  has_many :partner_aliases, dependent: :destroy

  # norm は画面から作らない。C++ の正規化を通す必要があるため、
  # 登録は Go 側（cmd/dmseed 相当）か API 経由で行う。
  # ここで Ruby の正規化を書くと、実装が3つ（C++/Go/Ruby）になる。
end
