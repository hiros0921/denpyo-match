# 閾値シミュレーション。**このプロダクトの核心。**
#
# 何をするものか
#
#   「自動承認の上限を95から90に下げたら、何件が自動承認に回り、
#     そのうち何件が誤りになりそうか」を、その場で見せる。
#
#   OCRの精度を上げることではなく、
#   「どの精度なら人の確認を省いてよいか」を現場が自分で決められることが、
#   この製品の売りになる。だからこの画面が中心にある。
#
# 設計の要点
#
#   ★照合をやり直さない。
#   保存済みの match_candidates を集計するだけで答えを出す。
#   スライダーを動かすたびに1万件を照合し直す作りにすると、
#   1秒どころか分単位になり、この機能自体が成立しない。
#
#   実測: 50万件（伝票1万件 × 候補50件）の集計が 86.9ms（第2段階で確認）。
#
# 【重要】ここが返すのは「もし変えたら」であって、変更の実行ではない。
# 見てから決められることに意味があるので、集計と適用は必ず分ける。
class ThresholdSimulator
  Result = Struct.new(
    :total, :auto, :review, :reject,
    :auto_pct, :review_pct, :reject_pct,
    :elapsed_ms, :distribution, keyword_init: true
  )

  # scope は集計の対象範囲。顧問先を指定しなければ組織全体。
  def initialize(organization_id:, client_id: nil, doc_type: nil)
    @organization_id = organization_id
    @client_id = client_id
    @doc_type = doc_type
  end

  # upper 以上を自動承認、lower 以上 upper 未満を要確認、lower 未満を却下。
  def simulate(upper:, lower:)
    t0 = Process.clock_gettime(Process::CLOCK_MONOTONIC)
    # ? の順番は SQL の並びどおり: upper / lower / upper / lower。
    row = ActiveRecord::Base.connection.select_one(
      ActiveRecord::Base.sanitize_sql_array(
        [count_sql, upper.to_f, lower.to_f, upper.to_f, lower.to_f]
      )
    )
    elapsed = ((Process.clock_gettime(Process::CLOCK_MONOTONIC) - t0) * 1000).round(1)

    total = row["total"].to_i
    auto = row["auto"].to_i
    review = row["review"].to_i
    reject = row["reject"].to_i
    pct = ->(n) { total.zero? ? 0.0 : (n * 100.0 / total).round(1) }

    Result.new(
      total: total, auto: auto, review: review, reject: reject,
      auto_pct: pct.call(auto), review_pct: pct.call(review), reject_pct: pct.call(reject),
      elapsed_ms: elapsed, distribution: distribution
    )
  end

  # スコアの分布。10点刻み。
  #
  # 件数だけ出しても、閾値をどこへ動かせばよいか分からない。
  # 「70〜80に固まっている」と見えて初めて、下げる意味があるか判断できる。
  def distribution
    rows = ActiveRecord::Base.connection.select_all(
      ActiveRecord::Base.sanitize_sql_array([distribution_sql])
    )
    buckets = Array.new(10, 0)
    rows.each { |r| buckets[[r["bucket"].to_i, 9].min] = r["n"].to_i }
    buckets
  end

  private

  # 1位の候補だけを見る。rank=1 を条件にできるので max() より速い。
  #
  # 【注意】まだ照合されていない伝票（候補が1件も無いもの）は
  # この集計に入らない。「20枚入れたのに18枚しか数えられていない」と
  # 見えることがあるが、残り2枚はまだ処理中か、OCRが名前を読めず
  # 候補ゼロだったもの。件数が合わないときは処理待ちを疑う。
  def base_from
    <<~SQL
      FROM match_candidates mc
      JOIN documents d ON d.id = mc.document_id
      JOIN clients  c ON c.id = d.client_id
      WHERE mc.rank = 1
        AND c.organization_id = #{@organization_id.to_i}
        #{"AND d.client_id = #{@client_id.to_i}" if @client_id.present?}
        #{"AND d.doc_type = #{@doc_type.to_i}" if @doc_type.present?}
    SQL
  end

  # 1回のクエリで3つとも数える。3回に分けると3往復になるうえ、
  # その間に処理が終わった伝票が混ざり、合計が内訳と合わなくなる。
  def count_sql
    <<~SQL
      SELECT count(*) AS total,
             count(*) FILTER (WHERE mc.score >= ?) AS auto,
             count(*) FILTER (WHERE mc.score >= ? AND mc.score < ?) AS review,
             count(*) FILTER (WHERE mc.score < ?) AS reject
      #{base_from}
    SQL
  end

  def distribution_sql
    <<~SQL
      SELECT floor(mc.score / 10)::int AS bucket, count(*) AS n
      #{base_from}
      GROUP BY 1 ORDER BY 1
    SQL
  end
end
