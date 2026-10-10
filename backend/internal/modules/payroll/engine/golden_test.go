package engine

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/shopspring/decimal"
)

// Golden test (Step 3 `06` §17). Setiap kasus punya:
//   - input yang dibangun di Go (mudah dibaca dan dibandingkan dengan hitungan manual di komentar);
//   - ekspektasi EKSPLISIT (net, status, kode issue, baris kunci) yang dihitung manual: golden file
//     tidak dipercaya buta, karena golden yang salah hanya mengunci bug;
//   - golden file lengkap (baris + explanation) di testdata/cases/<nama>.golden.json;
//   - recompute: testdata/cases/<nama>.input.json (snapshot) → Calculate → identik dengan golden.
//     Ini membuktikan snapshot input cukup untuk mereproduksi hasil (PI-12).
//
// Regenerasi setelah perubahan engine yang DISENGAJA: go test ./internal/modules/payroll/engine -update
// lalu tinjau diff golden di PR.

var update = flag.Bool("update", false, "rewrite golden and input snapshot files")

func d(s string) decimal.Decimal   { return decimal.RequireFromString(s) }
func dp(s string) *decimal.Decimal { v := d(s); return &v }
func dt(s string) *Date            { v := MustDate(s); return &v }
func rng(from, to string) DateRange {
	r := DateRange{From: MustDate(from)}
	if to != "" {
		r.To = dt(to)
	}
	return r
}

func loadRuleset(t testing.TB) Ruleset {
	t.Helper()
	raw, err := os.ReadFile("testdata/ruleset_ID-SIMPLIFIED_2026.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var rs Ruleset
	if err := json.Unmarshal(raw, &rs); err != nil {
		t.Fatal(err)
	}
	return rs
}

func statutoryDefs() []ComponentAssignment {
	var out []ComponentAssignment
	for _, c := range []struct{ code, name string }{
		{"BPJS_JHT_EE", "BPJS JHT (karyawan)"}, {"BPJS_JP_EE", "BPJS JP (karyawan)"}, {"BPJS_KES_EE", "BPJS Kesehatan (karyawan)"},
	} {
		out = append(out, ComponentAssignment{Range: rng("2026-01-01", ""), Def: ComponentDef{
			Code: c.code, Name: c.name, Category: Deduction, Stage: StageStatutory, CalcType: PercentOf,
			RulesetRef: c.code, BaseRef: BPJSWageRef, Rounding: HalfUp, Active: true}})
	}
	return out
}

// march: input dasar Maret 2026 (1 Maret = Minggu; Senin–Jumat = 22 hari kerja), gaji Rp5.000.000
// sepanjang periode, BPJS aktif, kategori pajak A, prorata hari kerja.
func march() Input {
	return Input{
		SnapshotVersion: 1, EmployeeID: "00000000-0000-7000-8000-000000000001",
		Period: rng("2026-03-01", "2026-04-01"), WorkDays: []int{1, 2, 3, 4, 5}, Holidays: []Date{},
		Employment: []DateRange{rng("2025-01-01", "")},
		Salary:     []SalarySpan{{Range: rng("2025-01-01", ""), BaseSalary: d("5000000")}},
		Components: statutoryDefs(), Attendance: []DayStatus{}, Overtime: []OvertimeEntry{}, Adjustments: []Adjustment{},
		TaxCategory: "A",
		Policy:      Policy{ProrationBasis: "WORKING_DAYS", OvertimeBasis: "BASE", IncompletePolicy: "COUNT_AS_PRESENT", OvertimeMaxHoursPerDay: dp("4")},
	}
}

func earning(code, name string, ct CalcType, opts func(*ComponentDef)) ComponentAssignment {
	def := ComponentDef{Code: code, Name: name, Category: Earning, Stage: StageEarning, CalcType: ct, Taxable: true, Rounding: HalfUp, Active: true}
	if opts != nil {
		opts(&def)
	}
	return ComponentAssignment{Def: def, Range: rng("2025-01-01", "")}
}

func raise(in Input, spans ...SalarySpan) Input { in.Salary = spans; return in }

type expect struct {
	net      string
	status   ItemStatus
	errors   []string
	warnings []string
	lines    map[string]string // Σ amount per code (baris dengan code sama dijumlahkan)
}

type goldenCase struct {
	name  string
	input func() Input
	want  expect
}

func goldenCases() []goldenCase {
	return []goldenCase{
		{"full_month_basic", march, expect{net: "4800000", // 5.000.000 − (100.000 + 50.000 + 50.000); pajak 0 (≤ 5,4 jt)
			lines: map[string]string{"BASE_SALARY": "5000000", "BPJS_JHT_EE": "100000", "BPJS_JP_EE": "50000", "BPJS_KES_EE": "50000"}}},

		{"tono_march_2026", tono, expect{net: "6888609", // Step 3 `06` §15
			lines: map[string]string{"BASE_SALARY": "6300000", "TUNJ_JABATAN": "500000", "TRANSPORT": "380000", "OVERTIME": "419653",
				"ABSENCE_UNPAID_LEAVE": "330000", "BPJS_JHT_EE": "136000", "BPJS_JP_EE": "68000", "BPJS_KES_EE": "68000", "PPH21": "109044"}}},

		{"joiner_mid_month_working_days", func() Input { // masuk Senin 16 Mar: 12/22 hari kerja
			in := march()
			in.Employment = []DateRange{rng("2026-03-16", "")}
			return in
		}, expect{net: "2618182", // 2.727.273 − (54.545 + 27.273 + 27.273)
			lines: map[string]string{"BASE_SALARY": "2727273", "BPJS_JHT_EE": "54545", "BPJS_JP_EE": "27273"}}},

		{"joiner_mid_month_calendar_days", func() Input { // 16/31 hari kalender
			in := march()
			in.Employment = []DateRange{rng("2026-03-16", "")}
			in.Policy.ProrationBasis = "CALENDAR_DAYS"
			return in
		}, expect{net: "2477420", // 2.580.645 − (51.613 + 25.806 + 25.806)
			lines: map[string]string{"BASE_SALARY": "2580645", "BPJS_JHT_EE": "51613", "BPJS_JP_EE": "25806"}}},

		{"joiner_on_weekend", func() Input { // masuk Sabtu 14 Mar: faktor dihitung mulai Senin 16 → sama dengan joiner Senin
			in := march()
			in.Employment = []DateRange{rng("2026-03-14", "")}
			return in
		}, expect{net: "2618182", lines: map[string]string{"BASE_SALARY": "2727273"}}},

		{"leaver_mid_month", func() Input { // aktif sampai Kamis 19 Mar (span [.., 20)): 14/22
			in := march()
			in.Employment = []DateRange{rng("2025-01-01", "2026-03-20")}
			return in
		}, expect{net: "3054546", // 3.181.818 − (63.636 + 31.818 + 31.818)
			lines: map[string]string{"BASE_SALARY": "3181818"}}},

		{"join_and_leave_same_month", func() Input { // [9 Mar, 21 Mar): 10/22
			in := march()
			in.Employment = []DateRange{rng("2026-03-09", "2026-03-21")}
			return in
		}, expect{net: "2181818", // 2.272.727 − (45.455 + 22.727 + 22.727)
			lines: map[string]string{"BASE_SALARY": "2272727", "BPJS_JHT_EE": "45455"}}},

		{"rehire_two_spans", func() Input { // [1, 7) = 5 hari kerja; [23, ∞) = 7 hari kerja → dua baris
			in := march()
			in.Employment = []DateRange{rng("2026-03-01", "2026-03-07"), rng("2026-03-23", "")}
			return in
		}, expect{net: "2618182", // 1.136.364 + 1.590.909 = 2.727.273
			lines: map[string]string{"BASE_SALARY": "2727273"}}},

		{"raise_mid_month_two_segments", raiseMid, expect{net: "5061818", // 2.272.727 + 3.000.000; BPJS 105.455 + 52.727 + 52.727
			lines: map[string]string{"BASE_SALARY": "5272727", "BPJS_JHT_EE": "105455", "BPJS_JP_EE": "52727"}}},

		{"two_raises_three_segments", func() Input {
			return raise(march(),
				SalarySpan{Range: rng("2025-01-01", "2026-03-09"), BaseSalary: d("4000000")}, // 5 hari → 909.091
				SalarySpan{Range: rng("2026-03-09", "2026-03-23"), BaseSalary: d("5000000")}, // 10 hari → 2.272.727
				SalarySpan{Range: rng("2026-03-23", ""), BaseSalary: d("6000000")})           // 7 hari → 1.909.091
		}, expect{net: "4887273", // 5.090.909 − (101.818 + 50.909 + 50.909)
			lines: map[string]string{"BASE_SALARY": "5090909", "BPJS_JHT_EE": "101818"}}},

		{"overtime_tiers_single_day", func() Input { // 3 jam: 1×1,5 + 2×2 = 5,5 × 5.000.000/173 = 158.959,54
			in := march()
			in.Overtime = []OvertimeEntry{{Date: MustDate("2026-03-10"), Hours: d("3")}}
			return in
		}, expect{net: "4958960", lines: map[string]string{"OVERTIME": "158960"}}},

		{"overtime_across_raise", func() Input { // 3,5 jam berbobot × 5 jt/173 + 3,5 × 5,5 jt/173 = 212.427,75
			in := raiseMid()
			in.Overtime = []OvertimeEntry{{Date: MustDate("2026-03-24"), Hours: d("2")}, {Date: MustDate("2026-03-10"), Hours: d("2")}}
			return in
		}, expect{net: "5219395", // gross 5.485.155; PPh 1% × 5.485.155 = 54.851 (DOWN); BPJS 210.909
			lines: map[string]string{"OVERTIME": "212428", "PPH21": "54851"}}},

		{"overtime_over_limit_warning", func() Input { // 5 jam > batas 4: 1,5 + 4×2 = 9,5 × 5 jt/173 = 274.566,47
			in := march()
			in.Overtime = []OvertimeEntry{{Date: MustDate("2026-03-10"), Hours: d("5")}}
			return in
		}, expect{net: "5074566", warnings: []string{WarnOvertimeOver}, lines: map[string]string{"OVERTIME": "274566"}}},

		{"unpaid_leave_deduction", func() Input { // 11 Mar: 5 jt/22 = 227.272,73; 25 Mar: 5,5 jt/22 = 250.000
			in := raiseMid()
			in.Attendance = []DayStatus{{Date: MustDate("2026-03-11"), Status: UnpaidLeave}, {Date: MustDate("2026-03-25"), Status: UnpaidLeave}}
			return in
		}, expect{net: "4584545", lines: map[string]string{"ABSENCE_UNPAID_LEAVE": "477273"}}},

		{"unexcused_absence_deduction", func() Input {
			in := march()
			in.Attendance = []DayStatus{{Date: MustDate("2026-03-04"), Status: AbsentUnexcused}}
			return in
		}, expect{net: "4572727", lines: map[string]string{"ABSENCE_UNEXCUSED": "227273"}}},

		{"paid_leave_reduces_per_day_present", func() Input { // transport 25.000 × (22 − 2 cuti dibayar); gaji pokok utuh
			in := march()
			in.Components = append(in.Components, transport())
			in.Attendance = []DayStatus{{Date: MustDate("2026-03-05"), Status: PaidLeave}, {Date: MustDate("2026-03-06"), Status: PaidLeave}}
			return in
		}, expect{net: "5245000", // 5.500.000 − 200.000 − PPh 55.000
			lines: map[string]string{"BASE_SALARY": "5000000", "TRANSPORT": "500000", "PPH21": "55000"}}},

		{"incomplete_attendance_policy_present", func() Input {
			in := march()
			in.Components = append(in.Components, transport())
			in.Attendance = []DayStatus{{Date: MustDate("2026-03-12"), Status: IncompleteRecord}}
			return in
		}, expect{net: "5294500", warnings: []string{WarnIncomplete}, lines: map[string]string{"TRANSPORT": "550000", "PPH21": "55500"}}},

		{"incomplete_attendance_policy_absent", func() Input {
			in := march()
			in.Components = append(in.Components, transport())
			in.Attendance = []DayStatus{{Date: MustDate("2026-03-12"), Status: IncompleteRecord}}
			in.Policy.IncompletePolicy = "COUNT_AS_ABSENT"
			return in
		}, expect{net: "5097727", warnings: []string{WarnIncomplete}, // pajak: 5.525.000 − 227.273 = 5.297.727 → 0
			lines: map[string]string{"TRANSPORT": "525000", "ABSENCE_UNEXCUSED": "227273"}}},

		{"bpjs_cap_applied", func() Input { // JP dibatasi 10 jt, Kes 12 jt; PPh 5% (A, > 12 jt)
			return raise(march(), SalarySpan{Range: rng("2025-01-01", ""), BaseSalary: d("15000000")})
		}, expect{net: "13730000", lines: map[string]string{"BPJS_JHT_EE": "300000", "BPJS_JP_EE": "100000", "BPJS_KES_EE": "120000", "PPH21": "750000"}}},

		{"bpjs_rounding_line_vs_total", func() Input { // §16: upah BPJS 6.548.387 → 130.968 + 65.484 + 65.484 = 261.936 (bukan 261.935)
			in := raise(march(), SalarySpan{Range: rng("2025-01-01", ""), BaseSalary: d("7000000")})
			in.Employment = []DateRange{rng("2026-03-03", "")} // 29/31 hari kalender
			in.Policy.ProrationBasis = "CALENDAR_DAYS"
			return in
		}, expect{net: "6220968", // 6.548.387 − 261.936 − PPh 65.483 (1% DOWN)
			lines: map[string]string{"BASE_SALARY": "6548387", "BPJS_JHT_EE": "130968", "BPJS_JP_EE": "65484", "BPJS_KES_EE": "65484", "PPH21": "65483"}}},

		{"tax_bracket_boundary", func() Input { // base tepat 7.000.000 = batas atas bracket 1% (inklusif)
			return raise(march(), SalarySpan{Range: rng("2025-01-01", ""), BaseSalary: d("7000000")})
		}, expect{net: "6650000", lines: map[string]string{"PPH21": "70000"}}},

		{"percent_of_component_chain", func() Input { // urutan input sengaja terbalik: engine mengurutkan topologis
			in := march()
			in.Components = append(in.Components,
				earning("BONUS_KECIL", "Bonus kecil", PercentOf, func(c *ComponentDef) { c.BaseRef = "TUNJ_KINERJA"; c.Rate = dp("0.5") }),
				earning("TUNJ_KINERJA", "Tunjangan kinerja", PercentOf, func(c *ComponentDef) { c.BaseRef = "TUNJ_JABATAN"; c.Rate = dp("0.1") }),
				earning("TUNJ_JABATAN", "Tunjangan jabatan", Fixed, func(c *ComponentDef) { c.DefaultAmount = dp("1000000"); c.BPJSBase = true }))
			return in
		}, expect{net: "5848500", // gross 6.150.000; BPJS atas 6 jt = 240.000; PPh 1% 61.500
			lines: map[string]string{"TUNJ_JABATAN": "1000000", "TUNJ_KINERJA": "100000", "BONUS_KECIL": "50000", "BPJS_JHT_EE": "120000", "PPH21": "61500"}}},

		{"adjustment_positive_taxable", func() Input {
			in := march()
			in.Adjustments = []Adjustment{{ID: "ADJ-1", OriginPeriod: "2026-02", Amount: d("600000"), Taxable: true, Reason: "Koreksi lembur Februari"}}
			return in
		}, expect{net: "5344000", lines: map[string]string{"ADJUSTMENT": "600000", "PPH21": "56000"}}},

		{"adjustment_negative", func() Input {
			in := march()
			in.Adjustments = []Adjustment{{ID: "ADJ-2", OriginPeriod: "2026-02", Amount: d("-300000"), Reason: "Kelebihan bayar Februari"}}
			return in
		}, expect{net: "4500000", lines: map[string]string{"ADJUSTMENT": "300000"}}},

		{"negative_net_error", func() Input { // baris tetap ada untuk diagnosis
			in := raise(march(), SalarySpan{Range: rng("2025-01-01", ""), BaseSalary: d("1000000")})
			in.Adjustments = []Adjustment{{ID: "ADJ-3", OriginPeriod: "2026-02", Amount: d("-2000000"), Reason: "Potongan besar"}}
			return in
		}, expect{net: "-1040000", status: StatusError, errors: []string{ErrNegativeNet}, lines: map[string]string{"BASE_SALARY": "1000000"}}},

		{"no_salary_error", func() Input {
			return raise(march(), SalarySpan{Range: rng("2026-04-01", ""), BaseSalary: d("5000000")})
		}, expect{net: "0", status: StatusError, errors: []string{ErrNoSalary}, lines: map[string]string{}}},

		{"missing_component_ref_error", func() Input {
			in := march()
			in.Components = append(in.Components, earning("TUNJ_X", "Tunjangan X", PercentOf, func(c *ComponentDef) { c.BaseRef = "TUNJ_HILANG"; c.Rate = dp("0.1") }))
			return in
		}, expect{net: "0", status: StatusError, errors: []string{ErrComponentRef}, lines: map[string]string{}}},

		{"component_cycle_error", func() Input {
			in := march()
			in.Components = append(in.Components,
				earning("TUNJ_A", "A", PercentOf, func(c *ComponentDef) { c.BaseRef = "TUNJ_B"; c.Rate = dp("0.1") }),
				earning("TUNJ_B", "B", PercentOf, func(c *ComponentDef) { c.BaseRef = "TUNJ_A"; c.Rate = dp("0.05") }))
			return in
		}, expect{net: "0", status: StatusError, errors: []string{ErrComponentCycle}, lines: map[string]string{}}},

		{"zero_base_salary_warning", func() Input {
			return raise(march(), SalarySpan{Range: rng("2025-01-01", ""), BaseSalary: d("0")})
		}, expect{net: "0", warnings: []string{WarnZeroBaseSalary}, lines: map[string]string{"BASE_SALARY": "0"}}},

		{"february_working_days", func() Input { // Feb 2026 = 20 hari kerja; masuk 16 Feb → 10/20
			in := march()
			in.Period = rng("2026-02-01", "2026-03-01")
			in.Employment = []DateRange{rng("2026-02-16", "")}
			return in
		}, expect{net: "2400000", lines: map[string]string{"BASE_SALARY": "2500000", "BPJS_JHT_EE": "50000"}}},

		{"percent_of_split_mid_period", func() Input { // rate 10% lalu 20% mulai 16 Mar: 10/22 + 12/22 hari kerja
			in := march()
			in.Components = append(in.Components,
				earning("TUNJ_JABATAN", "Tunjangan jabatan", Fixed, func(c *ComponentDef) { c.DefaultAmount = dp("1000000"); c.BPJSBase = true }),
				ComponentAssignment{Def: kinerja(), Rate: dp("0.1"), Range: rng("2025-01-01", "2026-03-16")},
				ComponentAssignment{Def: kinerja(), Rate: dp("0.2"), Range: rng("2026-03-16", "")})
			return in
		}, expect{net: "5853001", // 45.455 + 109.091 (bukan 100.000 + 200.000); PPh 1% × 6.154.546 = 61.545
			lines: map[string]string{"TUNJ_KINERJA": "154546", "PPH21": "61545", "BPJS_JHT_EE": "120000"}}},

		{"seeded_system_components", func() Input { // komponen sistem PERSIS seperti seed migrasi 00006 ikut di input
			in := tono()
			in.Components = append(in.Components, seededSystem()...)
			return in
		}, expect{net: "6888609", lines: map[string]string{"BPJS_JHT_EE": "136000", "PPH21": "109044"}}},

		{"not_eligible_error", func() Input { // keluar sebelum periode: tidak masuk run (plan), engine menolak defensif
			in := march()
			in.Employment = []DateRange{rng("2025-01-01", "2026-03-01")}
			return in
		}, expect{net: "0", status: StatusError, errors: []string{ErrNotEligible}, lines: map[string]string{}}},
	}
}

func kinerja() ComponentDef {
	return ComponentDef{Code: "TUNJ_KINERJA", Name: "Tunjangan kinerja", Category: Earning, Stage: StageEarning,
		CalcType: PercentOf, BaseRef: "TUNJ_JABATAN", Taxable: true, Rounding: HalfUp, Active: true}
}

// seededSystem: 9 komponen sistem dengan atribut yang sama dengan seed_system_components() (00006).
func seededSystem() []ComponentAssignment {
	sys := func(code string, cat Category, stage Stage, ct CalcType, rulesetRef, baseRef string, rounding Rounding) ComponentAssignment {
		return ComponentAssignment{Range: rng("2026-01-01", ""), Def: ComponentDef{Code: code, Name: code, Category: cat, Stage: stage,
			CalcType: ct, RulesetRef: rulesetRef, BaseRef: baseRef, Rounding: rounding, Active: true}}
	}
	return []ComponentAssignment{
		sys("BASE_SALARY", Earning, StageEarning, Fixed, "", "", HalfUp),
		sys("OVERTIME", Earning, StageOvertime, PerHourMultiplier, "", "", HalfUp),
		sys("ABSENCE_UNPAID_LEAVE", Deduction, StageAbsence, Fixed, "", "", HalfUp),
		sys("ABSENCE_UNEXCUSED", Deduction, StageAbsence, Fixed, "", "", HalfUp),
		sys("BPJS_JHT_EE", Deduction, StageStatutory, PercentOf, "BPJS_JHT_EE", BPJSWageRef, HalfUp),
		sys("BPJS_JP_EE", Deduction, StageStatutory, PercentOf, "BPJS_JP_EE", BPJSWageRef, HalfUp),
		sys("BPJS_KES_EE", Deduction, StageStatutory, PercentOf, "BPJS_KES_EE", BPJSWageRef, HalfUp),
		sys("PPH21", Deduction, StageTax, PercentOf, "TAX_BRACKETS", TaxableGrossRef, Down),
		sys("ADJUSTMENT", Earning, StageAdjustment, Fixed, "", "", HalfUp),
	}
}

func transport() ComponentAssignment {
	return earning("TRANSPORT", "Uang transport", PerDay, func(c *ComponentDef) { c.DefaultAmount = dp("25000"); c.DaySource = "PRESENT" })
}

// raiseMid: 5 jt sampai 15 Mar, 5,5 jt mulai Senin 16 Mar (10 + 12 hari kerja).
func raiseMid() Input {
	return raise(march(),
		SalarySpan{Range: rng("2025-01-01", "2026-03-16"), BaseSalary: d("5000000")},
		SalarySpan{Range: rng("2026-03-16", ""), BaseSalary: d("5500000")})
}

// tono: contoh lengkap §15 (libur tenant 19–20 Mar → 20 hari kerja).
func tono() Input {
	in := march()
	in.EmployeeID = "00000000-0000-7000-8000-000000000037"
	in.Holidays = []Date{MustDate("2026-03-19"), MustDate("2026-03-20")}
	in.Employment = []DateRange{rng("2025-06-02", "")}
	in.Salary = []SalarySpan{
		{Range: rng("2026-01-01", "2026-03-15"), BaseSalary: d("6000000")},
		{Range: rng("2026-03-15", ""), BaseSalary: d("6600000")},
	}
	in.Components = append(in.Components,
		earning("TUNJ_JABATAN", "Tunjangan jabatan", Fixed, func(c *ComponentDef) { c.DefaultAmount = dp("500000"); c.BPJSBase = true }),
		earning("TRANSPORT", "Uang transport", PerDay, func(c *ComponentDef) { c.DefaultAmount = dp("20000"); c.DaySource = "PRESENT" }))
	in.Attendance = []DayStatus{{Date: MustDate("2026-03-25"), Status: UnpaidLeave}}
	in.Overtime = []OvertimeEntry{{Date: MustDate("2026-03-23"), Hours: d("4")}, {Date: MustDate("2026-03-26"), Hours: d("2")}}
	return in
}

func issueCodes(is []Issue) []string {
	out := []string{}
	for _, i := range is {
		if !slices.Contains(out, i.Code) {
			out = append(out, i.Code)
		}
	}
	slices.Sort(out)
	return out
}

func sorted(s []string) []string {
	out := append([]string{}, s...)
	slices.Sort(out)
	return out
}

func checkExpect(t *testing.T, res Result, want expect) {
	t.Helper()
	status := want.status
	if status == "" {
		status = StatusOK
	}
	if res.Status != status {
		t.Errorf("status: got %s, want %s (errors %v)", res.Status, status, res.Errors)
	}
	if !res.Net.Equal(d(want.net)) {
		t.Errorf("net: got %s, want %s", res.Net, want.net)
	}
	if got := issueCodes(res.Errors); !slices.Equal(got, sorted(want.errors)) {
		t.Errorf("errors: got %v, want %v", got, want.errors)
	}
	if got := issueCodes(res.Warnings); !slices.Equal(got, sorted(want.warnings)) {
		t.Errorf("warnings: got %v, want %v", got, want.warnings)
	}
	sum := map[string]decimal.Decimal{}
	for _, l := range res.Lines {
		sum[l.Code] = sum[l.Code].Add(l.Amount)
	}
	for code, amt := range want.lines {
		if !sum[code].Equal(d(amt)) {
			t.Errorf("line %s: got %s, want %s", code, sum[code], amt)
		}
	}
}

func marshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

func TestGolden(t *testing.T) {
	rs := loadRuleset(t)
	cases := goldenCases()
	if len(cases) < 20 {
		t.Fatalf("golden catalogue must have ≥ 20 cases (DoD S3), has %d", len(cases))
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := c.input()
			res := Calculate(in, rs)
			checkExpect(t, res, c.want)

			inPath := filepath.Join("testdata", "cases", c.name+".input.json")
			goldPath := filepath.Join("testdata", "cases", c.name+".golden.json")
			got := marshal(t, res)
			if *update {
				if err := os.WriteFile(inPath, marshal(t, in), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(goldPath, got, 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			gold, err := os.ReadFile(goldPath)
			if err != nil {
				t.Fatalf("missing golden (run with -update and review): %v", err)
			}
			if !bytes.Equal(got, gold) {
				t.Errorf("result differs from %s (run with -update if intended, then review the diff)\n%s", goldPath, got)
			}
			// Recompute dari snapshot JSON: hasil identik dengan golden.
			raw, err := os.ReadFile(inPath)
			if err != nil {
				t.Fatal(err)
			}
			var snap Input
			if err := json.Unmarshal(raw, &snap); err != nil {
				t.Fatal(err)
			}
			if again := marshal(t, Calculate(snap, rs)); !bytes.Equal(again, gold) {
				t.Errorf("recompute from %s differs from golden", inPath)
			}
		})
	}
}

// Worked example §15 baris per baris, termasuk explanation lembur yang dijanjikan di dokumen.
func TestTonoMarch2026_LineByLine(t *testing.T) {
	res := Calculate(tono(), loadRuleset(t))
	want := []struct{ code, amount string }{
		{"BASE_SALARY", "3000000"}, {"BASE_SALARY", "3300000"}, {"TUNJ_JABATAN", "500000"}, {"TRANSPORT", "380000"},
		{"OVERTIME", "419653"}, {"ABSENCE_UNPAID_LEAVE", "330000"}, {"BPJS_JHT_EE", "136000"}, {"BPJS_JP_EE", "68000"},
		{"BPJS_KES_EE", "68000"}, {"PPH21", "109044"},
	}
	if len(res.Lines) != len(want) {
		t.Fatalf("lines: got %d, want %d: %+v", len(res.Lines), len(want), res.Lines)
	}
	for i, w := range want {
		if res.Lines[i].Code != w.code || !res.Lines[i].Amount.Equal(d(w.amount)) {
			t.Errorf("line %d: got %s %s, want %s %s", i+1, res.Lines[i].Code, res.Lines[i].Amount, w.code, w.amount)
		}
	}
	if !res.Gross.Equal(d("7599653")) || !res.Deductions.Equal(d("711044")) || !res.Net.Equal(d("6888609")) {
		t.Fatalf("totals: gross %s deductions %s net %s", res.Gross, res.Deductions, res.Net)
	}
	ot := res.Lines[4].Explanation
	wantOT := "Lembur 6 jam (23 Mar 4j: 1×1,5 + 3×2; 26 Mar 2j: 1×1,5 + 1×2) = 11 jam berbobot × Rp38.150,29/jam (Rp6.600.000 / 173)"
	if ot != wantOT {
		t.Errorf("overtime explanation:\n got  %s\n want %s", ot, wantOT)
	}
}
