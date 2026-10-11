// Package app merakit dependency dan router. Ini satu-satunya tempat yang boleh mengenal
// semua modul sekaligus (composition root); modul tidak saling meng-import (ADR-001).
//
// Router memasang middleware platform lengkap (request id, tracing, metrics, security header, client
// IP, access log, recover, body limit) sebelum modul mana pun, sehingga setiap modul mendapatkannya.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"payrun/db/migrations"
	"payrun/internal/modules/approval"
	"payrun/internal/modules/attendance"
	"payrun/internal/modules/compensation"
	"payrun/internal/modules/employee"
	"payrun/internal/modules/iam"
	"payrun/internal/modules/leave"
	"payrun/internal/modules/org"
	"payrun/internal/modules/overtime"
	"payrun/internal/modules/payroll"
	"payrun/internal/modules/tenancy"
	"payrun/internal/platform/apperr"
	"payrun/internal/platform/auth"
	"payrun/internal/platform/clock"
	"payrun/internal/platform/crypto"
	"payrun/internal/platform/httpx"
	"payrun/internal/platform/idempotency"
	"payrun/internal/platform/idgen"
	"payrun/internal/platform/mail"
	"payrun/internal/platform/metrics"
	"payrun/internal/platform/ratelimit"
	"payrun/internal/platform/reqmeta"
	"payrun/internal/platform/storage"
	"payrun/internal/platform/tracing"
)

// Deps adalah dependency yang dibuat oleh main (atau test).
type Deps struct {
	Logger *slog.Logger
	Pool   *pgxpool.Pool
	Tokens *auth.Tokens
	Clock  clock.Clock
	IDs    idgen.Generator

	CookieSecure   bool
	AllowedOrigins []string
	TrustedProxies []netip.Prefix
	PublicBaseURL  string
	Tiers          *ratelimit.Tiers // nil → ratelimit.DefaultTiers()

	IAM    iam.Config
	Mailer mail.Sender // verifikasi email (Mailpit di dev)

	// PII (ADR-P08): keyring enkripsi + kunci blind index (terpisah).
	Keyring    *crypto.Keyring
	BlindIndex *crypto.BlindIndex

	// Storage: object storage payslip PDF (ADR-012). nil = tanpa PDF (unduhan PAYSLIP_NOT_READY).
	Storage storage.Store

	// Mode demo (O15): berpengaruh HANYA di binary build tag `demo` (lihat demo_routes.go).
	DemoMode  bool
	DemoToken string

	DemoSandboxTTL      time.Duration // umur sandbox pengunjung; 0 → 24 jam
	DemoSandboxDailyMax int           // sandbox baru per 24 jam; 0 → 500
}

// ValidateAPI memastikan dependency wajib API terisi. Wiring yang lupa harus menggagalkan boot,
// bukan meledak di tengah request (pelajaran LedgerPay Sprint 6).
func (d Deps) ValidateAPI() error {
	var errs []error
	for name, missing := range map[string]bool{
		"Logger": d.Logger == nil, "Pool": d.Pool == nil, "Tokens": d.Tokens == nil, "Clock": d.Clock == nil, "IDs": d.IDs == nil, "Mailer": d.Mailer == nil,
		"Keyring": d.Keyring == nil, "BlindIndex": d.BlindIndex == nil,
	} {
		if missing {
			errs = append(errs, fmt.Errorf("app: dependency %s is required", name))
		}
	}
	if d.IAM.Argon2.Memory == 0 || d.IAM.RefreshTTL <= 0 || d.IAM.RefreshFamilyTTL < d.IAM.RefreshTTL {
		errs = append(errs, errors.New("app: IAM config (Argon2, RefreshTTL <= RefreshFamilyTTL) is required"))
	}
	return errors.Join(errs...)
}

// APIRouter adalah router API publik, di-mount di /api/v1.
func APIRouter(d Deps) http.Handler {
	r := chi.NewRouter()
	// Recover di DALAM Metrics dan AccessLog: panic diubah menjadi 500 sebelum keduanya mencatat,
	// sehingga request yang panic tetap muncul di log akses dan app_http_requests_total{status="500"}.
	r.Use(httpx.RequestID(d.Logger), tracing.Middleware, httpx.Metrics, httpx.SecurityHeaders,
		httpx.ClientIP(d.TrustedProxies), httpx.AccessLog, httpx.Recover, httpx.BodyLimit(httpx.MaxBodyBytes))
	// Route tidak dikenal dan method salah sama-sama 404 problem+json: tidak membocorkan route mana
	// yang ada (dan konsisten dengan aturan "resource tenant lain = 404").
	r.NotFound(func(w http.ResponseWriter, r *http.Request) { httpx.WriteError(w, r, apperr.ErrNotFound) })
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) { httpx.WriteError(w, r, apperr.ErrNotFound) })
	tiers := ratelimit.DefaultTiers()
	if d.Tiers != nil {
		tiers = *d.Tiers
	}
	limiter := ratelimit.New(d.Clock)
	iamCfg := d.IAM
	if iamCfg.PublicBaseURL == "" {
		iamCfg.PublicBaseURL = d.PublicBaseURL
	}
	iamSvc := iam.NewService(d.Pool, d.Tokens, d.Clock, d.IDs, iamCfg, d.Mailer)
	iamH := iam.NewHandler(iamSvc, iam.HandlerConfig{
		CookieSecure: d.CookieSecure, AllowedOrigins: d.AllowedOrigins, Limiter: limiter, Tiers: tiers,
	})
	tenancySvc := tenancy.NewService(d.Pool, iamSvc, d.Clock, d.IDs)
	iamSvc.OnEmailVerified(tenancySvc.ActivatePendingTenants) // verifikasi email → tenant PENDING menjadi ACTIVE
	tenancyH := tenancy.NewHandler(tenancySvc, tenancy.HandlerConfig{
		Strict:           iamH.Strict,
		StartSession:     sessionStarter(iamSvc),
		SetRefreshCookie: iamH.SetRefreshCookie,
	})

	idem := idempotency.NewService(d.Pool, d.IDs, d.Clock)
	orgSvc := org.NewService(d.Pool, d.IDs)
	orgH := org.NewHandler(orgSvc)
	employeeSvc := employee.NewService(d.Pool, employee.PII{Keyring: d.Keyring, BlindIndex: d.BlindIndex}, d.Clock, d.IDs)
	employeeH := employee.NewHandler(employeeSvc, idem)
	compSvc := compensation.NewService(d.Pool, employeeAccess{employeeSvc}, d.Clock, d.IDs)
	compH := compensation.NewHandler(compSvc, idem)
	payrollSvc := payroll.NewService(d.Pool, d.Clock, d.IDs, iamSvc)
	if d.Storage != nil {
		payrollSvc.SetStorage(d.Storage)
	}
	payrollH := payroll.NewHandler(payrollSvc, idem)
	approvalSvc := approval.NewService(d.Pool, d.IDs)
	approvalSvc.Register(approval.SubjectPayrollRun, payrollSvc) // efek approve/reject run (ADR-P09)
	payrollSvc.SetApprovals(runApprovals{approvalSvc})
	approvalH := approval.NewHandler(approvalSvc)
	approvalSvc.SetMemberships(iamSvc) // approver cuti/lembur: atasan → HR_ADMIN → OWNER (ADR-P09)
	// Input domain (S6): penulis lewat PeriodGuard; approval cuti/lembur lewat mesin approval yang sama.
	attendanceSvc := attendance.NewService(d.Pool, d.Clock, d.IDs, attendanceEmployees{employeeSvc}, tenancySvc, payroll.PeriodGuard{})
	attendanceH := attendance.NewHandler(attendanceSvc)
	leaveSvc := leave.NewService(d.Pool, d.Clock, d.IDs, leaveEmployees{employeeSvc}, tenancySvc, payroll.PeriodGuard{}, leaveApprovals{approvalSvc}, iamSvc)
	leaveH := leave.NewHandler(leaveSvc, idem)
	overtimeSvc := overtime.NewService(d.Pool, d.Clock, d.IDs, overtimeEmployees{employeeSvc}, tenancySvc, payroll.PeriodGuard{}, overtimeApprovals{approvalSvc}, iamSvc)
	overtimeH := overtime.NewHandler(overtimeSvc, idem)
	approvalSvc.Register(approval.SubjectLeave, leaveSvc)
	approvalSvc.Register(approval.SubjectOvertime, overtimeSvc)
	// Ketergantungan lintas modul lewat interface (ADR-001): modul tidak saling meng-import.
	iamSvc.SetEmployeeLinker(employeeSvc) // undangan dengan employee_id → tautan user saat accept
	employeeSvc.Wire(orgSvc, compSvc)     // W2/W7: departemen/posisi aktif; gaji awal
	employeeSvc.SetMemberships(iamSvc)    // W6: role dibaca ulang dari DB (ADR-P02)
	orgSvc.Wire(employeeSvc)              // G4: masih dipakai penempatan hari ini
	// PeriodGuard (S5): setiap penulis input payroll mengunci periode dan mencatat change log (O12, PI-14).
	guard := payroll.PeriodGuard{}
	employeeSvc.SetPeriodGuard(guard)
	compSvc.SetPeriodGuard(guard)
	tenancySvc.SetPeriodGuard(guard)

	byUser := func(r *http.Request) string { p, _ := auth.PrincipalFrom(r.Context()); return p.UserID.String() }
	// Tier heavy dikunci per TENANT: satu tenant tidak bisa membanjiri worker bersama (PF-29).
	byTenant := func(r *http.Request) string { p, _ := auth.PrincipalFrom(r.Context()); return p.TenantID.String() }
	r.Route("/api/v1", func(v1 chi.Router) {
		authed := v1.With(auth.RequireUser(d.Tokens))
		read := authed.With(limiter.Middleware(tiers.Read, byUser))
		write := authed.With(limiter.Middleware(tiers.Write, byUser))
		// Endpoint tenant-scoped: token tanpa `tid` → 403 "Select a tenant first".
		tenantRead := authed.With(auth.RequireTenant, limiter.Middleware(tiers.Read, byUser))
		tenantWrite := authed.With(auth.RequireTenant, limiter.Middleware(tiers.Write, byUser))
		iamH.Routes(v1, read, write)
		iamH.TenantRoutes(tenantRead, tenantWrite)
		tenancyH.Routes(v1.With(limiter.Middleware(tiers.AuthIP, ratelimit.ByIP)), tenantRead, tenantWrite)
		orgH.Routes(tenantRead, tenantWrite)
		employeeH.Routes(tenantRead, tenantWrite, limiter.Middleware(tiers.PIIView, byUser))
		compH.Routes(tenantRead, tenantWrite)
		payrollH.Routes(tenantRead, tenantWrite, limiter.Middleware(tiers.HeavyCalculate, byTenant), limiter.Middleware(tiers.HeavyFinalize, byTenant))
		approvalH.Routes(tenantRead, tenantWrite)
		attendanceH.Routes(tenantRead, tenantWrite)
		leaveH.Routes(tenantRead, tenantWrite)
		overtimeH.Routes(tenantRead, tenantWrite)
		// /api/v1/demo/*: build tag `demo` + DEMO_MODE; no-op di build release. Fault: + X-Demo-Token + OWNER;
		// sandbox pengunjung: publik, rate limit per IP + kuota harian.
		registerDemoRoutes(demoWiring{
			public: v1, authed: read,
			faults: authed.With(auth.RequireTenant, limiter.Middleware(tiers.HeavyCalculate, byTenant)),
			iam:    iamSvc, iamH: iamH, tenancy: tenancySvc, org: orgSvc, employees: employeeSvc,
			payroll: payrollSvc, approvals: approvalSvc, leave: leaveSvc, limiter: limiter, tiers: tiers,
		}, d)
	})
	return r
}

// demoWiring: apa yang dibutuhkan rute demo (demo_routes.go, demo_sandbox.go). Tidak dipakai build release.
type demoWiring struct {
	public, authed, faults chi.Router
	iam                    *iam.Service
	iamH                   *iam.Handler
	tenancy                *tenancy.Service
	org                    *org.Service
	employees              *employee.Service
	payroll                *payroll.Service
	approvals              *approval.Service
	leave                  *leave.Service
	limiter                *ratelimit.Limiter
	tiers                  ratelimit.Tiers
}

// OpsRouter: /healthz, /readyz, dan /metrics di port internal (tidak diekspos Caddy; Step 8 `01` §2).
func OpsRouter(pool *pgxpool.Pool) http.Handler {
	r := chi.NewRouter()
	// Liveness: proses hidup. Sengaja TIDAK memeriksa DB: kalau DB mati, me-restart API tidak menolong.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// Readiness: boleh menerima trafik? DB terjangkau dan skema cukup baru untuk binary ini
	// (versi DB >= minimum binary, bukan "=", supaya rollback app N-1 tetap ready).
	r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		checks := map[string]string{}
		ready := true
		v, err := migrations.SchemaVersion(ctx, pool)
		switch {
		case err != nil:
			checks["database"] = "unreachable"
			ready = false
		case v < migrations.MinSchemaVersion:
			checks["database"] = "ok"
			checks["schema"] = "too old"
			ready = false
		default:
			checks["database"] = "ok"
			checks["schema"] = "ok"
		}
		status, state := http.StatusOK, "ready"
		if !ready {
			status, state = http.StatusServiceUnavailable, "not_ready"
		}
		metrics.Ready.WithLabelValues("api").Set(boolFloat(ready))
		httpx.WriteJSON(w, status, map[string]any{"status": state, "checks": checks, "schema_version": v, "min_schema_version": migrations.MinSchemaVersion})
	})
	r.Handle("/metrics", metrics.Handler())
	return r
}

func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// sessionStarter mengadaptasi iam.StartSession untuk respons signup tenancy (tenancy tidak mengenal
// tipe iam; ADR-001).
func sessionStarter(svc *iam.Service) func(ctx context.Context, userID, tenantID uuid.UUID, meta reqmeta.Meta) (map[string]any, string, time.Duration, error) {
	return func(ctx context.Context, userID, tenantID uuid.UUID, meta reqmeta.Meta) (map[string]any, string, time.Duration, error) {
		sess, err := svc.StartSession(ctx, userID, tenantID, "auth.login_succeeded", meta)
		if err != nil {
			return nil, "", 0, err
		}
		r := iam.NewSessionResponse(sess, true)
		return map[string]any{"access_token": r.AccessToken, "token_type": r.TokenType, "expires_in": r.ExpiresIn,
			"user": r.User, "active_tenant": r.ActiveTenant}, sess.RefreshToken, sess.RefreshMaxAge, nil
	}
}

// runApprovals mengadaptasi approval.Service ke payroll.Approvals (payroll tidak mengenal tipe approval).
type runApprovals struct{ svc *approval.Service }

func (a runApprovals) CreateRunApprovalTx(ctx context.Context, tx pgx.Tx, runID, requester, approver uuid.UUID, self bool, reason *string, meta reqmeta.Meta) (uuid.UUID, error) {
	return a.svc.CreateTx(ctx, tx, approval.NewRequest{SubjectType: approval.SubjectPayrollRun, SubjectID: runID,
		Requester: requester, Approver: approver, SelfApproved: self, SelfApprovalReason: reason}, meta)
}

// Adapter employee.Self → tipe lokal tiap modul input (modul tidak saling meng-import, ADR-001).
type attendanceEmployees struct{ *employee.Service }

func (a attendanceEmployees) SelfTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, on time.Time) (attendance.EmployeeSelf, bool, error) {
	me, ok, err := a.Service.SelfTx(ctx, tx, userID, on)
	return attendance.EmployeeSelf{EmployeeID: me.EmployeeID, Active: me.ActiveOn}, ok, err
}

type leaveEmployees struct{ *employee.Service }

func (a leaveEmployees) SelfTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, on time.Time) (leave.EmployeeSelf, bool, error) {
	me, ok, err := a.Service.SelfTx(ctx, tx, userID, on)
	return leave.EmployeeSelf{EmployeeID: me.EmployeeID, ManagerUserID: me.ManagerUserID, Active: me.ActiveOn}, ok, err
}

type overtimeEmployees struct{ *employee.Service }

func (a overtimeEmployees) SelfTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, on time.Time) (overtime.EmployeeSelf, bool, error) {
	me, ok, err := a.Service.SelfTx(ctx, tx, userID, on)
	return overtime.EmployeeSelf{EmployeeID: me.EmployeeID, ManagerUserID: me.ManagerUserID, Active: me.ActiveOn}, ok, err
}

// Adapter approval.Service → kebutuhan modul leave/overtime.
type leaveApprovals struct{ *approval.Service }

func (a leaveApprovals) BriefsTx(ctx context.Context, tx pgx.Tx, subjectType string, ids []uuid.UUID) (map[uuid.UUID]leave.ApprovalBrief, error) {
	bs, err := a.Service.BriefsTx(ctx, tx, subjectType, ids)
	out := make(map[uuid.UUID]leave.ApprovalBrief, len(bs))
	for id, b := range bs {
		v := leave.ApprovalBrief{ID: b.ID, Status: b.Status}
		v.Approver.ID, v.Approver.DisplayName = b.Approver.ID, b.Approver.DisplayName
		out[id] = v
	}
	return out, err
}

type overtimeApprovals struct{ *approval.Service }

func (a overtimeApprovals) BriefsTx(ctx context.Context, tx pgx.Tx, subjectType string, ids []uuid.UUID) (map[uuid.UUID]overtime.ApprovalBrief, error) {
	bs, err := a.Service.BriefsTx(ctx, tx, subjectType, ids)
	out := make(map[uuid.UUID]overtime.ApprovalBrief, len(bs))
	for id, b := range bs {
		v := overtime.ApprovalBrief{ID: b.ID, Status: b.Status}
		v.Approver.ID, v.Approver.DisplayName = b.Approver.ID, b.Approver.DisplayName
		out[id] = v
	}
	return out, err
}

// employeeAccess mengadaptasi employee.Service.AccessTx ke interface compensation.Employees.
type employeeAccess struct{ svc *employee.Service }

func (a employeeAccess) AccessTx(ctx context.Context, tx pgx.Tx, p auth.Principal, id uuid.UUID, write bool) (time.Time, error) {
	mode := employee.AccessSalaryRead
	if write {
		mode = employee.AccessWrite
	}
	return a.svc.AccessTx(ctx, tx, p, id, mode)
}
