// dmapi は REST の口とワーカーを立ち上げる。
//
// 開発中は1つのプロセスで両方動かす。本番では -workers 0 で API だけ、
// -http "" でワーカーだけ、と分けて別々の台に置ける。
// 最初から分けて書くと、開発中に2つ起動する手間が毎回かかる。
//
//	go run ./cmd/dmapi -workers 2
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/hiros0921/denpo-match/api/internal/billing"
	"github.com/hiros0921/denpo-match/api/internal/core"
	"github.com/hiros0921/denpo-match/api/internal/httpapi"
	"github.com/hiros0921/denpo-match/api/internal/pipeline"
	"github.com/hiros0921/denpo-match/api/internal/settle"
	"github.com/hiros0921/denpo-match/api/internal/store"
	"github.com/hiros0921/denpo-match/api/internal/worker"
)

func main() {
	var (
		addr = flag.String("http", ":8080", "待ち受けアドレス（空でAPIを起動しない）")
		dsn  = flag.String("dsn", env("DATABASE_URL",
			"postgres://dm:dm_dev_only@localhost:55432/denpo_match?sslmode=disable"),
			"DBの接続文字列")
		bin     = flag.String("bin", env("DM_BIN", "/tmp/dmbin"), "C++の実行ファイル")
		masters = flag.String("masters", "/testdata/samples/masters.json",
			"全マスタ（候補生成をDBに任せるので通常は使わない）")
		workers = flag.Int("workers", 1, "ワーカーの数")
		orgID   = flag.Int64("org", 1, "組織ID")
		images  = flag.String("images", env("DM_IMAGES", "/tmp/dmimages"),
			"画像の置き場所（このプロセスから見たパス）")
		imagesC = flag.String("images-container", env("DM_IMAGES_CONTAINER", "/dmimages"),
			"同じ場所を C++ から見たパス")
		appURL = flag.String("app-url", env("APP_BASE_URL", "http://localhost:53000"),
			"画面のURL。Stripe から戻ってくる先に使う")
		work  = flag.String("work", "/tmp/dmwork", "作業フォルダ（このプロセスから見たパス）")
		workC = flag.String("work-container", "/dmwork", "同じ場所を C++ から見たパス")
	)
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelInfo})))

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, *dsn)
	if err != nil {
		slog.Error("起動できません", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	runner := core.New(*bin, *masters)
	pipe := pipeline.New(runner, *work, *workC)

	// Google Cloud Vision。設定が無ければ nil のまま。
	//
	// 【重要】無いことを黙って通さない。vision を選んでいる顧問先があるかを
	// 起動時に調べて、あれば警告する。設定漏れは「精度が落ちる」ではなく
	// 「処理が止まる」形で出るようにしてあるが、起動時に気付けるほうがよい。
	if k := os.Getenv("GOOGLE_VISION_API_KEY"); k != "" {
		var cost float64
		fmt.Sscan(os.Getenv("VISION_COST_PER_PAGE_YEN"), &cost)
		pipe.Vision = core.NewVision(k, cost, runner)
		slog.Info("Vision を有効化", "1枚あたりの費用", pipe.Vision.CostPerPageYen)
	} else {
		var n int
		_ = st.Pool.QueryRow(ctx,
			`SELECT count(*) FROM clients WHERE ocr_engine = 'vision'`).Scan(&n)
		if n > 0 {
			slog.Warn("Vision を使う設定の顧問先がありますが、APIキーがありません",
				"顧問先の数", n,
				"対処", "GOOGLE_VISION_API_KEY を設定するか、顧問先を tesseract に変える")
		}
	}

	var wg sync.WaitGroup

	for i := 0; i < *workers; i++ {
		w := worker.New(st, pipe, *orgID, *images, *imagesC)
		w.Settle = &settle.Runner{St: st, Core: runner,
			WorkDirHost: *work, WorkDirContainer: *workC}
		wg.Add(1)
		go func() { defer wg.Done(); w.Run(ctx) }()
	}
	if *workers > 0 {
		slog.Info("ワーカー起動", "台数", *workers)
	}

	if *addr != "" {
		api := httpapi.New(st, runner, *images)

		// 呼び出し元の確認。
		//
		// 【重要】未設定なら起動しない。
		// 警告にとどめると、警告は流れて動いてしまい、
		// 「認証が効いていない状態で本番に出ている」に気付けるのは事故のあとになる。
		// 開発で外したいときは、外したことが分かる形で明示させる。
		switch sec := os.Getenv("DM_API_SECRET"); {
		case sec != "":
			auth, err := httpapi.NewAuth([]byte(sec), api.MaxUpload)
			if err != nil {
				slog.Error("起動できません", "err", err)
				os.Exit(1)
			}
			api.Auth = auth
			slog.Info("要求の署名を検証します")
		case os.Getenv("DM_API_AUTH") == "off":
			api.Auth = httpapi.NoAuth()
			slog.Warn("署名の検証を切っています。" +
				"この状態では、伝票IDを順に叩くだけで全事務所の伝票が読めます。" +
				"開発以外で使わないこと（DM_API_AUTH=off）")
		default:
			slog.Error("起動できません",
				"理由", "DM_API_SECRET が設定されていません",
				"対処", "32文字以上の共有鍵を設定してください（openssl rand -hex 32）。"+
					"開発で外す場合のみ DM_API_AUTH=off")
			os.Exit(1)
		}
		// 切り分けは C++ を呼ぶので、コンテナから見たパスが要る。
		api.ImageRootContainer = *imagesC
		// 入出金の突合。明細を取り込んだら、その顧問先の受領伝票を回す。
		api.Settle = &settle.Runner{
			St: st, Core: runner,
			WorkDirHost: *work, WorkDirContainer: *workC,
		}
		api.AppBaseURL = *appURL

		// 決済。設定が揃っていなければ nil のまま。
		//
		// 【重要】揃っていないことを黙って通さない。起動時に何が足りないかを出す。
		// 「動いているつもりで、契約の判定だけが効いていない」状態を作らない。
		secret := os.Getenv("STRIPE_SECRET_KEY")
		price := os.Getenv("STRIPE_PRICE_ID")
		whsec := os.Getenv("STRIPE_WEBHOOK_SECRET")
		if secret != "" && price != "" {
			var trial int64
			fmt.Sscan(os.Getenv("STRIPE_TRIAL_DAYS"), &trial)
			api.Stripe = billing.NewStripe(secret, os.Getenv("STRIPE_PUBLISHABLE_KEY"),
				whsec, price, trial)
			slog.Info("決済を有効化", "価格", price, "試用日数", trial,
				"Webhook", whsec != "")
			if whsec == "" {
				// Webhook が無いと、支払い失敗も解約もこちらに伝わらない。
				// 契約したのに使えない／解約したのに使える、が起きる。
				slog.Warn("STRIPE_WEBHOOK_SECRET が未設定です。" +
					"契約の変化がこのシステムに伝わりません")
			}
			slog.Info("Webhook を登録するときの API 版", "version",
				billing.ExpectedAPIVersion())
		} else {
			slog.Warn("決済は無効です（STRIPE_SECRET_KEY と STRIPE_PRICE_ID が要ります）")
		}

		srv := &http.Server{
			Addr:    *addr,
			Handler: api.Routes(),
			// アップロードは大きいので読み取りは長めに取る。
			ReadTimeout:  60 * time.Second,
			WriteTimeout: 60 * time.Second,
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			slog.Info("API起動", "addr", *addr)
			if err := srv.ListenAndServe(); err != nil &&
				!errors.Is(err, http.ErrServerClosed) {
				slog.Error("APIが落ちました", "err", err)
			}
		}()
		go func() {
			<-ctx.Done()
			// 処理中の要求を取りこぼさないよう、猶予を置いて閉じる。
			sc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = srv.Shutdown(sc)
		}()
	}

	wg.Wait()
	slog.Info("終了しました")
}

func env(k, dflt string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return dflt
}
