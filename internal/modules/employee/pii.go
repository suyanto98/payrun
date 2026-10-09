package employee

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payrun/internal/modules/employee/store"
	"payrun/internal/platform/apperr"
	"payrun/internal/platform/audit"
	"payrun/internal/platform/auth"
	"payrun/internal/platform/crypto"
	"payrun/internal/platform/db"
	"payrun/internal/platform/reqmeta"
	"payrun/internal/platform/tenantctx"
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

// open mendekripsi satu field (full view W6 dan test round-trip).
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

// ---------------------------------------------------------------- W6 full view (Sprint 4)

// Memberships: role membership dibaca ulang dari DB untuk operasi sensitif (ADR-P02), diimplementasikan
// modul iam dan dipasang composition root (ADR-001: modul ini tidak meng-query memberships).
type Memberships interface {
	ActiveRolesTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID) ([]string, error)
}

// SetMemberships memasang implementasi Memberships (wajib sebelum ViewPII dipakai).
func (s *Service) SetMemberships(m Memberships) { s.members = m }

type PIIBank struct {
	Name          *string `json:"name"`
	AccountNumber *string `json:"account_number"`
	Holder        *string `json:"holder"`
}

// FullPII: respons W6. Field yang tidak pernah diisi → null.
type FullPII struct {
	NIK  *string  `json:"nik"`
	NPWP *string  `json:"npwp"`
	Bank *PIIBank `json:"bank"`
}

// ViewPII (W6): NIK/NPWP/rekening utuh. Boleh: OWNER/HR_ADMIN (role dibaca ulang dari DB) dan diri
// sendiri (role apa pun yang tertaut ke employee itu, file 17 👤). PAYROLL_ADMIN dan MANAGER atas
// bawahannya → 403; selain itu 404. Dekripsi dan audit employee.pii_viewed di transaksi yang sama: PII
// hanya dikembalikan kalau audit ikut COMMIT; audit gagal → 503 (T-PR-10).
func (s *Service) ViewPII(ctx context.Context, p auth.Principal, id uuid.UUID, meta reqmeta.Meta) (FullPII, error) {
	if s.members == nil {
		return FullPII{}, errors.New("employee: memberships not wired")
	}
	tid, _ := tenantctx.From(ctx)
	var out FullPII
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		roles, err := s.members.ActiveRolesTx(ctx, tx, p.UserID)
		if err != nil {
			return err
		}
		cur := auth.Principal{UserID: p.UserID, TenantID: p.TenantID, Roles: roles}
		if !cur.HasAnyRole(auth.RoleOwner, auth.RoleHRAdmin) {
			self, err := q.GetEmployeeIDByUser(ctx, &p.UserID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if err != nil || self != id {
				if cur.HasRole(auth.RolePayroll) {
					if _, err := q.GetEmployee(ctx, id); err != nil {
						return err // tidak ada / tenant lain → 404
					}
					return apperr.ErrForbidden.WithDetail("PAYROLL_ADMIN cannot view full PII")
				}
				ok, err := visible(ctx, q, cur, id)
				if err != nil {
					return err
				}
				if ok { // bawahan langsung MANAGER: terlihat, tetapi PII penuh bukan haknya
					return apperr.ErrForbidden.WithDetail("Managers cannot view full PII")
				}
				return apperr.ErrNotFound
			}
		}
		r, err := q.GetEmployeePII(ctx, id)
		if err != nil {
			return err
		}
		if err := db.GuardTenant(ctx, "employees", r.TenantID); err != nil {
			return err
		}
		var fields []string
		dec := func(field string, enc []byte) (*string, error) {
			if enc == nil {
				return nil, nil
			}
			v, err := s.pii.open(tid, field, enc)
			if err != nil {
				return nil, err // AAD tidak cocok (ciphertext dipindah) / kunci hilang: jangan kirim apa pun
			}
			fields = append(fields, field)
			return &v, nil
		}
		if out.NIK, err = dec("nik", r.NikEnc); err != nil {
			return err
		}
		if out.NPWP, err = dec("npwp", r.NpwpEnc); err != nil {
			return err
		}
		acc, err := dec("bank_account", r.BankAccountEnc)
		if err != nil {
			return err
		}
		if r.BankName != nil || acc != nil || r.BankAccountHolder != nil {
			out.Bank = &PIIBank{Name: r.BankName, AccountNumber: acc, Holder: r.BankAccountHolder}
		}
		if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: p.UserID, Action: "employee.pii_viewed",
			TargetType: "employee", TargetID: id, Diff: map[string]any{"fields": fields}}, meta); err != nil {
			return apperr.ErrUnavailable.WithCause(err)
		}
		return nil
	})
	if err != nil {
		return FullPII{}, db.MapError(err)
	}
	return out, nil
}
