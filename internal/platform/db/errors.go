package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"payrun/internal/platform/apperr"
	"payrun/internal/platform/metrics"
)

// constraintErrors memetakan nama constraint (atau unique index) → error domain.
// Nama constraint eksplisit dan stabil adalah kontrak (konvensi Step 4; katalog error §4).
// Constraint yang pelanggarannya berarti BUG (append-only, journal tidak seimbang) sengaja
// TIDAK dipetakan: ia jatuh ke INTERNAL_ERROR dan harus berbunyi di log/alert.
// Test TestDBErrorMapping_AllCatalogueConstraints memastikan setiap nama di sini benar-benar ada di DB.
var constraintErrors = map[string]*apperr.Error{
	// Diisi bersama tabelnya, contoh uq_users_email_lower → EMAIL_ALREADY_REGISTERED (S1),
	// uq_payroll_runs_active_per_period → ACTIVE_RUN_EXISTS (S4). Pelanggaran yang berarti BUG
	// (RLS WITH CHECK 42501, trigger immutability run final) sengaja TIDAK dipetakan → 500 + alert.
}

// ConstraintNames mengembalikan nama constraint yang dipetakan (dipakai test katalog).
func ConstraintNames() []string {
	names := make([]string, 0, len(constraintErrors))
	for n := range constraintErrors {
		names = append(names, n)
	}
	return names
}

// MapError mengubah error Postgres menjadi error domain. Error yang tidak dikenal dikembalikan
// apa adanya; lapisan HTTP akan menjadikannya 500 generik (pesan Postgres tidak bocor ke client).
func MapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return apperr.ErrNotFound
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return apperr.ErrUnavailable.WithCause(err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	if e, ok := constraintErrors[pgErr.ConstraintName]; ok {
		return e.WithCause(err)
	}
	switch pgErr.Code {
	case "55P03": // lock_not_available (lock_timeout)
		metrics.DBLockTimeouts.WithLabelValues("resource").Inc()
		return apperr.ErrResourceBusy.WithCause(err)
	case "40001", "40P01": // retry WithTx sudah habis
		return apperr.ErrResourceBusy.WithCause(err)
	case "57014": // query_canceled (statement_timeout)
		return apperr.ErrUnavailable.WithCause(err)
	case "23503": // FK: referensi tidak ada; tidak membedakan "tidak ada" vs "bukan milikmu"
		return apperr.ErrNotFound.WithCause(err)
	}
	return err
}

// IsUniqueViolation melaporkan apakah err adalah unique violation pada constraint/index bernama itu.
func IsUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}
