package approval

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"payrun/internal/platform/auth"
	"payrun/internal/platform/httpx"
	"payrun/internal/platform/reqmeta"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Routes: setiap anggota tenant; otorisasi per approval (approver yang di-snapshot, file 17 §2.3).
func (h *Handler) Routes(read, write chi.Router) {
	read.Get("/approvals/inbox", h.inbox)
	write.Post("/approvals/{id}/approve", h.approve)
	write.Post("/approvals/{id}/reject", h.reject)
}

func (h *Handler) inbox(w http.ResponseWriter, r *http.Request) {
	if err := httpx.OnlyQuery(r, "status", "subject_type", "scope", "limit", "cursor"); err != nil {
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
	out, page, err := h.svc.Inbox(r.Context(), p, InboxQuery{Status: q.Get("status"), SubjectType: q.Get("subject_type"),
		Scope: q.Get("scope"), Cursor: q.Get("cursor"), Limit: limit})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": out, "page": page})
}

func (h *Handler) approve(w http.ResponseWriter, r *http.Request) { h.decide(w, r, true) }
func (h *Handler) reject(w http.ResponseWriter, r *http.Request)  { h.decide(w, r, false) }

// decide (R2/R3): idempoten secara natural (guard PENDING); keputusan kedua → 409.
func (h *Handler) decide(w http.ResponseWriter, r *http.Request, approve bool) {
	id, err := httpx.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var in struct {
		Note string `json:"note"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	var out Decision
	if approve {
		out, err = h.svc.Approve(r.Context(), p, id, in.Note, reqmeta.From(r))
	} else {
		out, err = h.svc.Reject(r.Context(), p, id, in.Note, reqmeta.From(r))
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
