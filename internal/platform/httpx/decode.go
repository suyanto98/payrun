package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	"payrun/internal/platform/apperr"
)

// MaxBodyBytes adalah batas body JSON (C-14). Body lebih besar → 413.
const MaxBodyBytes = 64 << 10

// Decode membaca body JSON secara ketat:
//   - Content-Type harus application/json (415);
//   - field tak dikenal ditolak (mass assignment, C-13) → 400;
//   - hanya satu objek JSON, tidak boleh ada sisa data;
//   - ukuran dibatasi oleh middleware BodyLimit (413).
func Decode(r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil || mt != "application/json" {
			return apperr.ErrUnsupportedMedia
		}
	} else {
		return apperr.ErrUnsupportedMedia
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			return apperr.ErrPayloadTooLarge
		case errors.Is(err, io.EOF):
			return apperr.ErrMalformedJSON.WithDetail("request body is empty")
		default:
			var syn *json.SyntaxError
			var typ *json.UnmarshalTypeError
			// io.ErrUnexpectedEOF: JSON terpotong (contoh `{"a":`).
			if errors.As(err, &syn) || errors.As(err, &typ) || errors.Is(err, io.ErrUnexpectedEOF) {
				return apperr.ErrMalformedJSON.WithCause(err)
			}
			// Contoh: "json: unknown field \"role\"" → tolak sebagai validasi, sebutkan field-nya.
			return apperr.Validation(apperr.FieldError{Field: "body", Code: "INVALID", Message: err.Error()})
		}
	}
	// Decode kedua harus EOF. dec.More() tidak cukup: `}` atau `]` sisa (contoh `{"a":1}}`)
	// dianggap "tidak ada lagi", sehingga body seperti itu lolos.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return apperr.ErrMalformedJSON.WithDetail("body must contain a single JSON object")
	}
	return nil
}
