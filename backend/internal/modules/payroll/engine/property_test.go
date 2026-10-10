package engine

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

// Property test (§17): input acak, seed tetap (kegagalan bisa direproduksi dari seed di pesan).
// Invariant yang dicek di SETIAP iterasi:
//   - Net == Σ earning − Σ deduction (PI-05), Gross/Deductions = Σ per kategori;
//   - determinisme: Calculate(x) dua kali menghasilkan JSON identik (PI-12);
//   - setiap baris bilangan bulat rupiah dan ≥ 0;
//   - prorata ∈ [0, 1]: Σ baris BASE_SALARY ≤ gaji tertinggi;
//   - status ERROR ⇔ ada Errors.
const propertyIterations = 10000

func randomInput(r *rand.Rand) Input {
	in := march()
	year, month := 2025+r.IntN(3), 1+r.IntN(12)
	start := NewDate(year, timeMonth(month), 1)
	end := NewDate(year, timeMonth(month), 1).AddDays(40)
	end = NewDate(end.Year(), end.Month(), 1)
	in.Period = DateRange{From: start, To: &end}
	days := int(end.Time().Sub(start.Time()).Hours() / 24)
	pick := func() Date { return start.AddDays(r.IntN(days+6) - 3) } // kadang di luar periode

	if r.IntN(4) == 0 {
		in.Policy.ProrationBasis = "CALENDAR_DAYS"
	}
	if r.IntN(3) == 0 {
		in.Policy.IncompletePolicy = "COUNT_AS_ABSENT"
	}
	if r.IntN(3) == 0 {
		in.Policy.OvertimeBasis = "BASE_PLUS_FIXED"
	}
	in.TaxCategory = []string{"A", "B", "C"}[r.IntN(3)]
	for range r.IntN(3) {
		in.Holidays = append(in.Holidays, pick())
	}
	in.Employment = nil
	for range 1 + r.IntN(2) {
		a := pick()
		if r.IntN(2) == 0 {
			in.Employment = append(in.Employment, DateRange{From: a.AddDays(-400)})
		} else {
			b := a.AddDays(1 + r.IntN(25))
			in.Employment = append(in.Employment, DateRange{From: a, To: &b})
		}
	}
	in.Salary = nil
	cut := pick()
	amount := func() decimal.Decimal { return decimal.NewFromInt(int64(r.IntN(30_000_000))) }
	if r.IntN(2) == 0 {
		in.Salary = []SalarySpan{{Range: DateRange{From: start.AddDays(-500)}, BaseSalary: amount()}}
	} else {
		in.Salary = []SalarySpan{{Range: DateRange{From: start.AddDays(-500), To: &cut}, BaseSalary: amount()},
			{Range: DateRange{From: cut}, BaseSalary: amount()}}
	}
	if r.IntN(2) == 0 {
		in.Components = append(in.Components, earning("TUNJ", "Tunjangan", Fixed, func(c *ComponentDef) {
			c.DefaultAmount = dp(fmt.Sprint(r.IntN(3_000_000)))
			c.BPJSBase = r.IntN(2) == 0
		}))
	}
	if r.IntN(2) == 0 {
		in.Components = append(in.Components, earning("MAKAN", "Uang makan", PerDay, func(c *ComponentDef) {
			c.DefaultAmount = dp(fmt.Sprint(r.IntN(100_000)))
			c.DaySource = []string{"PRESENT", "WORKING"}[r.IntN(2)]
		}))
	}
	if r.IntN(3) == 0 {
		in.Components = append(in.Components, earning("POT_KOPERASI", "Potongan koperasi", Fixed, func(c *ComponentDef) {
			c.Category = Deduction
			c.Taxable = false
			c.DefaultAmount = dp(fmt.Sprint(r.IntN(500_000)))
		}))
	}
	statuses := []AttendanceStatus{AbsentUnexcused, PaidLeave, UnpaidLeave, IncompleteRecord}
	for range r.IntN(5) {
		in.Attendance = append(in.Attendance, DayStatus{Date: pick(), Status: statuses[r.IntN(len(statuses))]})
	}
	for range r.IntN(4) {
		h := decimal.NewFromInt(int64(r.IntN(9))).Add(decimal.NewFromInt(int64(r.IntN(4))).Div(decimal.NewFromInt(4)))
		in.Overtime = append(in.Overtime, OvertimeEntry{Date: pick(), Hours: h})
	}
	for i := range r.IntN(3) {
		amt := decimal.NewFromInt(int64(r.IntN(2_000_000) - 800_000)).Add(decimal.New(int64(r.IntN(100)), -2))
		in.Adjustments = append(in.Adjustments, Adjustment{ID: fmt.Sprintf("ADJ-%d", i), OriginPeriod: "2025-12", Amount: amt, Taxable: r.IntN(2) == 0, Reason: "acak"})
	}
	return in
}

func TestProperty_Invariants(t *testing.T) {
	rs := loadRuleset(t)
	const seed = 20261007
	r := rand.New(rand.NewPCG(seed, seed))
	for i := range propertyIterations {
		in := randomInput(r)
		res := Calculate(in, rs)
		fail := func(format string, args ...any) {
			raw, _ := json.Marshal(in)
			t.Fatalf("iteration %d (seed %d): %s\ninput: %s", i, seed, fmt.Sprintf(format, args...), raw)
		}
		gross, ded := zero, zero
		maxSalary := zero
		for _, s := range in.Salary {
			maxSalary = decimal.Max(maxSalary, s.BaseSalary)
		}
		base := zero
		for _, l := range res.Lines {
			if !l.Amount.Equal(l.Amount.Truncate(0)) || l.Amount.IsNegative() {
				fail("line %s amount %s is not a non-negative whole rupiah", l.Code, l.Amount)
			}
			if l.Category == Earning {
				gross = gross.Add(l.Amount)
			} else {
				ded = ded.Add(l.Amount)
			}
			if l.Code == "BASE_SALARY" {
				base = base.Add(l.Amount)
			}
		}
		if !res.Gross.Equal(gross) || !res.Deductions.Equal(ded) || !res.Net.Equal(gross.Sub(ded)) {
			fail("totals: gross %s/%s deductions %s/%s net %s", res.Gross, gross, res.Deductions, ded, res.Net)
		}
		if base.GreaterThan(maxSalary.Add(decimal.NewFromInt(1))) {
			fail("proration factor > 1: base lines %s > salary %s", base, maxSalary)
		}
		if (res.Status == StatusError) != (len(res.Errors) > 0) {
			fail("status %s with errors %v", res.Status, res.Errors)
		}
		a, _ := json.Marshal(res)
		b, _ := json.Marshal(Calculate(in, rs))
		if string(a) != string(b) {
			fail("non-deterministic result")
		}
	}
}

// TestEligibility_JoinerLeaverIncludedExactly (DoD S3, PI-07): masuk run ⇔ aktif minimal satu hari
// di periode [1 Mar, 1 Apr). Batas half-open diuji di kedua sisi.
func TestEligibility_JoinerLeaverIncludedExactly(t *testing.T) {
	period := rng("2026-03-01", "2026-04-01")
	for name, c := range map[string]struct {
		emp  []DateRange
		want bool
	}{
		"left the day before the period (last day 28 Feb)": {[]DateRange{rng("2025-01-01", "2026-03-01")}, false},
		"left on 2 Mar (last day 1 Mar, a Sunday)":         {[]DateRange{rng("2025-01-01", "2026-03-02")}, true},
		"joins on the last day of the period":              {[]DateRange{rng("2026-03-31", "")}, true},
		"joins the day after the period":                   {[]DateRange{rng("2026-04-01", "")}, false},
		"joined and left before the period":                {[]DateRange{rng("2026-01-05", "2026-02-10")}, false},
		"rehired inside the period":                        {[]DateRange{rng("2025-01-01", "2026-02-01"), rng("2026-03-20", "")}, true},
		"active the whole period":                          {[]DateRange{rng("2020-01-01", "")}, true},
		"no employment":                                    {nil, false},
	} {
		if got := Eligible(period, c.emp); got != c.want {
			t.Errorf("%s: Eligible = %v, want %v", name, got, c.want)
		}
		// Calculate harus sepakat dengan Eligible (engine memverifikasi ulang hasil plan).
		in := march()
		in.Employment = c.emp
		res := Calculate(in, loadRuleset(t))
		notEligible := len(res.Errors) == 1 && res.Errors[0].Code == ErrNotEligible
		if notEligible == c.want {
			t.Errorf("%s: Calculate disagrees with Eligible (errors %v)", name, res.Errors)
		}
	}
}

func TestValidateGraph(t *testing.T) {
	pct := func(code, ref string, stage Stage) ComponentDef {
		return ComponentDef{Code: code, CalcType: PercentOf, BaseRef: ref, Stage: stage, Active: true}
	}
	fixed := ComponentDef{Code: "TUNJ", CalcType: Fixed, Stage: StageEarning, Active: true}
	order, p := ValidateGraph([]ComponentDef{pct("C", "B", StageEarning), pct("B", "TUNJ", StageEarning), fixed})
	if p != nil || strings.Join(order, ",") != "TUNJ,B,C" {
		t.Fatalf("chain: order %v problem %+v", order, p)
	}
	if _, p := ValidateGraph([]ComponentDef{pct("A", "B", StageEarning), pct("B", "C", StageEarning), pct("C", "A", StageEarning)}); p == nil || p.Code != ProblemCycle || len(p.Cycle) != 4 {
		t.Fatalf("3-cycle: %+v", p)
	}
	if _, p := ValidateGraph([]ComponentDef{pct("A", "MISSING", StageEarning)}); p == nil || p.Code != ProblemBadRef {
		t.Fatalf("missing ref: %+v", p)
	}
	inactive := fixed
	inactive.Active = false
	if _, p := ValidateGraph([]ComponentDef{pct("A", "TUNJ", StageEarning), inactive}); p == nil || p.Code != ProblemBadRef {
		t.Fatalf("ref to inactive: %+v", p)
	}
	if _, p := ValidateGraph([]ComponentDef{pct("A", "PPH", StageEarning), {Code: "PPH", CalcType: Fixed, Stage: StageTax, Active: true}}); p == nil || p.Code != ProblemBadRef {
		t.Fatalf("ref to a later stage: %+v", p)
	}
	if _, p := ValidateGraph([]ComponentDef{pct("A", TaxableGrossRef, StageEarning)}); p == nil || p.Code != ProblemBadRef {
		t.Fatalf("earning based on TAXABLE_GROSS: %+v", p)
	}
}

// TestEngine_NoFloat: engine bebas float, jam sistem, dan angka acak (ADR-P06, PI-12). depguard
// sudah melarang import db/net/os/math/rand/clock; test ini menutup yang tidak bisa ditangkap
// depguard: tipe float dan time.Now.
func TestEngine_NoFloat(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				if x.Name == "float32" || x.Name == "float64" {
					t.Errorf("%s: float type in the payroll engine", fset.Position(x.Pos()))
				}
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok && id.Name == "time" && x.Sel.Name == "Now" {
					t.Errorf("%s: time.Now in the payroll engine", fset.Position(x.Pos()))
				}
			}
			return true
		})
	}
}
