package postgres

import (
	"context"
	"embed"
	"fmt"
	"path"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationsFS вшивает .sql-файлы прямо в бинарник (пакет embed из стандартной
// библиотеки). Контейнеру не нужны внешние файлы миграций — образ самодостаточен.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate — минимальный мигратор без внешних зависимостей.
//
// Каждая миграция выполняется в отдельной транзакции вместе с записью
// в schema_migrations: если SQL упадёт, версия не отметится применённой.
// В DDL PostgreSQL транзакционен, поэтому это действительно атомарно.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return fmt.Errorf("создание schema_migrations: %w", err)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("чтение миграций: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && path.Ext(e.Name()) == ".sql" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // порядок применения задаётся числовым префиксом имени

	for _, name := range names {
		var exists bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`, name).Scan(&exists)
		if err != nil {
			return fmt.Errorf("проверка версии %s: %w", name, err)
		}
		if exists {
			continue
		}

		body, err := migrationsFS.ReadFile(path.Join("migrations", name))
		if err != nil {
			return fmt.Errorf("чтение %s: %w", name, err)
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("применение %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("отметка %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit %s: %w", name, err)
		}
	}
	return nil
}
