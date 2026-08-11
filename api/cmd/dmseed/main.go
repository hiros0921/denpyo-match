// dmseed は取引先マスタを DB に入れる。
//
//	go run ./cmd/dmseed -masters ../testdata/samples/masters.json -client-name 検証用顧問先
//
// norm は必ず C++ の dm_match --normalize で計算する。
// Go 側で書き直すと、保存時と照会時でルールがずれる。ずれても例外は出ず、
// 候補生成が静かに当たらなくなるだけなので、気付くのが最も遅れる類いの不具合になる。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/hiros0921/denpo-match/api/internal/core"
	"github.com/hiros0921/denpo-match/api/internal/store"
)

type master struct {
	ID        int64    `json:"id"`
	Canonical string   `json:"canonical"`
	Variants  []string `json:"variants"`
}

func main() {
	var (
		mastersPath = flag.String("masters", "../testdata/samples/masters.json",
			"取引先マスタのJSON")
		dsn = flag.String("dsn", env("DATABASE_URL",
			"postgres://dm:dm_dev_only@localhost:55432/denpo_match?sslmode=disable"), "DB")
		bin        = flag.String("bin", env("DM_BIN", "/tmp/dmbin"), "C++の実行ファイル")
		orgID      = flag.Int64("org", 1, "組織ID")
		clientName = flag.String("client-name", "検証用顧問先", "顧問先の名前")
		aliases    = flag.Bool("aliases", true, "variants を別名として登録する")
	)
	flag.Parse()

	ctx := context.Background()
	b, err := os.ReadFile(*mastersPath)
	if err != nil {
		die("マスタを読めません: %v", err)
	}
	var ms []master
	if err := json.Unmarshal(b, &ms); err != nil {
		die("マスタの形式が違います: %v", err)
	}

	st, err := store.Open(ctx, *dsn)
	if err != nil {
		die("%v", err)
	}
	defer st.Close()

	// 顧問先を作る。既にあれば作り直さない。
	var clientID int64
	err = st.Pool.QueryRow(ctx,
		`SELECT id FROM clients WHERE organization_id=$1 AND name=$2`,
		*orgID, *clientName).Scan(&clientID)
	if err != nil {
		// ocr_engine は tesseract。開発中は Google Cloud Vision を呼ばない。
		if err := st.Pool.QueryRow(ctx, `
			INSERT INTO clients (organization_id, name, ocr_engine)
			VALUES ($1, $2, 'tesseract') RETURNING id`,
			*orgID, *clientName).Scan(&clientID); err != nil {
			die("顧問先を作れません: %v", err)
		}
		fmt.Printf("  顧問先を作成  id=%d %s\n", clientID, *clientName)
	} else {
		fmt.Printf("  既存の顧問先  id=%d %s\n", clientID, *clientName)
	}

	// 既に入っているなら何もしない。二重に入れると同じ名前が2件並び、
	// 候補生成の枠を食い合う。
	var n int
	_ = st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM partners WHERE client_id=$1`, clientID).Scan(&n)
	if n > 0 {
		fmt.Printf("  既に %d 件あります。何もしません\n", n)
		fmt.Printf("  入れ直すには: DELETE FROM partners WHERE client_id=%d;\n", clientID)
		return
	}

	// 正規化はまとめて1回で呼ぶ。1件ずつだとプロセス起動が件数分になる。
	runner := core.New(*bin, *mastersPath)
	names := make([]string, 0, len(ms))
	for _, m := range ms {
		names = append(names, m.Canonical)
	}
	t := time.Now()
	norms, err := runner.Normalize(ctx, names)
	if err != nil {
		die("正規化に失敗: %v", err)
	}
	fmt.Printf("  正規化  %d件 %dms（C++ を1回呼んだだけ）\n",
		len(norms), time.Since(t).Milliseconds())

	t = time.Now()
	ids := make([]int64, len(ms))
	for i, m := range ms {
		id, err := st.UpsertPartner(ctx, clientID, m.Canonical, norms[i])
		if err != nil {
			die("取引先を入れられません(%s): %v", m.Canonical, err)
		}
		ids[i] = id
	}
	fmt.Printf("  取引先  %d件 %dms\n", len(ms), time.Since(t).Milliseconds())

	if !*aliases {
		return
	}
	// variants を別名として入れる。
	//
	// 【注意】これは生成データなので「正解の表記揺れ」が分かっている。
	// 実運用では最初は空で、人が承認画面で修正するたびに貯まる（source=2）。
	// ここで入れるのは source=1（手動登録）に相当する。
	var av []string
	var ai []int64
	for i, m := range ms {
		for _, v := range m.Variants {
			if v == m.Canonical {
				continue
			}
			av = append(av, v)
			ai = append(ai, ids[i])
		}
	}
	if len(av) == 0 {
		return
	}
	t = time.Now()
	an, err := runner.Normalize(ctx, av)
	if err != nil {
		die("別名の正規化に失敗: %v", err)
	}
	for i := range av {
		if err := st.AddAlias(ctx, ai[i], av[i], an[i], 1); err != nil {
			die("別名を入れられません(%s): %v", av[i], err)
		}
	}
	fmt.Printf("  別名    %d件 %dms\n", len(av), time.Since(t).Milliseconds())
}

func die(f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(1)
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
