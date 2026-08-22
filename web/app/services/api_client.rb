require "net/http"
require "json"
require "securerandom"
require "openssl"
require "digest"

# Go の API を呼ぶ。
#
# なぜ Rails から直接DBに書かないのか
#
#   受付（伝票とジョブを作る）と承認（結果を書き替えて監査ログを残す）は、
#   どちらも「1つのトランザクションで全部書く」ことに意味がある処理。
#   同じ処理を Rails 側にもう1つ実装すると、片方だけ直したときに
#   片方の経路だけ壊れる。そして壊れ方が「監査ログが残らない」なので、
#   壊れていることに気付くのが最も遅くなる。
#
#   読み取り（一覧・集計）は Rails から直接DBを引く。
#   こちらは書き込みが無いので二重実装の問題が起きず、
#   API を経由すると集計のたびに往復が増えるだけになる。
#
# ── 事務所IDの扱い ──
#
# organization_id は各メソッドの引数ではなく、この物を作るときに渡す。
#
# 【重要】引数にしてはいけない。
# 引数だと、呼ぶ側が current_organization 以外の値を渡せてしまう。
# 11か所ある呼び出しのうち1か所で間違えれば、他事務所のデータに手が届く。
# 「間違えないように気をつける」ではなく「間違った値を渡す場所が無い」形にする。
# 画面側は ApplicationController#api を使い、直接 new しない。
class ApiClient
  class Unreachable < StandardError; end
  class Unconfigured < StandardError; end

  def initialize(organization_id:, base: ENV.fetch("API_BASE", "http://localhost:58080"))
    raise ArgumentError, "organization_id が要ります" if organization_id.to_i <= 0
    @base = base
    @org  = organization_id.to_i
  end

  def upload(file:, client_id:, doc_type:, direction: nil)
    uri = URI.join(@base, "/v1/documents")
    boundary = "dm#{SecureRandom.hex(12)}"
    body = build_multipart(boundary, file, client_id, doc_type, direction)

    req = Net::HTTP::Post.new(uri)
    req["Content-Type"] = "multipart/form-data; boundary=#{boundary}"
    req.body = body

    res = perform(uri, req, timeout: 120)
    if res.code == "202"
      body = JSON.parse(res.body)
      { ok: true, document_id: body["document_id"], count: body["count"] || 1 }
    else
      { ok: false, error: error_message(res) }
    end
  end

  # 明細CSVの取り込み。Go 側で解釈・正規化・保存・突合まで行う。
  def import_transactions(file:, client_id:, source_type:)
    uri = URI.join(@base, "/v1/transactions/import")
    boundary = "dm#{SecureRandom.hex(12)}"
    req = Net::HTTP::Post.new(uri)
    req["Content-Type"] = "multipart/form-data; boundary=#{boundary}"
    req.body = build_multipart_fields(boundary, file,
      "client_id" => client_id, "source_type" => source_type)

    # 取り込みに続けて突合まで走るので、伝票のアップロードより待つ。
    res = perform(uri, req, timeout: 300)
    if res.code == "200"
      b = JSON.parse(res.body)
      { ok: true, rows: b["rows"], skipped: b["skipped"], settle: b["settle"] }
    else
      { ok: false, error: error_message(res) }
    end
  end

  def run_settlements(client_id:)
    uri = URI.join(@base, "/v1/settlements/run")
    req = Net::HTTP::Post.new(uri)
    req["Content-Type"] = "application/json"
    req.body = { client_id: client_id.to_i }.to_json
    res = perform(uri, req, timeout: 300)
    res.code == "200" ? { ok: true, stats: JSON.parse(res.body) }
                      : { ok: false, error: error_message(res) }
  end

  def confirm_settlement(document_id:, actor_id:,
                         transaction_id: nil, none: false, learn_alias: nil)
    uri = URI.join(@base, "/v1/documents/#{document_id}/settlement")
    req = Net::HTTP::Post.new(uri)
    req["Content-Type"] = "application/json"
    req.body = {
      actor_id: actor_id,
      transaction_id: transaction_id&.to_i,
      none: none.presence && true,
      learn_alias: learn_alias
    }.compact.to_json
    res = perform(uri, req, timeout: 60)
    if res.code == "200"
      b = JSON.parse(res.body)
      { ok: true, alias_error: b["alias_error"] }
    else
      { ok: false, error: error_message(res) }
    end
  end

  def decide(document_id:, actor_id:, decision:,
             partner_id: nil, learn_alias: nil)
    uri = URI.join(@base, "/v1/documents/#{document_id}/decision")
    req = Net::HTTP::Post.new(uri)
    req["Content-Type"] = "application/json"
    req.body = {
      actor_id: actor_id,
      decision: decision,
      partner_id: partner_id&.to_i,
      learn_alias: learn_alias
    }.compact.to_json

    res = perform(uri, req, timeout: 30)
    res.code == "200" ? { ok: true } : { ok: false, error: error_message(res) }
  end

  # 覚えた表記の一覧。読み取りだが API 経由にする。
  # 取り消しが API 側にある以上、一覧も同じ口から見えたほうが、
  # 「画面に出ているものが消せる」対応が崩れない。
  def learned_aliases(limit: 100)
    uri = URI.join(@base, "/v1/aliases?limit=#{limit}")
    res = perform(uri, Net::HTTP::Get.new(uri), timeout: 15)
    return [] unless res.code == "200"
    JSON.parse(res.body)["aliases"] || []
  end

  def forget_alias(id:, actor_id:)
    uri = URI.join(@base, "/v1/aliases/#{id}?actor_id=#{actor_id}")
    res = perform(uri, Net::HTTP::Delete.new(uri), timeout: 15)
    res.code == "200" ? { ok: true } : { ok: false, error: error_message(res) }
  end

  # ── 契約 ──
  #
  # 【重要】契約の判定を Ruby 側に書かない。
  # 「支払っているのに使えない」も「解約したのに使える」も事故になるので、
  # 判定は Go の billing.Evaluate 1箇所だけにする。
  # 2箇所に書くと、片方だけ直したときに画面と実際の動きが食い違う。
  def billing_status
    uri = URI.join(@base, "/v1/billing")
    res = perform(uri, Net::HTTP::Get.new(uri), timeout: 10)
    return nil unless res.code == "200"
    JSON.parse(res.body)
  end

  # 申し込み画面のURLを作る。カード番号は Stripe の画面で入力してもらう。
  # 自前で受けると、その瞬間からカード情報を扱う側になる。
  def checkout_url(actor_email:)
    post_json("/v1/billing/checkout", actor_email: actor_email)
  end

  # 支払い方法の変更・解約を行う画面のURL。
  #
  # 【重要】この口は Stripe の管理画面へ入れてしまう。
  # 事務所IDを引数で受けていた頃は、番号を差し替えれば
  # 他事務所の解約もカード情報の閲覧もできた。署名から決める。
  def portal_url
    post_json("/v1/billing/portal")
  end

  private

  def post_json(path, **body)
    uri = URI.join(@base, path)
    req = Net::HTTP::Post.new(uri)
    req["Content-Type"] = "application/json"
    req.body = body.to_json
    res = perform(uri, req, timeout: 30)
    return { ok: true, url: JSON.parse(res.body)["url"] } if res.code == "200"
    { ok: false, error: error_message(res) }
  end

  def perform(uri, req, timeout:)
    sign(uri, req)
    Net::HTTP.start(uri.host, uri.port, open_timeout: 5, read_timeout: timeout) do |http|
      http.request(req)
    end
  rescue Errno::ECONNREFUSED, Net::OpenTimeout, SocketError => e
    raise Unreachable, e.message
  end

  # 要求に署名を付ける。
  #
  # ここが唯一の出口なので、1か所で全部の呼び出しに掛かる。
  # メソッドごとに付けて回ると、新しいメソッドを足したときに付け忘れる。
  # 付け忘れは 401 で必ず落ちるので静かには壊れないが、
  # そもそも忘れられない場所に置く。
  #
  # 署名する文字列は Go 側と1文字も違ってはいけない。
  # 対応は api/internal/httpapi/auth.go の sign。
  def sign(uri, req)
    ts = Time.now.to_i
    req["X-DM-Org"] = @org.to_s
    req["X-DM-Timestamp"] = ts.to_s
    return if secret.nil? # DM_API_AUTH=off の開発時。事務所IDだけ送る

    req["X-DM-Signature"] = ApiSignature.header(
      secret: secret,
      method: req.method,
      path: uri.path,
      query: uri.query,
      timestamp: ts,
      organization_id: @org,
      body: req.body
    )
  end

  # 共有鍵。無い状態を黙って通さない。
  #
  # 【重要】鍵が無いときに署名なしで送って 401 を見せる、にしない。
  # 401 は「鍵が違う」とも「Go 側の設定漏れ」とも読めるので、
  # 原因を探す場所が2つに増える。こちら側の設定漏れは、こちらで言い切る。
  def secret
    return @secret if defined?(@secret)
    s = ENV["DM_API_SECRET"].presence
    if s.nil? && ENV["DM_API_AUTH"] != "off"
      raise Unconfigured,
            "DM_API_SECRET が設定されていません。API を呼べません" \
            "（開発で外す場合のみ DM_API_AUTH=off）"
    end
    @secret = s
  end

  # API はエラーを日本語で返す。そのまま画面に出す。
  # ここで英語に直したり握りつぶしたりすると、現場が原因を判断できなくなる。
  def error_message(res)
    JSON.parse(res.body)["error"].presence || "処理に失敗しました（#{res.code}）"
  rescue JSON::ParserError
    "処理に失敗しました（#{res.code}）"
  end

  # 任意の項目でmultipartを組む。
  # build_multipart（伝票アップロード用）と分けているのは、
  # 項目の並びと必須項目が別だから。1つにまとめると
  # どちらかの経路にしか無い項目を空で送ることになる。
  def build_multipart_fields(boundary, file, fields)
    parts = []
    fields.each do |k, v|
      next if v.blank?
      parts << "--#{boundary}\r\n" \
               "Content-Disposition: form-data; name=\"#{k}\"\r\n\r\n#{v}\r\n"
    end
    parts << "--#{boundary}\r\n" \
             "Content-Disposition: form-data; name=\"file\"; " \
             "filename=\"#{file.original_filename}\"\r\n" \
             "Content-Type: text/csv\r\n\r\n"
    body = parts.join.dup.force_encoding(Encoding::BINARY)
    body << file.read.force_encoding(Encoding::BINARY)
    body << "\r\n--#{boundary}--\r\n".b
    body
  end

  def build_multipart(boundary, file, client_id, doc_type, direction = nil)
    parts = []
    # direction が nil のときは項目ごと送らない。
    # 空文字で送ると API 側が「指定あり」と解釈しかねないし、
    # 顧問先の既定を使うという意味は「送らない」でしか表せない。
    fields = { "client_id" => client_id, "doc_type" => doc_type }
    fields["direction"] = direction if direction.present?
    fields.each do |k, v|
      parts << "--#{boundary}\r\n" \
               "Content-Disposition: form-data; name=\"#{k}\"\r\n\r\n#{v}\r\n"
    end
    parts << "--#{boundary}\r\n" \
             "Content-Disposition: form-data; name=\"file\"; " \
             "filename=\"#{File.basename(file.original_filename)}\"\r\n" \
             "Content-Type: #{file.content_type}\r\n\r\n"
    # 画像は binary のまま連結する。ここで encode すると壊れる。
    parts.join.dup.force_encoding("BINARY") +
      file.read.force_encoding("BINARY") +
      "\r\n--#{boundary}--\r\n".dup.force_encoding("BINARY")
  end
end
