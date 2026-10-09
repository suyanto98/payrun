package attendance

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"payrun/internal/platform/auth"
	"payrun/internal/platform/httpx"
	"payrun/internal/platform/reqmeta"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Routes: D1/D2 untuk diri sendiri (role apa pun yang tertaut ke employee, file 17 👤); D3 sesuai scope;
// D4 OWNER/HR_ADMIN (ditegakkan service: 403 bila terlihat, 404 bila tidak).
func (h *Handler) Routes(read, write chi.Router) {
	write.Post("/attendance/clock-in", h.clockIn)
	write.Post("/attendance/clock-out", h.clockOut)
	read.Get("/attendance", h.list)
	write.Patch("/attendance/{id}", h.correct)
}

func (h *Handler) clockIn(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	rec, created, err := h.svc.ClockIn(r.Context(), p, reqmeta.From(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpx.SetETag(w, rec.Version)
	httpx.WriteJSON(w, status, rec)
}

func (h *Handler) clockOut(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	rec, err := h.svc.ClockOut(r.Context(), p, reqmeta.From(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.SetETag(w, rec.Version)
	httpx.WriteJSON(w, http.StatusOK, rec)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	if err := httpx.OnlyQuery(r, "employee_id", "period", "from", "to", "date", "limit", "cursor"); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	limit, err := httpx.Limit(r, 50, 200)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	q := r.URL.Query()
	p, _ := auth.PrincipalFrom(r.Context())
	recs, page, sum, err := h.svc.List(r.Context(), p, ListQuery{EmployeeID: q.Get("employee_id"), Period: q.Get("period"),
		From: q.Get("from"), To: q.Get("to"), Date: q.Get("date"), Cursor: q.Get("cursor"), Limit: limit})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	body := map[string]any{"data": recs, "page": page}
	if sum != nil {
		body["summary"] = sum
	}
	httpx.WriteJSON(w, http.StatusOK, body)
}

func (h *Handler) correct(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	v, perr := httpx.IfMatchVersion(r)
	var in Correction
	if err := httpx.Decode(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	rec, err := h.svc.Correct(r.Context(), p, id, Precondition{Version: v, Err: perr}, in, reqmeta.From(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.SetETag(w, rec.Version)
	httpx.WriteJSON(w, http.StatusOK, rec)
}
