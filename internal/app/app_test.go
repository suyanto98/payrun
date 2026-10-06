package app

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// Panic di handler harus tetap tercatat di log akses (dan metrik): Recover wajib berada di dalam
// AccessLog/Metrics. Kalau urutan middleware dibalik, panic melewati keduanya tanpa jejak.
func TestAPIRouter_PanicIsAccessLogged(t *testing.T) {
	var buf bytes.Buffer
	h := APIRouter(Deps{Logger: slog.New(slog.NewJSONHandler(&buf, nil))})
	h.(*chi.Mux).Get("/boom", func(http.ResponseWriter, *http.Request) { panic("boom") })

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(buf.String(), `"msg":"http.request"`) || !strings.Contains(buf.String(), `"status":500`) {
		t.Fatalf("panicking request missing from access log:\n%s", buf.String())
	}
}
