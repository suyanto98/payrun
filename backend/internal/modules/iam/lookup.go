package iam

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payrun/internal/platform/db"
)

type lookupRow struct {
	ID               uuid.UUID
	PasswordHash     string
	Status           string
	FailedLoginCount int32
	LockedUntil      *time.Time
}

// authLookup memanggil payrun.auth_lookup_user (SECURITY DEFINER, satu-satunya jalan membaca
// password_hash; 13-db-rls §2.3). Ditulis tangan karena sqlc tidak bisa menyimpulkan kolom fungsi
// RETURNS TABLE. Tanpa context (user belum diketahui).
func authLookup(ctx context.Context, q db.DBTX, email string) (lookupRow, bool, error) {
	var r lookupRow
	err := q.QueryRow(ctx, `-- name: AuthLookupUser
		SELECT id, password_hash, status, failed_login_count, locked_until FROM auth_lookup_user($1)`, email).
		Scan(&r.ID, &r.PasswordHash, &r.Status, &r.FailedLoginCount, &r.LockedUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return lookupRow{}, false, nil
	}
	if err != nil {
		return lookupRow{}, false, db.MapError(err)
	}
	return r, true, nil
}
