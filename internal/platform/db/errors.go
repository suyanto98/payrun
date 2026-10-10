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
	"uq_users_email_lower": apperr.ErrEmailRegistered,
	"uq_tenants_slug":      apperr.ErrDuplicateCode.WithFields(apperr.FieldError{Field: "slug", Code: "DUPLICATE"}),
	// Sprint 2.
	"uq_departments_code":          apperr.ErrDuplicateCode.WithFields(apperr.FieldError{Field: "code", Code: "DUPLICATE"}),
	"uq_positions_code":            apperr.ErrDuplicateCode.WithFields(apperr.FieldError{Field: "code", Code: "DUPLICATE"}),
	"uq_employees_employee_no":     apperr.ErrEmployeeNoTaken,
	"uq_employees_nik_bidx":        apperr.ErrNIKTaken,
	"uq_employees_npwp_bidx":       apperr.ErrNPWPTaken,
	"uq_memberships_user":          apperr.ErrMembershipExists,
	"uq_memberships_invited_email": apperr.ErrMembershipExists,
	// Sprint 3: EXCLUDE effective-dated (23P01) → OVERLAPPING_PERIOD; kode unik master data.
	"ex_salary_assignments_no_overlap":  apperr.ErrOverlappingPeriod,
	"ex_employee_components_no_overlap": apperr.ErrOverlappingPeriod,
	"ex_org_assignments_no_overlap":     apperr.ErrOverlappingPeriod,
	"ex_employment_spans_no_overlap":    apperr.ErrOverlappingPeriod,
	"uq_component_definitions_code":     apperr.ErrDuplicateCode.WithFields(apperr.FieldError{Field: "code", Code: "DUPLICATE"}),
	"uq_holidays_date":                  apperr.ErrDuplicateCode.WithFields(apperr.FieldError{Field: "date", Code: "DUPLICATE"}),
	// Sprint 6: cuti/lembur tumpang tindih; saldo cuti di bawah batas (backstop, layanan memeriksa lebih
	// dulu dan menyertakan saldo).
	"ex_leave_requests_no_overlap":    apperr.ErrOverlappingLeave,
	"ex_overtime_requests_no_overlap": apperr.ErrOverlappingPeriod,
	"ck_leave_balances_min":           apperr.ErrInsufficientBalance,
	// Berikutnya bersama tabelnya, contoh uq_payroll_runs_active_per_period → ACTIVE_RUN_EXISTS (S4).
	// Pelanggaran yang berarti BUG (RLS WITH CHECK 42501, trigger immutability run final) sengaja
	// TIDAK dipetakan → 500 + alert.
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
	case "P0005": // trigger PI-19 forbid_manager_cycle
		return apperr.ErrManagerCycle.WithCause(err)
	case "P0001": // trigger PI-04/PI-15: run/periode/adjustment final immutable
		metrics.FinalizedMutationAttempts.WithLabelValues(tableLabel(pgErr)).Inc()
		return apperr.ErrRunFinalized.WithCause(err)
	case "P0002": // trigger PI-14 guard_period_*: tanggal di periode FINALIZED
		metrics.PeriodLockBypass.WithLabelValues(tableLabel(pgErr)).Inc()
		return apperr.ErrPeriodLocked.WithCause(err)
	case "P0004": // trigger PI-16: owner terakhir
		return apperr.ErrLastOwner.WithCause(err)
	}
	return err
}

// IsUniqueViolation melaporkan apakah err adalah unique violation pada constraint/index bernama itu.
func IsUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

// tableLabel: tabel asal error trigger (TG_TABLE_NAME di pesan tidak terstruktur → pakai TableName bila
// ada), "unknown" bila kosong. Himpunan tabel terbatas (skema), aman sebagai label.
func tableLabel(e *pgconn.PgError) string {
	if e.TableName != "" {
		return e.TableName
	}
	return "unknown"
}
