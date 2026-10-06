// Package crypto: enkripsi PII at-rest dengan AES-256-GCM dan keyring berversi
// (docs/design/17-security-controls.md §6.3).
//
// Format ciphertext: "<versi>:" + nonce(12) + ciphertext+tag. Versi di depan membuat rotasi kunci
// tanpa downtime: data baru memakai kunci aktif, data lama tetap terbaca dengan kunci lamanya,
// lalu job re-encrypt memindahkannya.
//
// AAD (additional authenticated data) mengikat ciphertext ke pemiliknya (contoh: payout id). Blob
// rekening milik payout A yang disalin ke baris payout B gagal didekripsi, karena AAD-nya berbeda.
package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

var ErrDecrypt = errors.New("crypto: cannot decrypt (unknown key version, tampered data, or wrong AAD)")

type Keyring struct {
	active string
	aeads  map[string]cipher.AEAD
}

// ParseKeyring membaca JSON {"v1":"base64-32-byte", ...}; active = versi untuk enkripsi baru.
func ParseKeyring(raw, active string) (*Keyring, error) {
	var enc map[string]string
	if err := json.Unmarshal([]byte(raw), &enc); err != nil {
		return nil, fmt.Errorf("crypto: keyring harus JSON {versi: base64}: %w", err)
	}
	k := &Keyring{active: active, aeads: map[string]cipher.AEAD{}}
	for v, b64 := range enc {
		key, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("crypto: kunci %q harus base64 dari tepat 32 byte (AES-256)", v)
		}
		block, _ := aes.NewCipher(key)
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		k.aeads[v] = aead
	}
	if _, ok := k.aeads[active]; !ok {
		return nil, fmt.Errorf("crypto: versi aktif %q tidak ada di keyring", active)
	}
	return k, nil
}

// Encrypt mengenkripsi dengan kunci aktif. Nonce acak 96 bit per pesan (aman untuk volume ini).
func (k *Keyring) Encrypt(plain, aad []byte) ([]byte, error) {
	aead := k.aeads[k.active]
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte(k.active+":"), nonce...)
	return aead.Seal(out, nonce, plain, aad), nil
}

// Decrypt membaca versi dari prefix lalu membuka dengan kunci versi itu.
func (k *Keyring) Decrypt(blob, aad []byte) ([]byte, error) {
	i := bytes.IndexByte(blob, ':')
	if i <= 0 {
		return nil, ErrDecrypt
	}
	aead, ok := k.aeads[string(blob[:i])]
	if !ok || len(blob) < i+1+aead.NonceSize() {
		return nil, ErrDecrypt
	}
	nonce, ct := blob[i+1:i+1+aead.NonceSize()], blob[i+1+aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plain, nil
}
