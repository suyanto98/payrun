// Package meta: test tentang repo itu sendiri (binary, dokumen), bukan tentang perilaku aplikasi.
package meta

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// symbols membangun ./cmd/payrun dengan tag tertentu dan mengembalikan output `go tool nm`.
func symbols(t *testing.T, tags string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "payrun")
	args := []string{"build", "-o", bin}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, "./cmd/payrun")
	build := exec.CommandContext(t.Context(), "go", args...)
	build.Dir = repoRoot(t)
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build -tags %q: %v\n%s", tags, err, out)
	}
	out, err := exec.CommandContext(t.Context(), "go", "tool", "nm", bin).Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// TestBuild_NoTesthooksInRelease: titik hook test concurrency tidak bisa dipasang di binary release.
func TestBuild_NoTesthooksInRelease(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries")
	}
	// Registry hook yang dibaca Pause di build testhooks. testhook.Set sendiri dibuang linker (hanya
	// dipanggil test), jadi tidak bisa dijadikan bukti: versi pertama test ini gagal di sanity check.
	const hookRegistry = "payrun/internal/platform/testhook.hooks"
	if strings.Contains(symbols(t, ""), hookRegistry) {
		t.Fatal("release binary contains the testhook registry")
	}
	if !strings.Contains(symbols(t, "testhooks"), hookRegistry) {
		t.Fatal("sanity: the testhooks binary should contain the testhook registry")
	}
}

// TestBuild_DemoEndpointsAbsentWithoutDemoMode (27 §9.1, DoD S8): binary release tidak memuat kode demo
// sama sekali: rute /demo/* (demoPoints), implementasi faults.Check yang membaca demo_faults, dan file SQL
// migrasi demo. Perilaku HTTP-nya diuji TestDemoMode_ReleaseBuildHasNoDemoRoutes.
func TestBuild_DemoEndpointsAbsentWithoutDemoMode(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries")
	}
	demoOnly := []string{"payrun/internal/app.demoPoints", "payrun/db/migrations_demo.files", "payrun/internal/platform/faults.exit"}
	release, demo := symbols(t, ""), symbols(t, "demo")
	for _, sym := range demoOnly {
		if strings.Contains(release, sym) {
			t.Errorf("release binary contains demo symbol %s", sym)
		}
		if !strings.Contains(demo, sym) {
			t.Errorf("sanity: the demo binary should contain %s", sym)
		}
	}
}
