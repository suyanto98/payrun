package app_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"payrun/api/gen"
)

// assertMatchesSpec: respons nyata cocok dengan tipe hasil generate dari api/openapi.yaml (ADR-003,
// spec-first). Dua arah:
//   - field yang dikirim server tetapi TIDAK ada di spec → gagal (decode DisallowUnknownFields);
//   - field wajib di spec (tag json tanpa omitempty) yang TIDAK dikirim server → gagal.
//
// Bukan validator JSON Schema penuh (pattern/enum tidak dicek), tetapi menangkap drift yang paling
// sering: field diganti nama, ditambah, atau dihapus di salah satu sisi saja.
func assertMatchesSpec(t *testing.T, name string, body map[string]any, into any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		t.Errorf("%s: response does not match the OpenAPI schema: %v\n%s", name, err, raw)
		return
	}
	rt := reflect.TypeOf(into).Elem()
	for i := range rt.NumField() {
		tag := rt.Field(i).Tag.Get("json")
		key, opts, _ := strings.Cut(tag, ",")
		if key == "" || key == "-" || strings.Contains(opts, "omitempty") {
			continue
		}
		if _, ok := body[key]; !ok {
			t.Errorf("%s: required field %q (spec) missing from the response", name, key)
		}
	}
}

// TestContract_S1ResponsesMatchSpec: semua respons sukses endpoint Sprint 1 terhadap skemanya.
func TestContract_S1ResponsesMatchSpec(t *testing.T) {
	e := newEnv(t)
	_, signup := e.signup(t, newID().String(), signupBody("alpha", "dewi@alpha.test"))
	assertMatchesSpec(t, "signup", signup, &gen.SignupResult{})
	_, replay := e.signup(t, newID().String(), signupBody("beta", "budi@beta.test"))
	assertMatchesSpec(t, "signup (other tenant)", replay, &gen.SignupResult{})

	e.do(t, http.MethodPost, "/api/v1/auth/verify-email", "", map[string]string{"token": e.mailer.lastToken(t, "dewi@alpha.test"), "password": password})
	resp, login := e.do(t, http.MethodPost, "/api/v1/auth/login", "", map[string]string{"email": "dewi@alpha.test", "password": password})
	assertMatchesSpec(t, "login", login, &gen.Session{})
	token := login["access_token"].(string)

	_, me := e.do(t, http.MethodGet, "/api/v1/me", token, nil)
	assertMatchesSpec(t, "me", me, &gen.Me{})
	_, ms := e.do(t, http.MethodGet, "/api/v1/me/memberships", token, nil)
	assertMatchesSpec(t, "memberships", ms, &gen.MembershipList{})
	_, tn := e.do(t, http.MethodGet, "/api/v1/tenant", token, nil)
	assertMatchesSpec(t, "tenant", tn, &gen.Tenant{})
	_, sw := e.do(t, http.MethodPost, "/api/v1/session/switch-tenant", token, map[string]string{"tenant_id": activeTenantID(login)})
	assertMatchesSpec(t, "switch-tenant", sw, &gen.Session{})
	_, rf := e.cookiePost(t, "/api/v1/auth/refresh", cookieValue(resp), map[string]string{"X-Active-Tenant": activeTenantID(login)})
	assertMatchesSpec(t, "refresh", rf, &gen.Session{})

	// Error memakai Problem (RFC 9457).
	_, prob := e.do(t, http.MethodPost, "/api/v1/auth/login", "", map[string]string{"email": "x@y.test", "password": "wrong password"})
	assertMatchesSpec(t, "problem", prob, &gen.Problem{})
}
