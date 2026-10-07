package compensation

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

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

// Routes (file 17 §2.2): C1 O/H/P; C2–C4 O/H. W8 O/H/P + E self (service); W9/W10 O/H, role lain
// 403/404 menurut visibilitas employee (service, lewat modul employee).
func (h *Handler) Routes(read, write chi.Router) {
	read.With(auth.RequireAnyRole(auth.RoleOwner, auth.RoleHRAdmin, auth.RolePayroll)).Get("/component-definitions", h.listDefs)
	admin := write.With(auth.RequireAnyRole(auth.RoleOwner, auth.RoleHRAdmin))
	admin.Post("/component-definitions", h.createDef)
	admin.Patch("/component-definitions/{id}", h.updateDef)
	admin.Post("/component-definitions/{id}/deactivate", h.deactivateDef)

	read.Get("/employees/{id}/salary-history", h.history)
	write.Post("/employees/{id}/salary-changes", h.salaryChange)
	write.Post("/employees/{id}/component-assignments", h.assignComponent)
}

func (h *Handler) listDefs(w http.ResponseWriter, r *http.Request) {
	if err := httpx.OnlyQuery(r, "status", "category"); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	defs, err := h.svc.ListDefinitions(r.Context(), r.URL.Query().Get("status"), r.URL.Query().Get("category"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": defs})
}

func (h *Handler) createDef(w http.ResponseWriter, r *http.Request) {
	var in DefinitionInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	d, err := h.svc.CreateDefinition(r.Context(), p.UserID, in, reqmeta.From(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.SetETag(w, d.Version)
	httpx.WriteJSON(w, http.StatusCreated, d)
}

func (h *Handler) updateDef(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	version, err := httpx.IfMatchVersion(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var in DefinitionUpdate
	if err := httpx.Decode(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	d, err := h.svc.UpdateDefinition(r.Context(), p.UserID, id, version, in, reqmeta.From(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.SetETag(w, d.Version)
	httpx.WriteJSON(w, http.StatusOK, d)
}

func (h *Handler) deactivateDef(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	d, err := h.svc.DeactivateDefinition(r.Context(), p.UserID, id, reqmeta.From(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.SetETag(w, d.Version)
	httpx.WriteJSON(w, http.StatusOK, d)
}

func (h *Handler) history(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if err := httpx.OnlyQuery(r, "as_of"); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	hist, err := h.svc.History(r.Context(), p, id, r.URL.Query().Get("as_of"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, hist)
}

// idempotentWrite: W9/W10 wajib Idempotency-Key; key + efek di satu transaksi tenant (ADR-L02 pola a).
func (h *Handler) idempotentWrite(w http.ResponseWriter, r *http.Request, endpoint string, body any,
	fn func(ctx context.Context, tx pgx.Tx, p auth.Principal) (any, string, error)) {
	if _, err := httpx.ParseID(chi.URLParam(r, "id")); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	req, err := idempotency.FromHTTP(r, p.UserID, endpoint)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if err := httpx.Decode(r, body); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	resp, err := h.idem.Execute(r, req, func(ctx context.Context, tx pgx.Tx) (idempotency.Outcome, error) {
		out, resourceType, err := fn(ctx, tx, p)
		if err != nil {
			return idempotency.Outcome{}, err
		}
		return idempotency.Outcome{Status: http.StatusCreated, Body: out, ResourceType: resourceType}, nil
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	idempotency.Write(w, resp)
}

func (h *Handler) salaryChange(w http.ResponseWriter, r *http.Request) {
	var in SalaryChange
	meta := reqmeta.From(r)
	h.idempotentWrite(w, r, "POST /employees/{id}/salary-changes", &in, func(ctx context.Context, tx pgx.Tx, p auth.Principal) (any, string, error) {
		id, _ := httpx.ParseID(chi.URLParam(r, "id"))
		out, err := h.svc.ChangeSalaryTx(ctx, tx, p, id, in, meta)
		return out, "salary_assignment", err
	})
}

func (h *Handler) assignComponent(w http.ResponseWriter, r *http.Request) {
	var in ComponentAssignmentInput
	meta := reqmeta.From(r)
	h.idempotentWrite(w, r, "POST /employees/{id}/component-assignments", &in, func(ctx context.Context, tx pgx.Tx, p auth.Principal) (any, string, error) {
		id, _ := httpx.ParseID(chi.URLParam(r, "id"))
		out, err := h.svc.AssignComponentTx(ctx, tx, p, id, in, meta)
		return out, "employee_component", err
	})
}
