package employee

import (
	"context"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
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

// Routes (file 17 §2.2): W1 O/H/P + M (bawahan), E → 403 (di service, karena M butuh logika scope);
// W2 O/H; W3 semua role dengan scope; W4/W5 O/H, role lain 403/404 tergantung visibilitas (service).
func (h *Handler) Routes(read, write chi.Router) {
	read.Get("/employees", h.list)
	write.With(auth.RequireAnyRole(auth.RoleOwner, auth.RoleHRAdmin)).Post("/employees", h.create)
	read.Get("/employees/{id}", h.get)
	write.Patch("/employees/{id}", h.update)
	write.Post("/employees/{id}/terminate", h.terminate)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	if err := httpx.OnlyQuery(r, "status", "q", "limit", "cursor"); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	limit, err := httpx.Limit(r, 20, 100)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	qs := r.URL.Query()
	p, _ := auth.PrincipalFrom(r.Context())
	items, page, err := h.svc.List(r.Context(), p, ListQuery{Status: qs.Get("status"), Q: qs.Get("q"), Cursor: qs.Get("cursor"), Limit: limit})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": items, "page": page})
}

// create (W2): Idempotency-Key wajib; key + efek di satu transaksi tenant (ADR-L02 pola a).
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	req, err := idempotency.FromHTTP(r, p.UserID, "POST /employees")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var in CreateInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	meta := reqmeta.From(r)
	resp, err := h.idem.Execute(r, req, func(ctx context.Context, tx pgx.Tx) (idempotency.Outcome, error) {
		e, err := h.svc.CreateTx(ctx, tx, p.UserID, in, meta)
		if err != nil {
			return idempotency.Outcome{}, err
		}
		id := e.ID
		return idempotency.Outcome{Status: http.StatusCreated, Body: e, ResourceType: "employee", ResourceID: &id,
			Header: map[string]string{"ETag": strconv.Quote(strconv.Itoa(int(e.Version))), "Location": "/api/v1/employees/" + id.String()}}, nil
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	idempotency.Write(w, resp)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	e, version, err := h.svc.Get(r.Context(), p, id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.SetETag(w, version)
	httpx.WriteJSON(w, http.StatusOK, e)
}

// idAndVersion: {id} (tidak valid → 404) dan If-Match. Error If-Match (428/412) TIDAK langsung dikirim:
// service mengembalikannya setelah otorisasi, sehingga role tanpa hak tetap mendapat 403/404 (file 17).
func idAndVersion(r *http.Request) (uuid.UUID, Precondition, error) {
	id, err := httpx.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, Precondition{}, err
	}
	v, perr := httpx.IfMatchVersion(r)
	return id, Precondition{Version: v, Err: perr}, nil
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	id, pre, err := idAndVersion(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var in UpdateInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	e, err := h.svc.Update(r.Context(), p, id, pre, in, reqmeta.From(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.SetETag(w, e.Version)
	httpx.WriteJSON(w, http.StatusOK, e)
}

func (h *Handler) terminate(w http.ResponseWriter, r *http.Request) {
	id, pre, err := idAndVersion(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var in TerminateInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	e, err := h.svc.Terminate(r.Context(), p, id, pre, in, reqmeta.From(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.SetETag(w, e.Version)
	httpx.WriteJSON(w, http.StatusOK, e)
}
