package meta

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestTraceability_DocsReferenceExistingTests (docs/design/25-testing-strategy.md §5, DoD T1, M7): setiap nama
// test yang dirujuk dokumen PayRun benar-benar ada di kode. Dokumen yang menyebut test yang tidak ada adalah
// klaim palsu ("diuji oleh X") yang tidak ketahuan sampai ada yang mencarinya.
//
// Cakupan: dokumen milik repo ini (README, docs/*.md, ADR-P* dan ADR-012, laporan perf) dan bagian PayRun di
// strategi testing (katalog §4.4–4.6, invariant §5.2, baris PFS-xx di §5.3). Bagian LedgerPay di dokumen desain
// bersama sengaja tidak dicek di repo ini. Bentuk yang dikenali di dalam backtick: `TestNama`,
// `TestNama/subtest` (dicek Nama), `TestPrefix_*` (minimal satu test dengan prefix itu). Alias snake_case
// (`run_create_two_admins_one_wins`) adalah ID katalog, bukan nama Go, dan tidak dicek.
// Di baris PI §5.2, nama setelah ";" adalah rencana dan tidak dicek.
func TestTraceability_DocsReferenceExistingTests(t *testing.T) {
	root := repoRoot(t)
	defined := definedTests(t, root)

	type ref struct{ name, file string }
	var refs []ref
	add := func(file, text string) {
		for _, m := range backtickTest.FindAllStringSubmatch(text, -1) {
			refs = append(refs, ref{m[1], file})
		}
	}
	whole := []string{"README.md"}
	for _, pattern := range []string{"docs/*.md", "docs/adr/ADR-P*.md", "docs/adr/ADR-012-*.md", "perf/results/*/REPORT.md"} {
		matches, _ := filepath.Glob(filepath.Join(root, pattern))
		for _, m := range matches {
			rel, _ := filepath.Rel(root, m)
			if rel != "docs/README.md" { // indeks dokumen desain, tanpa klaim test
				whole = append(whole, rel)
			}
		}
	}
	for _, f := range whole {
		add(f, read(t, root, f))
	}
	strategy := "docs/design/25-testing-strategy.md"
	text := read(t, root, strategy)
	for _, heading := range []string{"### 4.4", "### 4.5", "### 4.6"} {
		add(strategy+" "+heading, section(text, heading))
	}
	for _, line := range strings.Split(section(text, "### 5.2"), "\n") {
		if strings.HasPrefix(line, "| PI-") {
			if i := strings.Index(line, ";"); i >= 0 {
				line = line[:i]
			}
			add(strategy+" §5.2", line)
		}
	}
	for _, line := range strings.Split(section(text, "### 5.3"), "\n") {
		if strings.HasPrefix(line, "| PFS-") {
			add(strategy+" §5.3", line)
		}
	}
	if len(refs) < 80 {
		t.Fatalf("only %d test references found: the parser is probably broken", len(refs))
	}

	var missing []string
	for _, r := range refs {
		if !exists(defined, r.name) {
			missing = append(missing, r.file+": "+r.name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("%d documented test name(s) do not exist in the code:\n  %s", len(missing), strings.Join(dedupe(missing), "\n  "))
	}
}

var (
	backtickTest = regexp.MustCompile("`(Test[A-Za-z0-9_]+(?:\\*)?)(?:/[^`]*)?`")
	funcTest     = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
)

func definedTests(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == ".git" || d.Name() == "web") {
			return filepath.SkipDir
		}
		if strings.HasSuffix(path, "_test.go") {
			b, err := os.ReadFile(path) //nolint:gosec // G304: file di dalam repo
			if err != nil {
				return err
			}
			for _, m := range funcTest.FindAllStringSubmatch(string(b), -1) {
				out[m[1]] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func exists(defined map[string]bool, name string) bool {
	if prefix, ok := strings.CutSuffix(name, "*"); ok {
		for d := range defined {
			if strings.HasPrefix(d, prefix) {
				return true
			}
		}
		return false
	}
	return defined[name]
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel)) //nolint:gosec // G304: file di dalam repo
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// section mengembalikan teks dari heading itu sampai heading berlevel sama atau lebih tinggi berikutnya.
func section(text, heading string) string {
	i := strings.Index(text, "\n"+heading)
	if i < 0 {
		return ""
	}
	rest := text[i+1:]
	level := strings.Count(strings.SplitN(heading, " ", 2)[0], "#")
	lines := strings.Split(rest, "\n")
	for n, l := range lines[1:] {
		if h := strings.SplitN(l, " ", 2)[0]; strings.Trim(h, "#") == "" && len(h) > 0 && len(h) <= level {
			return strings.Join(lines[:n+1], "\n")
		}
	}
	return rest
}

func dedupe(xs []string) []string {
	var out []string
	for i, x := range xs {
		if i == 0 || x != xs[i-1] {
			out = append(out, x)
		}
	}
	return out
}
