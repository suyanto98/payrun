package leave

import (
	"context"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
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

// Routes (file 17 §2.3): L1 semua role; L2–L4 untuk diri sendiri (👤); L5 sesuai scope.
func (h *Handler) Routes(read, write chi.Router) {
	read.Get("/leave-types", h.types)
	read.Get("/me/leave-balances", h.balances)
	write.Post("/leave-requests", h.submit)
	write.Post("/leave-requests/{id}/cancel", h.cancel)
	read.Get("/leave-requests", h.list)
}

func (h *Handler) types(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.ListTypes(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (h *Handler) balances(w http.ResponseWriter, r *http.Request) {
	if err := httpx.OnlyQuery(r, "year"); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	year := 0 // default: tahun berjalan (jam service)
	if v := r.URL.Query().Get("year"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			httpx.WriteError(w, r, apperr.Validation(apperr.FieldError{Field: "year", Code: "FORMAT"}))
			return
		}
		year = n
	}
	p, _ := auth.PrincipalFrom(r.Context())
	out, err := h.svc.MyBalances(r.Context(), p, year)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": out})
}

// submit (L3): Idempotency-Key wajib.
func (h *Handler) submit(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	req, err := idempotency.FromHTTP(r, p.UserID, "POST /leave-requests")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var in SubmitInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	meta := reqmeta.From(r)
	resp, err := h.idem.Execute(r, req, func(ctx context.Context, tx pgx.Tx) (idempotency.Outcome, error) {
		out, err := h.svc.SubmitTx(ctx, tx, p, in, meta)
		if err != nil {
			return idempotency.Outcome{}, err
		}
		return idempotency.Outcome{Status: http.StatusCreated, Body: out, ResourceType: "leave_request", ResourceID: &out.ID,
			Header: map[string]string{"Location": "/api/v1/leave-requests/" + out.ID.String()}}, nil
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	idempotency.Write(w, resp)
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	out, err := h.svc.Cancel(r.Context(), p, id, reqmeta.From(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	if err := httpx.OnlyQuery(r, "status", "employee_id", "from", "to", "limit", "cursor"); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	limit, err := httpx.Limit(r, 20, 100)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	q := r.URL.Query()
	p, _ := auth.PrincipalFrom(r.Context())
	out, page, err := h.svc.List(r.Context(), p, ListQuery{Status: q.Get("status"), EmployeeID: q.Get("employee_id"),
		From: q.Get("from"), To: q.Get("to"), Cursor: q.Get("cursor"), Limit: limit})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": out, "page": page})
}
