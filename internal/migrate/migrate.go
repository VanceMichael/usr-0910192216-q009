// Package migrate 内嵌 SQL 迁移并按版本顺序应用，记录 schema_migrations。
// 作为 compose 中独立的一次性服务运行（app 依赖其成功完成）。
package migrate

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Run 应用所有未执行的迁移；每个迁移在单独事务中执行。
// 迁移文件含多条语句，必须使用简单查询协议（PgConn.Exec），
// pgx 默认的扩展协议不接受多语句字符串。
func Run(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	pg := conn.Conn().PgConn()

	exec := func(sql string) error {
		_, err := pg.Exec(ctx, sql).ReadAll()
		return err
	}

	if err := exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version int PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		version, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: bad version prefix: %w", name, err)
		}
		var applied bool
		if err := conn.Conn().QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).
			Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if err := exec("BEGIN"); err != nil {
			return err
		}
		if err := exec(string(body)); err != nil {
			_ = exec("ROLLBACK")
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if err := exec(fmt.Sprintf(
			`INSERT INTO schema_migrations (version) VALUES (%d)`, version)); err != nil {
			_ = exec("ROLLBACK")
			return err
		}
		if err := exec("COMMIT"); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}
