// Package meta: test tentang repo itu sendiri (binary, dokumen), bukan tentang perilaku aplikasi.
package meta

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// TestOpenAPI_MoneyFieldsAreStrings (DoD V3, ADR-P06): setiap field uang di kontrak adalah string desimal —
// schema Money, atau string nullable — dan tidak pernah number/integer (float membulatkan rupiah diam-diam).
// Respons nyata divalidasi terhadap spec ini oleh TestContract_*.
func TestOpenAPI_MoneyFieldsAreStrings(t *testing.T) {
	spec := read(t, repoRoot(t), "api/openapi.yaml")
	field := regexp.MustCompile(`^\s+(amount|base_salary|gross|deductions|net|default_amount|total_(?:gross|deductions|net)|[a-z_]+_amount):\s*(.*)$`)
	checked := 0
	lines := strings.Split(spec, "\n")
	for i, l := range lines {
		m := field.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		def := m[2]
		if def == "" && i+1 < len(lines) { // bentuk multi-baris
			def = lines[i+1]
		}
		checked++
		isString := strings.Contains(def, "schemas/Money") || regexp.MustCompile(`type: (\[\s*)?'?string`).MatchString(def)
		if !isString || strings.Contains(def, "number") || strings.Contains(def, "integer") {
			t.Errorf("openapi.yaml:%d: money field %q must be a decimal string (Money), got %s", i+1, m[1], strings.TrimSpace(def))
		}
	}
	if checked < 15 {
		t.Fatalf("only %d money fields found: the scanner is probably broken", checked)
	}
}

// TestSPA_ErrorMessagesCoverCatalogue: setiap code error di internal/platform/apperr punya pesan Indonesia di
// web/src/errors.ts, dan tidak ada pesan untuk code yang sudah tidak ada. Tanpa ini, code baru muncul di UI
// sebagai `detail` berbahasa Inggris dari server.
func TestSPA_ErrorMessagesCoverCatalogue(t *testing.T) {
	root := repoRoot(t)
	codes := map[string]bool{}
	for _, m := range regexp.MustCompile(`def\(http\.\w+, "([A-Z][A-Z_]+)"`).FindAllStringSubmatch(read(t, root, "internal/platform/apperr/apperr.go"), -1) {
		codes[m[1]] = true
	}
	ui := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s+([A-Z][A-Z_]+): '`).FindAllStringSubmatch(read(t, root, "web/src/errors.ts"), -1) {
		ui[m[1]] = true
	}
	if len(codes) < 40 {
		t.Fatalf("only %d codes parsed from apperr.go: the scanner is probably broken", len(codes))
	}
	for c := range codes {
		if !ui[c] {
			t.Errorf("error code %s has no Indonesian message in web/src/errors.ts", c)
		}
	}
	for c := range ui {
		if !codes[c] {
			t.Errorf("web/src/errors.ts has a message for %s, which is not in the error catalogue", c)
		}
	}
}
