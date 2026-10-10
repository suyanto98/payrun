//go:build testhooks

package app_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"payrun/internal/app"
	"payrun/internal/platform/clock"
	"payrun/internal/platform/idgen"
	"payrun/internal/platform/testhook"
)

// Env proses anak payslip: mode + direktori FileStore bersama induk.
const (
	helperMode     = "PAYRUN_TEST_HELPER_MODE"
	helperStoreDir = "PAYRUN_TEST_HELPER_STORE_DIR"
)

// fileStore: storage.Store di direktori lokal, dipakai bersama proses anak (yang di-kill) dan induk.
// Setiap PUT dicatat (<key>.puts); PUT ulang dengan byte BERBEDA ditandai <key>.mismatch.
type fileStore struct{ dir string }

func (s fileStore) path(key string) string {
	return filepath.Join(s.dir, strings.ReplaceAll(key, "/", "_"))
}

func (s fileStore) Put(_ context.Context, key string, data []byte, _ string) error {
	p := s.path(key)
	if old, err := os.ReadFile(p); err == nil && !bytes.Equal(old, data) {
		if err := os.WriteFile(p+".mismatch", nil, 0o600); err != nil {
			return err
		}
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		return err
	}
	f, err := os.OpenFile(p+".puts", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString("x")
	return errors.Join(err, f.Close())
}

func (s fileStore) PresignGet(_ context.Context, key string, ttl time.Duration, _ string) (*url.URL, error) {
	return &url.URL{Scheme: "file", Path: s.path(key), RawQuery: fmt.Sprintf("expires=%d", int(ttl.Seconds()))}, nil
}

func (s fileStore) puts(key string) int {
	b, _ := os.ReadFile(s.path(key) + ".puts")
	return len(b)
}

// runKilledRenderer (proses anak): worker dengan lane documents. Slip pertama selesai normal; setelah
// PUT slip kedua, proses keluar tepat sebelum COMMIT penandaan RENDERED-nya (objek sudah ada, DB belum).
func runKilledRenderer() int {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv(helperWorkerDSN))
	if err != nil {
		return 2
	}
	defer pool.Close()
	var slips atomic.Int32
	var armed atomic.Bool
	testhook.Set("payslip.render.after_slip", func(context.Context) error {
		if slips.Add(1) == 1 {
			armed.Store(true) // commit berikutnya = MarkPayslipRendered slip kedua (setelah PUT-nya)
		}
		return nil
	})
	testhook.Set("db.before_commit", func(context.Context) error {
		if armed.Load() {
			os.Exit(killedExitCode)
		}
		return nil
	})
	deps := app.Deps{Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Pool: pool, Clock: clock.System{}, IDs: idgen.V7{},
		Storage: fileStore{dir: os.Getenv(helperStoreDir)}}
	w := app.NewWorker(deps, app.WorkerConfig{ID: "victim:" + uuid.NewString()[:8], Lease: time.Second, ChunkSize: 2})
	for range 50 {
		if n, err := w.RunOnce(ctx); err != nil || n == 0 {
			break
		}
	}
	return 0
}

// TestPayslipRender_CrashMidBatch_ResumesIdempotently (DoD S7, PFS-10): worker mati di tengah batch
// payslip.render. Setelah lease habis, worker lain melanjutkan: slip yang sudah RENDERED tidak dirender
// ulang, slip yang PUT-nya sudah terjadi ditulis ulang ke key yang sama dengan byte identik, sisanya
// dirender; checksum di DB = isi objek.
func TestPayslipRender_CrashMidBatch_ResumesIdempotently(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	run := e.approvedRun(t, f)
	if r, b := e.finalize(t, f.tokens[roleP], run, newID().String()); r.StatusCode != http.StatusOK {
		t.Fatalf("finalize: %d %v", r.StatusCode, b)
	}
	store := fileStore{dir: t.TempDir()}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperWorkerDSN+"="+e.pool.Config().ConnString(), helperMode+"=payslip", helperStoreDir+"="+store.dir)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != killedExitCode {
		t.Fatalf("worker process must die mid-batch (exit %d), got %v\n%s", killedExitCode, err, out)
	}
	type slip struct {
		key, status string
		sum         []byte
	}
	load := func() []slip {
		var out []slip
		e.inTenant(t, f.id, func(tx pgx.Tx) error {
			rows, err := tx.Query(context.Background(), `SELECT p.object_key, p.status, p.checksum_sha256 FROM payslips p
				JOIN employees e ON e.id = p.employee_id WHERE p.run_id = $1 ORDER BY p.employee_id`, run)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var s slip
				if err := rows.Scan(&s.key, &s.status, &s.sum); err != nil {
					return err
				}
				out = append(out, s)
			}
			return rows.Err()
		})
		return out
	}
	mid := load()
	if len(mid) != 3 || mid[0].status != "RENDERED" || mid[1].status != "PENDING" || mid[2].status != "PENDING" {
		t.Fatalf("after the kill: want RENDERED, PENDING, PENDING, got %+v", mid)
	}
	if store.puts(mid[1].key) != 1 || store.puts(mid[2].key) != 0 {
		t.Fatalf("after the kill: slip 2 object written once, slip 3 not yet; got %d / %d", store.puts(mid[1].key), store.puts(mid[2].key))
	}

	time.Sleep(1200 * time.Millisecond) // lease 1 dtk milik proses yang mati kedaluwarsa
	deps := e.deps
	deps.Storage = store
	w := app.NewWorker(deps, app.WorkerConfig{ID: "resume:" + newID().String()[:8], ChunkSize: 2})
	for range 20 {
		if n, err := w.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		} else if n == 0 {
			break
		}
	}
	for i, s := range load() {
		data, err := os.ReadFile(store.path(s.key))
		if err != nil {
			t.Fatalf("slip %d: object missing: %v", i+1, err)
		}
		sum := sha256.Sum256(data)
		if s.status != "RENDERED" || !bytes.Equal(s.sum, sum[:]) || !bytes.HasPrefix(data, []byte("%PDF-")) {
			t.Fatalf("slip %d after resume: %s, checksum match %v", i+1, s.status, bytes.Equal(s.sum, sum[:]))
		}
		if want := []int{1, 2, 1}[i]; store.puts(s.key) != want {
			t.Errorf("slip %d: want %d PUTs, got %d", i+1, want, store.puts(s.key))
		}
		if _, err := os.Stat(store.path(s.key) + ".mismatch"); err == nil {
			t.Errorf("slip %d: re-render produced different bytes", i+1)
		}
	}
}
