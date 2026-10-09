//go:build testhooks

package app_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"payrun/internal/platform/testhook"
)

// TestPII_AuditNotCommittedSendsNothing (T-PR-10): dekripsi sudah terjadi, tetapi transaksi yang memuat
// audit employee.pii_viewed gagal COMMIT. Respons harus gagal tanpa PII apa pun, dan tidak ada jejak
// audit setengah jadi. Panggilan berikutnya normal kembali.
func TestPII_AuditNotCommittedSendsNothing(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	id := e.piiEmployee(t, f.tokens[roleO])
	path := "/api/v1/employees/" + id.String() + "/pii"

	reset := testhook.Set("db.before_commit", func(context.Context) error { return errors.New("injected: connection lost before COMMIT") })
	r, b := e.do(t, http.MethodGet, path, f.tokens[roleH], nil)
	reset()
	if r.StatusCode < 500 {
		t.Fatalf("failed commit: want 5xx, got %d %v", r.StatusCode, b)
	}
	if s := mustJSON(b); strings.Contains(s, testNIK) || strings.Contains(s, testNPWP) || strings.Contains(s, testAccount) {
		t.Fatalf("PII sent although the audit was not committed: %s", s)
	}
	if got := e.auditCount(t, f.id, "employee.pii_viewed"); got != 0 {
		t.Fatalf("no audit row may survive a failed commit, got %d", got)
	}
	if r, b := e.do(t, http.MethodGet, path, f.tokens[roleH], nil); r.StatusCode != http.StatusOK || b["nik"] != testNIK {
		t.Fatalf("next view: %d %v", r.StatusCode, b)
	}
}
