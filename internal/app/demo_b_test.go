//go:build testhooks

package app_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestPayrollRun_CrashAndResume: Demo B (docs/design/33-demo-script.md §2 7:00–8:40, §6.1; DoD M6)
// sebagai test, sampai demo toolbar ada (S8). PT Alpha 100 karyawan, chunk demo 10 (O5):
//
//  1. Proses worker sungguhan mati (os.Exit) tepat sebelum COMMIT chunk ke-4 → run tetap CALCULATING
//     dengan 3/10 chunk; chunk 4 tidak setengah tersimpan.
//  2. Setelah lease habis worker lain melanjutkan → CALCULATED, 100 item (bukan 110), satu item dan satu
//     snapshot per karyawan, total run = Σ item, dan setiap item bisa dihitung ulang dari snapshot.
//  3. Bonus "run ganda": dua admin membuat run periode yang sama → satu 201, satu 409 ACTIVE_RUN_EXISTS.
func TestPayrollRun_CrashAndResume(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	o := f.tokens[roleO]
	for _, emp := range []string{f.empSelf.String(), f.empOther.String(), f.empManager.String()} {
		e.doH(t, http.MethodPost, "/api/v1/employees/"+emp+"/salary-changes", o, key(), map[string]any{"base_salary": "6000000.00", "valid_from": "2026-01-05"})
	}
	for i := range 97 {
		body := employeeBody(fmt.Sprintf("EMP-%04d", 100+i), fmt.Sprintf("Karyawan %d", i), fmt.Sprintf("317402%010d", 1000+i))
		body["salary"] = map[string]any{"base_salary": fmt.Sprintf("%d.00", 4500000+i*25000)}
		if r, b := e.doH(t, http.MethodPost, "/api/v1/employees", o, key(), body); r.StatusCode != http.StatusCreated {
			t.Fatalf("seed employee %d: %d %v", i, r.StatusCode, b)
		}
	}
	p := f.tokens[roleP]
	run := e.createRun(t, p, "2026-03")
	if r, b := e.doH(t, http.MethodPost, "/api/v1/payroll-runs/"+run.String()+"/calculate", p, key(), nil); r.StatusCode != http.StatusAccepted {
		t.Fatalf("calculate: %d %v", r.StatusCode, b)
	}

	// 1. Crash: worker proses anak, chunk 10, mati di chunk ke-4.
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperWorkerDSN+"="+e.pool.Config().ConnString(), helperKillChunk+"=4", helperChunkSize+"=10")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != killedExitCode {
		t.Fatalf("worker must die before committing chunk 4 (exit %d), got %v\n%s", killedExitCode, err, out)
	}
	mid := e.runState(t, f.id, run)
	if mid.status != "CALCULATING" || mid.chunksDone != 3 || mid.chunksTotal != 10 || mid.items != 30 || mid.snaps != 30 {
		t.Fatalf("after the crash: want CALCULATING with chunks 3/10 and 30 items, got %+v", mid)
	}

	// 2. Resume setelah lease (1 dtk) worker yang mati habis.
	time.Sleep(1200 * time.Millisecond)
	e.drainJobs(t)
	s := e.runState(t, f.id, run)
	if s.status != "CALCULATED" || s.chunksDone != 10 || s.items != 100 || s.employees != 100 || s.snaps != 100 {
		t.Fatalf("after resume: want CALCULATED, 10/10 chunks, 100 unique items, got %+v", s)
	}
	if !s.net.Decimal.Equal(s.sumNet.Decimal) || !s.gross.Decimal.Equal(s.sumGross.Decimal) {
		t.Fatalf("run total must equal the sum of items: %s vs %s", s.net.Decimal, s.sumNet.Decimal)
	}
	if _, got := e.do(t, http.MethodGet, "/api/v1/payroll-runs/"+run.String(), p, nil); got["counts"].(map[string]any)["ok"] != float64(100) {
		t.Fatalf("counts.ok must be 100: %v", got["counts"])
	}
	var attempts int
	e.inTenant(t, f.id, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT attempts FROM jobs WHERE kind = 'payroll.calc_chunk' AND payload->>'chunk_no' = '4'`).Scan(&attempts)
	})
	if attempts != 2 {
		t.Fatalf("chunk 4: want 2 attempts (crashed + resumed), got %d", attempts)
	}
	if rep := e.verifyRun(t, f.id, run); !rep.OK() || rep.ItemsChecked != 100 {
		t.Fatalf("every item must be reproducible from its snapshot: %+v", rep)
	}

	// 3. Run ganda: dua admin, key berbeda, periode yang sama.
	statuses, bodies := make([]int, 2), make([]map[string]any, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, tok := range []string{o, p} {
		wg.Go(func() {
			<-start
			r, b := e.doH(t, http.MethodPost, "/api/v1/payroll-runs", tok, key(), map[string]any{"period": "2026-04"})
			statuses[i], bodies[i] = r.StatusCode, b
		})
	}
	close(start)
	wg.Wait()
	created, conflict := -1, -1
	for i, st := range statuses {
		switch st {
		case http.StatusCreated:
			created = i
		case http.StatusConflict:
			conflict = i
		}
	}
	if created < 0 || conflict < 0 || bodies[conflict]["code"] != "ACTIVE_RUN_EXISTS" || bodies[conflict]["existing_resource_id"] != bodies[created]["id"] {
		t.Fatalf("double run: want one 201 and one 409 ACTIVE_RUN_EXISTS pointing at it, got %v %v", statuses, bodies)
	}
}
