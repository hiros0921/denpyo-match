require "openssl"
require "digest"

# Go の API へ送る要求の署名。
#
# ── なぜ ApiClient から切り離すのか ──
#
# 署名の取り決めは Go 側（api/internal/httpapi/auth.go）と1文字も違ってはいけない。
# 違うと全ての呼び出しが 401 になり、「鍵が違う」のか「並びが違う」のかを
# 切り分けるところから始めることになる。
#
# ApiClient に埋めたままだと、確かめるのに Rails と Net::HTTP が要る。
# ここだけ Rails に依存しない素の Ruby にしておけば、
#
#   ruby web/test/api_signature_test.rb
#
# だけで取り決めを確かめられる。Go 側と同じ固定値を突き合わせている。
module ApiSignature
  VERSION = "v1".freeze

  module_function

  # 署名する文字列を組み立てる。
  #
  # 【重要】改行で区切る。区切らずに連結すると、項目の切れ目が動いたときに
  # 別々の要求が同じ署名を持ちうる。Go 側の sign と同じ並び・同じ区切り。
  def canonical(method:, path:, query:, timestamp:, organization_id:, body:)
    [
      VERSION,
      method.to_s.upcase,
      path.to_s,
      query.to_s,
      timestamp.to_i.to_s,
      organization_id.to_i.to_s,
      Digest::SHA256.hexdigest(body.to_s)
    ].join("\n")
  end

  # X-DM-Signature に入れる値。
  def header(secret:, **args)
    "#{VERSION}=#{OpenSSL::HMAC.hexdigest('SHA256', secret, canonical(**args))}"
  end
end
