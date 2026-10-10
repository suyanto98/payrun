// Package observability menyiapkan logger terstruktur (JSON) dan konteks log per request.
// Skema field: docs/design/24-observability.md §1. Redaction dua lapis (C-23, ADR-P08):
//  1. allowlist: handler hanya me-log field yang memang dimaksudkan; body request tidak pernah di-log;
//  2. jaring pengaman: atribut bernama sensitif (password, token, nik, …) dan nilai bertipe
//     Sensitive[T] selalu ditulis "[REDACTED]", walaupun ada kode yang lupa aturan nomor 1.
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
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl, ReplaceAttr: redact})
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

// Redacted adalah nilai pengganti untuk data sensitif di log.
const Redacted = "[REDACTED]"

// sensitiveKeys: nama atribut (lowercase) yang nilainya tidak pernah boleh muncul di log (ADR-P08).
var sensitiveKeys = map[string]bool{
	"password": true, "password_hash": true, "token": true, "access_token": true, "refresh_token": true,
	"authorization": true, "cookie": true, "set-cookie": true, "secret": true, "api_key": true,
	"nik": true, "npwp": true, "account_number": true, "bank_account": true,
}

// redact dipasang sebagai ReplaceAttr: berlaku di semua level, termasuk atribut di dalam group.
func redact(_ []string, a slog.Attr) slog.Attr {
	if sensitiveKeys[strings.ToLower(a.Key)] {
		return slog.String(a.Key, Redacted)
	}
	return a
}

// Sensitive membungkus nilai yang tidak boleh terlihat di log/fmt (contoh NIK, nomor rekening).
// String(), LogValue(), dan JSON-nya selalu "[REDACTED]"; nilai asli hanya lewat Reveal().
type Sensitive[T any] struct{ v T }

func NewSensitive[T any](v T) Sensitive[T] { return Sensitive[T]{v: v} }

func (s Sensitive[T]) Reveal() T                  { return s.v }
func (Sensitive[T]) String() string               { return Redacted }
func (Sensitive[T]) LogValue() slog.Value         { return slog.StringValue(Redacted) }
func (Sensitive[T]) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }
func (s Sensitive[T]) GoString() string           { return Redacted }
