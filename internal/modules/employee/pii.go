package employee

import (
	"strings"

	"github.com/google/uuid"

	"payrun/internal/platform/crypto"
)

// PII (ADR-P08): NIK, NPWP, nomor rekening dienkripsi AES-256-GCM dengan AAD
// "<tenant_id>:employees.<field>" (ciphertext yang dipindah ke tenant atau kolom lain gagal
// didekripsi). NIK dan NPWP juga mendapat blind index HMAC (kunci terpisah, scope yang sama) untuk
// keunikan per tenant. Rekening tanpa blind index. last4 disimpan untuk masking tanpa dekripsi.

// PII adalah dependensi kripto modul ini (disuntikkan composition root dari config).
type PII struct {
	Keyring    *crypto.Keyring
	BlindIndex *crypto.BlindIndex
}

// sealed adalah satu field PII siap tulis.
type sealed struct {
	enc   []byte
	bidx  []byte // nil untuk field tanpa blind index
	last4 string
}

func scope(tenantID uuid.UUID, field string) string {
	return tenantID.String() + ":employees." + field
}

// seal mengenkripsi nilai yang sudah dinormalisasi (hanya digit).
func (p PII) seal(tenantID uuid.UUID, field, digits string, withIndex bool) (sealed, error) {
	enc, err := p.Keyring.Encrypt([]byte(digits), []byte(scope(tenantID, field)))
	if err != nil {
		return sealed{}, err
	}
	s := sealed{enc: enc, last4: digits[len(digits)-4:]}
	if withIndex {
		s.bidx = p.BlindIndex.Sum(scope(tenantID, field), digits)
	}
	return s, nil
}

// open mendekripsi satu field (dipakai full-view W6 di S4 dan test round-trip).
func (p PII) open(tenantID uuid.UUID, field string, enc []byte) (string, error) {
	b, err := p.Keyring.Decrypt(enc, []byte(scope(tenantID, field)))
	return string(b), err
}

// digitsOnly menormalisasi input: spasi, titik, dan tanda hubung dibuang (format NPWP
// "01.234.567.8-901.234"). Karakter lain dibiarkan sehingga validasi digit menolaknya.
func digitsOnly(s string) string {
	return strings.NewReplacer(" ", "", ".", "", "-", "").Replace(strings.TrimSpace(s))
}

func allDigits(s string, minLen, maxLen int) bool {
	if len(s) < minLen || len(s) > maxLen {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Masking default (ADR-P08): hanya 4 digit terakhir, panjang tetap supaya panjang asli tidak bocor.
func maskNIK(last4 *string) *string     { return mask("************", last4) }
func maskNPWP(last4 *string) *string    { return mask("***********", last4) }
func maskAccount(last4 *string) *string { return mask("******", last4) }

func mask(prefix string, last4 *string) *string {
	if last4 == nil {
		return nil
	}
	v := prefix + *last4
	return &v
}
