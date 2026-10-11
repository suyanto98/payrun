package db

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payrun/internal/platform/tenantctx"
)

// ErrNoTenantContext: WithTenantTx dipanggil tanpa tenant aktif. Fail closed: tidak pernah ada
// fallback ke "tanpa tenant" (ADR-P02). Kalau ini sampai ke client, itu bug wiring (500), karena
// endpoint tenant-scoped sudah ditolak lebih awal oleh middleware (403 "Select a tenant first").
var ErrNoTenantContext = errors.New("db: no tenant in context")

// WithTenantTx menjalankan fn di transaksi dengan app.tenant_id = tenant aktif request (dari claim
// `tid`, lewat tenantctx). Satu-satunya jalur modul bisnis ke data tenant. Mode eksklusif (final review C2):
// HANYA app.tenant_id yang disetel, sehingga policy "self" (users_self, memberships_self_read) mati.
func WithTenantTx(ctx context.Context, b Beginner, fn func(tx pgx.Tx) error) error {
	tid, ok := tenantctx.From(ctx)
	if !ok {
		return ErrNoTenantContext
	}
	return WithTenantTxFor(ctx, b, tid, fn)
}

// WithTenantTxFor sama dengan WithTenantTx, dengan tenant eksplisit. Dipakai jalur yang tenant-nya
// TIDAK berasal dari token tetapi tetap terverifikasi: handler job (jobs.tenant_id, dijaga trigger),
// signup (tenant baru yang dibuat di transaksi itu), accept undangan (tenant dari token rahasia).
func WithTenantTxFor(ctx context.Context, b Beginner, tenantID uuid.UUID, fn func(tx pgx.Tx) error) error {
	if tenantID == uuid.Nil {
		return ErrNoTenantContext
	}
	return WithTx(ctx, b, func(tx pgx.Tx) error {
		if err := SetTenant(ctx, tx, tenantID); err != nil {
			return err
		}
		return fn(tx)
	})
}

// WithTenantTxForOptions sama dengan WithTenantTxFor dengan isolation/akses pilihan. Dipakai txRead
// chunk payroll (REPEATABLE READ READ ONLY, `22-tx-concurrency.md` §3.2): satu snapshot konsisten untuk semua
// input chunk. set_config adalah statement pertama dan tidak membaca tabel, jadi snapshot RR baru
// diambil pada query data pertama (`22` §2).
func WithTenantTxForOptions(ctx context.Context, b Beginner, tenantID uuid.UUID, opts pgx.TxOptions, fn func(tx pgx.Tx) error) error {
	if tenantID == uuid.Nil {
		return ErrNoTenantContext
	}
	return WithTxOptions(ctx, b, opts, func(tx pgx.Tx) error {
		if err := SetTenant(ctx, tx, tenantID); err != nil {
			return err
		}
		return fn(tx)
	})
}

// WithPlatformTx menjalankan fn dengan app.user_id = userID saja (mode platform): yang terlihat hanya
// users (diri sendiri), memberships milik user (baca), dan tenants tempat user anggota ACTIVE.
// Hanya modul iam/tenancy yang boleh memakainya (depguard).
func WithPlatformTx(ctx context.Context, b Beginner, userID uuid.UUID, fn func(tx pgx.Tx) error) error {
	if userID == uuid.Nil {
		return errors.New("db: WithPlatformTx needs a user id")
	}
	return WithTx(ctx, b, func(tx pgx.Tx) error {
		if err := SetPlatformUser(ctx, tx, userID); err != nil {
			return err
		}
		return fn(tx)
	})
}

// SetTenant berpindah ke mode tenant di transaksi yang sedang berjalan: app.tenant_id disetel dan
// app.user_id dikosongkan (mode eksklusif). Transaction-local (is_local = true): hilang saat COMMIT
// atau ROLLBACK, tidak pernah menempel di koneksi pool. Dipakai langsung hanya oleh signup, yang
// berpindah dari fase platform ke fase tenant di SATU transaksi (13-db-rls §2.3).
func SetTenant(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	_, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true), set_config('app.user_id', '', true)`, tenantID.String())
	return err
}

// SetPlatformUser berpindah ke mode platform untuk userID (app.tenant_id dikosongkan).
func SetPlatformUser(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	_, err := tx.Exec(ctx, `SELECT set_config('app.user_id', $1, true), set_config('app.tenant_id', '', true)`, userID.String())
	return err
}
