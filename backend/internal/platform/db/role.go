package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// RoleCheck adalah hasil pemeriksaan role koneksi aplikasi (docs/design/13-db-rls-consistency-indexing.md §1.1).
type RoleCheck struct {
	Role       string
	Superuser  bool // superuser melewati RLS
	BypassRLS  bool // atribut BYPASSRLS melewati RLS
	OwnsSchema bool // pemilik schema/tabel melewati RLS di tabel yang lupa FORCE, dan bisa ALTER/DISABLE RLS
}

// ErrUnsafeRole: koneksi aplikasi memakai role yang bisa melewati RLS.
var ErrUnsafeRole = errors.New("db: application role can bypass row-level security")

// CheckAppRole adalah startup check (fail fast) api dan worker: RLS hanya melindungi kalau role
// aplikasi bukan superuser, tidak punya BYPASSRLS, dan tidak memiliki schema payrun maupun tabel
// di dalamnya. Salah DSN (misalnya memakai payrun_owner di container api) membuat isolasi tenant diam-diam
// bergantung pada FORCE di setiap tabel; lebih baik proses menolak start (residual risk "salah
// konfigurasi role", docs/design/06-multi-tenancy.md §5).
//
// Berjalan di transaksi READ ONLY: pemeriksaan tidak mungkin mengubah apa pun.
func CheckAppRole(ctx context.Context, b Beginner) (RoleCheck, error) {
	var rc RoleCheck
	err := WithTxOptions(ctx, b, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT r.rolname, r.rolsuper, r.rolbypassrls,
			       EXISTS (SELECT 1 FROM pg_namespace n WHERE n.nspname = 'payrun' AND n.nspowner = r.oid)
			       OR EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			                  WHERE n.nspname = 'payrun' AND c.relowner = r.oid) AS owns_schema
			FROM pg_roles r WHERE r.rolname = current_user`).
			Scan(&rc.Role, &rc.Superuser, &rc.BypassRLS, &rc.OwnsSchema)
	})
	if err != nil {
		return rc, fmt.Errorf("db: role check: %w", err)
	}
	if rc.Superuser || rc.BypassRLS || rc.OwnsSchema {
		return rc, fmt.Errorf("%w: role=%s superuser=%t bypassrls=%t owns_schema=%t",
			ErrUnsafeRole, rc.Role, rc.Superuser, rc.BypassRLS, rc.OwnsSchema)
	}
	return rc, nil
}
