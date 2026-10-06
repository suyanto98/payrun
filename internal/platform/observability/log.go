// Package observability menyiapkan logger terstruktur (JSON) dan konteks log per request.
// Skema field: docs/design/21-observability.md §1. Redaction berbasis allowlist (C-23):
// handler hanya me-log field yang memang dimaksudkan; body request tidak pernah di-log.
package observability

import (
	"context"
	"io"
	"log/slog"
	"strings"
)

type ctxKey struct{}

// NewLogger membuat logger JSON dengan field wajib service/env/version.
func NewLogger(w io.Writer, level, service, env, version string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})
	return slog.New(h).With("service", service, "env", env, "version", version)
}

// WithLogger menyimpan logger (yang sudah membawa request_id, dst.) di context.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// Logger mengambil logger dari context, atau default jika tidak ada.
func Logger(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}
