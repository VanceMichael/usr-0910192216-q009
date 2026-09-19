// 文化产品收益分账账本服务。
//
// 子命令：
//
//	migrate  应用数据库迁移（compose 中的一次性服务）
//	seed     载入 contracts/ 下的规则、参与方、合同版本与渠道费率（幂等）
//	serve    启动 HTTP API
//	verify   离线校验：哈希链完整、借贷平衡、重放与账本一致
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"example.com/culture-sharing/internal/api"
	"example.com/culture-sharing/internal/config"
	"example.com/culture-sharing/internal/ledger"
	"example.com/culture-sharing/internal/migrate"
	"example.com/culture-sharing/internal/seed"
	"example.com/culture-sharing/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cfg := config.FromEnv()
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "migrate":
		err = withStore(ctx, cfg, func(st *store.Store) error {
			return migrate.Run(ctx, st.Pool)
		})
	case "seed":
		err = withStore(ctx, cfg, func(st *store.Store) error {
			return seed.Run(ctx, st, cfg.RulesFile, cfg.VersionsFile)
		})
	case "serve":
		err = serve(ctx, cfg)
	case "verify":
		err = verify(ctx, cfg)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: service <migrate|seed|serve|verify>")
	os.Exit(2)
}

func withStore(ctx context.Context, cfg config.Config, fn func(*store.Store) error) error {
	st, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Pool.Close()
	return fn(st)
}

func serve(ctx context.Context, cfg config.Config) error {
	st, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Pool.Close()

	srv := &http.Server{
		Addr: cfg.ListenAddr,
		Handler: api.NewServer(st, api.Config{
			PlatformID:       cfg.PlatformID,
			EnableFaultHooks: cfg.EnableFaultHooks,
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	fmt.Printf("listening on %s\n", cfg.ListenAddr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// verify 离线校验账本完整性，供故障恢复后与定期审计使用。
func verify(ctx context.Context, cfg config.Config) error {
	return withStore(ctx, cfg, func(st *store.Store) error {
		events, err := st.AllEvents(ctx)
		if err != nil {
			return err
		}
		headSeq, headHash, err := st.ChainHead(ctx)
		if err != nil {
			return err
		}
		report := map[string]any{"head_seq": headSeq}

		computed, chainErr := ledger.VerifyChain(events, []byte{0})
		chainOK := chainErr == nil && string(computed) == string(headHash)
		report["chain_ok"] = chainOK
		report["events_checked"] = len(events)
		report["head_hash"] = hex.EncodeToString(headHash)
		if chainErr != nil {
			report["chain_error"] = chainErr.Error()
		}

		imbalanced, err := st.ImbalancedSettlements(ctx)
		if err != nil {
			return err
		}
		report["imbalanced_settlements"] = imbalanced

		snap, err := ledger.Replay(events, headSeq, cfg.PlatformID)
		if err != nil {
			return err
		}
		stored, err := st.StoredBalances(ctx, headSeq)
		if err != nil {
			return err
		}
		replayMatch := len(snap.Balances) == len(stored)
		if replayMatch {
			for k, v := range snap.Balances {
				if stored[k] != v {
					replayMatch = false
					break
				}
			}
		}
		report["replay_matches_ledger"] = replayMatch

		out, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(out))
		if !chainOK || len(imbalanced) > 0 || !replayMatch {
			return fmt.Errorf("verification failed")
		}
		return nil
	})
}
