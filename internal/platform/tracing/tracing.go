// Package tracing: OpenTelemetry (docs/design/21-observability.md §5).
//
//   - HTTP server: span per route template (otelhttp), atribut tanpa PII;
//   - pgx: span per query (nama query sqlc, TANPA nilai parameter);
//   - HTTP keluar (object storage): otelhttp.Transport;
//   - job: traceparent producer disimpan di jobs.trace_parent; worker membuat span ROOT baru
//     dengan LINK ke span producer (job bisa berjalan menit/jam kemudian; trace berjam-jam sulit dibaca).
//
// Exporter OTLP non-blocking (batch processor): Jaeger yang mati tidak memperlambat request.
// Tanpa OTEL_EXPORTER_OTLP_ENDPOINT, tracing nonaktif (no-op) dan tidak ada biaya.
package tracing

import (
	"context"
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "payrun"

// Setup memasang tracer provider global. endpoint kosong → no-op. Kembalikan fungsi shutdown
// (flush span sebelum proses keluar).
func Setup(ctx context.Context, service, version, endpoint string) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint))
	if err != nil {
		return nil, err
	}
	res := resource.NewSchemaless(attribute.String("service.name", service), attribute.String("service.version", version))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp), // non-blocking, antrean terbatas
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()), // V1: 100% (volume demo kecil)
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// Middleware: span HTTP server. Nama span = route template (diisi setelah routing).
func Middleware(next http.Handler) http.Handler {
	return otelhttp.NewHandler(next, "http.server",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return r.Method + " " + r.URL.Path }))
}

// Transport membungkus RoundTripper dengan span HTTP keluar.
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return otelhttp.NewTransport(base)
}

// Start membuat span biasa (child dari span di ctx).
func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return otel.Tracer(tracerName).Start(ctx, name, trace.WithAttributes(attrs...))
}

// TraceParent mengembalikan header W3C traceparent dari span aktif ("" kalau tidak ada).
func TraceParent(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	return carrier["traceparent"]
}

// StartLinked membuat span ROOT baru dengan link ke span producer (traceparent tersimpan).
func StartLinked(ctx context.Context, name, traceParent string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	opts := []trace.SpanStartOption{trace.WithNewRoot(), trace.WithAttributes(attrs...)}
	if traceParent != "" {
		producer := propagation.TraceContext{}.Extract(context.Background(), propagation.MapCarrier{"traceparent": traceParent})
		if sc := trace.SpanContextFromContext(producer); sc.IsValid() {
			opts = append(opts, trace.WithLinks(trace.Link{SpanContext: sc}))
		}
	}
	return otel.Tracer(tracerName).Start(ctx, name, opts...)
}
