//go:build !demo

package app_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"payrun/internal/app"
)

// TestDemoMode_ReleaseBuildHasNoDemoRoutes (lapis guard "build", 27 §9.1): binary release dengan
// DEMO_MODE=true dan token yang benar tetap tidak punya /api/v1/demo/* (404).
func TestDemoMode_ReleaseBuildHasNoDemoRoutes(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	d := e.deps
	d.DemoMode, d.DemoToken = true, "demo-token-0123456789abcdef"
	srv := httptest.NewServer(app.APIRouter(d))
	t.Cleanup(srv.Close)
	for _, m := range []string{http.MethodPost, http.MethodDelete} {
		req, _ := http.NewRequestWithContext(t.Context(), m, srv.URL+"/api/v1/demo/faults", nil)
		req.Header.Set("Authorization", "Bearer "+f.tokens[roleO])
		req.Header.Set("X-Demo-Token", d.DemoToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s /demo/faults on a release build: want 404, got %d", m, resp.StatusCode)
		}
	}
	// Sandbox pengunjung juga tidak ada: tidak ada tenant + user tanpa signup di binary release.
	for _, path := range []string{"/api/v1/demo/sessions", "/api/v1/demo/persona"} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path, nil)
		req.Header.Set("X-Requested-With", "fetch")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("POST %s on a release build: want 404, got %d", path, resp.StatusCode)
		}
	}
}
