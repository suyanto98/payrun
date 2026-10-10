package crypto

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

// BlindIndex: HMAC-SHA256 untuk keunikan dan pencarian kesamaan pada field terenkripsi (ADR-P08).
// Kunci TERPISAH dari keyring enkripsi: bocornya salah satu tidak membuka yang lain. Input selalu
// diikat ke tenant dan field, sehingga NIK yang sama di dua tenant menghasilkan index berbeda (tidak
// bisa dikorelasikan lintas tenant dari dump DB).
type BlindIndex struct{ key []byte }

// ParseBlindIndexKey membaca base64 dari tepat 32 byte.
func ParseBlindIndexKey(b64 string) (*BlindIndex, error) {
	key, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(key) != 32 {
		return nil, errors.New("crypto: kunci blind index harus base64 dari tepat 32 byte")
	}
	return &BlindIndex{key: key}, nil
}

// Sum = HMAC(key, scope || 0x00 || normalized). scope contoh "<tenant_id>:employees.nik".
func (b *BlindIndex) Sum(scope, normalized string) []byte {
	m := hmac.New(sha256.New, b.key)
	m.Write([]byte(scope))
	m.Write([]byte{0})
	m.Write([]byte(normalized))
	return m.Sum(nil)
}
