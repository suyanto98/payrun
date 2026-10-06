package httpx

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"payrun/internal/platform/apperr"
)

func TestDecode(t *testing.T) {
	cases := []struct {
		name, body string
		want       *apperr.Error // nil = lolos
	}{
		{"valid object", `{"a":1}`, nil},
		{"trailing whitespace ok", "{\"a\":1}\n  ", nil},
		{"empty body", ``, apperr.ErrMalformedJSON},
		{"truncated", `{"a":`, apperr.ErrMalformedJSON},
		{"syntax error", `{"a":}`, apperr.ErrMalformedJSON},
		{"trailing brace", `{"a":1}}`, apperr.ErrMalformedJSON},
		{"second object", `{"a":1}{"a":2}`, apperr.ErrMalformedJSON},
		{"unknown field", `{"a":1,"role":"OWNER"}`, apperr.ErrValidation},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(c.body))
			r.Header.Set("Content-Type", "application/json")
			var dst struct {
				A int `json:"a"`
			}
			err := Decode(r, &dst)
			switch {
			case c.want == nil && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case c.want != nil && !errors.Is(err, c.want):
				t.Fatalf("want %s, got %v", c.want.Code, err)
			}
		})
	}
}
