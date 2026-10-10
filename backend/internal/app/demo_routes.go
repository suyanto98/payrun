//go:build demo

package app

import (
	"crypto/subtle"
	"net/http"

	"github.com/go-chi/chi/v5"

	"payrun/internal/platform/apperr"
	"payrun/internal/platform/auth"
	"payrun/internal/platform/faults"
	"payrun/internal/platform/httpx"
)

// demoPoints: titik fault yang boleh dipasang (sama dengan ck_demo_faults_point).
var demoPoints = map[string]bool{faults.PointChunkBeforeCommit: true, faults.PointPayslipAfterPut: true}

// registerDemoRoutes (build tag `demo`): /api/v1/demo/faults hanya kalau DEMO_MODE=true dan token ada
// (lapis "runtime"), setiap request wajib X-Demo-Token yang cocok (lapis "request"), dan hanya OWNER
// tenant aktif. Token salah → 404, sama dengan rute yang tidak ada. Fault berlaku untuk tenant pemanggil
// saja (kolom tenant_id), jadi presenter tidak bisa mematikan pekerjaan tenant lain.
func registerDemoRoutes(tenant chi.Router, d Deps) {
	if !d.DemoMode || d.DemoToken == "" {
		return
	}
	guard := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Demo-Token")), []byte(d.DemoToken)) != 1 {
				httpx.WriteError(w, r, apperr.ErrNotFound)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	demo := tenant.With(guard, auth.RequireAnyRole(auth.RoleOwner))
	demo.Post("/demo/faults", func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.PrincipalFrom(r.Context())
		var in struct {
			Point  string `json:"point"`
			AfterN int    `json:"after_n"`
			Times  int    `json:"times"`
			Action string `json:"action"`
		}
		if err := httpx.Decode(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		if in.Times == 0 {
			in.Times = 1
		}
		if in.Action == "" {
			in.Action = "exit"
		}
		if !demoPoints[in.Point] || in.Times < 1 || in.Times > 10 || in.AfterN < 0 || in.AfterN > 100 || (in.Action != "exit" && in.Action != "panic") {
			httpx.WriteError(w, r, apperr.Validation(apperr.FieldError{Field: "point", Code: "INVALID_VALUE",
				Message: "payroll.calc_chunk.before_commit | payslip.render.after_put; after_n 0–100; times 1–10; action exit|panic"}))
			return
		}
		id := d.IDs.New()
		if _, err := d.Pool.Exec(r.Context(), `INSERT INTO demo_faults (id, point, tenant_id, after_n, times, action, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, id, in.Point, p.TenantID, in.AfterN, in.Times, in.Action, p.UserID); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		d.Logger.Warn("demo.fault_armed", "point", in.Point, "after_n", in.AfterN, "times", in.Times, "tenant_id", p.TenantID.String())
		httpx.WriteJSON(w, http.StatusCreated, map[string]any{"id": id, "point": in.Point, "after_n": in.AfterN, "times": in.Times, "action": in.Action})
	})
	demo.Delete("/demo/faults", func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.PrincipalFrom(r.Context())
		if _, err := d.Pool.Exec(r.Context(), `DELETE FROM demo_faults WHERE tenant_id = $1`, p.TenantID); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
