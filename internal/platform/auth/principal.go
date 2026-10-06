package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"payrun/internal/platform/apperr"
	"payrun/internal/platform/httpx"
	"payrun/internal/platform/observability"
	"payrun/internal/platform/tenantctx"
)

// Principal adalah identitas pemanggil yang sudah terverifikasi. Identitas SELALU diambil dari
// token yang diverifikasi server, tidak pernah dari body/query (cegah privilege escalation).
type Principal struct {
	UserID   uuid.UUID
	TenantID uuid.UUID // uuid.Nil = token tanpa `tid` (belum memilih tenant)
	Roles    []string  // role di TenantID; kosong kalau tanpa tenant
}

type principalKey struct{}

// PrincipalFrom mengambil principal; ok=false jika request tidak terautentikasi.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// WithPrincipal dipakai middleware (dan test) untuk menaruh principal di context.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// HasRole: apakah principal memiliki role tertentu di tenant aktif. Role melekat pada membership
// tenant (OWNER, HR_ADMIN, PAYROLL_ADMIN, MANAGER, EMPLOYEE), diterbitkan bersama claim `tid`.
func (p Principal) HasRole(role string) bool {
	for _, r := range p.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// RequireTenant dipasang SETELAH RequireUser pada semua endpoint tenant-scoped: token tanpa `tid`
// → 403 "Select a tenant first" (16-api-endpoints §1). Tenant aktif sudah ada di tenantctx.
func RequireTenant(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := PrincipalFrom(r.Context()); !ok || p.TenantID == uuid.Nil {
			httpx.WriteError(w, r, apperr.ErrForbidden.WithDetail("Select a tenant first"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireRole dipasang SETELAH RequireTenant. User tanpa role → 403 (ADR-010). Resource milik
// tenant lain tetap 404, bukan 403: itu urusan RLS + query, bukan middleware ini.
func RequireRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFrom(r.Context())
			if !ok {
				httpx.WriteError(w, r, apperr.ErrUnauthenticated)
				return
			}
			if !p.HasRole(role) {
				httpx.WriteError(w, r, apperr.ErrForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireUser memverifikasi "Authorization: Bearer <jwt>" dan menaruh Principal di context.
func RequireUser(t *Tokens) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || raw == "" {
				httpx.WriteError(w, r, apperr.ErrUnauthenticated)
				return
			}
			claims, err := t.Verify(raw)
			switch {
			case errors.Is(err, ErrTokenExpired):
				httpx.WriteError(w, r, apperr.ErrTokenExpired)
				return
			case err != nil:
				observability.Logger(r.Context()).Info("auth.token_rejected", "reason", err.Error())
				httpx.WriteError(w, r, apperr.ErrUnauthenticated)
				return
			}
			p := Principal{UserID: uuid.MustParse(claims.Subject), Roles: claims.Roles}
			httpx.SetUserID(r.Context(), p.UserID.String()) // agar log akses memuat user_id
			ctx := WithPrincipal(r.Context(), p)
			logger := observability.Logger(ctx).With("user_id", p.UserID.String())
			if claims.TenantID != "" { // sudah divalidasi Verify
				p.TenantID = uuid.MustParse(claims.TenantID)
				ctx = WithPrincipal(ctx, p)
				// Satu-satunya tempat tenant request ditentukan: dari claim yang ditandatangani (ADR-P02).
				ctx = tenantctx.With(ctx, p.TenantID)
				logger = logger.With("tenant_id", p.TenantID.String())
				httpx.SetTenantID(ctx, p.TenantID.String())
			}
			ctx = observability.WithLogger(ctx, logger)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
