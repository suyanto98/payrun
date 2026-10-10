package auth_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"payrun/internal/platform/auth"
	"payrun/internal/platform/clock"
	"payrun/internal/platform/idgen"
)

var (
	keyA = []byte("0123456789abcdef0123456789abcdef") // 32 byte
	keyB = []byte("fedcba9876543210fedcba9876543210")
)

func newTokens(t *testing.T, c clock.Clock) *auth.Tokens {
	t.Helper()
	tk, err := auth.NewTokens(map[string][]byte{"kA": keyA, "kB": keyB}, "kA", "payrun", 15*time.Minute, c, idgen.V7{})
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestJWT_RoundTrip(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
	tk := newTokens(t, clk)
	uid, tid := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	tok, _, err := tk.Issue(uid, tid, []string{"OWNER"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := tk.Verify(tok)
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if c.Subject != uid.String() || c.TenantID != tid.String() || len(c.Roles) != 1 || c.Roles[0] != "OWNER" {
		t.Fatalf("unexpected claims: %+v", c)
	}

	// Tanpa tenant: tidak ada `tid`, dan roles selalu kosong (role melekat pada membership tenant).
	tok, _, _ = tk.Issue(uid, uuid.Nil, []string{"OWNER"})
	if c, err = tk.Verify(tok); err != nil || c.TenantID != "" || len(c.Roles) != 0 {
		t.Fatalf("token without tenant: want no tid and no roles, got %+v (%v)", c, err)
	}
}

// Kunci lama tetap bisa memverifikasi setelah kunci aktif diganti (rotasi tanpa logout massal).
func TestJWT_VerifiesTokenSignedByNonActiveKnownKid(t *testing.T) {
	clk := clock.NewFake(time.Now())
	old, err := auth.NewTokens(map[string][]byte{"kB": keyB}, "kB", "payrun", 15*time.Minute, clk, idgen.V7{})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, _ := old.Issue(uuid.Must(uuid.NewV7()), uuid.Nil, nil)
	if _, err := newTokens(t, clk).Verify(tok); err != nil {
		t.Fatalf("token signed with known non-active kid should verify: %v", err)
	}
}

func TestJWT_RejectsAlgNone(t *testing.T) {
	clk := clock.NewFake(time.Now())
	claims := jwt.MapClaims{"sub": uuid.NewString(), "iss": "payrun", "aud": "payrun-api",
		"iat": clk.Now().Unix(), "exp": clk.Now().Add(time.Hour).Unix()}
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	tok.Header["kid"] = "kA"
	s, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newTokens(t, clk).Verify(s); !errors.Is(err, auth.ErrTokenInvalid) {
		t.Fatalf("alg=none must be rejected, got %v", err)
	}
}

// Alg swap: token ditandatangani dengan algoritma lain (HS512) memakai kunci yang benar.
// Algoritma dikunci ke HS256, jadi tetap ditolak.
func TestJWT_RejectsAlgSwap(t *testing.T) {
	clk := clock.NewFake(time.Now())
	claims := jwt.MapClaims{"sub": uuid.NewString(), "iss": "payrun", "aud": "payrun-api",
		"iat": clk.Now().Unix(), "exp": clk.Now().Add(time.Hour).Unix()}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS512, claims)
	tok.Header["kid"] = "kA"
	s, _ := tok.SignedString(keyA)
	if _, err := newTokens(t, clk).Verify(s); !errors.Is(err, auth.ErrTokenInvalid) {
		t.Fatalf("HS512 token must be rejected, got %v", err)
	}
}

// Payload diubah tanpa menandatangani ulang → signature tidak cocok. Demo A: `tid` diganti ke
// tenant lain, atau role dinaikkan, ditolak di verifikasi signature (401), bukan di handler.
func TestJWT_RejectsTamperedClaims(t *testing.T) {
	clk := clock.NewFake(time.Now())
	tk := newTokens(t, clk)
	tok, _, _ := tk.Issue(uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), []string{"EMPLOYEE"})
	for name, tamper := range map[string]func(map[string]any){
		"tid swapped to another tenant": func(m map[string]any) { m["tid"] = uuid.NewString() },
		"role escalated":                func(m map[string]any) { m["roles"] = []string{"OWNER"} },
	} {
		parts := strings.Split(tok, ".")
		payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var m map[string]any
		_ = json.Unmarshal(payload, &m)
		tamper(m)
		forged, _ := json.Marshal(m)
		parts[1] = base64.RawURLEncoding.EncodeToString(forged)
		if _, err := tk.Verify(strings.Join(parts, ".")); !errors.Is(err, auth.ErrTokenInvalid) {
			t.Errorf("%s: tampered token must be rejected, got %v", name, err)
		}
	}
}

func TestJWT_RejectsUnknownKid(t *testing.T) {
	clk := clock.NewFake(time.Now())
	other, _ := auth.NewTokens(map[string][]byte{"kZ": keyA}, "kZ", "payrun", time.Minute, clk, idgen.V7{})
	tok, _, _ := other.Issue(uuid.Must(uuid.NewV7()), uuid.Nil, nil)
	if _, err := newTokens(t, clk).Verify(tok); !errors.Is(err, auth.ErrTokenInvalid) {
		t.Fatalf("unknown kid must be rejected, got %v", err)
	}
}

func TestJWT_ExpiryWithClockSkew(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
	tk := newTokens(t, clk)
	tok, _, _ := tk.Issue(uuid.Must(uuid.NewV7()), uuid.Nil, nil)

	clk.Advance(15*time.Minute + 20*time.Second) // lewat exp, masih dalam toleransi 30 dtk
	if _, err := tk.Verify(tok); err != nil {
		t.Fatalf("token within 30s skew should still verify: %v", err)
	}
	clk.Advance(20 * time.Second) // total 15m40s: di luar toleransi
	if _, err := tk.Verify(tok); !errors.Is(err, auth.ErrTokenExpired) {
		t.Fatalf("expired token must return ErrTokenExpired, got %v", err)
	}
}

func TestJWT_RejectsWrongIssuerOrAudience(t *testing.T) {
	clk := clock.NewFake(time.Now())
	for name, claims := range map[string]jwt.MapClaims{
		"wrong issuer":   {"sub": uuid.NewString(), "iss": "evil", "aud": "payrun-api", "iat": clk.Now().Unix(), "exp": clk.Now().Add(time.Hour).Unix()},
		"wrong audience": {"sub": uuid.NewString(), "iss": "payrun", "aud": "ledgerpay-api", "iat": clk.Now().Unix(), "exp": clk.Now().Add(time.Hour).Unix()},
	} {
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
		tok.Header["kid"] = "kA"
		s, _ := tok.SignedString(keyA)
		if _, err := newTokens(t, clk).Verify(s); !errors.Is(err, auth.ErrTokenInvalid) {
			t.Errorf("%s must be rejected, got %v", name, err)
		}
	}
}

func TestPassword_HashAndVerify(t *testing.T) {
	h, err := auth.HashPassword("correct horse battery", auth.TestArgon2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=8192,t=1,p=1$") {
		t.Fatalf("unexpected encoding: %s", h)
	}
	if ok, err := auth.VerifyPassword("correct horse battery", h); err != nil || !ok {
		t.Fatalf("correct password must verify: ok=%v err=%v", ok, err)
	}
	if ok, _ := auth.VerifyPassword("wrong password!", h); ok {
		t.Fatal("wrong password must not verify")
	}
	h2, _ := auth.HashPassword("correct horse battery", auth.TestArgon2)
	if h == h2 {
		t.Fatal("same password must produce different hashes (random salt)")
	}
}

func TestPassword_RejectsMalformedHash(t *testing.T) {
	for _, bad := range []string{"", "plain", "$argon2i$v=19$m=1,t=1,p=1$a$b", "$argon2id$v=19$m=x$a$b"} {
		if _, err := auth.VerifyPassword("x", bad); err == nil {
			t.Errorf("malformed hash %q must return error", bad)
		}
	}
}
