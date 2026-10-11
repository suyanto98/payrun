package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payrun/api/gen"
	"payrun/internal/platform/ratelimit"
)

// PII uji: nilai unik yang mudah dicari di respons/audit mana pun.
const (
	testNIK     = "3174099988877701"
	testNPWP    = "174099988877701"
	testAccount = "88123456789"
)

func (e env) piiEmployee(t *testing.T, token string) uuid.UUID {
	t.Helper()
	body := employeeBody("PII-01", "Sinta PII", testNIK)
	body["npwp"] = testNPWP
	body["bank"] = map[string]any{"name": "BNI", "account_number": testAccount, "holder": "Sinta PII"}
	r, b := e.doH(t, http.MethodPost, "/api/v1/employees", token, key(), body)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %v", r.StatusCode, b)
	}
	if strings.Contains(mustJSON(b), testNIK) || strings.Contains(mustJSON(b), testNPWP) || strings.Contains(mustJSON(b), testAccount) {
		t.Fatalf("create response leaks PII: %v", b)
	}
	return uuid.MustParse(b["id"].(string))
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// TestPII_MaskedByDefault (C-22 pii_masked_by_default): selain W6, tidak ada respons atau baris audit yang
// memuat NIK, NPWP, atau nomor rekening utuh; tampilan default hanya 4 digit terakhir.
func TestPII_MaskedByDefault(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	o := f.tokens[roleO]
	id := e.piiEmployee(t, o)
	path := "/api/v1/employees/" + id.String()

	var seen []string
	for _, role := range []string{roleO, roleH, roleP} {
		_, b := e.do(t, http.MethodGet, path, f.tokens[role], nil)
		seen = append(seen, mustJSON(b))
		if role == roleO && (b["nik_masked"] != "************7701" || b["bank"].(map[string]any)["account_masked"] != "******6789") {
			t.Fatalf("default view must be masked to the last 4 digits: %v", b)
		}
	}
	_, list := e.do(t, http.MethodGet, "/api/v1/employees", o, nil)
	r, upd := e.doH(t, http.MethodPatch, path, o, map[string]string{"If-Match": `"1"`},
		map[string]any{"npwp": "274099988877702", "bank": map[string]any{"name": "BNI", "account_number": "88123456780", "holder": "Sinta"}})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("update: %d %v", r.StatusCode, upd)
	}
	seen = append(seen, mustJSON(list), mustJSON(upd))
	var audits string
	e.inTenant(t, f.id, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT coalesce(string_agg(coalesce(diff::text, '') || coalesce(reason, ''), ' '), '') FROM audit_log`).Scan(&audits)
	})
	seen = append(seen, audits)
	for _, s := range seen {
		for _, raw := range []string{testNIK, testNPWP, testAccount, "274099988877702", "88123456780"} {
			if strings.Contains(s, raw) {
				t.Fatalf("PII %s leaked outside W6: %s", raw, s)
			}
		}
	}
}

// TestPII_FullViewAudited (C-22 pii_full_view_audited): W6 mengembalikan nilai utuh dengan no-store, dan
// SETIAP panggilan menambah tepat satu audit employee.pii_viewed (actor, target, field) — juga untuk diri
// sendiri. Respons cocok dengan skema.
func TestPII_FullViewAudited(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	id := e.piiEmployee(t, f.tokens[roleO])
	for i, role := range []string{roleO, roleH, roleH} {
		r, b := e.do(t, http.MethodGet, "/api/v1/employees/"+id.String()+"/pii", f.tokens[role], nil)
		if r.StatusCode != http.StatusOK || r.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %d %v %v", role, r.StatusCode, r.Header, b)
		}
		if b["nik"] != testNIK || b["npwp"] != testNPWP || b["bank"].(map[string]any)["account_number"] != testAccount {
			t.Fatalf("%s: full values expected, got %v", role, b)
		}
		assertMatchesSpec(t, "employee pii", b, &gen.EmployeePII{})
		if got := e.auditCount(t, f.id, "employee.pii_viewed"); got != i+1 {
			t.Fatalf("after %d views: audit employee.pii_viewed = %d", i+1, got)
		}
	}
	var actor uuid.UUID
	var fields string
	e.inTenant(t, f.id, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT actor_id, diff->>'fields' FROM audit_log
			WHERE action = 'employee.pii_viewed' AND target_id = $1 ORDER BY created_at LIMIT 1`, id).Scan(&actor, &fields)
	})
	if actor != f.users[roleO] || fields != `["nik", "npwp", "bank_account"]` {
		t.Fatalf("audit must name the actor and the fields read: actor=%s fields=%s", actor, fields)
	}

	// Diri sendiri (EMPLOYEE yang tertaut): boleh, diaudit; tanpa PII yang pernah diisi → null.
	r, self := e.do(t, http.MethodGet, "/api/v1/employees/"+f.empSelf.String()+"/pii", f.tokens[roleE], nil)
	if r.StatusCode != http.StatusOK || self["nik"] == nil {
		t.Fatalf("self view: %d %v", r.StatusCode, self)
	}
	if got := e.auditCount(t, f.id, "employee.pii_viewed"); got != 4 {
		t.Fatalf("self view must be audited too: %d", got)
	}
}

// TestPII_RolesReadFromDB (ADR-P02): HR_ADMIN yang sudah diturunkan menjadi EMPLOYEE masih memegang token
// HR ≤ 15 menit, tetapi W6 membaca role dari DB → 404 (bukan dirinya, tidak terlihat), tanpa audit.
func TestPII_RolesReadFromDB(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	id := e.piiEmployee(t, f.tokens[roleO])
	hrToken := f.tokens[roleH]
	if r, b := e.setRoles(t, f.tokens[roleO], e.memberOf(t, f.id, f.users[roleH]), 1, roleE); r.StatusCode != http.StatusOK {
		t.Fatalf("demote HR: %d %v", r.StatusCode, b)
	}
	if r, b := e.do(t, http.MethodGet, "/api/v1/employees/"+id.String()+"/pii", hrToken, nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("demoted HR with stale token: want 404, got %d %v", r.StatusCode, b)
	}
	if got := e.auditCount(t, f.id, "employee.pii_viewed"); got != 0 {
		t.Fatalf("denied view must not be audited, got %d", got)
	}
}

// TestPII_RateLimitedPerUser (T-PR-10): tier pii-view 30/jam per user; user lain tidak terpengaruh.
func TestPII_RateLimitedPerUser(t *testing.T) {
	tiers := ratelimit.DefaultTiers()
	loose := []ratelimit.Rule{{Events: 100000, Per: 60e9}}
	tiers.AuthStrict.Rules, tiers.AuthIP.Rules, tiers.Read.Rules, tiers.Write.Rules = loose, loose, loose, loose
	e := newEnvTiers(t, &tiers)
	f := e.newTenantFx(t, "alpha")
	path := "/api/v1/employees/" + f.empOther.String() + "/pii"
	for i := range 30 {
		if r, b := e.do(t, http.MethodGet, path, f.tokens[roleH], nil); r.StatusCode != http.StatusOK {
			t.Fatalf("view %d: %d %v", i+1, r.StatusCode, b)
		}
	}
	if r, b := e.do(t, http.MethodGet, path, f.tokens[roleH], nil); r.StatusCode != http.StatusTooManyRequests || r.Header.Get("Retry-After") == "" {
		t.Fatalf("31st view in an hour: want 429 + Retry-After, got %d %v", r.StatusCode, b)
	}
	if r, _ := e.do(t, http.MethodGet, path, f.tokens[roleO], nil); r.StatusCode != http.StatusOK {
		t.Fatalf("another user is not limited: got %d", r.StatusCode)
	}
}

// TestInvitation_ReinviteRevokedMemberReactivatesRow: member yang dicabut diundang lagi → accept
// mengaktifkan ULANG baris membership lamanya dengan role undangan (satu baris per user, riwayat utuh),
// baris undangan ditutup, dan user kembali bisa memilih tenant itu.
func TestInvitation_ReinviteRevokedMemberReactivatesRow(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	o := f.tokens[roleO]
	old := e.memberOf(t, f.id, f.users[roleM])
	if r, b := e.revokeMember(t, o, old); r.StatusCode != http.StatusOK {
		t.Fatalf("revoke: %d %v", r.StatusCode, b)
	}
	_, inv := e.do(t, http.MethodPost, "/api/v1/invitations", o, map[string]any{"email": f.emails[roleM], "roles": []string{roleE}})
	tok, _, _ := e.login(t, f.emails[roleM])
	r, b := e.do(t, http.MethodPost, "/api/v1/invitations/accept", tok, map[string]string{"token": inviteToken(t, inv)})
	if r.StatusCode != http.StatusOK || b["membership"].(map[string]any)["status"] != "ACTIVE" {
		t.Fatalf("accept after revoke: want 200 ACTIVE, got %d %v", r.StatusCode, b)
	}
	var rows int
	var status string
	var roles []string
	var revokedAt *string
	e.inTenant(t, f.id, func(tx pgx.Tx) error {
		ctx := context.Background()
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE user_id = $1`, f.users[roleM]).Scan(&rows); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT status, roles, revoked_at::text FROM memberships WHERE id = $1`, old).Scan(&status, &roles, &revokedAt)
	})
	if rows != 1 || status != "ACTIVE" || len(roles) != 1 || roles[0] != roleE || revokedAt != nil {
		t.Fatalf("old row must be reactivated with the invited roles: rows=%d status=%s roles=%v revoked_at=%v", rows, status, roles, revokedAt)
	}
	_, pending := e.do(t, http.MethodGet, "/api/v1/members?status=INVITED", o, nil)
	if n := len(pending["data"].([]any)); n != 0 {
		t.Fatalf("the invitation row must be closed, %d still INVITED", n)
	}
	if e.auditCount(t, f.id, "membership.reactivated") != 1 {
		t.Fatal("reactivation must be audited as membership.reactivated")
	}
	if r, b := e.do(t, http.MethodPost, "/api/v1/session/switch-tenant", tok, map[string]string{"tenant_id": f.id.String()}); r.StatusCode != http.StatusOK {
		t.Fatalf("switch into the tenant again: %d %v", r.StatusCode, b)
	}
}

// TestPayrollItems_RolesReadFromDB (final review R2-6): Y6/Y7 membaca role pemanggil dari DB. PAYROLL_ADMIN
// yang sudah diturunkan tetapi tokennya masih membawa role itu → 404, tanpa audit bulk_read.
func TestPayrollItems_RolesReadFromDB(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	run, item := e.errorItem(t, f.tokens[roleP], "2026-03")
	before := e.auditCount(t, f.id, "payroll.bulk_read")
	stale := f.tokens[roleP]
	if r, b := e.setRoles(t, f.tokens[roleO], e.memberOf(t, f.id, f.users[roleP]), 1, roleE); r.StatusCode != http.StatusOK {
		t.Fatalf("demote payroll admin: %d %v", r.StatusCode, b)
	}
	for _, path := range []string{"/items", "/items/" + item.String()} {
		if r, b := e.do(t, http.MethodGet, "/api/v1/payroll-runs/"+run.String()+path, stale, nil); r.StatusCode != http.StatusNotFound {
			t.Fatalf("%s with a stale PAYROLL_ADMIN token: want 404, got %d %v", path, r.StatusCode, b)
		}
	}
	if got := e.auditCount(t, f.id, "payroll.bulk_read"); got != before {
		t.Fatalf("denied reads must not be audited: %d → %d", before, got)
	}
}
