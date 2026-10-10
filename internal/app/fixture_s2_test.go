package app_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Fixture Sprint 2 (file 17 §4.1, tanpa data payroll yang baru ada di S4+): per tenant satu user untuk
// setiap role, satu departemen, satu posisi, dan dua employee: empSelf tertaut ke user EMPLOYEE,
// empOther tidak tertaut ke siapa pun. MANAGER belum punya bawahan (org_assignments masuk S3).

const (
	roleO = "OWNER"
	roleH = "HR_ADMIN"
	roleP = "PAYROLL_ADMIN"
	roleM = "MANAGER"
	roleE = "EMPLOYEE"
)

var allRoleNames = []string{roleO, roleH, roleP, roleM, roleE}

type tenantFx struct {
	id         uuid.UUID
	slug       string
	users      map[string]uuid.UUID // role → user
	emails     map[string]string
	tokens     map[string]string // role → access token (tid = tenant ini)
	dept       uuid.UUID
	pos        uuid.UUID
	empSelf    uuid.UUID // tertaut ke user EMPLOYEE; bawahan langsung empManager (S3)
	empOther   uuid.UUID // bukan bawahan siapa pun
	empManager uuid.UUID // tertaut ke user MANAGER
	holiday    uuid.UUID
	comp       uuid.UUID // komponen tenant (FIXED) yang bisa ditugaskan
}

func (e env) verifyUser(t *testing.T, id uuid.UUID) {
	t.Helper()
	if _, err := e.tdb.Super(t).Exec(context.Background(), `UPDATE payrun.users SET email_verified_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

func (e env) newTenantFx(t *testing.T, slug string) *tenantFx {
	t.Helper()
	f := &tenantFx{slug: slug, users: map[string]uuid.UUID{}, emails: map[string]string{}, tokens: map[string]string{}}
	for _, role := range allRoleNames {
		email := role + "@" + slug + ".test"
		f.emails[role] = email
		f.users[role] = e.createUser(t, email)
		e.verifyUser(t, f.users[role])
	}
	f.id = e.createTenant(t, slug, f.users[roleO])
	for _, role := range allRoleNames[1:] {
		e.addMember(t, f.id, f.users[role], role)
	}
	for _, role := range allRoleNames {
		f.tokens[role], _, _ = e.login(t, f.emails[role])
	}
	f.dept = e.mustCreate(t, f.tokens[roleO], "/api/v1/departments", map[string]any{"code": "WH", "name": "Gudang"})
	f.pos = e.mustCreate(t, f.tokens[roleO], "/api/v1/positions", map[string]any{"code": "STAFF", "name": "Staf"})
	f.empSelf = e.createEmployee(t, f.tokens[roleO], "EMP-0001", "Tono Saputra", "3174012304950001")
	f.empOther = e.createEmployee(t, f.tokens[roleO], "EMP-0002", "Rina Wati", "3174012304950002")
	f.empManager = e.createEmployee(t, f.tokens[roleO], "EMP-0090", "Manajer Satu", "3174012304950090")
	e.inTenant(t, f.id, func(tx pgx.Tx) error {
		ctx := context.Background()
		if _, err := tx.Exec(ctx, `UPDATE employees SET user_id = $1 WHERE id = $2`, f.users[roleE], f.empSelf); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE employees SET user_id = $1 WHERE id = $2`, f.users[roleM], f.empManager); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE employees SET manager_employee_id = $1 WHERE id = $2`, f.empManager, f.empSelf)
		return err
	})
	f.holiday = e.mustCreate(t, f.tokens[roleO], "/api/v1/holidays", map[string]any{"date": "2026-03-19", "name": "Libur perusahaan"})
	f.comp = e.mustCreate(t, f.tokens[roleO], "/api/v1/component-definitions", map[string]any{
		"code": "TUNJ_JABATAN", "name": "Tunjangan jabatan", "category": "EARNING", "calc_type": "FIXED",
		"default_amount": "500000.00", "taxable": true, "bpjs_base": true})
	return f
}

// mustCreate POST JSON → 201, mengembalikan id.
func (e env) mustCreate(t *testing.T, token, path string, body any) uuid.UUID {
	t.Helper()
	r, b := e.do(t, http.MethodPost, path, token, body)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("POST %s: %d %v", path, r.StatusCode, b)
	}
	return uuid.MustParse(b["id"].(string))
}

// mustCreateH: mustCreate dengan header tambahan (contoh Idempotency-Key).
func (e env) mustCreateH(t *testing.T, token, path string, headers map[string]string, body any) uuid.UUID {
	t.Helper()
	r, b := e.doH(t, http.MethodPost, path, token, headers, body)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("POST %s: %d %v", path, r.StatusCode, b)
	}
	return uuid.MustParse(b["id"].(string))
}

func employeeBody(no, name, nik string) map[string]any {
	return map[string]any{
		"employee_no": no, "full_name": name, "work_email": "x" + no + "@corp.test", "hire_date": "2026-01-05",
		"ptkp_status": "K/1", "nik": nik, "npwp": strings.ReplaceAll(nik, " ", "")[1:], // NPWP unik per tenant: diturunkan dari NIK
		"bank": map[string]any{"name": "BCA", "account_number": "1234567890", "holder": name},
	}
}

func (e env) createEmployee(t *testing.T, token, no, name, nik string) uuid.UUID {
	t.Helper()
	r, b := e.doH(t, http.MethodPost, "/api/v1/employees", token, map[string]string{"Idempotency-Key": newID().String()}, employeeBody(no, name, nik))
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create employee %s: %d %v", no, r.StatusCode, b)
	}
	return uuid.MustParse(b["id"].(string))
}

// auditCount: jumlah baris audit tenant (dibaca di bawah RLS tenant itu).
func (e env) auditCount(t *testing.T, tenant uuid.UUID, action string) int {
	t.Helper()
	var n int
	e.inTenant(t, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE action = $1`, action).Scan(&n)
	})
	return n
}
