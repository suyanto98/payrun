//go:build testhooks

package app_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"payrun/internal/platform/testhook"
)

// pauseOnce: eksekusi PERTAMA yang sampai di hook berhenti sampai release dipanggil; berikutnya lewat.
// Cleanup selalu melepas jeda (sebelum server test ditutup, LIFO): test yang gagal di tengah tidak
// meninggalkan request menggantung yang membuat httptest.Server.Close menunggu selamanya.
func pauseOnce(t *testing.T, hook string) (reached chan struct{}, release func()) {
	t.Helper()
	reached, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	var first atomic.Bool
	reset := testhook.Set(hook, func(context.Context) error {
		if first.CompareAndSwap(false, true) {
			close(reached)
			<-gate
		}
		return nil
	})
	t.Cleanup(func() { release(); reset() })
	return reached, release
}

type httpOut struct {
	status int
	body   map[string]any
}

func async(f func() (result, map[string]any)) chan httpOut {
	ch := make(chan httpOut, 1)
	go func() { r, b := f(); ch <- httpOut{r.StatusCode, b} }()
	return ch
}

// blocked: belum ada hasil dalam d (permintaan sedang menunggu lock).
func blocked(ch chan httpOut, d time.Duration) bool {
	select {
	case <-ch:
		return false
	case <-time.After(d):
		return true
	}
}

// TestApprove_BlocksConcurrentInputWrite (Step 7 `22` §4.c, O3; DoD test O3): approve memegang periode
// FOR UPDATE di antara verifikasi fingerprint dan COMMIT. Koreksi gaji yang datang saat itu MENUNGGU
// (PeriodGuard FOR SHARE), lalu tercatat SETELAH approve commit — run disetujui dengan input yang memang
// dihitung, dan perubahan berikutnya tetap punya jejak di change log (finalize akan menangkapnya).
func TestApprove_BlocksConcurrentInputWrite(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	run := e.salariedRun(t, f)
	apID := e.submitRun(t, f.tokens[roleP], run, map[string]any{})

	reached, release := pauseOnce(t, "payroll.approve.before_update")
	approve := async(func() (result, map[string]any) { return e.approve(t, f.tokens[roleO], apID) })
	<-reached
	writer := async(func() (result, map[string]any) {
		return e.doH(t, http.MethodPost, "/api/v1/employees/"+f.empOther.String()+"/salary-changes", f.tokens[roleH], key(),
			map[string]any{"base_salary": "6000000.00", "valid_from": "2026-03-15"})
	})
	if !blocked(writer, 400*time.Millisecond) {
		t.Fatal("an input write must wait while approve holds the period lock (O3)")
	}
	release()
	if a := <-approve; a.status != http.StatusOK {
		t.Fatalf("approve: %d %v", a.status, a.body)
	}
	if w := <-writer; w.status != http.StatusCreated {
		t.Fatalf("writer after approve: %d %v", w.status, w.body)
	}
	if _, rs := e.approvalStatus(t, f.id, apID); rs != "APPROVED" {
		t.Fatalf("run must be APPROVED with the calculated input, got %s", rs)
	}
	if _, ok := e.inputChanges(t, f.id, run)[f.empOther]; !ok {
		t.Fatal("the write after approve must leave a change-log row for finalize to catch")
	}
}

// TestApprove_StaleDetectedForWriterCommittedAfterChunkRead (§4.d1, O12): penulis mengambil lock
// SETELAH calculate commit, mencatat changed_at > calc_started_at, tetapi baru COMMIT setelah chunk
// membaca input lama. Hasil kalkulasi tidak memuat perubahannya → approve wajib RUN_STALE.
func TestApprove_StaleDetectedForWriterCommittedAfterChunkRead(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	for _, emp := range []string{f.empSelf.String(), f.empOther.String(), f.empManager.String()} {
		e.doH(t, http.MethodPost, "/api/v1/employees/"+emp+"/salary-changes", f.tokens[roleO], key(), map[string]any{"base_salary": "5000000.00", "valid_from": "2026-01-05"})
	}
	p := f.tokens[roleP]
	run := e.createRun(t, p, "2026-03")
	if r, b := e.doH(t, http.MethodPost, "/api/v1/payroll-runs/"+run.String()+"/calculate", p, key(), nil); r.StatusCode != http.StatusAccepted {
		t.Fatalf("calculate: %d %v", r.StatusCode, b)
	}

	reached, release := pauseOnce(t, "periodguard.after_record")
	writer := async(func() (result, map[string]any) {
		return e.doH(t, http.MethodPost, "/api/v1/employees/"+f.empOther.String()+"/salary-changes", f.tokens[roleH], key(),
			map[string]any{"base_salary": "7000000.00", "valid_from": "2026-03-01"})
	})
	<-reached      // W: lock + changed_at diambil, belum commit
	e.drainJobs(t) // chunk membaca input TANPA perubahan W dan menyelesaikan run
	release()      // W commit
	if w := <-writer; w.status != http.StatusCreated {
		t.Fatalf("writer: %d %v", w.status, w.body)
	}
	if s := e.runState(t, f.id, run); s.status != "CALCULATED" {
		t.Fatalf("run: %+v", s)
	}
	apID := e.submitRun(t, p, run, map[string]any{})
	r, b := e.approve(t, f.tokens[roleO], apID)
	if r.StatusCode != http.StatusConflict || b["code"] != "RUN_STALE" {
		t.Fatalf("writer committed after the chunk read: want RUN_STALE, got %d %v", r.StatusCode, b)
	}
	if ch := b["employees_changed"].([]any); len(ch) != 1 || ch[0].(map[string]any)["employee_id"] != f.empOther.String() {
		t.Fatalf("employees_changed: %v", ch)
	}
}

// TestCreateRun_WaitsForInputWriterOfMissingPeriod (§4.d2, D1): penulis input untuk periode yang BELUM
// ada memegang tenant_settings FOR KEY SHARE (tanpa baris periode untuk dicatat). Pembuatan periode
// (create run, tenant_settings FOR UPDATE) wajib menunggu penulis itu, sehingga kalkulasi selalu melihat
// perubahannya — tanpa D1 perubahan itu tidak terlihat dan juga tidak punya change log.
func TestCreateRun_WaitsForInputWriterOfMissingPeriod(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	for _, emp := range []string{f.empSelf.String(), f.empOther.String(), f.empManager.String()} {
		e.doH(t, http.MethodPost, "/api/v1/employees/"+emp+"/salary-changes", f.tokens[roleO], key(), map[string]any{"base_salary": "5000000.00", "valid_from": "2026-01-05"})
	}
	reached, release := pauseOnce(t, "periodguard.after_lock")
	writer := async(func() (result, map[string]any) {
		return e.doH(t, http.MethodPost, "/api/v1/employees/"+f.empOther.String()+"/salary-changes", f.tokens[roleH], key(),
			map[string]any{"base_salary": "7000000.00", "valid_from": "2026-04-01"})
	})
	<-reached
	create := async(func() (result, map[string]any) {
		return e.doH(t, http.MethodPost, "/api/v1/payroll-runs", f.tokens[roleP], key(), map[string]any{"period": "2026-04"})
	})
	if !blocked(create, 400*time.Millisecond) {
		t.Fatal("creating the period must wait for the writer holding tenant_settings KEY SHARE (D1)")
	}
	release()
	if w := <-writer; w.status != http.StatusCreated {
		t.Fatalf("writer: %d %v", w.status, w.body)
	}
	c := <-create
	if c.status != http.StatusCreated {
		t.Fatalf("create run: %d %v", c.status, c.body)
	}
	run := c.body["id"].(string)
	if r, b := e.doH(t, http.MethodPost, "/api/v1/payroll-runs/"+run+"/calculate", f.tokens[roleP], key(), nil); r.StatusCode != http.StatusAccepted {
		t.Fatalf("calculate: %d %v", r.StatusCode, b)
	}
	e.drainJobs(t)
	var gross string
	for _, it := range e.runItems(t, f.tokens[roleP], uuidMust(run)) {
		if it["employee_id"] == f.empOther.String() {
			gross = it["gross"].(string)
		}
	}
	if gross == "" || decimal.RequireFromString(gross).LessThan(decimal.RequireFromString("7000000.00")) {
		t.Fatalf("the calculation must see the writer's April salary, got gross %q", gross)
	}
	apID := e.submitRun(t, f.tokens[roleP], uuidMust(run), map[string]any{})
	if r, b := e.approve(t, f.tokens[roleO], apID); r.StatusCode != http.StatusOK {
		t.Fatalf("no stale input: approve want 200, got %d %v", r.StatusCode, b)
	}
}

// TestFinalize_RetryAfterUnknownCommit_Idempotent (DoD S6, PFS-04/FS-03): COMMIT finalize berhasil tetapi
// respons tidak sampai (hook db.after_commit). Klien mengulang dengan Idempotency-Key yang sama → replay 200
// yang identik, tanpa efek kedua; key baru → 409 RUN_FINALIZED.
func TestFinalize_RetryAfterUnknownCommit_Idempotent(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	run := e.approvedRun(t, f)
	var fired atomic.Bool
	t.Cleanup(testhook.Set("db.after_commit", func(context.Context) error {
		if fired.CompareAndSwap(false, true) {
			return errors.New("injected: connection lost after COMMIT")
		}
		return nil
	}))
	k := newID().String()
	r1, b1 := e.finalize(t, f.tokens[roleP], run, k)
	if r1.StatusCode < 500 {
		t.Fatalf("lost response: want 5xx, got %d %v", r1.StatusCode, b1)
	}
	if st, _ := e.periodStatus(t, f.id, run); st != "FINALIZED" {
		t.Fatalf("the commit happened: period must be FINALIZED, got %s", st)
	}
	r2, b2 := e.finalize(t, f.tokens[roleP], run, k)
	if r2.StatusCode != http.StatusOK || r2.Header.Get("Idempotent-Replayed") != "true" || b2["status"] != "FINALIZED" {
		t.Fatalf("retry with the same key: want replayed 200, got %d %v %v", r2.StatusCode, r2.Header, b2)
	}
	if n := e.auditCount(t, f.id, "payroll_run.finalized"); n != 1 {
		t.Fatalf("exactly one finalize effect, got %d audit rows", n)
	}
	if r3, b3 := e.finalize(t, f.tokens[roleP], run, newID().String()); r3.StatusCode != http.StatusConflict || b3["code"] != "RUN_FINALIZED" {
		t.Fatalf("new key: want 409 RUN_FINALIZED, got %d %v", r3.StatusCode, b3)
	}
}
