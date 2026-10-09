package idempotency

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"payrun/internal/platform/apperr"
)

// Canonical JSON: urutan key dan whitespace tidak mengubah hash; nilai, method, dan path mengubahnya.
func TestHash_Canonical(t *testing.T) {
	h := func(method, path, body string) []byte {
		t.Helper()
		b, err := Hash(method, path, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	base := h("POST", "/api/v1/transfers", `{"amount":"5000","currency":"IDR","to":"a@b.c"}`)
	same := []string{
		`{"currency":"IDR","to":"a@b.c","amount":"5000"}`,
		"{\n  \"amount\": \"5000\",\n  \"currency\": \"IDR\",\n  \"to\": \"a@b.c\"\n}",
	}
	for _, s := range same {
		if !bytes.Equal(base, h("POST", "/api/v1/transfers", s)) {
			t.Errorf("hash must be equal for %s", s)
		}
	}
	different := map[string][]byte{
		"amount":     h("POST", "/api/v1/transfers", `{"amount":"6000","currency":"IDR","to":"a@b.c"}`),
		"path":       h("POST", "/api/v1/topups", `{"amount":"5000","currency":"IDR","to":"a@b.c"}`),
		"method":     h("PUT", "/api/v1/transfers", `{"amount":"5000","currency":"IDR","to":"a@b.c"}`),
		"number 1.0": h("POST", "/x", `{"n":1.0}`),
	}
	for name, d := range different {
		if bytes.Equal(base, d) {
			t.Errorf("hash must differ when %s changes", name)
		}
	}
	// json.Number: 1 dan 1.0 tidak disamakan diam-diam.
	if bytes.Equal(h("POST", "/x", `{"n":1}`), different["number 1.0"]) {
		t.Error("1 and 1.0 must hash differently (numbers are kept verbatim)")
	}
	if len(base) != 32 {
		t.Fatalf("sha-256 must be 32 bytes, got %d", len(base))
	}
}

// Body kosong (endpoint tanpa body) di-hash sama dengan `{}`, bukan MALFORMED_JSON.
func TestHash_EmptyBodyIsEmptyObject(t *testing.T) {
	empty, err := Hash("POST", "/x", nil)
	if err != nil {
		t.Fatalf("empty body: %v", err)
	}
	obj, _ := Hash("POST", "/x", []byte(" {} "))
	if !bytes.Equal(empty, obj) {
		t.Error("empty body must hash like {}")
	}
	if _, err := Hash("POST", "/x", []byte("{")); !errors.Is(err, apperr.ErrMalformedJSON) {
		t.Errorf("truncated JSON: want MALFORMED_JSON, got %v", err)
	}
}

func TestKeyFrom(t *testing.T) {
	cases := map[string]error{
		"":                                     apperr.ErrIdempotencyKeyRequired,
		"short":                                apperr.ErrIdempotencyKeyInvalid,
		"has space inside":                     apperr.ErrIdempotencyKeyInvalid,
		"line\nbreak-injection":                apperr.ErrIdempotencyKeyInvalid,
		strings.Repeat("k", 256):               apperr.ErrIdempotencyKeyInvalid,
		"7c9e6679-7425-40de-944b-e07fc1f90ae7": nil,
		"demo-a:1700000000.01_x":               nil,
	}
	for key, want := range cases {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		if key != "" {
			r.Header[HeaderKey] = []string{key} // tanpa normalisasi, supaya \n ikut teruji
		}
		_, err := KeyFrom(r)
		if !errors.Is(err, want) { // errors.Is(nil, nil) == true
			t.Errorf("key %q: want %v, got %v", key, want, err)
		}
	}
}
