package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"sync"
	"time"

	"payrun/internal/platform/apperr"
	"payrun/internal/platform/observability"
)

type requestIDKey struct{}

// reqInfo adalah holder yang dibuat di awal request dan diisi middleware yang berjalan LEBIH
// DALAM (contoh: auth mengisi user_id). AccessLog berjalan di luar auth, jadi ia membaca holder
// ini setelah handler selesai — begitulah log akses bisa memuat user_id (skema log Step 8).
type reqInfo struct {
	mu     sync.Mutex
	userID string
}

type reqInfoKey struct{}

// SetUserID dipanggil middleware auth setelah token terverifikasi.
func SetUserID(ctx context.Context, id string) {
	if ri, ok := ctx.Value(reqInfoKey{}).(*reqInfo); ok {
		ri.mu.Lock()
		ri.userID = id
		ri.mu.Unlock()
	}
}

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// RequestIDFrom mengambil request ID dari context.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// RequestID menerima X-Request-ID dari client jika formatnya aman, atau membuat yang baru.
// ID dikembalikan di header respons dan dibawa logger di context.
func RequestID(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-ID")
			if !validRequestID.MatchString(id) { // jangan percaya input: cegah log injection
				id = newID()
			}
			w.Header().Set("X-Request-ID", id)
			ctx := context.WithValue(r.Context(), requestIDKey{}, id)
			ctx = context.WithValue(ctx, reqInfoKey{}, &reqInfo{})
			ctx = observability.WithLogger(ctx, base.With("request_id", id))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// AccessLog menulis satu baris log per request. Tidak me-log body, header Authorization,
// cookie, atau query string (bisa berisi data sensitif) — hanya metadata (C-23).
func AccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
		}
		if ri, ok := r.Context().Value(reqInfoKey{}).(*reqInfo); ok {
			ri.mu.Lock()
			if ri.userID != "" {
				attrs = append(attrs, "user_id", ri.userID)
			}
			ri.mu.Unlock()
		}
		observability.Logger(r.Context()).Info("http.request", attrs...)
	})
}

// Recover mengubah panic menjadi 500 generik dan me-log stack trace (tidak dikirim ke client).
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler { //nolint:errorlint // sentinel dari net/http, harus dibandingkan langsung
					panic(v)
				}
				observability.Logger(r.Context()).Error("http.panic", "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
				WriteError(w, r, apperr.ErrInternal)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// BodyLimit membatasi ukuran body request (C-14).
func BodyLimit(n int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, n)
			next.ServeHTTP(w, r)
		})
	}
}
