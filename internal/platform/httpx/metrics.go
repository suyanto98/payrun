package httpx

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"payrun/internal/platform/metrics"
)

// Metrics mencatat app_http_* per ROUTE TEMPLATE ("POST /v1/transfers/{id}"), bukan path konkret:
// path dengan ID akan membuat satu seri metrik per resource (kardinalitas meledak).
func Metrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		metrics.HTTPInFlight.WithLabelValues().Inc()
		defer metrics.HTTPInFlight.WithLabelValues().Dec()
		start := time.Now() //nolint:forbidigo // durasi teknis
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		route := "unmatched"
		if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
			route = strings.TrimPrefix(rc.RoutePattern(), "/api")
		}
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		// Nama span HTTP juga route template (ditentukan SETELAH routing), bukan path berisi ID.
		span := trace.SpanFromContext(r.Context())
		span.SetName(r.Method + " " + route)
		span.SetAttributes(attribute.String("http.route", route))
		metrics.HTTPRequests.WithLabelValues(route, r.Method, strconv.Itoa(status)).Inc()
		metrics.HTTPDuration.WithLabelValues(route, r.Method).Observe(metrics.Since(start))
	})
}
