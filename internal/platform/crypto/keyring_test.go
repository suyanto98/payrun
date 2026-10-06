package crypto

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"
)

func key(b byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)) }

func TestKeyring_RoundTripRotationAndAAD(t *testing.T) {
	old, err := ParseKeyring(`{"v1":"`+key(1)+`"}`, "v1")
	if err != nil {
		t.Fatal(err)
	}
	blobV1, _ := old.Encrypt([]byte("1234567890"), []byte("payout-A"))
	if !bytes.HasPrefix(blobV1, []byte("v1:")) || bytes.Contains(blobV1, []byte("1234567890")) {
		t.Fatal("ciphertext must carry its key version and not contain the plaintext")
	}

	rotated, _ := ParseKeyring(`{"v1":"`+key(1)+`","v2":"`+key(2)+`"}`, "v2")
	if got, err := rotated.Decrypt(blobV1, []byte("payout-A")); err != nil || string(got) != "1234567890" {
		t.Fatalf("old data must stay readable after rotation: %q %v", got, err)
	}
	blobV2, _ := rotated.Encrypt([]byte("1234567890"), []byte("payout-A"))
	if !bytes.HasPrefix(blobV2, []byte("v2:")) {
		t.Fatal("new data must use the active key")
	}

	if _, err := rotated.Decrypt(blobV1, []byte("payout-B")); !errors.Is(err, ErrDecrypt) {
		t.Fatal("ciphertext copied to another row (different AAD) must not decrypt")
	}
	tampered := append([]byte{}, blobV2...)
	tampered[len(tampered)-1] ^= 1
	if _, err := rotated.Decrypt(tampered, []byte("payout-A")); !errors.Is(err, ErrDecrypt) {
		t.Fatal("tampered ciphertext must not decrypt (GCM tag)")
	}
	if _, err := ParseKeyring(`{"v1":"`+base64.StdEncoding.EncodeToString([]byte("short"))+`"}`, "v1"); err == nil {
		t.Fatal("keys must be exactly 32 bytes")
	}
}
