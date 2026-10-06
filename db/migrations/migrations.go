// Package migrations menyimpan migrasi SQL yang di-embed ke binary, sehingga
// `payrun migrate up` bisa berjalan di container tanpa file tambahan.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed *.sql
var files embed.FS

// MinSchemaVersion adalah versi skema minimum yang dibutuhkan binary ini.
// Readiness lulus jika versi DB >= nilai ini (bukan "=", supaya rollback app N-1 tetap ready;
// docs/design/26-deploy-migrations-backup.md, Step 12 R3-8). Naikkan saat menambah migrasi
// yang WAJIB ada untuk kode baru.
const MinSchemaVersion int64 = 1

// MigrateUp menjalankan semua migrasi yang belum diterapkan. dsn harus role payrun_owner.
func MigrateUp(ctx context.Context, dsn string) error {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("migrate: parse dsn: %w", err)
	}
	sqlDB := stdlib.OpenDB(*cfg)
	defer func() { _ = sqlDB.Close() }()
	return migrateUp(ctx, sqlDB)
}

func migrateUp(ctx context.Context, sqlDB *sql.DB) error {
	goose.SetBaseFS(files)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	goose.SetLogger(goose.NopLogger())
	if err := goose.UpContext(ctx, sqlDB, "."); err != nil {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// SchemaVersion membaca versi skema terakhir yang diterapkan (dipakai readiness).
func SchemaVersion(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (int64, error) {
	var v sql.NullInt64
	err := q.QueryRow(ctx, `SELECT max(version_id) FROM goose_db_version WHERE is_applied`).Scan(&v)
	if err != nil {
		return 0, err
	}
	if !v.Valid {
		return 0, errors.New("no migrations applied")
	}
	return v.Int64, nil
}
