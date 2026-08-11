require "net/http"
require "json"
require "securerandom"

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
class ApiClient
  class Unreachable < StandardError; end

  def initialize(base: ENV.fetch("API_BASE", "http://localhost:58080"))
    @base = base
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

  def decide(document_id:, organization_id:, actor_id:, decision:,
             partner_id: nil, learn_alias: nil)
    uri = URI.join(@base, "/v1/documents/#{document_id}/decision")
    req = Net::HTTP::Post.new(uri)
    req["Content-Type"] = "application/json"
    req.body = {
      organization_id: organization_id, actor_id: actor_id,
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
  def learned_aliases(organization_id:, limit: 100)
    uri = URI.join(@base, "/v1/aliases?organization_id=#{organization_id}&limit=#{limit}")
    res = perform(uri, Net::HTTP::Get.new(uri), timeout: 15)
    return [] unless res.code == "200"
    JSON.parse(res.body)["aliases"] || []
  end

  def forget_alias(id:, organization_id:, actor_id:)
    uri = URI.join(@base,
                   "/v1/aliases/#{id}?organization_id=#{organization_id}&actor_id=#{actor_id}")
    res = perform(uri, Net::HTTP::Delete.new(uri), timeout: 15)
    res.code == "200" ? { ok: true } : { ok: false, error: error_message(res) }
  end

  # ── 契約 ──
  #
  # 【重要】契約の判定を Ruby 側に書かない。
  # 「支払っているのに使えない」も「解約したのに使える」も事故になるので、
  # 判定は Go の billing.Evaluate 1箇所だけにする。
  # 2箇所に書くと、片方だけ直したときに画面と実際の動きが食い違う。
  def billing_status(organization_id:)
    uri = URI.join(@base, "/v1/billing?organization_id=#{organization_id}")
    res = perform(uri, Net::HTTP::Get.new(uri), timeout: 10)
    return nil unless res.code == "200"
    JSON.parse(res.body)
  end

  # 申し込み画面のURLを作る。カード番号は Stripe の画面で入力してもらう。
  # 自前で受けると、その瞬間からカード情報を扱う側になる。
  def checkout_url(organization_id:, actor_email:)
    post_json("/v1/billing/checkout",
              organization_id: organization_id, actor_email: actor_email)
  end

  # 支払い方法の変更・解約を行う画面のURL。
  def portal_url(organization_id:)
    post_json("/v1/billing/portal", organization_id: organization_id)
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
    Net::HTTP.start(uri.host, uri.port, open_timeout: 5, read_timeout: timeout) do |http|
      http.request(req)
    end
  rescue Errno::ECONNREFUSED, Net::OpenTimeout, SocketError => e
    raise Unreachable, e.message
  end

  # API はエラーを日本語で返す。そのまま画面に出す。
  # ここで英語に直したり握りつぶしたりすると、現場が原因を判断できなくなる。
  def error_message(res)
    JSON.parse(res.body)["error"].presence || "処理に失敗しました（#{res.code}）"
  rescue JSON::ParserError
    "処理に失敗しました（#{res.code}）"
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
