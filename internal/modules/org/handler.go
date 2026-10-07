package org

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"payrun/internal/platform/auth"
	"payrun/internal/platform/httpx"
	"payrun/internal/platform/reqmeta"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Routes: G1 baca untuk semua role; G2–G4 hanya OWNER/HR_ADMIN (file 17 §2.2), role lain 403.
// read/write sudah melewati RequireUser + RequireTenant + rate tier.
func (h *Handler) Routes(read, write chi.Router) {
	admin := write.With(auth.RequireAnyRole(auth.RoleOwner, auth.RoleHRAdmin))
	for _, k := range []struct {
		path string
		kind Kind
	}{{"/departments", Departments}, {"/positions", Positions}} {
		read.Get(k.path, h.list(k.kind))
		admin.Post(k.path, h.create(k.kind))
		admin.Patch(k.path+"/{id}", h.update(k.kind))
		admin.Post(k.path+"/{id}/deactivate", h.deactivate(k.kind))
	}
}

func (h *Handler) list(k Kind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := httpx.OnlyQuery(r, "status"); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		units, err := h.svc.List(r.Context(), k, r.URL.Query().Get("status"))
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": units})
	}
}

func (h *Handler) create(k Kind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Code string `json:"code"`
			Name string `json:"name"`
		}
		if err := httpx.Decode(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		u, err := h.svc.Create(r.Context(), k, p.UserID, in.Code, in.Name, reqmeta.From(r))
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.SetETag(w, u.Version)
		httpx.WriteJSON(w, http.StatusCreated, u)
	}
}

func (h *Handler) update(k Kind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		var in struct {
			Name string `json:"name"`
		}
		if err := httpx.Decode(r, &in); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		u, err := h.svc.Update(r.Context(), k, p.UserID, id, version, in.Name, reqmeta.From(r))
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.SetETag(w, u.Version)
		httpx.WriteJSON(w, http.StatusOK, u)
	}
}

func (h *Handler) deactivate(k Kind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := httpx.ParseID(chi.URLParam(r, "id"))
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		p, _ := auth.PrincipalFrom(r.Context())
		u, err := h.svc.Deactivate(r.Context(), k, p.UserID, id, reqmeta.From(r))
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		httpx.SetETag(w, u.Version)
		httpx.WriteJSON(w, http.StatusOK, u)
	}
}
