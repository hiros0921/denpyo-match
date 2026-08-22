package httpapi

import (
	"encoding/hex"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 署名する文字列は Go と Ruby の2箇所にある。
// 片方だけ直すと全ての呼び出しが 401 になり、原因が
// 「鍵が違う」なのか「並びが違う」なのか分からなくなる。
//
// そこで、決まった入力に対する署名を値で固定する。
// Ruby 側は web/test/api_client_signature_test.rb が同じ値を確かめる。
// どちらかを直したら、必ずもう片方が落ちる。
const (
	fixtureSecret = "0123456789abcdef0123456789abcdef" // 32文字
	fixtureTS     = 1750000000
	fixtureOrg    = 7

	// 上の入力に対する署名。この値そのものが取り決めである。
	fixtureSig = "8274202118b2d35eee81d89029969fd02bd6b0b444303390c48762f4f2c1fc2a"
)

func testAuth(t *testing.T) *Auth {
	t.Helper()
	a, err := NewAuth([]byte(fixtureSecret), 1<<20)
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	return a
}

// Ruby と突き合わせるための値を出す。
// 取り決めを変えたときは、この出力を fixtureSig と Ruby 側へ写す。
func TestSignFixture(t *testing.T) {
	a := testAuth(t)
	got := hex.EncodeToString(
		a.sign("POST", "/v1/documents/12/decision", "", fixtureTS, fixtureOrg,
			[]byte(`{"actor_id":3,"decision":2}`)))
	t.Logf("固定値の署名 = %s", got)
	if fixtureSig != got {
		t.Errorf("取り決めが変わっています。\n  Go   = %s\n  固定値 = %s\n"+
			"Ruby 側（web/app/services/api_client.rb の sign）も揃えてください。", got, fixtureSig)
	}
}

func TestSecretTooShort(t *testing.T) {
	if _, err := NewAuth([]byte("みじかい"), 1<<20); err == nil {
		t.Fatal("短い鍵が通ってしまった。起動時に弾けていない")
	}
}

// 署名の材料を1つ変えたら、必ず検証に落ちること。
//
// これが落ちないと「署名しているつもりで、実は見ていない項目がある」
// 状態になる。項目ごとに確かめる。
func TestVerifyRejects(t *testing.T) {
	a := testAuth(t)
	body := []byte(`{"actor_id":3}`)

	ok := func() (method, path, query string, ts, org int64) {
		return "POST", "/v1/documents/12/decision", "candidates=5", time.Now().Unix(), 7
	}

	cases := []struct {
		name string
		// 署名を作るときの材料
		sm, sp, sq string
		sts, sorg  int64
		sbody      []byte
		// 実際に送るときの材料
		rm, rp, rq string
		rts, rorg  int64
		rbody      []byte
	}{
		{name: "そのままなら通る"},
		{name: "メソッドを変える", rm: "GET"},
		{name: "パスを変える", rp: "/v1/documents/13/decision"},
		{name: "問い合わせ文字列を変える", rq: "candidates=50"},
		{name: "事務所を変える", rorg: 8},
		{name: "本文を変える", rbody: []byte(`{"actor_id":4}`)},
		{name: "時刻を変える", rts: 1},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, p, q, ts, org := ok()
			sm, sp, sq, sts, sorg, sbody := m, p, q, ts, org, body
			rm, rp, rq, rts, rorg, rbody := m, p, q, ts, org, body

			if c.rm != "" {
				rm = c.rm
			}
			if c.rp != "" {
				rp = c.rp
			}
			if c.rq != "" {
				rq = c.rq
			}
			if c.rts != 0 {
				rts = c.rts
			}
			if c.rorg != 0 {
				rorg = c.rorg
			}
			if c.rbody != nil {
				rbody = c.rbody
			}

			sig := hex.EncodeToString(a.sign(sm, sp, sq, sts, sorg, sbody))

			url := rp
			if rq != "" {
				url += "?" + rq
			}
			r := httptest.NewRequest(rm, url, strings.NewReader(string(rbody)))
			r.Header.Set("X-DM-Org", strconv.FormatInt(rorg, 10))
			r.Header.Set("X-DM-Timestamp", strconv.FormatInt(rts, 10))
			r.Header.Set("X-DM-Signature", "v1="+sig)

			gotOrg, _, err := a.verify(r)

			if c.name == "そのままなら通る" {
				if err != nil {
					t.Fatalf("通るはずが落ちた: %v", err)
				}
				if gotOrg != org {
					t.Fatalf("事務所が違う: got=%d want=%d", gotOrg, org)
				}
				return
			}
			if err == nil {
				t.Fatalf("%s が検出されていない。署名の材料から漏れている", c.name)
			}
		})
	}
}

// 版を変えた署名を受け付けないこと。
func TestVerifyRejectsOldVersion(t *testing.T) {
	a := testAuth(t)
	r := httptest.NewRequest("GET", "/v1/billing", nil)
	r.Header.Set("X-DM-Org", "7")
	r.Header.Set("X-DM-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	r.Header.Set("X-DM-Signature", "v0=deadbeef")
	if _, _, err := a.verify(r); err == nil {
		t.Fatal("v0 の署名が通ってしまった")
	}
}

// 署名を付けずに送った要求を受け付けないこと。
func TestVerifyRejectsUnsigned(t *testing.T) {
	a := testAuth(t)
	r := httptest.NewRequest("GET", "/v1/billing", nil)
	r.Header.Set("X-DM-Org", "7")
	if _, _, err := a.verify(r); err == nil {
		t.Fatal("署名なしが通ってしまった")
	}
}
