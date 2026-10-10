package app_test

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payrun/api/gen"
)

// ---------------------------------------------------------------- U4/U5 role change & revoke (S4)

func (e env) memberOf(t *testing.T, tenant, user uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	e.inTenant(t, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT id FROM memberships WHERE user_id = $1`, user).Scan(&id)
	})
	return id
}

func (e env) activeOwners(t *testing.T, tenant uuid.UUID) int {
	t.Helper()
	var n int
	e.inTenant(t, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM memberships WHERE status = 'ACTIVE' AND 'OWNER' = ANY (roles)`).Scan(&n)
	})
	return n
}

func (e env) setRoles(t *testing.T, token string, member uuid.UUID, version int, roles ...string) (result, map[string]any) {
	t.Helper()
	return e.doH(t, http.MethodPatch, "/api/v1/members/"+member.String()+"/roles", token,
		map[string]string{"If-Match": `"` + itoa(version) + `"`}, map[string]any{"roles": roles})
}

func (e env) revokeMember(t *testing.T, token string, member uuid.UUID) (result, map[string]any) {
	t.Helper()
	return e.do(t, http.MethodPost, "/api/v1/members/"+member.String()+"/revoke", token, map[string]any{"reason": "Resign"})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// TestMemberRoles_LastOwnerGuard (PI-16): owner terakhir tidak bisa menurunkan atau mencabut dirinya
// (409 LAST_OWNER, tanpa efek dan tanpa audit). Transfer kepemilikan (tambah owner baru, lalu yang lama
// turun) berhasil. Owner lama yang tokennya masih membawa OWNER ditolak karena role dibaca ulang dari DB.
func TestMemberRoles_LastOwnerGuard(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	o := f.tokens[roleO]
	self := e.memberOf(t, f.id, f.users[roleO])

	if r, b := e.setRoles(t, o, self, 1, roleH); r.StatusCode != http.StatusConflict || b["code"] != "LAST_OWNER" {
		t.Fatalf("last owner demoting self: want 409 LAST_OWNER, got %d %v", r.StatusCode, b)
	}
	if r, b := e.revokeMember(t, o, self); r.StatusCode != http.StatusConflict || b["code"] != "LAST_OWNER" {
		t.Fatalf("last owner revoking self: want 409 LAST_OWNER, got %d %v", r.StatusCode, b)
	}
	if n := e.auditCount(t, f.id, "membership.roles_changed") + e.auditCount(t, f.id, "membership.revoked"); n != 0 {
		t.Fatalf("rejected changes must not leave audit rows, got %d", n)
	}

	payroll := e.memberOf(t, f.id, f.users[roleP])
	r, b := e.setRoles(t, o, payroll, 1, roleO, roleP)
	if r.StatusCode != http.StatusOK || r.Header.Get("ETag") != `"2"` {
		t.Fatalf("promote to OWNER: %d %v %v", r.StatusCode, r.Header, b)
	}
	assertMatchesSpec(t, "member (roles changed)", b, &gen.Member{})
	if r, b := e.setRoles(t, o, self, 1, roleH); r.StatusCode != http.StatusOK {
		t.Fatalf("old owner stepping down after transfer: %d %v", r.StatusCode, b)
	}
	if n := e.activeOwners(t, f.id); n != 1 {
		t.Fatalf("want exactly 1 active owner after the transfer, got %d", n)
	}
	// Token lama masih bertuliskan OWNER, tetapi DB sudah HR_ADMIN: memberi role admin → 403.
	if r, b := e.setRoles(t, o, e.memberOf(t, f.id, f.users[roleM]), 1, roleO); r.StatusCode != http.StatusForbidden {
		t.Fatalf("stale OWNER token: want 403 (roles re-read from DB), got %d %v", r.StatusCode, b)
	}
}

// TestMemberRoles_PreconditionsAndStates: If-Match wajib (428) dan harus cocok (412); roles yang sama
// tidak menulis apa pun; revoke kedua dan ubah role member REVOKED → 409 INVALID_STATE_TRANSITION.
func TestMemberRoles_PreconditionsAndStates(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	o := f.tokens[roleO]
	m := e.newMember(t, f.id, "staff@alpha.test", roleE)
	path := "/api/v1/members/" + m.String() + "/roles"

	if r, b := e.do(t, http.MethodPatch, path, o, map[string]any{"roles": []string{roleM}}); r.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("without If-Match: want 428, got %d %v", r.StatusCode, b)
	}
	if r, b := e.setRoles(t, o, m, 7, roleM); r.StatusCode != http.StatusPreconditionFailed || b["code"] != "VERSION_CONFLICT" {
		t.Fatalf("stale If-Match: want 412, got %d %v", r.StatusCode, b)
	}
	if r, b := e.setRoles(t, o, m, 1, roleE, roleE); r.StatusCode != http.StatusOK || b["version"] != float64(1) {
		t.Fatalf("same roles: want 200 without a new version, got %d %v", r.StatusCode, b)
	}
	if r, b := e.setRoles(t, o, m, 1, "SUPERUSER"); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown role: want 400, got %d %v", r.StatusCode, b)
	}
	if e.auditCount(t, f.id, "membership.roles_changed") != 0 {
		t.Fatal("no-op / rejected role changes must not be audited")
	}

	r, b := e.revokeMember(t, o, m)
	if r.StatusCode != http.StatusOK || b["status"] != "REVOKED" || b["user"] != nil {
		t.Fatalf("revoke: want 200 REVOKED (user hidden like in the list), got %d %v", r.StatusCode, b)
	}
	assertMatchesSpec(t, "member (revoked)", b, &gen.Member{})
	if r, b := e.revokeMember(t, o, m); r.StatusCode != http.StatusConflict || b["code"] != "INVALID_STATE_TRANSITION" {
		t.Fatalf("second revoke: want 409 INVALID_STATE_TRANSITION, got %d %v", r.StatusCode, b)
	}
	if r, b := e.setRoles(t, o, m, 2, roleM); r.StatusCode != http.StatusConflict || b["code"] != "INVALID_STATE_TRANSITION" {
		t.Fatalf("roles of a revoked member: want 409, got %d %v", r.StatusCode, b)
	}
	if r, b := e.do(t, http.MethodPost, "/api/v1/members/"+m.String()+"/revoke", o, map[string]any{"reason": "x"}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("short reason: want 400, got %d %v", r.StatusCode, b)
	}
}

// TestMemberRevoke_EndsAccess: user yang dicabut kehilangan tenant di refresh berikutnya (token tanpa
// `tid`); undangan INVITED yang dicabut tidak bisa diterima lagi (410).
func TestMemberRevoke_EndsAccess(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	o := f.tokens[roleO]
	_, cookie, _ := e.login(t, f.emails[roleE])
	if r, b := e.revokeMember(t, o, e.memberOf(t, f.id, f.users[roleE])); r.StatusCode != http.StatusOK {
		t.Fatalf("revoke: %d %v", r.StatusCode, b)
	}
	r, b := e.cookiePost(t, "/api/v1/auth/refresh", cookie, map[string]string{"X-Active-Tenant": f.id.String()})
	if r.StatusCode != http.StatusOK || b["active_tenant"] != nil {
		t.Fatalf("refresh after revoke: want 200 without active tenant, got %d %v", r.StatusCode, b)
	}

	_, inv := e.do(t, http.MethodPost, "/api/v1/invitations", o, map[string]any{"email": "late@alpha.test", "roles": []string{roleE}})
	pending := uuid.MustParse(inv["membership_id"].(string))
	if r, b := e.revokeMember(t, o, pending); r.StatusCode != http.StatusOK || b["status"] != "REVOKED" {
		t.Fatalf("revoke invitation: %d %v", r.StatusCode, b)
	}
	u := e.createUser(t, "late@alpha.test")
	e.verifyUser(t, u)
	tok, _, _ := e.login(t, "late@alpha.test")
	if r, b := e.do(t, http.MethodPost, "/api/v1/invitations/accept", tok, map[string]string{"token": inviteToken(t, inv)}); r.StatusCode != http.StatusGone {
		t.Fatalf("accepting a revoked invitation: want 410, got %d %v", r.StatusCode, b)
	}
}

// TestMemberRoles_SoDConflictAudited (file 17 §3.1): HR_ADMIN + PAYROLL_ADMIN pada satu orang diizinkan
// tetapi tercatat `membership.sod_conflict`, sekali saat kombinasi itu terbentuk.
func TestMemberRoles_SoDConflictAudited(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	m := e.memberOf(t, f.id, f.users[roleP])
	if r, b := e.setRoles(t, f.tokens[roleO], m, 1, roleP, roleH); r.StatusCode != http.StatusOK {
		t.Fatalf("grant HR to payroll admin: %d %v", r.StatusCode, b)
	}
	if r, b := e.setRoles(t, f.tokens[roleO], m, 2, roleP, roleH, roleE); r.StatusCode != http.StatusOK {
		t.Fatalf("add EMPLOYEE: %d %v", r.StatusCode, b)
	}
	if n := e.auditCount(t, f.id, "membership.sod_conflict"); n != 1 {
		t.Fatalf("membership.sod_conflict: want 1 (when the combination appears), got %d", n)
	}
}

// TestMembership_RoleGrantRulesEnforced (DoD role_grant_rules_enforced, file 17 §2.1 ᴿ): aturan grant
// lengkap per aktor, termasuk HR_ADMIN yang rolenya sudah diturunkan tetapi tokennya belum.
func TestMembership_RoleGrantRulesEnforced(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	plain := func() uuid.UUID { return e.newMember(t, f.id, "p"+newID().String()+"@alpha.test", roleE) }
	admin := func(role string) uuid.UUID { return e.newMember(t, f.id, "a"+newID().String()+"@alpha.test", role) }
	cases := []struct {
		name   string
		actor  string
		target func() uuid.UUID
		roles  []string // nil = revoke
		want   int
	}{
		{"owner grants OWNER", roleO, plain, []string{roleO}, 200},
		{"owner demotes HR", roleO, func() uuid.UUID { return admin(roleH) }, []string{roleE}, 200},
		{"owner revokes payroll admin", roleO, func() uuid.UUID { return admin(roleP) }, nil, 200},
		{"HR grants MANAGER", roleH, plain, []string{roleM, roleE}, 200},
		{"HR revokes employee", roleH, plain, nil, 200},
		{"HR grants HR_ADMIN", roleH, plain, []string{roleH}, 403},
		{"HR grants PAYROLL_ADMIN", roleH, plain, []string{roleP}, 403},
		{"HR grants OWNER", roleH, plain, []string{roleO}, 403},
		{"HR edits payroll admin", roleH, func() uuid.UUID { return admin(roleP) }, []string{roleE}, 403},
		{"HR edits another HR", roleH, func() uuid.UUID { return admin(roleH) }, []string{roleE}, 403},
		{"HR edits self", roleH, func() uuid.UUID { return e.memberOf(t, f.id, f.users[roleH]) }, []string{roleH, roleM}, 403},
		{"HR revokes owner", roleH, func() uuid.UUID { return e.memberOf(t, f.id, f.users[roleO]) }, nil, 403},
		{"payroll admin grants", roleP, plain, []string{roleM}, 403},
		{"manager revokes", roleM, plain, nil, 403},
		{"employee grants self", roleE, func() uuid.UUID { return e.memberOf(t, f.id, f.users[roleE]) }, []string{roleO}, 403},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			target := c.target()
			var r result
			var b map[string]any
			if c.roles == nil {
				r, b = e.revokeMember(t, f.tokens[c.actor], target)
			} else {
				r, b = e.setRoles(t, f.tokens[c.actor], target, 1, c.roles...)
			}
			if r.StatusCode != c.want {
				t.Fatalf("want %d, got %d %v", c.want, r.StatusCode, b)
			}
		})
	}

	// HR_ADMIN diturunkan menjadi EMPLOYEE; token HR lamanya masih berlaku ≤ 15 menit tetapi ditolak.
	hrToken := f.tokens[roleH]
	if r, b := e.setRoles(t, f.tokens[roleO], e.memberOf(t, f.id, f.users[roleH]), 1, roleE); r.StatusCode != http.StatusOK {
		t.Fatalf("demote HR: %d %v", r.StatusCode, b)
	}
	if r, b := e.revokeMember(t, hrToken, plain()); r.StatusCode != http.StatusForbidden {
		t.Fatalf("demoted HR with a stale token: want 403, got %d %v", r.StatusCode, b)
	}
}

// TestMembership_OwnerParallelRevokeKeepsOne (DoD owner_parallel_revoke_keeps_one, PI-16, 22-tx §1):
// dua OWNER saling mencabut (dan saling menurunkan) bersamaan. Di READ COMMITTED keduanya melihat
// "masih ada owner lain"; advisory lock `owners:<tenant>` menyerialkan keduanya sehingga tepat satu
// berhasil, dan tenant selalu menyisakan tepat satu owner aktif. Diulang supaya interleaving bervariasi.
func TestMembership_OwnerParallelRevokeKeepsOne(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	for i := range 10 {
		f := e.newTenantFx(t, "par"+itoa(i))
		second := f.emails[roleP]
		pMember := e.memberOf(t, f.id, f.users[roleP])
		if r, b := e.setRoles(t, f.tokens[roleO], pMember, 1, roleO); r.StatusCode != http.StatusOK {
			t.Fatalf("promote second owner: %d %v", r.StatusCode, b)
		}
		tokB, _, _ := e.login(t, second) // token dengan role OWNER
		a, bID := e.memberOf(t, f.id, f.users[roleO]), pMember
		demote := i%2 == 1

		statuses := make([]int, 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for j, call := range []struct {
			tok    string
			target uuid.UUID
		}{{f.tokens[roleO], bID}, {tokB, a}} {
			wg.Go(func() {
				<-start
				var r result
				if demote {
					r, _ = e.setRoles(t, call.tok, call.target, 2-j, roleE) // versi: B=2 (sudah dipromosikan), A=1
				} else {
					r, _ = e.revokeMember(t, call.tok, call.target)
				}
				statuses[j] = r.StatusCode
			})
		}
		close(start)
		wg.Wait()

		ok := 0
		for _, st := range statuses {
			switch st {
			case http.StatusOK:
				ok++
			case http.StatusForbidden, http.StatusConflict: // kalah: sudah bukan owner (403) atau owner terakhir (409)
			default:
				t.Fatalf("round %d: unexpected status %d (%v)", i, st, statuses)
			}
		}
		if ok != 1 || e.activeOwners(t, f.id) != 1 {
			t.Fatalf("round %d (demote=%v): want exactly one winner and one active owner, got statuses %v, owners %d",
				i, demote, statuses, e.activeOwners(t, f.id))
		}
	}
}
