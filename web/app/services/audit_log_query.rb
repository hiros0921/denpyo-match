# 監査ログの絞り込みと検証。
#
# ページ送りに OFFSET を使わない
#
#   OFFSET 50000 は、捨てる5万行を実際に読んでから捨てる。
#   監査ログは消えずに増え続ける表なので、古いところを見にいくほど遅くなる。
#   「id が これより小さいもの」で辿る（キーセット方式）。
#   追記のみの表なので id が実際の順序であり、重複しないので起点に使える。
#
# 検証は表示中の範囲だけ行う
#
#   全件の検証は行数に比例して伸びる。実測（5万件）で 24ms なので、
#   100万件なら約0.5秒、1000万件なら約5秒。画面を開くたびには走らせられない。
#   表示中の範囲だけを見て、全件は人が明示的に実行する。
class AuditLogQuery
  PER_PAGE = 50

  Result = Struct.new(:rows, :next_cursor, :elapsed_ms, keyword_init: true)

  def initialize(organization_id:, params: {})
    @org = organization_id
    @action = params[:action_name].presence
    @actor_id = params[:actor_id].presence
    @target_table = params[:target_table].presence
    @target_id = params[:target_id].presence
    @from = parse_date(params[:from])
    @to = parse_date(params[:to])
    @cursor = params[:cursor].presence
  end

  def call
    t0 = Process.clock_gettime(Process::CLOCK_MONOTONIC)
    rows = AuditLog.where(organization_id: @org)
    rows = rows.where(action: @action) if @action
    rows = rows.where(actor_id: @actor_id) if @actor_id
    rows = rows.where(target_table: @target_table) if @target_table
    rows = rows.where(target_id: @target_id) if @target_id
    # 期間は「その日を含む」で扱う。現場は 8/1〜8/11 と言われたら
    # 8/11 の分も入っていると考える。
    rows = rows.where("occurred_at >= ?", @from.beginning_of_day) if @from
    rows = rows.where("occurred_at <= ?", @to.end_of_day) if @to
    rows = rows.where("audit_logs.id < ?", @cursor) if @cursor

    # 1件多く取って、次のページがあるかを確かめる。
    # 総件数を数えると、絞り込みのたびに全件を数えることになる。
    list = rows.includes(:actor).order(id: :desc).limit(PER_PAGE + 1).to_a
    has_more = list.size > PER_PAGE
    list = list.first(PER_PAGE)

    Result.new(
      rows: list,
      next_cursor: has_more ? list.last.id : nil,
      elapsed_ms: ((Process.clock_gettime(Process::CLOCK_MONOTONIC) - t0) * 1000).round(1)
    )
  end

  # 表示中の範囲だけを検証する。
  #
  # 【重要】範囲の先頭行は、その1つ前の行と繋がっているかを見る必要がある。
  # DB側の関数がその1行を手前から含める作りになっている。
  def self.verify(from_id: nil, to_id: nil)
    t0 = Process.clock_gettime(Process::CLOCK_MONOTONIC)
    rows = ActiveRecord::Base.connection.select_all(
      ActiveRecord::Base.sanitize_sql_array(
        ["SELECT * FROM verify_audit_chain(?, ?)", from_id, to_id]
      )
    ).to_a
    { errors: rows,
      elapsed_ms: ((Process.clock_gettime(Process::CLOCK_MONOTONIC) - t0) * 1000).round(1) }
  end

  # 絞り込みの選択肢。実際に存在する値だけを出す。
  # 使われていない操作を並べても、現場は選びようがない。
  def self.available_actions(organization_id)
    ActiveRecord::Base.connection.select_values(
      ActiveRecord::Base.sanitize_sql_array(
        ["SELECT DISTINCT action FROM audit_logs WHERE organization_id = ? ORDER BY 1",
         organization_id]
      )
    )
  end

  private

  def parse_date(v)
    return nil if v.blank?
    Date.parse(v)
  rescue ArgumentError
    nil
  end
end
