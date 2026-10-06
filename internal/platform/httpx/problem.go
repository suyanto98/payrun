package httpx

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"payrun/internal/platform/apperr"
	"payrun/internal/platform/metrics"
	"payrun/internal/platform/observability"
)

// Problem adalah body RFC 9457 (docs/design/14-api-conventions.md §4).
type Problem struct {
	Type      string              `json:"type"`
	Title     string              `json:"title"`
	Status    int                 `json:"status"`
	Code      string              `json:"code"`
	Detail    string              `json:"detail,omitempty"`
	Instance  string              `json:"instance,omitempty"`
	RequestID string              `json:"request_id"`
	Errors    []apperr.FieldError `json:"errors,omitempty"`
	// ID resource yang sudah ada (ACTIVE_RUN_EXISTS, IMPORT_DUPLICATE), supaya client bisa melanjutkan.
	ExistingResourceID string `json:"existing_resource_id,omitempty"`
}

const problemTypeBase = "https://payrun.dev/errors/"

// WriteError mengubah error apa pun menjadi respons problem+json.
// Error tak dikenal → 500 INTERNAL_ERROR dengan penyebab hanya di log (tidak bocor ke client).
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	p := NewProblem(r, err)
	metrics.Errors.WithLabelValues(p.Code).Inc()
	if p.Status >= 500 {
		observability.Logger(r.Context()).Error("request.failed", "code", p.Code, "err", err.Error())
	}
	if e := apperr.As(err); e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	w.Header().Set("Content-Type", ProblemContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

// ProblemContentType adalah media type respons error (RFC 9457).
const ProblemContentType = "application/problem+json"

// NewProblem membangun body problem untuk error, tanpa menulisnya. Dipakai juga oleh idempotency
// untuk menyimpan penolakan domain (422) agar bisa di-replay persis.
func NewProblem(r *http.Request, err error) Problem {
	e := apperr.As(err)
	return Problem{
		Type:      problemTypeBase + strings.ToLower(strings.ReplaceAll(e.Code, "_", "-")),
		Title:     e.Title,
		Status:    e.Status,
		Code:      e.Code,
		Detail:    e.Detail,
		Instance:  r.URL.Path,
		RequestID: RequestIDFrom(r.Context()),
		Errors:    e.Fields,

		ExistingResourceID: e.ExistingResourceID,
	}
}

// WriteJSON menulis respons JSON sukses. Semua respons API memakai no-store (data finansial; ADR-008).
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}
