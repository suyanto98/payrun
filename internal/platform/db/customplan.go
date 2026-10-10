package db

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier: bentuk DBTX hasil sqlc (struktural, sehingga paket ini tidak bergantung pada store modul).
type Querier interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// CustomPlan membungkus tx supaya setiap query dikirim sebagai unnamed statement
// (pgx.QueryExecModeCacheDescribe): Postgres selalu merencanakannya dengan nilai parameter sebenarnya
// (custom plan), tidak pernah generic plan dari prepared statement yang di-cache.
//
// Hanya untuk query dengan filter opsional `($n IS NULL OR kolom = $n)`. Generic plan tidak tahu filter
// mana yang aktif, sehingga salah memilih index (PR-3, S9: daftar karyawan 8 ms generic vs 1,5 ms custom
// pada tenant 10.000). Harganya: parse + plan setiap eksekusi (±0,5–1 ms). Jangan dipakai di jalur panas
// yang bentuk query-nya sudah tetap. Deskripsi tipe parameter tetap di-cache, sehingga encoding biner
// uuid/date sama dengan mode default.
func CustomPlan(q Querier) Querier { return customPlan{q} }

type customPlan struct{ q Querier }

func (c customPlan) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return c.q.Exec(ctx, sql, withMode(args)...)
}

func (c customPlan) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return c.q.Query(ctx, sql, withMode(args)...)
}

func (c customPlan) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return c.q.QueryRow(ctx, sql, withMode(args)...)
}

func withMode(args []any) []any {
	return append([]any{pgx.QueryExecModeCacheDescribe}, args...)
}
