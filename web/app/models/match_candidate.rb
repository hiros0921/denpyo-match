# 照合候補とスコア。★閾値シミュレーションの元データ。
#
# この表を消すと、スライダーを動かすたびに1万件を照合し直すことになる。
# 保存済みのスコアを集計するだけ、という設計が成立しなくなる。
class MatchCandidate < ApplicationRecord
  belongs_to :document
  belongs_to :partner

  scope :top, -> { where(rank: 1) }
end
