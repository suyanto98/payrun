//go:build demo

package app

// Sandbox pengunjung demo publik ("Coba demo tanpa daftar"). Satu klik = satu perusahaan contoh milik
// pengunjung (tenant baru, jadi isolasi RLS yang sama dengan pelanggan sungguhan): 15 karyawan di tiga
// departemen, satu pengajuan cuti yang menunggu approval, dan payroll bulan lalu yang SUDAH final dengan
// slip gaji. Pengunjung masuk sebagai Dina (admin payroll) dan bisa berganti peran ke Pak Hendra (owner,
// approver) atau Rizky (karyawan): aturan empat mata dan self-service terlihat tanpa penjelasan teknis.
//
// Semua data lewat service ASLI. Payroll bulan lalu benar-benar dihitung worker (job plan + calc_chunk),
// diajukan Dina, di-approve Hendra lewat mesin approval, lalu difinalisasi; slip PDF dirender lane documents.
// Request menunggu perhitungan selesai (±1–3 detik untuk 15 karyawan), batas demoCalcTimeout.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payrun/internal/modules/employee"
	"payrun/internal/modules/iam"
	"payrun/internal/modules/leave"
	"payrun/internal/modules/org"
	"payrun/internal/modules/payroll"
	"payrun/internal/modules/tenancy"
	"payrun/internal/platform/apperr"
	"payrun/internal/platform/auth"
	"payrun/internal/platform/db"
	"payrun/internal/platform/httpx"
	"payrun/internal/platform/jobs"
	"payrun/internal/platform/ratelimit"
	"payrun/internal/platform/reqmeta"
	"payrun/internal/platform/tenantctx"
)

const (
	demoEmailDomain   = "demo.payrun.test"
	demoCompanyName   = "PT Kopi Nusantara"
	demoKindExpire    = "demo.sandbox_expire"
	demoCalcTimeout   = 45 * time.Second
	defaultSandboxTTL = 24 * time.Hour
	defaultDailyMax   = 500
)

// demoPersona: tiga peran yang bisa dipakai pengunjung. key dipakai SPA (POST /demo/persona).
type demoPersona struct {
	Key   string   `json:"key"`
	Name  string   `json:"name"`
	Title string   `json:"title"`
	Roles []string `json:"-"`
}

var (
	personaPayroll  = demoPersona{"payroll", "Dina Lestari", "Admin payroll", []string{auth.RolePayroll}}
	personaOwner    = demoPersona{"owner", "Hendra Gunawan", "Owner · menyetujui payroll dan cuti", []string{auth.RoleOwner}}
	personaEmployee = demoPersona{"employee", "Rizky Pratama", "Karyawan · barista", []string{auth.RoleEmployee}}
	demoPersonas    = []demoPersona{personaPayroll, personaOwner, personaEmployee}
)

type demoEmployee struct {
	name, dept, pos, salary, ptkp, hire string
}

// Departemen → jabatan. Kode pendek dipakai sebagai code unit org.
var (
	demoDepartments = [][2]string{{"OPS", "Operasional"}, {"FIN", "Keuangan"}, {"MKT", "Pemasaran"}}
	demoPositions   = [][2]string{{"BAR", "Barista"}, {"HBR", "Head Barista"}, {"KSR", "Kasir"}, {"KTK", "Kepala Toko"},
		{"GDG", "Staf Gudang"}, {"TKN", "Teknisi Mesin Kopi"}, {"SKU", "Staf Keuangan"}, {"MKU", "Manajer Keuangan"},
		{"SMS", "Spesialis Media Sosial"}, {"MPM", "Manajer Pemasaran"}, {"DSG", "Desainer Grafis"}}
	// Karyawan pertama = Rizky (persona karyawan, ditautkan ke user-nya).
	demoEmployees = []demoEmployee{
		{"Rizky Pratama", "OPS", "BAR", "5200000", "TK/0", "2025-03-03"},
		{"Agus Setiawan", "OPS", "KTK", "9800000", "K/2", "2023-06-12"},
		{"Bayu Saputra", "OPS", "HBR", "7500000", "K/1", "2024-01-08"},
		{"Ayu Kartika", "OPS", "BAR", "5200000", "TK/0", "2025-02-17"},
		{"Fajar Nugroho", "OPS", "BAR", "5300000", "TK/1", "2024-09-02"},
		{"Dewi Anggraini", "OPS", "BAR", "5200000", "K/0", "2025-05-19"},
		{"Putri Maharani", "OPS", "KSR", "5000000", "TK/0", "2024-11-04"},
		{"Maya Sari", "OPS", "KSR", "5000000", "TK/0", "2025-07-01"},
		{"Joko Susilo", "OPS", "GDG", "5100000", "K/1", "2023-10-16"},
		{"Eko Prasetyo", "OPS", "TKN", "6500000", "K/1", "2024-04-22"},
		{"Yusuf Hakim", "FIN", "MKU", "15500000", "K/3", "2022-08-01"},
		{"Siti Nurhaliza", "FIN", "SKU", "7000000", "K/0", "2024-02-05"},
		{"Rahmat Hidayat", "MKT", "MPM", "14000000", "K/2", "2023-01-09"},
		{"Nadia Putri", "MKT", "SMS", "6800000", "TK/0", "2024-07-15"},
		{"Citra Lestari", "MKT", "DSG", "7200000", "TK/0", "2025-01-13"},
	}
)

type sandboxer struct {
	d        Deps
	w        demoWiring
	ttl      time.Duration
	dailyMax int
}

func newSandboxer(w demoWiring, d Deps) *sandboxer {
	s := &sandboxer{d: d, w: w, ttl: d.DemoSandboxTTL, dailyMax: d.DemoSandboxDailyMax}
	if s.ttl <= 0 {
		s.ttl = defaultSandboxTTL
	}
	if s.dailyMax <= 0 {
		s.dailyMax = defaultDailyMax
	}
	return s
}

func registerSandboxRoutes(w demoWiring, d Deps) {
	s := newSandboxer(w, d)
	// Endpoint yang MENERBITKAN cookie sesi: CSRF guard mencegah situs lain diam-diam me-login-kan browser korban.
	csrf := httpx.CSRFGuard(d.AllowedOrigins)
	w.public.With(csrf, w.limiter.Middleware(w.tiers.DemoSession, ratelimit.ByIP)).Post("/demo/sessions", s.createSession)
	// Tier read (per IP), BUKAN auth-ip: halaman login memanggil ini setiap dibuka; memakai kuota login
	// membuat pengguna di belakang satu NAT (dan suite E2E) kehabisan jatah login.
	w.public.With(w.limiter.Middleware(w.tiers.Read, ratelimit.ByIP)).Get("/demo", func(rw http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(rw, http.StatusOK, map[string]any{"sandbox": true, "ttl_seconds": int64(s.ttl / time.Second)})
	})
	w.authed.Get("/demo/sandbox", s.getSandbox)
	w.authed.With(csrf).Post("/demo/persona", s.switchPersona)
	// Simulasi gangguan (demo_crash.go): token harus ber-tenant sandbox pemanggil.
	byUser := func(r *http.Request) string {
		p, _ := auth.PrincipalFrom(r.Context())
		return "crash|" + p.UserID.String()
	}
	w.authed.With(w.limiter.Middleware(w.tiers.DemoSession, byUser)).Post("/demo/crash-test", s.armCrash)
	w.authed.Get("/demo/runs/{id}/timeline", s.runTimeline)
	w.authed.With(w.limiter.Middleware(w.tiers.DemoSession, byUser)).Post("/demo/runs/{id}/verify", s.verifyRun)
}

type sandboxRow struct {
	tenantID                       uuid.UUID
	payrollUser, ownerUser, empUse uuid.UUID
	expiresAt                      time.Time
}

func (r sandboxRow) userFor(key string) (uuid.UUID, bool) {
	switch key {
	case personaPayroll.Key:
		return r.payrollUser, true
	case personaOwner.Key:
		return r.ownerUser, true
	case personaEmployee.Key:
		return r.empUse, true
	}
	return uuid.Nil, false
}

func (r sandboxRow) personaOf(user uuid.UUID) string {
	for _, p := range demoPersonas {
		if u, _ := r.userFor(p.Key); u == user {
			return p.Key
		}
	}
	return ""
}

// sandboxOf: sandbox aktif tempat user menjadi anggota; bukan anggota / kedaluwarsa → 404.
func (s *sandboxer) sandboxOf(ctx context.Context, user uuid.UUID) (sandboxRow, error) {
	var r sandboxRow
	err := s.d.Pool.QueryRow(ctx, `SELECT tenant_id, payroll_user_id, owner_user_id, employee_user_id, expires_at
		FROM demo_sandboxes WHERE $1 = ANY(member_user_ids) AND expired_at IS NULL`, user).
		Scan(&r.tenantID, &r.payrollUser, &r.ownerUser, &r.empUse, &r.expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, apperr.ErrNotFound
	}
	return r, err
}

func (s *sandboxer) writeSession(w http.ResponseWriter, sess iam.Session) {
	s.w.iamH.SetRefreshCookie(w, sess.RefreshToken, sess.RefreshMaxAge)
	httpx.WriteJSON(w, http.StatusOK, iam.NewSessionResponse(sess, true))
}

func (s *sandboxer) createSession(w http.ResponseWriter, r *http.Request) {
	meta := reqmeta.From(r)
	sb, err := s.create(r.Context(), meta)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	sess, err := s.w.iam.StartSession(r.Context(), sb.payrollUser, sb.tenantID, "auth.login_succeeded", meta)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	s.d.Logger.Info("demo.sandbox_created", "tenant_id", sb.tenantID.String(), "request_id", meta.RequestID)
	s.writeSession(w, sess)
}

// switchPersona: sesi baru sebagai persona lain DI SANDBOX YANG SAMA. Tidak ada "login sebagai siapa saja":
// target harus anggota sandbox yang sama dengan pemanggil, dan tenant-nya sandbox itu.
func (s *sandboxer) switchPersona(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	var in struct {
		Persona string `json:"persona"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	sb, err := s.sandboxOf(r.Context(), p.UserID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	target, ok := sb.userFor(in.Persona)
	if !ok {
		httpx.WriteError(w, r, apperr.Validation(apperr.FieldError{Field: "persona", Code: "INVALID_ENUM", Message: "payroll | owner | employee"}))
		return
	}
	sess, err := s.w.iam.StartSession(r.Context(), target, sb.tenantID, "auth.demo_persona_switched", reqmeta.From(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	s.writeSession(w, sess)
}

func (s *sandboxer) getSandbox(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	sb, err := s.sandboxOf(r.Context(), p.UserID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"expires_at": sb.expiresAt, "company": demoCompanyName,
		"persona": sb.personaOf(p.UserID), "personas": demoPersonas})
}

// create membangun sandbox. Baris demo_sandboxes ditulis di transaksi PERTAMA (bersama user dan tenant):
// kalau langkah berikutnya gagal, sandbox setengah jadi tetap kedaluwarsa dan tenant-nya disuspend.
func (s *sandboxer) create(ctx context.Context, meta reqmeta.Meta) (sandboxRow, error) {
	now := s.d.Clock.Now()
	var recent int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM demo_sandboxes WHERE created_at > $1`, now.Add(-24*time.Hour)).Scan(&recent); err != nil {
		return sandboxRow{}, err
	}
	// Batas lunak: dua request paralel tepat di batas bisa sama-sama lolos (tujuannya melindungi disk, bukan presisi).
	if recent >= s.dailyMax {
		return sandboxRow{}, apperr.ErrUnavailable.WithDetail("demo sandbox quota for today is used up; try again tomorrow")
	}
	tag, err := randomHex(4)
	if err != nil {
		return sandboxRow{}, err
	}
	secret, err := randomSecret() // tidak disimpan di mana pun: sesi diterbitkan lewat StartSession, bukan password
	if err != nil {
		return sandboxRow{}, err
	}
	hash, err := s.w.iam.HashPassword(ctx, secret) // argon2 di luar transaksi
	if err != nil {
		return sandboxRow{}, err
	}
	sb := sandboxRow{tenantID: s.d.IDs.New(), payrollUser: s.d.IDs.New(), ownerUser: s.d.IDs.New(), empUse: s.d.IDs.New(), expiresAt: now.Add(s.ttl)}
	emailOf := func(key string) string { return key + "@" + tag + "." + demoEmailDomain }
	members := []uuid.UUID{sb.payrollUser, sb.ownerUser, sb.empUse}

	// [1] user (mode platform) → tenant + OWNER (mode tenant) → anggota lain → catatan sandbox.
	err = db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		for _, p := range demoPersonas {
			id, _ := sb.userFor(p.Key)
			if err := s.w.iam.ProvisionVerifiedUserTx(ctx, tx, id, emailOf(p.Key), hash, p.Name); err != nil {
				return err
			}
		}
		if err := s.w.tenancy.CreateActiveTenantTx(ctx, tx, sb.tenantID, sb.ownerUser, demoCompanyName, "demo-"+tag, meta); err != nil {
			return err
		}
		for _, p := range []demoPersona{personaPayroll, personaEmployee} {
			id, _ := sb.userFor(p.Key)
			if err := s.w.iam.AddMemberTx(ctx, tx, sb.tenantID, id, sb.ownerUser, p.Roles); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO demo_sandboxes (id, tenant_id, payroll_user_id, owner_user_id, employee_user_id,
			member_user_ids, created_at, expires_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			s.d.IDs.New(), sb.tenantID, sb.payrollUser, sb.ownerUser, sb.empUse, members, now, sb.expiresAt)
		return err
	})
	if err != nil {
		return sandboxRow{}, db.MapError(err)
	}

	tctx := tenantctx.With(ctx, sb.tenantID)
	if err := s.populate(tctx, sb, meta); err != nil {
		return sandboxRow{}, fmt.Errorf("demo sandbox %s: %w", sb.tenantID, err)
	}
	return sb, nil
}

// populate: struktur organisasi, karyawan, cuti menunggu approval, lalu payroll bulan lalu sampai final.
func (s *sandboxer) populate(ctx context.Context, sb sandboxRow, meta reqmeta.Meta) error {
	units := map[string]uuid.UUID{}
	for kind, list := range map[*org.Kind][][2]string{&org.Departments: demoDepartments, &org.Positions: demoPositions} {
		for _, u := range list {
			created, err := s.w.org.Create(ctx, *kind, sb.ownerUser, u[0], u[1], meta)
			if err != nil {
				return err
			}
			units[u[0]] = created.ID
		}
	}
	// [2] karyawan + gaji awal + penempatan (W2), satu transaksi; Rizky ditautkan ke user-nya.
	err := db.WithTenantTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		for i, e := range demoEmployees {
			created, err := s.w.employees.CreateTx(ctx, tx, sb.ownerUser, employee.CreateInput{
				EmployeeNo: fmt.Sprintf("KN-%03d", i+1), FullName: e.name, HireDate: e.hire, PtkpStatus: e.ptkp,
				Org:    &employee.OrgInput{DepartmentID: units[e.dept], PositionID: units[e.pos]},
				Salary: &employee.SalaryInput{BaseSalary: e.salary},
			}, meta)
			if err != nil {
				return err
			}
			if i == 0 {
				if err := s.w.employees.LinkUserTx(ctx, tx, created.ID, sb.empUse); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := s.pendingLeave(ctx, sb, meta); err != nil {
		return err
	}
	return s.lastMonthPayroll(ctx, sb, meta)
}

// pendingLeave: Rizky mengajukan cuti tahunan dua hari, minggu depan. Approver dipilih server (atasan →
// HR_ADMIN → OWNER) = Pak Hendra; muncul di kotak approval saat pengunjung berganti peran ke owner.
func (s *sandboxer) pendingLeave(ctx context.Context, sb sandboxRow, meta reqmeta.Meta) error {
	types, err := s.w.leave.ListTypes(ctx)
	if err != nil {
		return err
	}
	var annual uuid.UUID
	for _, t := range types {
		if t.Code == "ANNUAL" {
			annual = t.ID
		}
	}
	jkt, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		return err
	}
	start := s.d.Clock.Now().In(jkt).AddDate(0, 0, 7)
	for start.Weekday() == time.Saturday || start.Weekday() == time.Sunday || start.Weekday() == time.Friday {
		start = start.AddDate(0, 0, 1) // dua hari kerja berturut-turut
	}
	end := start.AddDate(0, 0, 1)
	if end.Year() != start.Year() {
		end = start
	}
	reason := "Acara keluarga di Yogyakarta"
	p := auth.Principal{UserID: sb.empUse, TenantID: sb.tenantID, Roles: personaEmployee.Roles}
	return db.WithTenantTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		_, err := s.w.leave.SubmitTx(ctx, tx, p, leave.SubmitInput{LeaveTypeID: annual,
			StartDate: start.Format(time.DateOnly), EndDate: end.Format(time.DateOnly), Reason: &reason}, meta)
		return err
	})
}

// lastMonthPayroll: payroll bulan lalu lewat pipeline asli: Dina membuat + menghitung (worker), Dina
// mengajukan, Hendra menyetujui, Dina memfinalisasi. Job payslip.render dibuat oleh finalisasi.
func (s *sandboxer) lastMonthPayroll(ctx context.Context, sb sandboxRow, meta reqmeta.Meta) error {
	jkt, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		return err
	}
	period := s.d.Clock.Now().In(jkt).AddDate(0, -1, 0).Format("2006-01")
	var runID uuid.UUID
	err = db.WithTenantTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		run, err := s.w.payroll.CreateRunTx(ctx, tx, sb.payrollUser, period, meta)
		if err != nil {
			return err
		}
		runID = run.ID
		_, err = s.w.payroll.CalculateTx(ctx, tx, sb.payrollUser, run.ID, meta)
		return err
	})
	if err != nil {
		return err
	}
	if err := s.waitCalculated(ctx, runID); err != nil {
		return err
	}
	run, err := s.w.payroll.Submit(ctx, sb.payrollUser, runID, payroll.SubmitInput{}, meta)
	if err != nil {
		return err
	}
	if run.ApprovalRequestID == nil {
		return errors.New("submitted run has no approval request")
	}
	owner := auth.Principal{UserID: sb.ownerUser, TenantID: sb.tenantID, Roles: personaOwner.Roles}
	if _, err := s.w.approvals.Approve(ctx, owner, *run.ApprovalRequestID, "Sudah saya cek, silakan diproses.", meta); err != nil {
		return err
	}
	return db.WithTenantTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		_, err := s.w.payroll.FinalizeTx(ctx, tx, sb.payrollUser, runID, meta)
		return err
	})
}

// waitCalculated menunggu worker menyelesaikan perhitungan (status CALCULATED). Worker tidak jalan /
// macet → 503 setelah demoCalcTimeout; sandbox setengah jadi dibersihkan job kedaluwarsa.
func (s *sandboxer) waitCalculated(ctx context.Context, runID uuid.UUID) error {
	deadline := time.NewTimer(demoCalcTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		run, err := s.w.payroll.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		switch run.Status {
		case "CALCULATED":
			return nil
		case "FAILED", "CANCELLED":
			return fmt.Errorf("demo payroll run ended %s", run.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return apperr.ErrUnavailable.WithDetail("payroll worker did not finish the demo calculation in time; try again")
		case <-tick.C:
		}
	}
}

// demoMaintenanceKinds: job demo.sandbox_expire tiap 5 menit (lane maintenance).
func demoMaintenanceKinds(d Deps) ([]jobs.Kind, map[string]time.Duration) {
	kind := jobs.Kind{Name: demoKindExpire, MaxAttempts: 3, Timeout: 2 * time.Minute,
		Handler: func(ctx context.Context, _ *jobs.Job) error {
			n, err := ExpireDemoSandboxes(ctx, d, 50)
			d.Logger.Info("demo.sandbox_expire_done", "expired", n)
			return err
		}}
	return []jobs.Kind{kind}, map[string]time.Duration{demoKindExpire: 5 * time.Minute}
}

// ExpireDemoSandboxes: sandbox lewat expires_at → tenant SUSPENDED (sesi baru tidak mendapat tenant ini) dan
// semua refresh token anggotanya dicabut. Data tidak dihapus: payroll final bersifat immutable (PI-05),
// dan tenant yang disuspend sudah tidak terjangkau siapa pun. SKIP LOCKED: dua worker tidak bentrok.
func ExpireDemoSandboxes(ctx context.Context, d Deps, batch int) (int, error) {
	now := d.Clock.Now()
	expired := 0
	err := db.WithTx(ctx, d.Pool, func(tx pgx.Tx) error {
		expired = 0
		rows, err := tx.Query(ctx, `SELECT id, tenant_id, member_user_ids FROM demo_sandboxes
			WHERE expired_at IS NULL AND expires_at <= $1 ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED`, now, batch)
		if err != nil {
			return err
		}
		type due struct {
			id, tenant uuid.UUID
			members    []uuid.UUID
		}
		var list []due
		for rows.Next() {
			var x due
			if err := rows.Scan(&x.id, &x.tenant, &x.members); err != nil {
				rows.Close()
				return err
			}
			list = append(list, x)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, x := range list {
			if err := tenancy.SuspendTenantTx(ctx, tx, x.tenant, now); err != nil {
				return err
			}
			if err := iam.RevokeSessionsTx(ctx, tx, now, x.members); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE demo_sandboxes SET expired_at = $1 WHERE id = $2`, now, x.id); err != nil {
				return err
			}
			expired++
		}
		return nil
	})
	return expired, err
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func randomSecret() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
