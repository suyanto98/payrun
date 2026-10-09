// Package employee: data karyawan (PF-06) dengan PII terenkripsi (PF-20, ADR-P08).
//
// Scope baca (file 17 §1.2, §2.2):
//   - OWNER, HR_ADMIN, PAYROLL_ADMIN: seluruh tenant (PII selalu ter-mask; full view hanya W6, pii.go);
//   - MANAGER: bawahan langsung hari ini. org_assignments baru ada di S3, jadi di S2 himpunan
//     bawahan selalu kosong (list kosong, detail 404);
//   - EMPLOYEE: dirinya sendiri (employees.user_id = principal), versi dasar tanpa PII/PTKP.
//
// Ubah (W4, W5) hanya OWNER/HR_ADMIN. Role lain: 403 kalau resource terlihat olehnya (self,
// bawahan, atau PAYROLL_ADMIN), 404 kalau tidak (tidak membocorkan keberadaan; file 17 §1.3).
package employee

import (
	"context"
	"errors"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"payrun/internal/modules/employee/store"
	"payrun/internal/platform/apperr"
	"payrun/internal/platform/audit"
	"payrun/internal/platform/auth"
	"payrun/internal/platform/clock"
	"payrun/internal/platform/db"
	"payrun/internal/platform/httpx"
	"payrun/internal/platform/idgen"
	"payrun/internal/platform/periodguard"
	"payrun/internal/platform/reqmeta"
	"payrun/internal/platform/tenantctx"
)

const dateLayout = "2006-01-02"

// Basic adalah versi yang diterima MANAGER/EMPLOYEE (W3ᴮ): tanpa bank, NIK, NPWP, dan PTKP.
type Basic struct {
	ID         uuid.UUID   `json:"id"`
	EmployeeNo string      `json:"employee_no"`
	FullName   string      `json:"full_name"`
	WorkEmail  *string     `json:"work_email"`
	Status     string      `json:"status"`
	HireDate   string      `json:"hire_date"`
	EndDate    *string     `json:"end_date"`
	CurrentOrg *CurrentOrg `json:"current_org"` // penempatan yang berlaku hari ini (null = belum ada)
	UserID     *uuid.UUID  `json:"user_id"`
	Version    int32       `json:"version"`
}

// Ref: rujukan ringkas ke departemen/posisi.
type Ref struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// ManagerRef: atasan saat ini (amandemen S14: tidak effective-dated).
type ManagerRef struct {
	ID       uuid.UUID `json:"id"`
	FullName string    `json:"full_name"`
}

// CurrentOrg: department/position/valid_from null bila hanya atasan yang diset (belum ada penempatan).
type CurrentOrg struct {
	Department *Ref        `json:"department"`
	Position   *Ref        `json:"position"`
	Manager    *ManagerRef `json:"manager"`
	ValidFrom  *string     `json:"valid_from"`
}

// Employee adalah model lengkap (16-api §16) dengan PII ter-mask.
type Employee struct {
	Basic
	PtkpStatus string  `json:"ptkp_status"`
	NIKMasked  *string `json:"nik_masked"`
	NPWPMasked *string `json:"npwp_masked"`
	Bank       *Bank   `json:"bank"`
}

type Bank struct {
	Name          *string `json:"name"`
	AccountMasked *string `json:"account_masked"`
	Holder        *string `json:"holder"`
}

// ListItem: daftar tidak memuat PII apa pun, termasuk yang ter-mask (W1).
type ListItem struct {
	ID         uuid.UUID `json:"id"`
	EmployeeNo string    `json:"employee_no"`
	FullName   string    `json:"full_name"`
	Status     string    `json:"status"`
	Department *Ref      `json:"department"`
	Position   *Ref      `json:"position"`
	HireDate   string    `json:"hire_date"`
}

// OrgUnits: departemen/posisi milik modul org (ADR-001), dipasang composition root.
type OrgUnits interface {
	// CheckAssignableTx: keduanya ada di tenant (404) dan ACTIVE (400 field INACTIVE).
	CheckAssignableTx(ctx context.Context, tx pgx.Tx, departmentID, positionID uuid.UUID) error
}

// Salaries: gaji awal saat employee dibuat (tabel milik modul compensation).
type Salaries interface {
	CreateInitialSalaryTx(ctx context.Context, tx pgx.Tx, actor, employeeID uuid.UUID, baseSalary string, from time.Time, meta reqmeta.Meta) error
}

type Service struct {
	pool     *pgxpool.Pool
	pii      PII
	clock    clock.Clock
	ids      idgen.Generator
	orgUnits OrgUnits
	salaries Salaries
	members  Memberships
	guard    periodguard.Guard
}

// SetPeriodGuard memasang PeriodGuard (modul payroll): penulis employment dan PTKP (S5).
func (s *Service) SetPeriodGuard(g periodguard.Guard) { s.guard = g }

func (s *Service) lockInput(ctx context.Context, tx pgx.Tx, c periodguard.Change) (periodguard.Recorder, error) {
	if s.guard == nil {
		return nil, errors.New("employee: period guard not wired")
	}
	return s.guard.Lock(ctx, tx, c)
}

func NewService(pool *pgxpool.Pool, pii PII, c clock.Clock, ids idgen.Generator) *Service {
	return &Service{pool: pool, pii: pii, clock: c, ids: ids}
}

// Wire memasang dependensi lintas modul (composition root).
func (s *Service) Wire(orgUnits OrgUnits, salaries Salaries) {
	s.orgUnits, s.salaries = orgUnits, salaries
}

// today: tanggal "hari ini" untuk penempatan yang berlaku (UTC; zona tenant masuk bersama kalender S4).
func (s *Service) today() time.Time {
	n := s.clock.Now().UTC()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

// tenantWide: role yang melihat seluruh employee tenant.
func tenantWide(p auth.Principal) bool {
	return p.HasAnyRole(auth.RoleOwner, auth.RoleHRAdmin, auth.RolePayroll)
}

func canWrite(p auth.Principal) bool { return p.HasAnyRole(auth.RoleOwner, auth.RoleHRAdmin) }

// visible: apakah employee id terlihat oleh p (role non-tenant-wide): dirinya sendiri, atau — untuk
// MANAGER — bawahan langsung saat ini (employees.manager_employee_id; amandemen S14: atasan tidak
// effective-dated).
func visible(ctx context.Context, q *store.Queries, p auth.Principal, id uuid.UUID) (bool, error) {
	self, err := q.GetEmployeeIDByUser(ctx, &p.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if self == id {
		return true, nil
	}
	if !p.HasRole(auth.RoleManager) {
		return false, nil
	}
	r, err := q.GetEmployeeAccess(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return r.ManagerEmployeeID != nil && *r.ManagerEmployeeID == self, nil
}

// AccessMode untuk AccessTx.
type AccessMode int

const (
	// AccessSalaryRead (W8): OWNER/HR_ADMIN/PAYROLL_ADMIN; EMPLOYEE hanya dirinya; MANAGER → 404
	// (manager tidak melihat gaji bawahan, file 17 §3.4).
	AccessSalaryRead AccessMode = iota
	// AccessWrite (W7, W9, W10): OWNER/HR_ADMIN; role lain 403 bila employee terlihat, 404 bila tidak.
	AccessWrite
)

// AccessTx dipakai modul lain (compensation) lewat interface di composition root (ADR-001): otorisasi
// akses ke data turunan employee + tanggal mulai kerja (validasi valid_from ≥ hire_date).
func (s *Service) AccessTx(ctx context.Context, tx pgx.Tx, p auth.Principal, id uuid.UUID, mode AccessMode) (time.Time, error) {
	q := store.New(tx)
	switch mode {
	case AccessWrite:
		if err := authorizeWrite(ctx, q, p, id); err != nil {
			return time.Time{}, db.MapError(err)
		}
	case AccessSalaryRead:
		if !tenantWide(p) {
			self, err := q.GetEmployeeIDByUser(ctx, &p.UserID)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && self != id) {
				return time.Time{}, apperr.ErrNotFound
			}
			if err != nil {
				return time.Time{}, err
			}
		}
	}
	r, err := q.GetEmployeeAccess(ctx, id)
	if err != nil {
		return time.Time{}, db.MapError(err)
	}
	if err := db.GuardTenant(ctx, "employees", r.TenantID); err != nil {
		return time.Time{}, err
	}
	return r.HireDate, nil
}

// ---------------------------------------------------------------- read

func (s *Service) Get(ctx context.Context, p auth.Principal, id uuid.UUID) (any, int32, error) {
	var out any
	var version int32
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if !tenantWide(p) {
			ok, err := visible(ctx, q, p, id)
			if err != nil {
				return err
			}
			if !ok {
				return apperr.ErrNotFound
			}
		}
		e, err := s.load(ctx, q, id)
		if err != nil {
			return err
		}
		version = e.Version
		if tenantWide(p) {
			out = e
		} else {
			out = e.Basic
		}
		return nil
	})
	return out, version, db.MapError(err)
}

func (s *Service) load(ctx context.Context, q *store.Queries, id uuid.UUID) (Employee, error) {
	r, err := q.GetEmployee(ctx, id)
	if err != nil {
		return Employee{}, err
	}
	if err := db.GuardTenant(ctx, "employees", r.TenantID); err != nil {
		return Employee{}, err
	}
	e := Employee{
		Basic: Basic{ID: r.ID, EmployeeNo: r.EmployeeNo, FullName: r.FullName, WorkEmail: r.WorkEmail, Status: r.Status,
			HireDate: r.HireDate.Format(dateLayout), UserID: r.UserID, Version: r.Version},
		PtkpStatus: r.PtkpStatus, NIKMasked: maskNIK(r.NikLast4), NPWPMasked: maskNPWP(r.NpwpLast4),
	}
	if !r.OpenEnded {
		d := r.EndDate.Format(dateLayout)
		e.EndDate = &d
	}
	if r.BankName != nil || r.BankAccountLast4 != nil || r.BankAccountHolder != nil {
		e.Bank = &Bank{Name: r.BankName, AccountMasked: maskAccount(r.BankAccountLast4), Holder: r.BankAccountHolder}
	}
	org, err := q.GetCurrentOrg(ctx, store.GetCurrentOrgParams{EmployeeID: id, AsOf: s.today()})
	switch {
	case err == nil:
		vf := org.ValidFrom.Format(dateLayout)
		e.CurrentOrg = &CurrentOrg{Department: &Ref{ID: org.DepartmentID, Name: org.DepartmentName},
			Position: &Ref{ID: org.PositionID, Name: org.PositionName}, ValidFrom: &vf}
	case !errors.Is(err, pgx.ErrNoRows):
		return Employee{}, err
	}
	m, err := q.GetManagerRef(ctx, id)
	switch {
	case err == nil:
		if e.CurrentOrg == nil {
			e.CurrentOrg = &CurrentOrg{}
		}
		e.CurrentOrg.Manager = &ManagerRef{ID: m.ID, FullName: m.FullName}
	case !errors.Is(err, pgx.ErrNoRows):
		return Employee{}, err
	}
	return e, nil
}

// ListQuery: filter W1.
type ListQuery struct {
	Status       string
	Q            string
	Cursor       string
	DepartmentID string
	Limit        int32
}

func refOrNil(id uuid.UUID, name string) *Ref {
	if id == uuid.Nil {
		return nil
	}
	return &Ref{ID: id, Name: name}
}

func (s *Service) List(ctx context.Context, p auth.Principal, lq ListQuery) ([]ListItem, httpx.Page, error) {
	items := []ListItem{}
	page := httpx.Page{}
	if !tenantWide(p) && !p.HasRole(auth.RoleManager) {
		return nil, page, apperr.ErrForbidden
	}
	var fe []apperr.FieldError
	if lq.Status != "" && lq.Status != "ACTIVE" && lq.Status != "RESIGNED" && lq.Status != "TERMINATED" {
		fe = append(fe, apperr.FieldError{Field: "status", Code: "INVALID_ENUM"})
	}
	q := strings.ToLower(strings.TrimSpace(lq.Q))
	if lq.Q != "" && (utf8.RuneCountInString(q) < 2 || utf8.RuneCountInString(q) > 50) {
		fe = append(fe, apperr.FieldError{Field: "q", Code: "LENGTH", Message: "must be 2..50 characters"})
	}
	if len(fe) > 0 {
		return nil, page, apperr.Validation(fe...)
	}
	var after *httpx.KeyCursor
	if lq.Cursor != "" {
		c, err := httpx.DecodeKeyCursor(lq.Cursor)
		if err != nil {
			return nil, page, err
		}
		after = &c
	}
	var deptID *uuid.UUID
	if lq.DepartmentID != "" {
		id, err := uuid.Parse(lq.DepartmentID)
		if err != nil {
			return nil, page, apperr.Validation(apperr.FieldError{Field: "department_id", Code: "FORMAT", Message: "must be a UUID"})
		}
		deptID = &id
	}
	var managerID *uuid.UUID
	if !tenantWide(p) {
		// MANAGER: hanya bawahan langsung saat ini (employees.manager_employee_id = employee milik user).
		self, err := s.selfID(ctx, p.UserID)
		if err != nil || self == nil {
			return items, page, err
		}
		managerID = self
	}
	var status, afterKey *string
	var afterID *uuid.UUID
	if lq.Status != "" {
		status = &lq.Status
	}
	if after != nil {
		afterKey, afterID = &after.Key, &after.ID
	}
	lim := lq.Limit + 1 // satu ekstra untuk has_more
	// sort_key dari SQL (lower() Postgres): kunci cursor harus PERSIS kunci urut di DB. strings.ToLower
	// Go bisa berbeda untuk nama non-ASCII → baris terlewat/berulang (review S2).
	var sortKeys []string
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		st := store.New(tx)
		if q == "" {
			rs, err := st.ListEmployeesByNo(ctx, store.ListEmployeesByNoParams{AsOf: s.today(), Status: status, ManagerID: managerID,
				DepartmentID: deptID, AfterKey: afterKey, AfterID: afterID, Lim: lim})
			if err != nil {
				return err
			}
			for _, r := range rs {
				if err := db.GuardTenant(ctx, "employees", r.TenantID); err != nil {
					return err
				}
				items = append(items, ListItem{ID: r.ID, EmployeeNo: r.EmployeeNo, FullName: r.FullName, Status: r.Status, HireDate: r.HireDate.Format(dateLayout),
					Department: refOrNil(r.DepartmentID, r.DepartmentName), Position: refOrNil(r.PositionID, r.PositionName)})
			}
			return nil
		}
		rs, err := st.ListEmployeesByName(ctx, store.ListEmployeesByNameParams{AsOf: s.today(), Prefix: likeEscape(q), Status: status,
			ManagerID: managerID, DepartmentID: deptID, AfterKey: afterKey, AfterID: afterID, Lim: lim})
		if err != nil {
			return err
		}
		for _, r := range rs {
			if err := db.GuardTenant(ctx, "employees", r.TenantID); err != nil {
				return err
			}
			items = append(items, ListItem{ID: r.ID, EmployeeNo: r.EmployeeNo, FullName: r.FullName, Status: r.Status, HireDate: r.HireDate.Format(dateLayout),
				Department: refOrNil(r.DepartmentID, r.DepartmentName), Position: refOrNil(r.PositionID, r.PositionName)})
			sortKeys = append(sortKeys, r.SortKey)
		}
		return nil
	})
	if err != nil {
		return nil, page, db.MapError(err)
	}
	if int32(len(items)) > lq.Limit { //nolint:gosec // G115: panjang ≤ limit+1
		items = items[:lq.Limit]
		// cursor = baris terakhir yang DIKIRIM (bukan baris ekstra)
		lastSent := items[len(items)-1]
		key := lastSent.EmployeeNo
		if q != "" {
			key = sortKeys[len(items)-1]
		}
		c := httpx.EncodeKeyCursor(httpx.KeyCursor{Key: key, ID: lastSent.ID})
		page = httpx.Page{NextCursor: &c, HasMore: true}
	}
	return items, page, nil
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// ---------------------------------------------------------------- create (W2)

type BankInput struct {
	Name          *string `json:"name"`
	AccountNumber *string `json:"account_number"`
	Holder        *string `json:"holder"`
}

// OrgInput: penempatan awal (W2) / baru (W7). manager_employee_id null = tanpa atasan.
type OrgInput struct {
	DepartmentID      uuid.UUID  `json:"department_id"`
	PositionID        uuid.UUID  `json:"position_id"`
	ManagerEmployeeID *uuid.UUID `json:"manager_employee_id"`
}

type SalaryInput struct {
	BaseSalary string `json:"base_salary"`
}

type CreateInput struct {
	Org        *OrgInput    `json:"org"`
	Salary     *SalaryInput `json:"salary"`
	EmployeeNo string       `json:"employee_no"`
	FullName   string       `json:"full_name"`
	WorkEmail  *string      `json:"work_email"`
	HireDate   string       `json:"hire_date"`
	PtkpStatus string       `json:"ptkp_status"`
	NIK        *string      `json:"nik"`
	NPWP       *string      `json:"npwp"`
	Bank       *BankInput   `json:"bank"`
}

var (
	employeeNoPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,30}$`)
	ptkpValues        = map[string]bool{"TK/0": true, "TK/1": true, "TK/2": true, "TK/3": true, "K/0": true, "K/1": true, "K/2": true, "K/3": true}
)

// piiFields adalah hasil normalisasi + validasi field PII (nil = tidak dikirim).
type piiFields struct {
	nik, npwp, account          *string
	bankName, bankHolder        *string
	sealedNIK, sealedNPWP, sAcc *sealed
}

// validFullName: 1..120 karakter, tanpa karakter kontrol (NUL ditolak Postgres → 500; review S2).
func validFullName(v string) bool {
	n := utf8.RuneCountInString(v)
	return n >= 1 && n <= 120 && !hasControl(v)
}

func hasControl(v string) bool {
	return strings.IndexFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0
}

func validEmail(v string) bool {
	a, err := mail.ParseAddress(v)
	return err == nil && a.Address == v && len(v) <= 254
}

func collectPII(nik, npwp *string, bank *BankInput, fe *[]apperr.FieldError) piiFields {
	var f piiFields
	if nik != nil {
		v := digitsOnly(*nik)
		if !allDigits(v, 16, 16) {
			*fe = append(*fe, apperr.FieldError{Field: "nik", Code: "FORMAT", Message: "must be 16 digits"})
		}
		f.nik = &v
	}
	if npwp != nil {
		v := digitsOnly(*npwp)
		if !allDigits(v, 15, 16) {
			*fe = append(*fe, apperr.FieldError{Field: "npwp", Code: "FORMAT", Message: "must be 15 or 16 digits"})
		}
		f.npwp = &v
	}
	if bank != nil {
		if bank.AccountNumber != nil {
			v := digitsOnly(*bank.AccountNumber)
			if !allDigits(v, 6, 20) {
				*fe = append(*fe, apperr.FieldError{Field: "bank.account_number", Code: "FORMAT", Message: "must be 6..20 digits"})
			}
			f.account = &v
		}
		if bank.Name != nil {
			v := strings.TrimSpace(*bank.Name)
			if n := utf8.RuneCountInString(v); n < 1 || n > 50 || hasControl(v) {
				*fe = append(*fe, apperr.FieldError{Field: "bank.name", Code: "LENGTH", Message: "must be 1..50 characters"})
			}
			f.bankName = &v
		}
		if bank.Holder != nil {
			v := strings.TrimSpace(*bank.Holder)
			if !validFullName(v) {
				*fe = append(*fe, apperr.FieldError{Field: "bank.holder", Code: "LENGTH", Message: "must be 1..120 characters"})
			}
			f.bankHolder = &v
		}
	}
	return f
}

// seal mengenkripsi field yang dikirim (setelah validasi lolos).
func (s *Service) seal(tid uuid.UUID, f *piiFields) error {
	var err error
	if f.nik != nil {
		v, e := s.pii.seal(tid, "nik", *f.nik, true)
		f.sealedNIK, err = &v, e
		if err != nil {
			return err
		}
	}
	if f.npwp != nil {
		v, e := s.pii.seal(tid, "npwp", *f.npwp, true)
		f.sealedNPWP, err = &v, e
		if err != nil {
			return err
		}
	}
	if f.account != nil {
		v, e := s.pii.seal(tid, "bank_account", *f.account, false)
		f.sAcc, err = &v, e
	}
	return err
}

func (f piiFields) names() []string {
	var n []string
	for name, set := range map[string]bool{"nik": f.nik != nil, "npwp": f.npwp != nil, "bank.account_number": f.account != nil,
		"bank.name": f.bankName != nil, "bank.holder": f.bankHolder != nil} {
		if set {
			n = append(n, name)
		}
	}
	return n
}

// CreateTx membuat employee + span [hire_date, ∞) di transaksi idempotency (SAVEPOINT).
// org dan salary (16-api W2) menyusul di S3 bersama tabelnya.
func (s *Service) CreateTx(ctx context.Context, tx pgx.Tx, actor uuid.UUID, in CreateInput, meta reqmeta.Meta) (Employee, error) {
	tid, ok := tenantctx.From(ctx)
	if !ok {
		return Employee{}, db.ErrNoTenantContext
	}
	in.EmployeeNo, in.FullName = strings.TrimSpace(in.EmployeeNo), strings.TrimSpace(in.FullName)
	var fe []apperr.FieldError
	if !employeeNoPattern.MatchString(in.EmployeeNo) {
		fe = append(fe, apperr.FieldError{Field: "employee_no", Code: "FORMAT", Message: "must match ^[A-Za-z0-9-]{1,30}$"})
	}
	if !validFullName(in.FullName) {
		fe = append(fe, apperr.FieldError{Field: "full_name", Code: "LENGTH", Message: "must be 1..120 characters"})
	}
	if in.WorkEmail != nil && !validEmail(strings.TrimSpace(*in.WorkEmail)) {
		fe = append(fe, apperr.FieldError{Field: "work_email", Code: "FORMAT"})
	}
	if in.PtkpStatus == "" {
		in.PtkpStatus = "TK/0"
	}
	if !ptkpValues[in.PtkpStatus] {
		fe = append(fe, apperr.FieldError{Field: "ptkp_status", Code: "INVALID_ENUM"})
	}
	hire, err := time.Parse(dateLayout, in.HireDate)
	if err != nil {
		fe = append(fe, apperr.FieldError{Field: "hire_date", Code: "FORMAT", Message: "must be YYYY-MM-DD"})
	} else if hire.After(s.clock.Now().AddDate(0, 0, 90)) {
		fe = append(fe, apperr.FieldError{Field: "hire_date", Code: "OUT_OF_RANGE", Message: "must be at most 90 days ahead"})
	}
	f := collectPII(in.NIK, in.NPWP, in.Bank, &fe)
	if in.Bank != nil && in.Bank.AccountNumber == nil {
		fe = append(fe, apperr.FieldError{Field: "bank.account_number", Code: "REQUIRED"})
	}
	if len(fe) > 0 {
		return Employee{}, apperr.Validation(fe...)
	}
	if err := s.seal(tid, &f); err != nil {
		return Employee{}, err
	}
	var workEmail *string
	if in.WorkEmail != nil {
		v := strings.TrimSpace(*in.WorkEmail)
		workEmail = &v
	}
	id := s.ids.New()
	params := store.CreateEmployeeParams{TenantID: tid, ID: id, EmployeeNo: in.EmployeeNo, FullName: in.FullName,
		WorkEmail: workEmail, PtkpStatus: in.PtkpStatus, BankName: f.bankName, BankAccountHolder: f.bankHolder, Actor: &actor}
	applySealed(&params.NikEnc, &params.NikBidx, &params.NikLast4, f.sealedNIK)
	applySealed(&params.NpwpEnc, &params.NpwpBidx, &params.NpwpLast4, f.sealedNPWP)
	if f.sAcc != nil {
		params.BankAccountEnc, params.BankAccountLast4 = f.sAcc.enc, &f.sAcc.last4
	}
	if f.sealedNIK != nil || f.sealedNPWP != nil || f.sAcc != nil {
		params.PiiKeyVersion = ptr(s.pii.Keyring.ActiveVersion())
	}
	// PeriodGuard [hire_date, ∞): hire di periode final → 409 PERIOD_LOCKED; karyawan baru di periode
	// non-final dengan run terhitung → stale (D6). Gaji awal ikut tercakup.
	record, err := s.lockInput(ctx, tx, periodguard.Change{Employees: []uuid.UUID{id}, Range: periodguard.From(hire)})
	if err != nil {
		return Employee{}, err
	}
	q := store.New(tx)
	if err := q.CreateEmployee(ctx, params); err != nil {
		return Employee{}, db.MapError(err)
	}
	if err := q.CreateSpan(ctx, store.CreateSpanParams{TenantID: tid, ID: s.ids.New(), EmployeeID: id, HireDate: hire, Actor: &actor}); err != nil {
		return Employee{}, db.MapError(err)
	}
	// W2: penempatan dan gaji awal berlaku mulai hire_date, di transaksi yang sama (16-api W2).
	if in.Org != nil {
		if err := s.placeTx(ctx, tx, tid, actor, id, *in.Org, hire); err != nil {
			return Employee{}, err
		}
	}
	if in.Salary != nil {
		if err := s.salaries.CreateInitialSalaryTx(ctx, tx, actor, id, in.Salary.BaseSalary, hire, meta); err != nil {
			return Employee{}, err
		}
	}
	fields := append([]string{"employee_no", "full_name", "hire_date", "ptkp_status"}, f.names()...)
	if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: actor, Action: "employee.created",
		TargetType: "employee", TargetID: id, Diff: map[string]any{"fields": fields}}, meta); err != nil {
		return Employee{}, err
	}
	if err := record(ctx); err != nil {
		return Employee{}, err
	}
	e, err := s.load(ctx, q, id)
	return e, db.MapError(err)
}

func applySealed(enc, bidx *[]byte, last4 **string, s *sealed) {
	if s == nil {
		return
	}
	*enc, *bidx, *last4 = s.enc, s.bidx, &s.last4
}

func ptr[T any](v T) *T { return &v }

// ---------------------------------------------------------------- update (W4)

type UpdateInput struct {
	FullName   *string    `json:"full_name"`
	WorkEmail  *string    `json:"work_email"`
	PtkpStatus *string    `json:"ptkp_status"`
	NIK        *string    `json:"nik"`
	NPWP       *string    `json:"npwp"`
	Bank       *BankInput `json:"bank"`
}

// Precondition adalah hasil If-Match (Err = 428/412 yang ditunda sampai otorisasi lolos).
type Precondition struct {
	Version int32
	Err     error
}

// authorizeWrite: OWNER/HR_ADMIN lolos; role lain 403 kalau employee terlihat olehnya, 404 kalau tidak.
func authorizeWrite(ctx context.Context, q *store.Queries, p auth.Principal, id uuid.UUID) error {
	if canWrite(p) {
		return nil
	}
	if tenantWide(p) { // PAYROLL_ADMIN melihat employee tetapi tidak boleh mengubah
		if _, err := q.GetEmployee(ctx, id); err != nil {
			return err
		}
		return apperr.ErrForbidden
	}
	ok, err := visible(ctx, q, p, id)
	if err != nil {
		return err
	}
	if ok {
		return apperr.ErrForbidden
	}
	return apperr.ErrNotFound
}

func (s *Service) Update(ctx context.Context, p auth.Principal, id uuid.UUID, pre Precondition, in UpdateInput, meta reqmeta.Meta) (Employee, error) {
	tid, _ := tenantctx.From(ctx)
	var e Employee
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := authorizeWrite(ctx, q, p, id); err != nil {
			return err
		}
		if pre.Err != nil {
			return pre.Err
		}
		var fe []apperr.FieldError
		params := store.UpdateEmployeeParams{ID: id, Version: pre.Version, Actor: &p.UserID}
		var changed []string
		if in.FullName != nil {
			v := strings.TrimSpace(*in.FullName)
			if !validFullName(v) {
				fe = append(fe, apperr.FieldError{Field: "full_name", Code: "LENGTH", Message: "must be 1..120 characters"})
			}
			params.FullName, changed = &v, append(changed, "full_name")
		}
		if in.WorkEmail != nil {
			v := strings.TrimSpace(*in.WorkEmail)
			if !validEmail(v) {
				fe = append(fe, apperr.FieldError{Field: "work_email", Code: "FORMAT"})
			}
			params.WorkEmail, changed = &v, append(changed, "work_email")
		}
		if in.PtkpStatus != nil {
			if !ptkpValues[*in.PtkpStatus] {
				fe = append(fe, apperr.FieldError{Field: "ptkp_status", Code: "INVALID_ENUM"})
			}
			params.PtkpStatus, changed = in.PtkpStatus, append(changed, "ptkp_status")
		}
		f := collectPII(in.NIK, in.NPWP, in.Bank, &fe)
		if len(fe) > 0 {
			return apperr.Validation(fe...)
		}
		if err := s.seal(tid, &f); err != nil {
			return err
		}
		applySealed(&params.NikEnc, &params.NikBidx, &params.NikLast4, f.sealedNIK)
		applySealed(&params.NpwpEnc, &params.NpwpBidx, &params.NpwpLast4, f.sealedNPWP)
		if f.sAcc != nil {
			params.BankAccountEnc, params.BankAccountLast4 = f.sAcc.enc, &f.sAcc.last4
		}
		params.BankName, params.BankAccountHolder = f.bankName, f.bankHolder
		if f.sealedNIK != nil || f.sealedNPWP != nil || f.sAcc != nil {
			params.PiiKeyVersion = ptr(s.pii.Keyring.ActiveVersion())
		}
		changed = append(changed, f.names()...)
		if len(changed) == 0 {
			// PATCH {} tidak menaikkan versi dan tidak menulis audit kosong (review S2).
			return apperr.Validation(apperr.FieldError{Field: "body", Code: "REQUIRED", Message: "at least one field"})
		}
		// Hanya PTKP yang masuk input payroll (kategori pajak). Tidak effective-dated: periode final tidak
		// terblokir (hasilnya tetap dari snapshot), periode non-final dicatat (PFS-03).
		record := periodguard.Recorder(func(context.Context) error { return nil })
		if in.PtkpStatus != nil {
			var err error
			if record, err = s.lockInput(ctx, tx, periodguard.Change{Employees: []uuid.UUID{id}, NotEffectiveDated: true}); err != nil {
				return err
			}
		}
		if _, err := q.UpdateEmployee(ctx, params); errors.Is(err, pgx.ErrNoRows) {
			// 0 baris: tidak ada / tenant lain (404) ATAU versi basi (412). Wajib dibedakan lewat lookup:
			// menjawab 412 untuk id tenant lain membocorkan keberadaannya (ditangkap TestCrossTenant).
			if _, gerr := q.GetEmployee(ctx, id); gerr != nil {
				return gerr
			}
			return apperr.ErrVersionConflict
		} else if err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: p.UserID, Action: "employee.updated",
			TargetType: "employee", TargetID: id, Diff: map[string]any{"fields": changed}}, meta); err != nil {
			return err
		}
		if err := record(ctx); err != nil {
			return err
		}
		var err error
		e, err = s.load(ctx, q, id)
		return err
	})
	return e, db.MapError(err)
}

// ---------------------------------------------------------------- terminate (W5)

type TerminateInput struct {
	Type    string `json:"type"`
	EndDate string `json:"end_date"`
	Reason  string `json:"reason"`
}

func (s *Service) Terminate(ctx context.Context, p auth.Principal, id uuid.UUID, pre Precondition, in TerminateInput, meta reqmeta.Meta) (Employee, error) {
	tid, _ := tenantctx.From(ctx)
	var fe []apperr.FieldError
	if in.Type != "RESIGNED" && in.Type != "TERMINATED" {
		fe = append(fe, apperr.FieldError{Field: "type", Code: "INVALID_ENUM", Message: "must be RESIGNED or TERMINATED"})
	}
	end, err := time.Parse(dateLayout, in.EndDate)
	if err != nil {
		fe = append(fe, apperr.FieldError{Field: "end_date", Code: "FORMAT", Message: "must be YYYY-MM-DD"})
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if utf8.RuneCountInString(in.Reason) > 500 || strings.ContainsRune(in.Reason, 0) {
		fe = append(fe, apperr.FieldError{Field: "reason", Code: "LENGTH", Message: "must be at most 500 characters"})
	}
	if len(fe) > 0 {
		return Employee{}, apperr.Validation(fe...)
	}
	var e Employee
	err = db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := authorizeWrite(ctx, q, p, id); err != nil {
			return err
		}
		if pre.Err != nil {
			return pre.Err
		}
		version := pre.Version
		cur, err := s.load(ctx, q, id)
		if err != nil {
			return err
		}
		switch {
		case cur.Status != "ACTIVE":
			return apperr.ErrInvalidState.WithDetail("Employee is already " + cur.Status)
		case cur.Version != version:
			return apperr.ErrVersionConflict
		case in.EndDate < cur.HireDate: // YYYY-MM-DD dibandingkan leksikal = kronologis
			return apperr.Validation(apperr.FieldError{Field: "end_date", Code: "OUT_OF_RANGE", Message: "must not be before hire_date"})
		}
		// Tanggal yang berubah: [end_date + 1, ∞) (span ditutup). Di periode final → PERIOD_LOCKED.
		record, err := s.lockInput(ctx, tx, periodguard.Change{Employees: []uuid.UUID{id}, Range: periodguard.From(end.AddDate(0, 0, 1))})
		if err != nil {
			return err
		}
		if _, err := q.TerminateEmployee(ctx, store.TerminateEmployeeParams{Status: in.Type, Actor: &p.UserID, ID: id, Version: version}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return apperr.ErrVersionConflict // diubah paralel di antara load dan update
			}
			return err
		}
		// end_date = hari kerja terakhir (inklusif) → batas atas span eksklusif end_date + 1.
		n, err := q.CloseOpenSpan(ctx, store.CloseOpenSpanParams{EndExclusive: end.AddDate(0, 0, 1), EndReason: &in.Type, Actor: &p.UserID, EmployeeID: id})
		if err != nil {
			return err
		}
		if n != 1 {
			return apperr.ErrInvalidState.WithDetail("Employee has no open employment span")
		}
		if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: p.UserID, Action: "employee.terminated",
			TargetType: "employee", TargetID: id, Diff: map[string]any{"type": in.Type, "end_date": in.EndDate}, Reason: in.Reason}, meta); err != nil {
			return err
		}
		if err := record(ctx); err != nil {
			return err
		}
		e, err = s.load(ctx, q, id)
		return err
	})
	return e, db.MapError(err)
}

// ---------------------------------------------------------------- tautan user (undangan, PF-02)

// CheckLinkableTx: employee ada di tenant transaksi dan belum tertaut ke user (U2). Dipanggil modul
// iam lewat interface di composition root (ADR-001).
func (s *Service) CheckLinkableTx(ctx context.Context, tx pgx.Tx, employeeID uuid.UUID) error {
	r, err := store.New(tx).GetEmployeeLinkForUpdate(ctx, employeeID)
	if err != nil {
		return db.MapError(err)
	}
	if err := db.GuardTenant(ctx, "employees", r.TenantID); err != nil && !errors.Is(err, db.ErrNoTenantContext) {
		return err
	}
	if r.UserID != nil {
		return apperr.ErrInvalidState.WithDetail("Employee is already linked to a user")
	}
	return nil
}

// LinkUserTx menautkan user ke employee saat undangan diterima (U3). Sudah tertaut ke user lain
// (diundang dua kali) → 409 INVALID_STATE_TRANSITION.
func (s *Service) LinkUserTx(ctx context.Context, tx pgx.Tx, employeeID, userID uuid.UUID) error {
	n, err := store.New(tx).LinkEmployeeUser(ctx, store.LinkEmployeeUserParams{UserID: &userID, ID: employeeID})
	if err != nil {
		return db.MapError(err)
	}
	if n != 1 {
		return apperr.ErrInvalidState.WithDetail("Employee is already linked to a user")
	}
	return nil
}

// SelfID: employee milik user di tenant aktif (GET /me `employee_id`). Tanpa tenant aktif → nil.
func (s *Service) SelfID(ctx context.Context, userID uuid.UUID) (*uuid.UUID, error) {
	if _, ok := tenantctx.From(ctx); !ok {
		return nil, nil
	}
	var out *uuid.UUID
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		id, err := store.New(tx).GetEmployeeIDByUser(ctx, &userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		out = &id
		return err
	})
	return out, db.MapError(err)
}

func (s *Service) selfID(ctx context.Context, userID uuid.UUID) (*uuid.UUID, error) {
	var out *uuid.UUID
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		id, err := store.New(tx).GetEmployeeIDByUser(ctx, &userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		out = &id
		return err
	})
	return out, db.MapError(err)
}

// ---------------------------------------------------------------- W7 org assignment

type OrgAssignment struct {
	ID         uuid.UUID   `json:"id"`
	ValidFrom  string      `json:"valid_from"`
	ValidTo    *string     `json:"valid_to"`
	Department Ref         `json:"department"`
	Position   Ref         `json:"position"`
	Manager    *ManagerRef `json:"manager"`
}

type OrgAssignmentInput struct {
	ValidFrom string `json:"valid_from"`
	OrgInput
}

// placeTx: tutup penempatan yang berlaku di from, sisipkan [from, ∞), set atasan saat ini.
func (s *Service) placeTx(ctx context.Context, tx pgx.Tx, tid, actor, employeeID uuid.UUID, in OrgInput, from time.Time) error {
	if err := s.orgUnits.CheckAssignableTx(ctx, tx, in.DepartmentID, in.PositionID); err != nil {
		return err
	}
	q := store.New(tx)
	if in.ManagerEmployeeID != nil {
		if *in.ManagerEmployeeID == employeeID {
			return apperr.Validation(apperr.FieldError{Field: "manager_employee_id", Code: "SELF", Message: "an employee cannot manage themselves"})
		}
		m, err := q.GetManagerStatus(ctx, *in.ManagerEmployeeID)
		if err != nil {
			return db.MapError(err) // tidak ada / tenant lain → 404
		}
		if err := db.GuardTenant(ctx, "employees", m.TenantID); err != nil {
			return err
		}
		if m.Status != "ACTIVE" {
			return apperr.Validation(apperr.FieldError{Field: "manager_employee_id", Code: "INACTIVE", Message: "manager is not an active employee"})
		}
	}
	if _, err := q.CloseOrgAssignmentAt(ctx, store.CloseOrgAssignmentAtParams{ValidFrom: from, Actor: &actor, EmployeeID: employeeID}); err != nil {
		return db.MapError(err)
	}
	if _, err := q.InsertOrgAssignment(ctx, store.InsertOrgAssignmentParams{TenantID: tid, ID: s.ids.New(), EmployeeID: employeeID,
		DepartmentID: in.DepartmentID, PositionID: in.PositionID, ValidFrom: from, Actor: &actor}); err != nil {
		return db.MapError(err)
	}
	if _, err := q.SetManager(ctx, store.SetManagerParams{ManagerID: in.ManagerEmployeeID, Actor: &actor, ID: employeeID}); err != nil {
		return db.MapError(err) // siklus → P0005 → 422 MANAGER_CYCLE
	}
	return nil
}

// AssignOrgTx (W7) di transaksi idempotency: OWNER/HR_ADMIN; valid_from ≥ hire_date. Penempatan masa
// depan yang sudah ada → EXCLUDE → 409 OVERLAPPING_PERIOD. Atasan berlaku segera (S14).
func (s *Service) AssignOrgTx(ctx context.Context, tx pgx.Tx, p auth.Principal, employeeID uuid.UUID, in OrgAssignmentInput, meta reqmeta.Meta) (OrgAssignment, error) {
	tid, _ := tenantctx.From(ctx)
	hire, err := s.AccessTx(ctx, tx, p, employeeID, AccessWrite)
	if err != nil {
		return OrgAssignment{}, err
	}
	from, perr := time.Parse(dateLayout, in.ValidFrom)
	switch {
	case perr != nil:
		return OrgAssignment{}, apperr.Validation(apperr.FieldError{Field: "valid_from", Code: "FORMAT", Message: "must be YYYY-MM-DD"})
	case from.Before(hire):
		return OrgAssignment{}, apperr.Validation(apperr.FieldError{Field: "valid_from", Code: "OUT_OF_RANGE", Message: "must not be before hire_date " + hire.Format(dateLayout)})
	}
	if err := s.placeTx(ctx, tx, tid, p.UserID, employeeID, in.OrgInput, from); err != nil {
		return OrgAssignment{}, err
	}
	q := store.New(tx)
	org, err := q.GetCurrentOrg(ctx, store.GetCurrentOrgParams{EmployeeID: employeeID, AsOf: from})
	if err != nil {
		return OrgAssignment{}, db.MapError(err)
	}
	out := OrgAssignment{ID: org.ID, ValidFrom: in.ValidFrom, Department: Ref{ID: org.DepartmentID, Name: org.DepartmentName},
		Position: Ref{ID: org.PositionID, Name: org.PositionName}}
	if m, err := q.GetManagerRef(ctx, employeeID); err == nil {
		out.Manager = &ManagerRef{ID: m.ID, FullName: m.FullName}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return OrgAssignment{}, db.MapError(err)
	}
	diff := map[string]any{"valid_from": in.ValidFrom, "department_id": in.DepartmentID.String(), "position_id": in.PositionID.String()}
	if in.ManagerEmployeeID != nil {
		diff["manager_employee_id"] = in.ManagerEmployeeID.String()
	}
	if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: p.UserID, Action: "employee.org_assigned",
		TargetType: "employee", TargetID: employeeID, Diff: diff}, meta); err != nil {
		return OrgAssignment{}, err
	}
	return out, nil
}

// CountAssignedTodayTx: dipakai modul org (G4) lewat interface: berapa penempatan yang berlaku hari ini
// memakai departemen/posisi ini.
func (s *Service) CountAssignedTodayTx(ctx context.Context, tx pgx.Tx, departmentID, positionID *uuid.UUID) (int64, error) {
	n, err := store.New(tx).CountAssignedToday(ctx, store.CountAssignedTodayParams{AsOf: s.today(), DepartmentID: departmentID, PositionID: positionID})
	return n, db.MapError(err)
}
