// Package auth: hashing password (argon2id), access token JWT, dan principal di context.
// Keputusan: docs/adr/ADR-009-authentication.md; threat T-SH-01..05 (docs/design/15-…).
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2Params: memory dalam KiB. Nilai awal ADR-009: 64 MiB, 3 iterasi, paralelisme 2.
type Argon2Params struct {
	Memory      uint32
	Iterations  uint32
	Parallelism uint8
	SaltLen     uint32
	KeyLen      uint32
}

var DefaultArgon2 = Argon2Params{Memory: 64 * 1024, Iterations: 3, Parallelism: 2, SaltLen: 16, KeyLen: 32}

// TestArgon2 jauh lebih murah, hanya untuk test (jangan dipakai di production).
var TestArgon2 = Argon2Params{Memory: 8 * 1024, Iterations: 1, Parallelism: 1, SaltLen: 16, KeyLen: 32}

var errBadHash = errors.New("auth: malformed password hash")

// HashPassword menghasilkan string encoded standar:
// $argon2id$v=19$m=65536,t=3,p=2$<salt b64>$<hash b64>
// Parameter ikut tersimpan, jadi bisa dinaikkan nanti tanpa merusak hash lama (rehash saat login).
func HashPassword(password string, p Argon2Params) (string, error) {
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, p.Iterations, p.Memory, p.Parallelism, p.KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Iterations, p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword membandingkan password dengan hash secara constant-time.
func VerifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errBadHash
	}
	var p Argon2Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Iterations, &p.Parallelism); err != nil {
		return false, errBadHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, errBadHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, errBadHash
	}
	got := argon2.IDKey([]byte(password), salt, p.Iterations, p.Memory, p.Parallelism, uint32(len(want))) //nolint:gosec // len(want) = 32
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// DummyHash dipakai saat email tidak ditemukan: server tetap menjalankan argon2 supaya waktu
// respons "email tidak ada" ≈ "password salah". Tanpa ini, penyerang bisa menebak email
// terdaftar dari selisih waktu (timing side channel, C-03).
func DummyHash(p Argon2Params) string {
	h, _ := HashPassword("dummy-password-for-timing-equalization", p)
	return h
}
