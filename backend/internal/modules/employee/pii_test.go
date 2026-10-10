package employee

import (
	"bytes"
	"encoding/base64"
	"testing"

	"github.com/google/uuid"

	"payrun/internal/platform/crypto"
)

func testPII(t *testing.T) PII {
	t.Helper()
	k := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	kr, err := crypto.ParseKeyring(`{"v1":"`+k+`"}`, "v1")
	if err != nil {
		t.Fatal(err)
	}
	bi, err := crypto.ParseBlindIndexKey(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return PII{Keyring: kr, BlindIndex: bi}
}

// pii_roundtrip: nilai yang disegel terbaca kembali utuh, ciphertext tidak memuat plaintext, last4 benar.
func TestPII_Roundtrip(t *testing.T) {
	p := testPII(t)
	tenant := uuid.New()
	s, err := p.seal(tenant, "nik", "3174012304950001", true)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(s.enc, []byte("3174012304950001")) || s.last4 != "0001" || len(s.bidx) != 32 {
		t.Fatalf("sealed: enc leaks plaintext or wrong last4/bidx: %+v", s)
	}
	got, err := p.open(tenant, "nik", s.enc)
	if err != nil || got != "3174012304950001" {
		t.Fatalf("open: %q %v", got, err)
	}
	acc, _ := p.seal(tenant, "bank_account", "1234567890", false)
	if acc.bidx != nil {
		t.Fatal("bank account must not get a blind index (ADR-P08)")
	}
}

// pii_aad_swap_fails: ciphertext NIK tenant A yang disalin ke baris tenant B, atau ke kolom lain
// (NPWP), tidak bisa didekripsi: AAD mengikat ciphertext ke tenant + kolom.
func TestPII_AADSwapFails(t *testing.T) {
	p := testPII(t)
	alpha, beta := uuid.New(), uuid.New()
	s, _ := p.seal(alpha, "nik", "3174012304950001", true)
	if _, err := p.open(beta, "nik", s.enc); err == nil {
		t.Fatal("ciphertext copied to another tenant must not decrypt")
	}
	if _, err := p.open(alpha, "npwp", s.enc); err == nil {
		t.Fatal("ciphertext copied to another column must not decrypt")
	}
}

func TestPII_NormalizeAndMask(t *testing.T) {
	if got := digitsOnly(" 01.234.567.8-901.234 "); got != "012345678901234" {
		t.Fatalf("npwp normalize: %q", got)
	}
	if allDigits("31740123x4950001", 16, 16) || !allDigits("3174012304950001", 16, 16) {
		t.Fatal("digit validation")
	}
	l4 := "0001"
	if *maskNIK(&l4) != "************0001" || *maskNPWP(&l4) != "***********0001" || *maskAccount(&l4) != "******0001" {
		t.Fatal("masks")
	}
	if maskNIK(nil) != nil {
		t.Fatal("absent value stays null")
	}
}
