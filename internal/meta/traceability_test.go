package meta

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestTraceability_PayRunInvariantsReferenceExistingTests (docs/design/25-testing-strategy.md §5): setiap
// baris invariant PayRun bertanda ✅ hanya boleh menyebut test yang benar-benar ada (`func TestX(` di
// suatu *_test.go). Nama setelah ";" di sel itu adalah rencana sprint berikutnya dan tidak dicek.
// Dokumen sebagai klaim yang diuji: matriks tidak bisa diam-diam basi.
func TestTraceability_PayRunInvariantsReferenceExistingTests(t *testing.T) {
	doc, err := os.ReadFile("../../docs/design/25-testing-strategy.md")
	if err != nil {
		t.Fatal(err)
	}
	defined := map[string]bool{}
	funcRe := regexp.MustCompile(`(?m)^func (Test\w+)\(`)
	if err := filepath.WalkDir("../..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == ".git" || d.Name() == "web") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range funcRe.FindAllStringSubmatch(string(src), -1) {
			defined[m[1]] = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	nameRe := regexp.MustCompile("`(Test\\w+)")
	checked := 0
	for _, line := range strings.Split(string(doc), "\n") {
		if !strings.HasPrefix(line, "| PI-") || !strings.Contains(line, "✅") {
			continue
		}
		cell := line[strings.Index(line, "✅"):]
		if i := strings.Index(cell, ";"); i >= 0 {
			cell = cell[:i]
		}
		for _, m := range nameRe.FindAllStringSubmatch(cell, -1) {
			checked++
			if !defined[m[1]] {
				t.Errorf("%s references %s, which does not exist", strings.Fields(line)[1], m[1])
			}
		}
	}
	if checked < 20 {
		t.Fatalf("expected the PayRun ✅ rows to reference at least 20 tests, found %d", checked)
	}
}
