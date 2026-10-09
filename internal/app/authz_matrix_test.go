package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.yaml.in/yaml/v3"
)

// Matriks otorisasi sebagai test (file 17 §4; PF-04 AC3). Sumber: api/authz_matrix.yaml.

type matrixCase struct {
	Target      string         `yaml:"target"`
	Query       string         `yaml:"query"`
	Body        map[string]any `yaml:"body"`
	IfMatch     bool           `yaml:"if_match"`
	Idempotency bool           `yaml:"idempotency"`
	Expect      map[string]int `yaml:"expect"`
}

type matrixRoute struct {
	ID     string       `yaml:"id"`
	Route  string       `yaml:"route"`
	Access string       `yaml:"access"`
	Audit  string       `yaml:"audit"`
	Cases  []matrixCase `yaml:"cases"`
}

func loadMatrix(t *testing.T) []matrixRoute {
	t.Helper()
	raw, err := os.ReadFile("../../api/authz_matrix.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Routes []matrixRoute `yaml:"routes"`
	}
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m.Routes
}

var roleOfColumn = map[string]string{"O": roleO, "H": roleH, "P": roleP, "M": roleM, "E": roleE}

// routerRoutes: semua rute API dari router sungguhan ("METHOD /path" tanpa prefix /api/v1).
func routerRoutes(t *testing.T, e env) []string {
	t.Helper()
	mux, ok := e.handler.(chi.Routes)
	if !ok {
		t.Fatal("API handler is not a chi router")
	}
	var out []string
	if err := chi.Walk(mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		out = append(out, method+" "+strings.TrimPrefix(route, "/api/v1"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// Aturan CI 1 + 2: rute tanpa baris matriks = gagal; baris tanpa rute (matriks basi) = gagal; baris
// tenant tanpa case = gagal.
func TestAuthzMatrix_CoversEveryRoute(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	rows := loadMatrix(t)
	inMatrix := map[string]matrixRoute{}
	for _, r := range rows {
		if _, dup := inMatrix[r.Route]; dup {
			t.Errorf("duplicate matrix row for %s", r.Route)
		}
		inMatrix[r.Route] = r
		if !slices.Contains([]string{"public", "cookie", "user", "tenant"}, r.Access) {
			t.Errorf("%s: unknown access %q", r.Route, r.Access)
		}
		if r.Access == "tenant" && len(r.Cases) == 0 {
			t.Errorf("%s (%s): tenant route without cases", r.Route, r.ID)
		}
	}
	routes := routerRoutes(t, e)
	for _, rt := range routes {
		if _, ok := inMatrix[rt]; !ok {
			t.Errorf("route %s has no row in api/authz_matrix.yaml", rt)
		}
	}
	for rt := range inMatrix {
		if !slices.Contains(routes, rt) {
			t.Errorf("matrix row %s has no route (stale matrix)", rt)
		}
	}
}

// matrixRunner menjalankan case di atas fixture Alpha.
type matrixRunner struct {
	e    env
	f    *tenantFx
	seq  atomic.Int64
	past atomic.Int64 // periode run unik: 2025-12, 2025-11, … (tanpa employee eligible → cepat)
	cur  atomic.Int64 // periode run_item: 2026-01 … 2026-11 (employee fixture eligible)
	// items: run → item ERROR miliknya, untuk {item_id}. run/runItem: target baca bersama.
	items       map[uuid.UUID]uuid.UUID
	run, runItm uuid.UUID
	callN       int64 // nomor per panggilan: placeholder tanggal dalam satu body memakai nomor yang sama
}

// workday: hari kerja (Sen–Jum) unik di 2027 untuk nomor n (cuti/lembur tanpa tumpang tindih).
func workday(n int64) time.Time {
	base := time.Date(2027, 1, 4, 0, 0, 0, 0, time.UTC) // Senin
	return base.AddDate(0, 0, int(n/5)*7+int(n%5))
}

func (m *matrixRunner) leaveType(t *testing.T, code string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	m.e.inTenant(t, m.f.id, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT id FROM leave_types WHERE code = $1`, code).Scan(&id)
	})
	return id
}

func (m *matrixRunner) next() int64 { return m.seq.Add(1) }

func (m *matrixRunner) pastPeriod() string {
	return time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC).AddDate(0, -int(m.past.Add(1)-1), 0).Format("2006-01")
}

func (m *matrixRunner) currentPeriod(t *testing.T) string {
	n := m.cur.Add(1)
	if n > 11 { // jam fake 2026-10-06: periode paling jauh 2026-11
		t.Fatal("matrix: out of 2026 periods for run_item targets")
	}
	return fmt.Sprintf("2026-%02d", n)
}

func (m *matrixRunner) fill(t *testing.T, v any) any {
	switch x := v.(type) {
	case string:
		n := m.next()
		x = strings.ReplaceAll(x, "{n}", fmt.Sprint(n))
		x = strings.ReplaceAll(x, "{nik}", fmt.Sprintf("3174%012d", 500000+n))
		x = strings.ReplaceAll(x, "{email}", fmt.Sprintf("invitee%d@%s.test", n, m.f.slug))
		x = strings.ReplaceAll(x, "{date}", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, int(n)).Format("2006-01-02"))
		x = strings.ReplaceAll(x, "{dept}", m.f.dept.String())
		x = strings.ReplaceAll(x, "{pos}", m.f.pos.String())
		x = strings.ReplaceAll(x, "{comp}", m.f.comp.String())
		day := workday(m.callN)
		x = strings.ReplaceAll(x, "{workday}", day.Format(time.DateOnly))
		x = strings.ReplaceAll(x, "{ot_date}", day.Format(time.DateOnly))
		x = strings.ReplaceAll(x, "{ot_start}", day.Format(time.DateOnly)+"T18:00:00+07:00")
		x = strings.ReplaceAll(x, "{ot_end}", day.Format(time.DateOnly)+"T21:00:00+07:00")
		if strings.Contains(x, "{leave_annual}") {
			x = strings.ReplaceAll(x, "{leave_annual}", m.leaveType(t, "ANNUAL").String())
		}
		x = strings.ReplaceAll(x, "{owner_user}", m.f.users[roleO].String())
		x = strings.ReplaceAll(x, "{payroll_user}", m.f.users[roleP].String())
		if strings.Contains(x, "{period}") {
			x = strings.ReplaceAll(x, "{period}", m.pastPeriod())
		}
		return x
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			out[k] = m.fill(t, val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = m.fill(t, val)
		}
		return out
	}
	return v
}

// target membuat (atau memilih) resource untuk {id}. fresh = buat baru (endpoint yang mengubah data).
func (m *matrixRunner) target(t *testing.T, kind string, fresh bool) uuid.UUID {
	t.Helper()
	owner := m.f.tokens[roleO]
	switch kind {
	case "department":
		if !fresh {
			return m.f.dept
		}
		return m.e.mustCreate(t, owner, "/api/v1/departments", map[string]any{"code": fmt.Sprintf("T%d", m.next()), "name": "Target"})
	case "position":
		if !fresh {
			return m.f.pos
		}
		return m.e.mustCreate(t, owner, "/api/v1/positions", map[string]any{"code": fmt.Sprintf("T%d", m.next()), "name": "Target"})
	case "employee_other":
		if !fresh {
			return m.f.empOther
		}
		n := m.next()
		return m.e.createEmployee(t, owner, fmt.Sprintf("TGT-%d", n), "Target", fmt.Sprintf("3175%012d", n))
	case "employee_self", "employee_report": // empSelf = diri user EMPLOYEE = bawahan user MANAGER
		return m.f.empSelf
	case "holiday":
		if !fresh {
			return m.f.holiday
		}
		return m.e.mustCreate(t, owner, "/api/v1/holidays", map[string]any{"date": m.fill(t, "{date}"), "name": "Target"})
	case "component":
		if !fresh {
			return m.f.comp
		}
		return m.e.mustCreate(t, owner, "/api/v1/component-definitions", map[string]any{
			"code": fmt.Sprintf("T%d", m.next()), "name": "Target", "category": "EARNING", "calc_type": "FIXED", "default_amount": "1000.00"})
	case "member_plain", "member_admin": // member ACTIVE baru: EMPLOYEE, atau PAYROLL_ADMIN (role admin)
		role := roleE
		if kind == "member_admin" {
			role = roleP
		}
		n := m.next()
		return m.e.newMember(t, m.f.id, fmt.Sprintf("member%d@%s.test", n, m.f.slug), role)
	case "attendance_other", "attendance_self": // catatan INCOMPLETE milik empOther / empSelf (bawahan MANAGER)
		emp := m.f.empOther
		if kind == "attendance_self" {
			emp = m.f.empSelf
		}
		id := newID()
		day := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, int(m.next()))
		m.e.inTenant(t, m.f.id, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `INSERT INTO attendance_records (tenant_id, id, employee_id, work_date, clock_in_at, status, source)
				VALUES ($1, $2, $3, $4, '2026-01-01T00:00:00Z', 'INCOMPLETE', 'CLOCK')`, m.f.id, id, emp, day)
			return err
		})
		return id
	case "leave_own", "approval_leave": // cuti PENDING milik EMPLOYEE (approver = MANAGER, atasan langsungnya)
		day := workday(m.next() + 500).Format(time.DateOnly)
		r, b := m.e.doH(t, http.MethodPost, "/api/v1/leave-requests", m.f.tokens[roleE], map[string]string{"Idempotency-Key": newID().String()},
			map[string]any{"leave_type_id": m.leaveType(t, "ANNUAL"), "start_date": day, "end_date": day})
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("target leave: %d %v", r.StatusCode, b)
		}
		if kind == "approval_leave" {
			return uuid.MustParse(b["approval"].(map[string]any)["id"].(string))
		}
		return uuid.MustParse(b["id"].(string))
	case "overtime_own": // lembur PENDING milik EMPLOYEE
		day := workday(m.next() + 500).Format(time.DateOnly)
		r, b := m.e.doH(t, http.MethodPost, "/api/v1/overtime-requests", m.f.tokens[roleE], map[string]string{"Idempotency-Key": newID().String()},
			map[string]any{"work_date": day, "start_at": day + "T18:00:00+07:00", "end_at": day + "T20:00:00+07:00", "reason": "Stok opname"})
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("target overtime: %d %v", r.StatusCode, b)
		}
		return uuid.MustParse(b["id"].(string))
	case "run":
		if !fresh && m.run != uuid.Nil {
			return m.run
		}
		id := m.e.createRun(t, owner, m.pastPeriod())
		if !fresh {
			m.run = id
		}
		return id
	case "run_calculated": // run CALCULATED tanpa item ERROR (periode lampau: 0 karyawan eligible)
		run, _ := m.e.calculatedRun(t, owner, m.pastPeriod())
		return run
	case "run_approved": // run APPROVED, siap finalize
		run, _ := m.e.calculatedRun(t, owner, m.pastPeriod())
		ap := m.e.submitRun(t, m.f.tokens[roleP], run, map[string]any{"approver_user_id": m.f.users[roleO]})
		if r, b := m.e.approve(t, owner, ap); r.StatusCode != http.StatusOK {
			t.Fatalf("target run_approved: %d %v", r.StatusCode, b)
		}
		return run
	case "approval_run": // approval PAYROLL_RUN yang di-submit PAYROLL_ADMIN; approver otomatis = OWNER
		run, _ := m.e.calculatedRun(t, owner, m.pastPeriod())
		return m.e.submitRun(t, m.f.tokens[roleP], run, map[string]any{"approver_user_id": m.f.users[roleO]})
	case "run_item":
		if !fresh && m.runItm != uuid.Nil {
			return m.runItm
		}
		run, item := m.e.errorItem(t, owner, m.currentPeriod(t))
		m.items[run] = item
		if !fresh {
			m.runItm = run
		}
		return run
	}
	t.Fatalf("unknown target %q", kind)
	return uuid.Nil
}

func (m *matrixRunner) call(t *testing.T, row matrixRoute, c matrixCase, token string, id uuid.UUID) (result, map[string]any) {
	t.Helper()
	method, path, _ := strings.Cut(row.Route, " ")
	if id != uuid.Nil {
		path = strings.Replace(path, "{id}", id.String(), 1)
		if item, ok := m.items[id]; ok {
			path = strings.Replace(path, "{item_id}", item.String(), 1)
		}
	}
	if c.Query != "" {
		path += "?" + c.Query
	}
	headers := map[string]string{}
	if c.IfMatch {
		headers["If-Match"] = `"1"`
	}
	if c.Idempotency {
		headers["Idempotency-Key"] = newID().String()
	}
	var body any
	m.callN = m.next()
	if c.Body != nil {
		body = m.fill(t, c.Body)
	} else if method != http.MethodGet {
		body = map[string]any{}
	}
	return m.e.doH(t, method, "/api/v1"+path, token, headers, body)
}

// TestAuthzMatrix: setiap baris × case × role → status yang diharapkan; respons 2xx pada endpoint
// ber-audit menambah tepat satu baris audit_log dengan action yang benar.
func TestAuthzMatrix(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	m := &matrixRunner{e: e, f: e.newTenantFx(t, "alpha"), items: map[uuid.UUID]uuid.UUID{}}
	for _, row := range loadMatrix(t) {
		for ci, c := range row.Cases {
			cols := make([]string, 0, len(c.Expect))
			for col := range c.Expect {
				cols = append(cols, col)
			}
			sort.Strings(cols)
			for _, col := range cols {
				want := c.Expect[col]
				role, ok := roleOfColumn[col]
				if !ok {
					t.Fatalf("%s: unknown role column %q", row.ID, col)
				}
				name := fmt.Sprintf("%s/case%d/%s", row.ID, ci, role)
				t.Run(name, func(t *testing.T) {
					method, _, _ := strings.Cut(row.Route, " ")
					var id uuid.UUID
					if c.Target != "" {
						id = m.target(t, c.Target, method != http.MethodGet)
					}
					before := 0
					if row.Audit != "" {
						before = e.auditCount(t, m.f.id, row.Audit)
					}
					r, b := m.call(t, row, c, m.f.tokens[role], id)
					if r.StatusCode != want {
						raw, _ := json.Marshal(b)
						t.Fatalf("%s as %s: want %d, got %d %s", row.Route, role, want, r.StatusCode, raw)
					}
					if row.Audit != "" && want < 300 {
						if got := e.auditCount(t, m.f.id, row.Audit); got != before+1 {
							t.Fatalf("%s as %s: audit %q must grow by exactly 1 (%d → %d)", row.Route, role, row.Audit, before, got)
						}
					}
				})
			}
		}
	}
}

// TestCrossTenant (file 17 §4.3, Demo A): untuk SETIAP rute tenant ber-{id}, semua aktor Beta yang
// memanggil id milik Alpha tidak pernah sukses. Role yang diizinkan endpoint mendapat 404 (tidak
// membedakan "tidak ada" dan "milik tenant lain"). Daftar milik Beta tidak pernah memuat id Alpha.
func TestCrossTenant(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	alpha := e.newTenantFx(t, "alpha")
	beta := e.newTenantFx(t, "beta")
	alphaRun, alphaItem := e.errorItem(t, alpha.tokens[roleO], "2026-01")
	m := &matrixRunner{e: e, f: beta, items: map[uuid.UUID]uuid.UUID{alphaRun: alphaItem}}
	alphaMember := e.newMember(t, alpha.id, "member@alpha.test", roleE)
	ma := &matrixRunner{e: e, f: alpha, items: map[uuid.UUID]uuid.UUID{}}
	ma.seq.Store(9000)
	alphaAttendance := ma.target(t, "attendance_other", true)
	alphaLeave := ma.target(t, "leave_own", true)
	alphaOvertime := ma.target(t, "overtime_own", true)
	alphaCalc, _ := e.calculatedRun(t, alpha.tokens[roleO], "2025-06")
	alphaApproval := e.submitRun(t, alpha.tokens[roleP], alphaCalc, map[string]any{})
	alphaIDs := map[string]uuid.UUID{"/departments": alpha.dept, "/positions": alpha.pos, "/employees": alpha.empOther,
		"/holidays": alpha.holiday, "/component-definitions": alpha.comp, "/payroll-runs": alphaRun, "/members": alphaMember,
		"/approvals": alphaApproval, "/attendance": alphaAttendance, "/leave-requests": alphaLeave, "/overtime-requests": alphaOvertime}
	tested := 0
	for _, row := range loadMatrix(t) {
		if row.Access != "tenant" || !strings.Contains(row.Route, "{id}") {
			continue
		}
		_, path, _ := strings.Cut(row.Route, " ")
		var target uuid.UUID
		for prefix, id := range alphaIDs {
			if strings.HasPrefix(path, prefix+"/") {
				target = id
			}
		}
		if target == uuid.Nil {
			t.Fatalf("%s: no Alpha resource mapped for cross-tenant check (extend alphaIDs)", row.Route)
		}
		c := row.Cases[0]
		for col, role := range roleOfColumn {
			allowed := false
			for _, cs := range row.Cases {
				if st, ok := cs.Expect[col]; ok && st < 300 {
					allowed = true
				}
			}
			r, b := m.call(t, row, c, beta.tokens[role], target)
			switch {
			case r.StatusCode < 300:
				t.Errorf("LEAK: %s as Beta %s on Alpha resource → %d %v", row.Route, role, r.StatusCode, b)
			case allowed && r.StatusCode != http.StatusNotFound:
				t.Errorf("%s as Beta %s on Alpha resource: want 404, got %d %v", row.Route, role, r.StatusCode, b)
			case !allowed && r.StatusCode != http.StatusNotFound && r.StatusCode != http.StatusForbidden:
				t.Errorf("%s as Beta %s on Alpha resource: want 403/404, got %d %v", row.Route, role, r.StatusCode, b)
			}
			tested++
		}
	}
	if tested == 0 {
		t.Fatal("no cross-tenant case ran")
	}
	for _, path := range []string{"/departments", "/departments?status=INACTIVE", "/positions", "/employees", "/members", "/tenant",
		"/holidays?year=2026", "/component-definitions", "/payroll-runs", "/approvals/inbox", "/approvals/inbox?status=APPROVED",
		"/attendance?period=2026-02", "/leave-requests", "/overtime-requests", "/leave-types"} {
		r, b := e.do(t, http.MethodGet, "/api/v1"+path, beta.tokens[roleO], nil)
		raw, _ := json.Marshal(b)
		for _, id := range []uuid.UUID{alpha.id, alpha.dept, alpha.pos, alpha.empSelf, alpha.empOther, alpha.empManager, alpha.holiday, alpha.comp, alpha.users[roleO],
			alphaRun, alphaItem, alphaMember, alphaApproval, alphaAttendance, alphaLeave, alphaOvertime} {
			if strings.Contains(string(raw), id.String()) {
				t.Errorf("LEAK: Beta GET %s (%d) contains Alpha id %s", path, r.StatusCode, id)
			}
		}
	}
}
