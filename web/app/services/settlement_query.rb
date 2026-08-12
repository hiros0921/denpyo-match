# 入出金との突合。
#
# 会計事務所がこの画面で知りたいのは順に3つ。
#
#   ① 人が見なければならないのはどれか（要確認）
#   ② 突合相手が無いのはどれか（現金払いか、明細の取り込み漏れか）
#   ③ 自動で紐づいたものは、なぜそう判断されたか
#
# ③は「あとから説明できること」のために要る。スコアの内訳
# （名前・金額・日付）を出さないと、現場は結果を信用できない。
#
# ============================================================
# 【重要】件数と一覧を別のクエリで出す
# ============================================================
#
# 影響範囲の画面で一度やった誤り。一覧を200件で切って、その配列の
# 大きさを件数として出していた。範囲を決めるための画面で、
# 範囲を過小に見せることになる。件数は絞り込み全体を数え、一覧だけを切る。
class SettlementQuery
  LIMIT = 200

  # Go 側の settle.Status と同じ値。ずれると意味が変わる。
  AUTO       = 1
  CONFIRMED  = 2
  REVIEW     = 3
  NONE       = 4
  NONE_FIXED = 5

  LABEL = {
    AUTO       => "自動突合",
    CONFIRMED  => "人が確定",
    REVIEW     => "要確認",
    NONE       => "相手なし",
    NONE_FIXED => "相手なし（確定）"
  }.freeze

  # 人が見るべきもの。既定の絞り込み。
  ATTENTION = [REVIEW, NONE].freeze

  def initialize(organization_id, client_id: nil, only: "attention")
    @org = organization_id
    @client_id = client_id.presence
    @only = only
  end

  def counts
    rows = exec(<<~SQL, base_params)
      SELECT s.status, count(*) AS n
        FROM settlements s
        JOIN documents d  ON d.id = s.document_id
        JOIN clients   cl ON cl.id = d.client_id
       WHERE cl.organization_id = $1
         AND ($2::bigint IS NULL OR d.client_id = $2)
       GROUP BY s.status
    SQL
    rows.each_with_object(Hash.new(0)) { |r, h| h[r["status"].to_i] = r["n"].to_i }
  end

  # 突合そのものが行われていない伝票の数。
  #
  # settlements に行が無い＝金額が読めなかった伝票。
  # 「相手なし」とは意味がまったく違う（読めていないだけ）ので分けて出す。
  # ここを混ぜると、現場は現金取引だと誤解して調べるのをやめてしまう。
  def unsettled_count
    exec(<<~SQL, base_params).first["n"].to_i
      SELECT count(*) AS n
        FROM documents d
        JOIN clients cl ON cl.id = d.client_id
       WHERE cl.organization_id = $1
         AND ($2::bigint IS NULL OR d.client_id = $2)
         AND d.direction = 1
         AND NOT EXISTS (SELECT 1 FROM settlements s WHERE s.document_id = d.id)
    SQL
  end

  def total
    exec(<<~SQL, base_params).first["n"].to_i
      SELECT count(*) AS n
        FROM settlements s
        JOIN documents d  ON d.id = s.document_id
        JOIN clients   cl ON cl.id = d.client_id
       WHERE cl.organization_id = $1
         AND ($2::bigint IS NULL OR d.client_id = $2)
         #{status_filter}
    SQL
  end

  def rows
    exec(<<~SQL, base_params)
      SELECT s.document_id, s.status, s.score, s.why, s.decided_at,
             d.doc_type, d.source_name, d.source_page, d.source_region,
             cl.name AS client_name,
             (SELECT value_text FROM extracted_fields
               WHERE document_id = d.id AND field_key = 'issuer_name' LIMIT 1) AS issuer,
             (SELECT value_text FROM extracted_fields
               WHERE document_id = d.id AND field_key = 'total' LIMIT 1) AS total_text,
             (SELECT value_text FROM extracted_fields
               WHERE document_id = d.id AND field_key = 'issue_date' LIMIT 1) AS issue_date,
             t.transaction_date, t.amount AS tx_amount,
             t.description AS tx_desc, t.source_type AS tx_source
        FROM settlements s
        JOIN documents d  ON d.id = s.document_id
        JOIN clients   cl ON cl.id = d.client_id
        LEFT JOIN transactions t ON t.id = s.transaction_id
       WHERE cl.organization_id = $1
         AND ($2::bigint IS NULL OR d.client_id = $2)
         #{status_filter}
       ORDER BY s.decided_at DESC, s.document_id DESC
       LIMIT #{LIMIT}
    SQL
  end

  # 1伝票ぶんの候補と内訳。承認画面で「なぜ1位か」を見せる。
  def self.candidates(document_id)
    ActiveRecord::Base.connection.exec_query(<<~SQL, "SettleCandidates", [document_id]).to_a
      SELECT c.rank, c.transaction_id, c.score,
             c.name_score, c.amount_score, c.date_score, c.why,
             t.transaction_date, t.amount, t.description, t.source_type
        FROM settlement_candidates c
        JOIN transactions t ON t.id = c.transaction_id
       WHERE c.document_id = $1
       ORDER BY c.rank
    SQL
  end

  # 取り込みの履歴。「いつの明細まで入っているか」が分からないと、
  # 「相手なし」が現金なのか取り込み漏れなのか判断できない。
  def batches
    exec(<<~SQL, base_params)
      SELECT b.id, b.source_type, b.filename, b.row_count, b.skipped_count,
             b.created_at, cl.name AS client_name,
             (SELECT min(transaction_date) FROM transactions WHERE batch_id = b.id) AS from_date,
             (SELECT max(transaction_date) FROM transactions WHERE batch_id = b.id) AS to_date
        FROM import_batches b
        JOIN clients cl ON cl.id = b.client_id
       WHERE cl.organization_id = $1
         AND ($2::bigint IS NULL OR b.client_id = $2)
       ORDER BY b.created_at DESC
       LIMIT 20
    SQL
  end

  def self.label(status) = LABEL[status.to_i] || "不明"

  def self.source_ja(n) = { 1 => "銀行", 2 => "カード", 3 => "仕訳" }[n.to_i] || "不明"

  private

  def base_params = [@org, @client_id]

  def status_filter
    case @only
    when "all"       then ""
    when "attention" then "AND s.status IN (#{ATTENTION.join(',')})"
    else                  "AND s.status = #{@only.to_i}"
    end
  end

  def exec(sql, params)
    ActiveRecord::Base.connection.exec_query(sql, "SettlementQuery", params).to_a
  end
end
