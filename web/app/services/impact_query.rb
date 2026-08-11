# 誤認識の影響範囲。
#
# 誤りが見つかったとき、現場が最初に知りたいのは2つ。
#
#   ① 同じ誤りが何件に及んだか
#   ② そのうち何件が人の目を通らずに確定したか   ← こちらが本質
#
# ②が本質なのは、要確認に回っていれば人が見ているから。
# 自動承認された分だけは誰も見ていない。件数の合計より、この内訳を先に出す。
#
# 起点は3つ。
#
#   partner    この取引先に紐づいた伝票は全部おかしいかもしれない
#   alias      誤って覚えた表記が、どの伝票に効いたか
#   threshold  緩すぎた設定の期間に自動承認されたものを洗い直す
#
# ============================================================
# 【重要】件数と一覧を別のクエリで出す
# ============================================================
#
# 最初、一覧を200件で切って、その配列の大きさを件数として出していた。
# 影響が5,000件あっても画面には「合計200」と表示される。
# 見直しの範囲を決めるための画面で、範囲を過小に見せることになる。
# **この画面でいちばんやってはいけない誤り。**
#
# 件数は絞り込みをかけた全体を数え、一覧だけを切る。
#
# 実測（照合候補50万件・照合結果1万件）
#
#   取引先で辿る    0.039 ms
#   閾値設定で辿る  0.424 ms
#   表記で辿る      0.045 ms （索引を足す前は 46.050 ms の全件走査）
class ImpactQuery
  LIMIT = 200

  Summary = Struct.new(
    :total, :auto, :review, :reject, :approved_by_human,
    :first_at, :last_at, :rows, :truncated, :elapsed_ms, keyword_init: true
  )

  KINDS = {
    "partner"   => "取引先",
    "alias"     => "覚えた表記",
    "threshold" => "閾値設定"
  }.freeze

  def initialize(organization_id:, kind:, value:)
    @org = organization_id
    @kind = kind
    @value = value
  end

  def call
    return nil if @value.blank? || !KINDS.key?(@kind)

    t0 = Process.clock_gettime(Process::CLOCK_MONOTONIC)
    counts = ActiveRecord::Base.connection.select_one(
      ActiveRecord::Base.sanitize_sql_array([count_sql, *binds])
    )
    rows = ActiveRecord::Base.connection.select_all(
      ActiveRecord::Base.sanitize_sql_array([list_sql, *binds])
    ).to_a
    elapsed = ((Process.clock_gettime(Process::CLOCK_MONOTONIC) - t0) * 1000).round(1)

    total = counts["total"].to_i
    Summary.new(
      total: total,
      auto: counts["auto"].to_i,
      review: counts["review"].to_i,
      reject: counts["reject"].to_i,
      approved_by_human: counts["by_human"].to_i,
      first_at: counts["first_at"],
      last_at: counts["last_at"],
      rows: rows,
      truncated: total > rows.size,
      elapsed_ms: elapsed
    )
  end

  private

  # 起点ごとに辿り方が違うが、絞り込みの条件は1箇所にまとめる。
  # 件数と一覧で条件がずれると、「合計と内訳が合わない」画面になる。
  #
  # 【注意】どの経路でも、組織の絞り込み（clients.organization_id）を必ず通す。
  # 影響範囲の調査は普段より広く見る操作なので、
  # ここで絞り忘れると他の事務所のデータまで出る。
  def from_where
    base = <<~SQL
      FROM match_results mr
      JOIN documents d ON d.id = mr.document_id
      JOIN clients   c ON c.id = d.client_id
      LEFT JOIN partners p ON p.id = mr.partner_id
      -- 1位の候補。rank に一意制約があるので、この結合で行数は増えない
      -- （増えると件数が二重に数えられ、影響範囲を過大に見せる）。
      LEFT JOIN match_candidates mc
             ON mc.document_id = mr.document_id AND mc.rank = 1
      WHERE c.organization_id = ?
    SQL
    case @kind
    when "partner"   then base + " AND mr.partner_id = ?"
    when "threshold" then base + " AND mr.threshold_id = ?"
    when "alias"     then base + " AND mc.score_detail->>'matched' = ?"
    end
  end

  def binds
    v = @kind == "alias" ? @value.to_s : @value.to_i
    [@org, v]
  end

  def count_sql
    <<~SQL
      SELECT count(*) AS total,
             count(*) FILTER (WHERE mr.decision = 1) AS auto,
             count(*) FILTER (WHERE mr.decision = 5) AS review,
             count(*) FILTER (WHERE mr.decision = 4) AS reject,
             count(*) FILTER (WHERE mr.decision IN (2,3)) AS by_human,
             min(mr.decided_at) AS first_at,
             max(mr.decided_at) AS last_at
      #{from_where}
    SQL
  end

  def list_sql
    <<~SQL
      SELECT mr.document_id, mr.decision, mr.score, mr.decided_at,
             d.client_id, c.name AS client_name, d.doc_type,
             p.name AS partner_name,
             mc.score_detail->>'matched' AS matched_form
      #{from_where}
      -- 自動承認を先に出す。誰も見ていない分から確認したいため。
      ORDER BY (mr.decision = 1) DESC, mr.decided_at DESC
      LIMIT #{LIMIT}
    SQL
  end
end
