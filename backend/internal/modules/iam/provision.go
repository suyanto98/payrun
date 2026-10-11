package iam

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payrun/internal/modules/iam/store"
	"payrun/internal/platform/db"
)

// Provisioning langsung (tanpa email verifikasi / undangan). Hanya dipakai sandbox demo (build tag `demo`),
// yang membuat perusahaan contoh beserta penggunanya dalam satu klik. Jalur produk tetap signup + undangan.

// ProvisionVerifiedUserTx membuat user yang emailnya dianggap terverifikasi (alamat sandbox tidak bisa
// menerima email). Mode platform untuk user baru, sama dengan CreateUserTx; pemanggil berpindah ke mode
// tenant sendiri sesudahnya.
func (s *Service) ProvisionVerifiedUserTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, email, passwordHash, displayName string) error {
	if err := s.CreateUserTx(ctx, tx, id, email, passwordHash, displayName); err != nil {
		return err
	}
	now := s.clock.Now()
	_, err := store.New(tx).MarkEmailVerified(ctx, store.MarkEmailVerifiedParams{Now: &now, ID: id})
	return err
}

// AddMemberTx membuat membership ACTIVE dengan role tertentu (mode tenant).
func (s *Service) AddMemberTx(ctx context.Context, tx pgx.Tx, tenantID, userID, actor uuid.UUID, roles []string) error {
	now := s.clock.Now()
	return store.New(tx).AddActiveMembership(ctx, store.AddActiveMembershipParams{
		TenantID: tenantID, ID: s.ids.New(), UserID: &userID, Roles: roles, Now: &now, Actor: &actor,
	})
}

// RevokeSessionsTx mencabut semua refresh token user (reason ADMIN). Fungsi paket karena dipanggil worker,
// yang tidak punya Service. Access token yang sudah terbit tetap berlaku sampai kedaluwarsa (≤ 15 menit).
func RevokeSessionsTx(ctx context.Context, tx pgx.Tx, now time.Time, userIDs []uuid.UUID) error {
	reason := "ADMIN"
	q := store.New(tx)
	for _, id := range userIDs {
		if err := q.RevokeUserRefreshTokens(ctx, store.RevokeUserRefreshTokensParams{Now: &now, Reason: &reason, UserID: id}); err != nil {
			return db.MapError(err)
		}
	}
	return nil
}
