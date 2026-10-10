package payslippdf_test

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"payrun/internal/modules/payroll/payslippdf"
)

var update = flag.Bool("update", false, "rewrite testdata/slip.golden.pdf")

func sample() payslippdf.Slip {
	d := decimal.RequireFromString
	return payslippdf.Slip{TenantName: "PT Contoh Sejahtera", Period: "2026-04", EmployeeNo: "EMP-0001", EmployeeName: "Tono Saputra",
		Position: "Staf Gudang", Department: "Operasional", BankMasked: "BCA ••••1234",
		Lines: []payslippdf.Line{
			{Name: "Gaji pokok", Earning: true, Amount: d("5000000"), Explanation: "Gaji pokok penuh 22/22 hari kerja"},
			{Name: "Koreksi", Earning: true, Amount: d("76301"), Explanation: "Koreksi periode 2026-03: lembur kurang 2 jam (ADJ-1)"},
			{Name: "BPJS Kesehatan", Earning: false, Amount: d("50000"), Explanation: "1% × Rp 5.000.000"},
			{Name: "PPh 21", Earning: false, Amount: d("25381")},
		},
		Gross: d("5076301"), Deductions: d("75381"), Net: d("5000920"), Ruleset: "ID-SIMPLIFIED 2026.1",
		FinalizedAt: time.Date(2026, 5, 2, 3, 4, 5, 0, time.UTC)}
}

// TestPayslipPDF_DeterministicBytes (PFS-10, PI-12): render ulang (retry job, worker lain) menghasilkan
// byte identik, sama dengan golden. Ubah tata letak dengan sengaja: go test ./... -run Deterministic -update.
func TestPayslipPDF_DeterministicBytes(t *testing.T) {
	a, err := payslippdf.Render(sample())
	if err != nil {
		t.Fatal(err)
	}
	b, err := payslippdf.Render(sample())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("two renders of the same slip differ")
	}
	golden := filepath.Join("testdata", "slip.golden.pdf")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, a, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("golden missing (run with -update): %v", err)
	}
	if !bytes.Equal(a, want) {
		t.Fatal("rendered PDF differs from testdata/slip.golden.pdf")
	}
	if !bytes.HasPrefix(a, []byte("%PDF-")) {
		t.Fatal("not a PDF")
	}
	other := sample()
	other.Net = decimal.RequireFromString("5000921")
	c, _ := payslippdf.Render(other)
	if bytes.Equal(a, c) {
		t.Fatal("different slips rendered identical bytes")
	}
}

func TestRupiahAndPeriodLabel(t *testing.T) {
	for in, want := range map[string]string{"0": "Rp 0", "999": "Rp 999", "1000": "Rp 1.000", "76300.58": "Rp 76.301", "-1234567": "-Rp 1.234.567"} {
		if got := payslippdf.Rupiah(decimal.RequireFromString(in)); got != want {
			t.Errorf("Rupiah(%s) = %q, want %q", in, got, want)
		}
	}
	if got := payslippdf.PeriodLabel("2026-03"); got != "Maret 2026" {
		t.Errorf("PeriodLabel = %q", got)
	}
}
