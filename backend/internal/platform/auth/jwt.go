package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"payrun/internal/platform/clock"
	"payrun/internal/platform/idgen"
)

// Claims adalah isi access token (ADR-009, ADR-P02): sub, tid, roles, iat, exp, jti, iss, aud.
// `tid` = tenant aktif, hanya diterbitkan setelah membership ACTIVE dicek di DB; `roles` = role di
// tenant itu (kosong kalau token tanpa tid). Ditandatangani, jadi tidak bisa diubah client.
type Claims struct {
	TenantID string   `json:"tid,omitempty"`
	Roles    []string `json:"roles"`
	jwt.RegisteredClaims
}

const audience = "payrun-api"

// clockSkew: toleransi selisih jam antar mesin saat memeriksa exp/nbf/iat.
const clockSkew = 30 * time.Second

// Tokens menerbitkan dan memverifikasi access token HS256.
// keys berisi SEMUA kunci yang masih valid untuk verifikasi (map kid → secret); activeKID
// adalah kunci untuk menandatangani. Rotasi: tambahkan kunci baru, jadikan aktif, lalu buang
// kunci lama setelah semua token lama kedaluwarsa (≥ 15 menit). Runbook: Step 6 `04` §6.1.
type Tokens struct {
	keys      map[string][]byte
	activeKID string
	issuer    string
	ttl       time.Duration
	clock     clock.Clock
	ids       idgen.Generator
}

func NewTokens(keys map[string][]byte, activeKID, issuer string, ttl time.Duration, c clock.Clock, ids idgen.Generator) (*Tokens, error) {
	if _, ok := keys[activeKID]; !ok {
		return nil, fmt.Errorf("auth: active kid %q not in keys", activeKID)
	}
	return &Tokens{keys: keys, activeKID: activeKID, issuer: issuer, ttl: ttl, clock: c, ids: ids}, nil
}

// Issue membuat access token untuk user. tenantID = uuid.Nil → token tanpa `tid` (user belum memilih
// tenant); roles diabaikan karena role selalu melekat pada membership tenant.
func (t *Tokens) Issue(userID, tenantID uuid.UUID, roles []string) (token string, expiresAt time.Time, err error) {
	now := t.clock.Now()
	expiresAt = now.Add(t.ttl)
	claims := Claims{Roles: []string{}}
	if tenantID != uuid.Nil {
		claims.TenantID, claims.Roles = tenantID.String(), roles
	}
	claims.RegisteredClaims = jwt.RegisteredClaims{
		Subject:   userID.String(),
		Issuer:    t.issuer,
		Audience:  jwt.ClaimStrings{audience},
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(expiresAt),
		ID:        t.ids.New().String(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tok.Header["kid"] = t.activeKID
	token, err = tok.SignedString(t.keys[t.activeKID])
	return token, expiresAt, err
}

var (
	ErrTokenInvalid = errors.New("auth: invalid token")
	ErrTokenExpired = errors.New("auth: token expired")
)

// Verify memeriksa token secara ketat (C-04):
//   - algoritma DIKUNCI ke HS256: token "alg":"none" atau RS256/ES256 ditolak (alg confusion);
//   - kid wajib dan harus dikenal;
//   - iss, aud, exp diperiksa, dengan toleransi jam 30 detik.
func (t *Tokens) Verify(token string) (*Claims, error) {
	var claims Claims
	parsed, err := jwt.ParseWithClaims(token, &claims,
		func(tok *jwt.Token) (any, error) {
			kid, _ := tok.Header["kid"].(string)
			key, ok := t.keys[kid]
			if !ok {
				return nil, fmt.Errorf("unknown kid %q", kid)
			}
			return key, nil
		},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(t.issuer),
		jwt.WithAudience(audience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(clockSkew),
		jwt.WithTimeFunc(t.clock.Now),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, fmt.Errorf("%w: %v", ErrTokenInvalid, err) //nolint:errorlint // detail hanya untuk log
	}
	if !parsed.Valid {
		return nil, ErrTokenInvalid
	}
	if _, err := uuid.Parse(claims.Subject); err != nil {
		return nil, ErrTokenInvalid
	}
	if claims.TenantID != "" {
		if tid, err := uuid.Parse(claims.TenantID); err != nil || tid == uuid.Nil {
			return nil, ErrTokenInvalid
		}
	}
	return &claims, nil
}
