package overtime

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

// Routes (file 17 §2.3): V1/V2 untuk diri sendiri (👤); V3 sesuai scope.
func (h *Handler) Routes(read, write chi.Router) {
	write.Post("/overtime-requests", h.submit)
	write.Post("/overtime-requests/{id}/cancel", h.cancel)
	read.Get("/overtime-requests", h.list)
}

// submit (V1): Idempotency-Key wajib.
func (h *Handler) submit(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	req, err := idempotency.FromHTTP(r, p.UserID, "POST /overtime-requests")
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
		return idempotency.Outcome{Status: http.StatusCreated, Body: out, ResourceType: "overtime_request", ResourceID: &out.ID,
			Header: map[string]string{"Location": "/api/v1/overtime-requests/" + out.ID.String()}}, nil
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
