package app_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestTenantIsolation_DemoA: Demo A (docs/design/16-api-endpoints.md §17, DoD M5) sebagai test.
// Dewi (OWNER PT Alpha) mencoba menjangkau data PT Beta lewat setiap jalur yang tersedia di S2.
func TestTenantIsolation_DemoA(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	alpha := e.newTenantFx(t, "alpha")
	beta := e.newTenantFx(t, "beta")
	dewi := alpha.tokens[roleO]

	// 2. GET employee milik Beta → 404 (RLS + scoping), sama persis dengan id yang tidak ada.
	r1, b1 := e.do(t, http.MethodGet, "/api/v1/employees/"+beta.empOther.String(), dewi, nil)
	r2, b2 := e.do(t, http.MethodGet, "/api/v1/employees/"+newID().String(), dewi, nil)
	if r1.StatusCode != http.StatusNotFound || b1["code"] != "NOT_FOUND" || r2.StatusCode != http.StatusNotFound || b1["title"] != b2["title"] {
		t.Fatalf("step 2: Beta employee must look exactly like a missing one: %d %v / %d %v", r1.StatusCode, b1, r2.StatusCode, b2)
	}

	// 3. Claim `tid` diubah manual ke Beta → 401 (signature tidak valid).
	parts := strings.Split(dewi, ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	forged := strings.Replace(string(payload), alpha.id.String(), beta.id.String(), 1)
	if forged == string(payload) {
		t.Fatal("token payload does not carry the tenant id")
	}
	parts[1] = base64.RawURLEncoding.EncodeToString([]byte(forged))
	if r, b := e.do(t, http.MethodGet, "/api/v1/employees/"+beta.empOther.String(), strings.Join(parts, "."), nil); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("step 3: forged tid: want 401, got %d %v", r.StatusCode, b)
	}

	// 4. Referensi ke resource Beta di body → 404 (composite FK 23503 / RLS). Di S2 jalurnya undangan
	//    dengan employee_id; org.department_id pada POST /employees menyusul di S3.
	if r, b := e.do(t, http.MethodPost, "/api/v1/invitations", dewi, map[string]any{"email": "n@alpha.test", "roles": []string{"EMPLOYEE"}, "employee_id": beta.empOther}); r.StatusCode != http.StatusNotFound {
		t.Fatalf("step 4: Beta employee_id: want 404, got %d %v", r.StatusCode, b)
	}

	// 5. "tenant_id" di body → 400 VALIDATION_ERROR (field tidak dikenal; tenant hanya dari `tid`).
	body := employeeBody("EMP-0500", "Penyusup", "3174012304950500")
	body["tenant_id"] = beta.id.String()
	r, b := e.doH(t, http.MethodPost, "/api/v1/employees", dewi, map[string]string{"Idempotency-Key": newID().String()}, body)
	if r.StatusCode != http.StatusBadRequest || b["code"] != "VALIDATION_ERROR" {
		t.Fatalf("step 5: tenant_id in body: want 400, got %d %v", r.StatusCode, b)
	}

	// 6. Query mentah TANPA filter tenant di bawah konteks Alpha → hanya baris Alpha (RLS, bukan WHERE).
	var total, foreign int
	e.inTenant(t, alpha.id, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT count(*), count(*) FILTER (WHERE tenant_id <> $1) FROM employees`, alpha.id).Scan(&total, &foreign)
	})
	if total != 2 || foreign != 0 {
		t.Fatalf("step 6: unfiltered query under Alpha: total=%d foreign=%d, want 2/0", total, foreign)
	}
}
