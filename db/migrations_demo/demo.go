//go:build demo

// Package migrationsdemo: migrasi khusus image demo (tabel demo_faults). Hanya ter-compile dengan tag
// `demo`; binary release tidak memuat file SQL-nya sama sekali.
package migrationsdemo

import (
	"context"
	"embed"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed *.sql
var files embed.FS

// MigrateUp menjalankan migrasi demo SETELAH migrasi utama, dengan tabel versi sendiri.
func MigrateUp(ctx context.Context, dsn string) error {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("migrate demo: parse dsn: %w", err)
	}
	sqlDB := stdlib.OpenDB(*cfg)
	defer func() { _ = sqlDB.Close() }()
	goose.SetBaseFS(files)
	goose.SetTableName("goose_demo_version")
	defer goose.SetTableName("goose_db_version")
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	goose.SetLogger(goose.NopLogger())
	if err := goose.UpContext(ctx, sqlDB, "."); err != nil {
		return fmt.Errorf("migrate demo up: %w", err)
	}
	return nil
}
