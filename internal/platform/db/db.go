// Package db membungkus pgx: pool dengan timeout dari ADR-011, WithTx dengan retry terbatas,
// dan pemetaan error Postgres → error domain (errors.go).
//
// Aturan transaksi (docs/design/18-tx-global-rules.md):
//   - service layer yang membuka transaksi (WithTx), lalu meneruskan pgx.Tx ke modul lain;
//   - tidak ada panggilan jaringan di dalam transaksi;
//   - deadlock / serialization failure → seluruh fungsi transaksi diulang, maksimal 3×.
package db

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"

	"github.com/exaring/otelpgx"

	"payrun/internal/platform/metrics"
	"payrun/internal/platform/testhook"
	"payrun/internal/platform/tracing"
)

// Options adalah pengaturan pool dan timeout per sesi.
type Options struct {
	MaxConns         int32
	StatementTimeout time.Duration
	LockTimeout      time.Duration
	IdleInTxTimeout  time.Duration
}

// NewPool membuat pool. Timeout dipasang sebagai parameter sesi sehingga berlaku untuk setiap
// koneksi: query lambat dibatalkan (statement_timeout), menunggu lock dibatasi (lock_timeout),
// dan transaksi yang lupa ditutup tidak menahan lock selamanya (idle_in_transaction...).
func NewPool(ctx context.Context, dsn string, o Options) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("db: parse dsn: %w", err)
	}
	if o.MaxConns > 0 {
		cfg.MaxConns = o.MaxConns
	}
	rp := cfg.ConnConfig.RuntimeParams
	rp["application_name"] = "payrun"
	if o.StatementTimeout > 0 {
		rp["statement_timeout"] = fmt.Sprint(o.StatementTimeout.Milliseconds())
	}
	if o.LockTimeout > 0 {
		rp["lock_timeout"] = fmt.Sprint(o.LockTimeout.Milliseconds())
	}
	if o.IdleInTxTimeout > 0 {
		rp["idle_in_transaction_session_timeout"] = fmt.Sprint(o.IdleInTxTimeout.Milliseconds())
	}
	// Span per query, dinamai dengan nama query sqlc ("-- name: LockWalletAccounts :many"): bebas PII
	// dan bermakna. Teks SQL dan nilai parameter TIDAK ikut ke span. No-op kalau tracing nonaktif.
	cfg.ConnConfig.Tracer = otelpgx.NewTracer(otelpgx.WithSpanNameFunc(QueryName), otelpgx.WithDisableSQLStatementInAttributes())
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: new pool: %w", err)
	}
	return pool, nil
}

// DBTX adalah apa pun yang bisa menjalankan query: *pgxpool.Pool atau pgx.Tx. Bentuknya sama
// dengan DBTX hasil sqlc, sehingga nilai bertipe ini bisa diteruskan ke store.New() modul mana pun.
// Dipakai di interface antar-modul, supaya modul tidak perlu meng-import package store modul lain.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Beginner adalah *pgxpool.Pool (atau apa pun yang bisa membuka transaksi).
type Beginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

const maxTxAttempts = 3

// WithTx menjalankan fn di dalam satu transaksi Read Committed (default ADR-011).
// fn bisa dipanggil lebih dari sekali (retry), jadi fn TIDAK boleh punya efek samping di luar
// database (panggilan HTTP, kirim email): efek seperti itu lewat outbox (ADR-006).
func WithTx(ctx context.Context, b Beginner, fn func(tx pgx.Tx) error) error {
	return WithTxOptions(ctx, b, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, fn)
}

// WithTxOptions sama dengan WithTx dengan isolation level/akses yang bisa dipilih
// (contoh: RepeatableRead + ReadOnly untuk job verifikasi).
func WithTxOptions(ctx context.Context, b Beginner, opts pgx.TxOptions, fn func(tx pgx.Tx) error) error {
	var err error
	for attempt := 1; attempt <= maxTxAttempts; attempt++ {
		err = runTx(ctx, b, opts, fn)
		if isDeadlock(err) {
			deadlocks.Add(1)
			metrics.DBDeadlocks.WithLabelValues().Inc()
		}
		if err == nil || !retryable(err) || attempt == maxTxAttempts {
			return err
		}
		reason := "serialization"
		if isDeadlock(err) {
			reason = "deadlock"
		}
		metrics.DBTxRetries.WithLabelValues(reason).Inc()
		// Backoff 10 ms · 2^n ± 50% jitter, supaya dua transaksi yang bertabrakan tidak
		// bertabrakan lagi di waktu yang persis sama.
		base := 10 * time.Millisecond << (attempt - 1)
		jitter := time.Duration(rand.Int64N(int64(base))) - base/2 //nolint:gosec // G404: jitter retry, bukan kebutuhan kriptografi
		select {
		case <-time.After(base + jitter):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func runTx(ctx context.Context, b Beginner, opts pgx.TxOptions, fn func(tx pgx.Tx) error) (err error) {
	ctx, span := tracing.Start(ctx, "db.tx")
	defer func() {
		outcome := "commit"
		if err != nil {
			outcome = "rollback"
		}
		span.SetAttributes(attribute.String("db.tx.outcome", outcome))
		span.End()
	}()
	tx, err := b.BeginTx(ctx, opts)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	// Titik hook FS-04: "koneksi putus sebelum COMMIT" → rollback, tidak ada efek.
	if err = testhook.Pause(ctx, "db.before_commit"); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	// Titik hook FS-03: COMMIT sudah berhasil, tetapi pemanggil menerima error ("commit outcome
	// unknown"). Client akan retry dengan Idempotency-Key yang sama dan harus mendapat replay.
	return testhook.Pause(ctx, "db.after_commit")
}

// retryable: 40001 serialization_failure, 40P01 deadlock_detected. Keduanya berarti
// "transaksi ini kalah dalam tabrakan; mengulang dari awal aman".
func retryable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "40001" || pgErr.Code == "40P01"
	}
	return false
}

// deadlocks menghitung 40P01 yang pernah terjadi. Dengan urutan lock yang benar (ADR-L03) nilainya
// harus selalu 0; test concurrency memeriksanya. Sprint 6: diekspos sebagai metrik db_deadlock_total.
var deadlocks atomic.Int64

// Deadlocks mengembalikan jumlah deadlock yang terdeteksi sejak proses mulai.
func Deadlocks() int64 { return deadlocks.Load() }

func isDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

var sqlcName = regexp.MustCompile(`^--\s*name:\s*(\w+)`)

// QueryName mengambil nama query sqlc dari komentar pertama SQL; query tanpa nama (BEGIN, COMMIT,
// query tangan di test) diberi nama kata kunci pertamanya.
func QueryName(stmt string) string {
	stmt = strings.TrimSpace(stmt)
	if m := sqlcName.FindStringSubmatch(stmt); m != nil {
		return m[1]
	}
	if i := strings.IndexAny(stmt, " \n\t;"); i > 0 {
		return strings.ToUpper(stmt[:i])
	}
	return strings.ToUpper(stmt)
}
