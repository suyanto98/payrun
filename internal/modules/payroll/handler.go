package payroll

import (
	"context"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payrun/internal/platform/apperr"
	"payrun/internal/platform/auth"
	"payrun/internal/platform/httpx"
	"payrun/internal/platform/idempotency"
	"payrun/internal/platform/reqmeta"
)

type Handler struct {
	svc  *Service
	idem *idempotency.Service
}

func NewHandler(svc *Service, idem *idempotency.Service) *Handler {
	return &Handler{svc: svc, idem: idem}
}

// Otorisasi (file 17 §2.4, §3.1 pemisahan tugas): run dijalankan OWNER/PAYROLL_ADMIN; HR_ADMIN hanya
// membaca (ter-audit). Bagi MANAGER/EMPLOYEE resource run tidak terlihat → 404; koleksi → 403.
var (
	runners = []string{auth.RoleOwner, auth.RolePayroll}
	readers = []string{auth.RoleOwner, auth.RolePayroll, auth.RoleHRAdmin}
)

// hidden: resource run tidak terlihat bagi role selain pembaca → 404 (bukan 403; file 17 §1.3).
func hidden(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, _ := auth.PrincipalFrom(r.Context()); !p.HasAnyRole(readers...) {
			httpx.WriteError(w, r, apperr.ErrNotFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Routes: heavy = rate limit tier heavy-calculate per tenant (Y5, 16-api). Dipasang SETELAH cek role,
// supaya request yang ditolak (MANAGER/EMPLOYEE) tidak menghabiskan kuota kalkulasi tenant.
func (h *Handler) Routes(read, write chi.Router, heavy func(http.Handler) http.Handler) {
	read.With(auth.RequireAnyRole(readers...)).Get("/payroll-runs", h.list)
	write.With(auth.RequireAnyRole(runners...)).Post("/payroll-runs", h.create)
	read.With(hidden).Get("/payroll-runs/{id}", h.get)
	write.With(hidden, auth.RequireAnyRole(runners...), heavy).Post("/payroll-runs/{id}/calculate", h.calculate)
	read.With(hidden).Get("/payroll-runs/{id}/items", h.items)
	read.With(hidden).Get("/payroll-runs/{id}/items/{item_id}", h.item)
	write.With(hidden, auth.RequireAnyRole(runners...)).Post("/payroll-runs/{id}/items/{item_id}/exclude", h.exclude)
	write.With(hidden, auth.RequireAnyRole(runners...)).Post("/payroll-runs/{id}/cancel", h.cancel)
	write.With(hidden, auth.RequireAnyRole(runners...)).Post("/payroll-runs/{id}/submit", h.submit)
}

// submit (Y10): approver dipilih server; idempoten secara natural (guard CALCULATED).
func (h *Handler) submit(w http.ResponseWriter, r *http.Request) {
	id, err := ids(r, "id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var in SubmitInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	run, err := h.svc.Submit(r.Context(), p.UserID, id[0], in, reqmeta.From(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, run)
}

func ids(r *http.Request, names ...string) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, len(names))
	for i, n := range names {
		id, err := httpx.ParseID(chi.URLParam(r, n))
		if err != nil {
			return nil, err
		}
		out[i] = id
	}
	return out, nil
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	if err := httpx.OnlyQuery(r, "period", "status", "limit", "cursor"); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	limit, err := httpx.Limit(r, 20, 100)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	q := r.URL.Query()
	runs, page, err := h.svc.ListRuns(r.Context(), q.Get("period"), q.Get("status"), q.Get("cursor"), limit)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": runs, "page": page})
}

// create (Y2): Idempotency-Key wajib; klik ganda = replay 201 (PFS-06).
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	req, err := idempotency.FromHTTP(r, p.UserID, "POST /payroll-runs")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var in struct {
		Period string `json:"period"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	meta := reqmeta.From(r)
	resp, err := h.idem.Execute(r, req, func(ctx context.Context, tx pgx.Tx) (idempotency.Outcome, error) {
		run, err := h.svc.CreateRunTx(ctx, tx, p.UserID, in.Period, meta)
		if err != nil {
			return idempotency.Outcome{}, err
		}
		return idempotency.Outcome{Status: http.StatusCreated, Body: run, ResourceType: "payroll_run", ResourceID: &run.ID,
			Header: map[string]string{"Location": "/api/v1/payroll-runs/" + run.ID.String()}}, nil
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	idempotency.Write(w, resp)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := ids(r, "id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	run, err := h.svc.GetRun(r.Context(), id[0])
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, run)
}

// calculate (Y5): 202; Idempotency-Key wajib, replay tidak menaikkan calc_version lagi.
func (h *Handler) calculate(w http.ResponseWriter, r *http.Request) {
	id, err := ids(r, "id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	req, err := idempotency.FromHTTP(r, p.UserID, "POST /payroll-runs/{id}/calculate")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	meta := reqmeta.From(r)
	resp, err := h.idem.Execute(r, req, func(ctx context.Context, tx pgx.Tx) (idempotency.Outcome, error) {
		run, err := h.svc.CalculateTx(ctx, tx, p.UserID, id[0], meta)
		if err != nil {
			return idempotency.Outcome{}, err
		}
		return idempotency.Outcome{Status: http.StatusAccepted, Body: run, ResourceType: "payroll_run", ResourceID: &run.ID,
			Header: map[string]string{"Location": "/api/v1/payroll-runs/" + run.ID.String()}}, nil
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	idempotency.Write(w, resp)
}

func (h *Handler) items(w http.ResponseWriter, r *http.Request) {
	id, err := ids(r, "id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if err := httpx.OnlyQuery(r, "status", "department_id", "has_warning", "limit", "cursor"); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	limit, err := httpx.Limit(r, 50, 200)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	q := r.URL.Query()
	hasWarning, _ := strconv.ParseBool(q.Get("has_warning"))
	p, _ := auth.PrincipalFrom(r.Context())
	items, page, err := h.svc.ListItems(r.Context(), p.UserID, id[0], ItemQuery{Status: q.Get("status"), DepartmentID: q.Get("department_id"),
		Cursor: q.Get("cursor"), HasWarning: hasWarning, Limit: limit}, reqmeta.From(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": items, "page": page})
}

func (h *Handler) item(w http.ResponseWriter, r *http.Request) {
	id, err := ids(r, "id", "item_id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	// ᴬ (file 17): HR_ADMIN tanpa role runner membaca hasil payroll → setiap akses detail diaudit
	// (diputuskan service dari role di DB, bukan token).
	it, err := h.svc.GetItem(r.Context(), p.UserID, id[0], id[1], reqmeta.From(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, it)
}

func (h *Handler) exclude(w http.ResponseWriter, r *http.Request) {
	id, err := ids(r, "id", "item_id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	it, err := h.svc.Exclude(r.Context(), p.UserID, id[0], id[1], in.Reason, reqmeta.From(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, it)
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	id, err := ids(r, "id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	run, err := h.svc.Cancel(r.Context(), p.UserID, id[0], in.Reason, reqmeta.From(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, run)
}
