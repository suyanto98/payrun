//go:build demo

package faults

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"

	"payrun/internal/platform/db"
)

const (
	PointChunkBeforeCommit = "payroll.calc_chunk.before_commit"
	PointPayslipAfterPut   = "payslip.render.after_put"
)

// exit dapat diganti test (proses test tidak boleh benar-benar mati).
var exit = os.Exit

// Check: pemanggilan ke-(fired+1) pada fault yang cocok (point, tenant atau semua tenant, belum kedaluwarsa)
// dihitung atomik (UPDATE … RETURNING, aman untuk beberapa worker). Fault terpicu hanya bila hitungan baru
// melewati after_n: after_n = 3 → pemanggilan ke-4 (Demo B: chunk ke-4 dari 10 karyawan per chunk).
// q harus BUKAN transaksi bisnis (pool): counter ter-commit walau proses mati sesudahnya. Error membaca
// tabel diabaikan: titik demo tidak boleh membuat jalur payroll gagal.
func Check(ctx context.Context, q db.DBTX, point string, tenant uuid.UUID) {
	var (
		fired, afterN int
		action        string
		delayMS       *int
	)
	err := q.QueryRow(ctx, `UPDATE demo_faults SET fired = fired + 1
		WHERE id = (SELECT id FROM demo_faults
		            WHERE point = $1 AND (tenant_id IS NULL OR tenant_id = $2) AND expires_at > now() AND fired < after_n + times
		            ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING fired, after_n, action, delay_ms`, point, tenant).Scan(&fired, &afterN, &action, &delayMS)
	if err != nil || fired <= afterN {
		return
	}
	slog.Default().Warn("demo.fault_fired", "point", point, "action", action, "call", fired) // log terakhir sebelum mati
	switch action {
	case "panic":
		panic("demo fault at " + point)
	case "delay":
		if delayMS != nil {
			time.Sleep(time.Duration(*delayMS) * time.Millisecond)
		}
	default:
		exit(137)
	}
}

// Enabled melaporkan apakah binary ini dibangun dengan tag `demo`.
func Enabled() bool { return true }

// SetExitForTest mengganti fungsi exit (hanya test build demo).
func SetExitForTest(fn func(int)) (restore func()) {
	old := exit
	exit = fn
	return func() { exit = old }
}
