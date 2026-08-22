# 署名の取り決めが Go 側と一致していることを確かめる。
#
#   ruby web/test/api_signature_test.rb
#
# Rails を起動しない。素の Ruby だけで走る。
#
# ── なぜ固定値で持つのか ──
#
# 「Ruby で作った署名を Ruby で検証する」テストは、両方を同時に間違えると
# 通ってしまう。確かめたいのは Ruby の中の一貫性ではなく、
# Go 側と食い違っていないことなので、値そのものを両側に置く。
#
# 対応する Go 側: api/internal/httpapi/auth_test.go の fixtureSig
# どちらかの取り決めを変えたら、必ずもう片方が落ちる。

require "minitest/autorun"
require_relative "../app/services/api_signature"

class ApiSignatureTest < Minitest::Test
  SECRET = "0123456789abcdef0123456789abcdef".freeze # 32文字
  TS     = 1_750_000_000
  ORG    = 7

  # Go 側の fixtureSig と同じ値。
  EXPECTED = "8274202118b2d35eee81d89029969fd02bd6b0b444303390c48762f4f2c1fc2a".freeze

  def fixture(**over)
    {
      method: "POST",
      path: "/v1/documents/12/decision",
      query: "",
      timestamp: TS,
      organization_id: ORG,
      body: '{"actor_id":3,"decision":2}'
    }.merge(over)
  end

  def test_go_と同じ署名になる
    got = ApiSignature.header(secret: SECRET, **fixture)
    assert_equal "v1=#{EXPECTED}", got,
                 "Go 側（api/internal/httpapi/auth.go の sign）と取り決めが食い違っています"
  end

  # 材料を1つ変えたら署名が必ず変わること。
  # 変わらない項目があれば、それは署名に含まれていない＝改ざんできる。
  def test_材料を変えると署名も変わる
    base = ApiSignature.header(secret: SECRET, **fixture)

    {
      "メソッド"     => { method: "GET" },
      "パス"         => { path: "/v1/documents/13/decision" },
      "問い合わせ"   => { query: "candidates=5" },
      "時刻"         => { timestamp: TS + 1 },
      "事務所"       => { organization_id: ORG + 1 },
      "本文"         => { body: '{"actor_id":4,"decision":2}' }
    }.each do |name, over|
      refute_equal base, ApiSignature.header(secret: SECRET, **fixture(**over)),
                   "#{name} を変えても署名が同じです。署名の材料から漏れています"
    end
  end

  def test_鍵が違えば署名も違う
    refute_equal ApiSignature.header(secret: SECRET, **fixture),
                 ApiSignature.header(secret: SECRET.succ, **fixture)
  end

  # 本文が空の GET でも組み立てられること。
  # req.body が nil のまま渡ってくる経路があるので、nil を空文字として扱う。
  def test_本文がnilでも空文字と同じ扱いになる
    assert_equal ApiSignature.header(secret: SECRET, **fixture(body: nil)),
                 ApiSignature.header(secret: SECRET, **fixture(body: ""))
  end
end
