// Package store 是 PostgreSQL 访问层：所有多步写入都在单事务内完成，
// 并发安全依赖行锁（chain_state）、咨询锁（结算确认）与唯一约束，
// 纯计算全部委托给 internal/ledger。
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store 持有连接池。
type Store struct {
	Pool *pgxpool.Pool
}

// Connect 建立连接池并带重试地等待数据库就绪（compose 启动窗口）。
func Connect(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	cfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err = pool.Ping(ctx); err == nil {
			break
		}
		if time.Now().After(deadline) {
			pool.Close()
			return nil, fmt.Errorf("database not ready: %w", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return &Store{Pool: pool}, nil
}
