package httpx

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"payrun/internal/platform/apperr"
)

// Optimistic concurrency lewat HTTP (docs/design/14-api-conventions.md): ETag = "<version>" pada
// GET/PATCH resource ber-`version`; If-Match wajib pada perubahan. Tanpa header → 428
// PRECONDITION_REQUIRED; header yang bukan versi yang kita terbitkan → 412 VERSION_CONFLICT (sama
// seperti versi lama: client harus memuat ulang).

// SetETag menulis ETag kuat berisi versi resource.
func SetETag(w http.ResponseWriter, version int32) {
	w.Header().Set("ETag", strconv.Quote(strconv.Itoa(int(version))))
}

// IfMatchVersion membaca If-Match → versi yang diharapkan client.
func IfMatchVersion(r *http.Request) (int32, error) {
	raw := strings.TrimSpace(r.Header.Get("If-Match"))
	if raw == "" {
		return 0, apperr.ErrPreconditionReq
	}
	raw = strings.TrimPrefix(raw, "W/") // ETag lemah tidak pernah kita terbitkan, tetapi browser kadang menambahkannya
	s, err := strconv.Unquote(raw)
	if err != nil {
		s = raw
	}
	v, err := strconv.ParseInt(s, 10, 32)
	if err != nil || v < 1 {
		return 0, apperr.ErrVersionConflict
	}
	return int32(v), nil
}

// ParseID membaca id dari path; bukan UUID → 404 (sama dengan resource yang tidak ada: tidak membedakan
// "format salah" dan "tidak ada", dan tidak membocorkan apa pun).
func ParseID(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, apperr.ErrNotFound
	}
	return id, nil
}

// KeyCursor adalah posisi keyset (kunci urut teks, id) untuk daftar yang tidak diurutkan menurut
// waktu, contoh employee (`employee_no, id` atau `lower(full_name), id`). Opaque di kabel.
type KeyCursor struct {
	Key string
	ID  uuid.UUID
}

type keyCursorJSON struct {
	K string    `json:"k"`
	I uuid.UUID `json:"i"`
}

// EncodeKeyCursor mengubah posisi menjadi string opaque.
func EncodeKeyCursor(c KeyCursor) string {
	b, _ := json.Marshal(keyCursorJSON{K: c.Key, I: c.ID})
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeKeyCursor membaca cursor dari client; apa pun yang tidak valid → 400 INVALID_CURSOR.
func DecodeKeyCursor(s string) (KeyCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return KeyCursor{}, apperr.ErrInvalidCursor
	}
	var c keyCursorJSON
	if err := json.Unmarshal(b, &c); err != nil || c.I == uuid.Nil {
		return KeyCursor{}, apperr.ErrInvalidCursor
	}
	return KeyCursor{Key: c.K, ID: c.I}, nil
}

// Limit membaca ?limit (default def, 1..max); nilai di luar rentang → 400 VALIDATION_ERROR.
func Limit(r *http.Request, def, maxLimit int) (int32, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return int32(def), nil //nolint:gosec // G115: def kecil, ditentukan kode
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxLimit {
		return 0, apperr.Validation(apperr.FieldError{Field: "limit", Code: "OUT_OF_RANGE", Message: "must be 1.." + strconv.Itoa(maxLimit)})
	}
	return int32(n), nil //nolint:gosec // G115: dibatasi maxLimit
}

// OnlyQuery menolak parameter query yang tidak dikenal (konvensi: filter tidak dikenal → 400).
func OnlyQuery(r *http.Request, allowed ...string) error {
	var bad []apperr.FieldError
	for k := range r.URL.Query() {
		ok := false
		for _, a := range allowed {
			if k == a {
				ok = true
				break
			}
		}
		if !ok {
			bad = append(bad, apperr.FieldError{Field: k, Code: "UNKNOWN_PARAMETER"})
		}
	}
	if len(bad) > 0 {
		return apperr.Validation(bad...)
	}
	return nil
}
