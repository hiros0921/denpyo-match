# インボイス登録番号の検査結果を集める。
#
# 会計事務所がこの画面で知りたいのは1つだけ。
#
#   「先方に問い合わせなければならないのは、どれか」
#
# 全件を並べても現場は読まない。既定は「問題があるもの」だけを出す。
# 正常なものは、件数として上に出せば足りる。
#
# ============================================================
# 【重要】件数と一覧を別のクエリで出す
# ============================================================
#
# 誤認識の影響範囲の画面で一度やった誤り。一覧を200件で切って、
# その配列の大きさを件数として出していた。5,000件あっても「200件」と表示される。
# 問い合わせの範囲を決めるための画面で、範囲を過小に見せることになる。
# 件数は絞り込み全体を数え、一覧だけを切る。
class InvoiceRegQuery
  LIMIT = 200

  # 状態。Go 側の invoice.Status と同じ値。ずれると意味が変わる。
  MISSING    = 1
  BAD_FORMAT = 2
  BAD_CHECK  = 3
  FORMAT_OK  = 4
  REGISTERED = 5
  NOT_FOUND  = 6

  LABEL = {
    MISSING    => "記載なし",
    BAD_FORMAT => "形式が違う",
    BAD_CHECK  => "検査数字が合わない",
    FORMAT_OK  => "形式は正しい（実在は未確認）",
    REGISTERED => "登録あり",
    NOT_FOUND  => "登録が見つからない"
  }.freeze

  # 問題があるとみなす状態。Go 側の Status#NeedsAttention と同じ。
  ATTENTION = [MISSING, BAD_FORMAT, BAD_CHECK, NOT_FOUND].freeze

  def initialize(organization_id, client_id: nil, only: "attention")
    @org = organization_id
    @client_id = client_id.presence
    @only = only
  end

  # 状態ごとの件数。絞り込みの前に、全体の姿を見せる。
  def counts
    rows = exec(<<~SQL, base_params)
      SELECT c.status, count(*) AS n
        FROM invoice_reg_checks c
        JOIN documents d ON d.id = c.document_id
        JOIN clients  cl ON cl.id = d.client_id
       WHERE cl.organization_id = $1
         AND ($2::bigint IS NULL OR d.client_id = $2)
       GROUP BY c.status
    SQL
    rows.each_with_object(Hash.new(0)) { |r, h| h[r["status"].to_i] = r["n"].to_i }
  end

  # 絞り込みに一致した件数。一覧の長さではない。
  def total
    exec(<<~SQL, base_params).first["n"].to_i
      SELECT count(*) AS n
        FROM invoice_reg_checks c
        JOIN documents d ON d.id = c.document_id
        JOIN clients  cl ON cl.id = d.client_id
       WHERE cl.organization_id = $1
         AND ($2::bigint IS NULL OR d.client_id = $2)
         #{status_filter}
    SQL
  end

  def rows
    exec(<<~SQL, base_params)
      SELECT c.document_id, c.reg_no, c.status, c.why, c.checked_at,
             d.source_name, d.source_page, d.source_region,
             cl.name AS client_name,
             (SELECT value_text FROM extracted_fields
               WHERE document_id = d.id
                 AND field_key = CASE WHEN d.direction = 1
                                      THEN 'issuer_name' ELSE 'recipient_name' END
               LIMIT 1) AS partner_text
        FROM invoice_reg_checks c
        JOIN documents d ON d.id = c.document_id
        JOIN clients  cl ON cl.id = d.client_id
       WHERE cl.organization_id = $1
         AND ($2::bigint IS NULL OR d.client_id = $2)
         #{status_filter}
       ORDER BY c.checked_at DESC, c.document_id DESC
       LIMIT #{LIMIT}
    SQL
  end

  # 同じ登録番号が複数の取引先名で出てきていないか。
  #
  # 番号の書き写し間違いは、こう現れる。ある会社の番号が
  # まったく別の会社の請求書に載る。1件ずつ見ても気付けない。
  # 番号を軸に並べて初めて見える。
  def conflicts
    exec(<<~SQL, base_params)
      SELECT c.reg_no,
             count(DISTINCT f.value_text) AS names,
             count(*) AS docs,
             string_agg(DISTINCT f.value_text, ' / ') AS name_list
        FROM invoice_reg_checks c
        JOIN documents d ON d.id = c.document_id
        JOIN clients  cl ON cl.id = d.client_id
        JOIN extracted_fields f
          ON f.document_id = d.id
         AND f.field_key = CASE WHEN d.direction = 1
                                THEN 'issuer_name' ELSE 'recipient_name' END
       WHERE cl.organization_id = $1
         AND ($2::bigint IS NULL OR d.client_id = $2)
         AND c.reg_no IS NOT NULL
         AND f.value_text <> ''
       GROUP BY c.reg_no
      HAVING count(DISTINCT f.value_text) > 1
       ORDER BY names DESC, docs DESC
       LIMIT 50
    SQL
  end

  def self.label(status) = LABEL[status.to_i] || "不明"

  private

  def base_params = [@org, @client_id]

  def status_filter
    case @only
    when "all"        then ""
    when "attention"  then "AND c.status IN (#{ATTENTION.join(',')})"
    else                   "AND c.status = #{@only.to_i}"
    end
  end

  def exec(sql, params)
    ActiveRecord::Base.connection.exec_query(sql, "InvoiceRegQuery", params).to_a
  end
end
