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
var planned = map[string]string{}

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

// TestObservability_AlertsLinkToRunbook (DoD OP4): setiap alert punya annotation runbook yang menunjuk ke
// heading yang ADA di docs/runbook.md. Link mati ditemukan saat alert menyala jam 3 pagi; di sini lebih murah.
func TestObservability_AlertsLinkToRunbook(t *testing.T) {
	book, err := os.ReadFile("../../docs/runbook.md")
	if err != nil {
		t.Fatal(err)
	}
	anchors := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^#{2,4} (.+)$`).FindAllStringSubmatch(string(book), -1) {
		a := strings.ToLower(strings.TrimSpace(m[1])) // heading alert = nama alert (tanpa spasi/tanda baca)
		anchors[regexp.MustCompile(`[^a-z0-9 -]`).ReplaceAllString(strings.ReplaceAll(a, " ", "-"), "")] = true
	}
	rules, _ := filepath.Glob("../../deploy/prometheus/rules/*.yml")
	alertRe := regexp.MustCompile(`(?m)^\s*- alert: (\w+)$`)
	linkRe := regexp.MustCompile(`runbook: "docs/runbook\.md#([a-z0-9-]+)"`)
	checked := 0
	for _, f := range rules {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// Setiap blok alert (dari "- alert:" sampai alert berikutnya) harus memuat satu link runbook yang valid.
		idx := alertRe.FindAllStringSubmatchIndex(string(b), -1)
		for i, m := range idx {
			end := len(b)
			if i+1 < len(idx) {
				end = idx[i+1][0]
			}
			name, block := string(b[m[2]:m[3]]), string(b[m[0]:end])
			link := linkRe.FindStringSubmatch(block)
			switch {
			case link == nil:
				t.Errorf("%s: alert %s has no runbook annotation pointing to docs/runbook.md", filepath.Base(f), name)
			case !anchors[link[1]]:
				t.Errorf("%s: alert %s links to docs/runbook.md#%s, which has no such heading", filepath.Base(f), name, link[1])
			}
			checked++
		}
	}
	if checked < 10 {
		t.Fatalf("expected at least 10 alerts, found %d", checked)
	}
}
