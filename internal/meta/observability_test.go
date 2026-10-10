package meta

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// planned: metrik yang sengaja sudah dirujuk alert (dokumen 24 §7) tetapi baru diisi sprint berikutnya.
// Rule tetap valid (seri tidak ada = kondisi itu tidak memicu); daftar ini membuatnya eksplisit.
var planned = map[string]string{
	"payrun_payroll_total_mismatch_runs": "job verifikasi berkala (S8)",
}

// TestObservability_RulesAndDashboardsUseDefinedMetrics: setiap metrik app_/payrun_ yang dirujuk alert
// rule atau dashboard didefinisikan di internal/platform/metrics. Tanpa ini, typo nama metrik membuat
// alert diam selamanya dan panel "No data" tanpa ada yang sadar.
func TestObservability_RulesAndDashboardsUseDefinedMetrics(t *testing.T) {
	defined := map[string]bool{}
	src, err := filepath.Glob("../platform/metrics/*.go")
	if err != nil {
		t.Fatal(err)
	}
	nameRe := regexp.MustCompile(`"((?:app|payrun)_[a-z0-9_]+)"`)
	for _, f := range src {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range nameRe.FindAllStringSubmatch(string(b), -1) {
			defined[m[1]] = true
		}
	}
	files, _ := filepath.Glob("../../deploy/prometheus/rules/*.yml")
	dash, _ := filepath.Glob("../../deploy/grafana/dashboards/*.json")
	files = append(files, dash...)
	if len(files) < 3 {
		t.Fatalf("expected alert rules and dashboards, found %v", files)
	}
	useRe := regexp.MustCompile(`\b((?:app|payrun)_[a-z0-9_]+)`)
	used := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range useRe.FindAllStringSubmatch(string(b), -1) {
			name := m[1]
			for _, suf := range []string{"_bucket", "_sum", "_count"} {
				if base := strings.TrimSuffix(name, suf); base != name && defined[base] {
					name = base
				}
			}
			used++
			if !defined[name] && planned[name] == "" {
				t.Errorf("%s references undefined metric %s", filepath.Base(f), name)
			}
		}
	}
	if used == 0 {
		t.Fatal("no metric references found")
	}
}
