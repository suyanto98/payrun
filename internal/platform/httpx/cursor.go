package httpx

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"payrun/internal/platform/apperr"
)

// Cursor adalah posisi keyset (created_at, id) untuk pagination (docs/design/11-api-conventions.md).
// Di kabel ia opaque: base64url(JSON). Client tidak boleh (dan tidak perlu) merakitnya sendiri.
type Cursor struct {
	At time.Time
	ID uuid.UUID
}

type cursorJSON struct {
	At time.Time `json:"t"`
	ID uuid.UUID `json:"i"`
}

// EncodeCursor mengubah posisi menjadi string opaque.
func EncodeCursor(c Cursor) string {
	b, _ := json.Marshal(cursorJSON(c))
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor membaca cursor dari client; apa pun yang tidak valid → 400 INVALID_CURSOR.
func DecodeCursor(s string) (Cursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, apperr.ErrInvalidCursor
	}
	var c cursorJSON
	if err := json.Unmarshal(b, &c); err != nil || c.ID == uuid.Nil || c.At.IsZero() {
		return Cursor{}, apperr.ErrInvalidCursor
	}
	return Cursor(c), nil
}

// Page adalah metadata pagination di respons daftar.
type Page struct {
	NextCursor *string `json:"next_cursor"`
	HasMore    bool    `json:"has_more"`
}
