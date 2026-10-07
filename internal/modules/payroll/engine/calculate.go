package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/shopspring/decimal"
)

// Eligible (PI-07): employee masuk run periode ini jika aktif minimal satu hari di dalamnya. Dipakai
// job plan (S4) dan diverifikasi ulang oleh Calculate (NOT_ELIGIBLE).
func Eligible(period DateRange, employment []DateRange) bool {
	for _, e := range employment {
		if _, ok := e.Intersect(period); ok {
			return true
		}
	}
	return false
}

// TaxCategoryForPTKP memetakan status PTKP ke kategori tarif efektif (simplified, mengikuti pola TER:
// A = TK/0, TK/1, K/0; B = TK/2, TK/3, K/1, K/2; C = K/3).
func TaxCategoryForPTKP(ptkp string) string {
	switch ptkp {
	case "TK/0", "TK/1", "K/0":
		return "A"
	case "TK/2", "TK/3", "K/1", "K/2":
		return "B"
	case "K/3":
		return "C"
	}
	return "A"
}

// calc menampung state satu kalkulasi. Tidak ada map yang diiterasi tanpa diurutkan (determinisme).
type calc struct {
	in          Input
	rs          Ruleset
	period      DateRange
	holidays    map[Date]bool
	workDays    map[int]bool
	attendance  map[Date]AttendanceStatus
	active      []DateRange // employment ∩ periode, urut
	periodBasis decimal.Decimal
	periodWork  decimal.Decimal // hari kerja periode (untuk daily rate absensi, §8)
	res         Result
	byCode      map[string]decimal.Decimal // Σ baris dibulatkan per code (base PERCENT_OF)
	bpjsLine    map[int]bool               // indeks baris yang masuk BPJS_WAGE
}

func (c *calc) isWorkDay(d Date) bool { return c.workDays[d.ISOWeekday()] && !c.holidays[d] }

// basisDays: hari pada rentang (∩ periode) menurut metode prorata.
func (c *calc) basisDays(r DateRange) int {
	r, ok := r.Intersect(c.period)
	if !ok {
		return 0
	}
	n := 0
	r.Days(func(d Date) {
		if c.in.Policy.ProrationBasis == "CALENDAR_DAYS" || c.isWorkDay(d) {
			n++
		}
	})
	return n
}

func (c *calc) workingDays(r DateRange) int {
	r, ok := r.Intersect(c.period)
	if !ok {
		return 0
	}
	n := 0
	r.Days(func(d Date) {
		if c.isWorkDay(d) {
			n++
		}
	})
	return n
}

func (c *calc) isActive(d Date) bool {
	for _, a := range c.active {
		if a.Contains(d) {
			return true
		}
	}
	return false
}

func (c *calc) salaryOn(d Date) (decimal.Decimal, bool) {
	for _, s := range c.in.Salary {
		if s.Range.Contains(d) {
			return s.BaseSalary, true
		}
	}
	return zero, false
}

// notPresent: hari kerja yang tidak dihitung hadir untuk PER_DAY(PRESENT).
func (c *calc) notPresent(d Date) bool {
	switch c.attendance[d] {
	case PaidLeave, UnpaidLeave, AbsentUnexcused:
		return true
	case IncompleteRecord:
		return c.in.Policy.IncompletePolicy == "COUNT_AS_ABSENT"
	}
	return false
}

func (c *calc) addLine(l Line, bpjsBase bool) {
	c.res.Lines = append(c.res.Lines, l)
	c.byCode[l.Code] = c.byCode[l.Code].Add(l.Amount)
	if bpjsBase {
		c.bpjsLine[len(c.res.Lines)-1] = true
	}
}

func (c *calc) fail(code, detail string) Result {
	c.res.Errors = append(c.res.Errors, Issue{Code: code, Detail: detail})
	c.res.Status = StatusError
	c.res.Lines = []Line{}
	c.res.Gross, c.res.Deductions, c.res.Net = zero, zero, zero
	return c.res
}

func (c *calc) warn(code, detail string) {
	c.res.Warnings = append(c.res.Warnings, Issue{Code: code, Detail: detail})
}

func shortDate(d Date) string {
	months := [...]string{"Jan", "Feb", "Mar", "Apr", "Mei", "Jun", "Jul", "Agu", "Sep", "Okt", "Nov", "Des"}
	return fmt.Sprintf("%d %s", d.Day(), months[d.Month()-1])
}

func rangeText(r DateRange) string {
	return shortDate(r.From) + "–" + shortDate(r.Last())
}

func (c *calc) basisName() string {
	if c.in.Policy.ProrationBasis == "CALENDAR_DAYS" {
		return "hari kalender"
	}
	return "hari kerja"
}

// Calculate menghitung satu employee untuk satu periode (pipeline §2.4 / ADR-P03). Masalah data tidak
// dikembalikan sebagai error Go, melainkan sebagai Result.Errors (item ERROR, §13).
func Calculate(in Input, rs Ruleset) Result {
	c := &calc{in: in, rs: rs, period: in.Period, holidays: map[Date]bool{}, workDays: map[int]bool{},
		attendance: map[Date]AttendanceStatus{}, byCode: map[string]decimal.Decimal{}, bpjsLine: map[int]bool{}}
	c.res = Result{Status: StatusOK, Ruleset: rs.Code + "-" + rs.Version, Lines: []Line{}, Errors: []Issue{}, Warnings: []Issue{}}

	if rs.Code == "" || rs.Version == "" || !rs.OvertimeDivisor.IsPositive() {
		return c.fail(ErrRulesetMissing, "no ruleset in force for the period")
	}
	if in.Period.To == nil || !in.Period.From.Before(*in.Period.To) {
		return c.fail(ErrNotEligible, "invalid period")
	}
	for _, h := range in.Holidays {
		c.holidays[h] = true
	}
	for _, w := range in.WorkDays {
		c.workDays[w] = true
	}
	for _, a := range in.Attendance {
		c.attendance[a.Date] = a.Status
	}

	// 1. Eligibility + proration basis (PI-07).
	for _, e := range in.Employment {
		if r, ok := e.Intersect(c.period); ok {
			c.active = append(c.active, r)
		}
	}
	c.active = mergeRanges(c.active)
	if len(c.active) == 0 {
		return c.fail(ErrNotEligible, "not employed on any day of the period")
	}
	c.periodBasis = decimal.NewFromInt(int64(c.basisDays(c.period)))
	c.periodWork = decimal.NewFromInt(int64(c.workingDays(c.period)))
	if c.periodBasis.IsZero() || c.periodWork.IsZero() {
		return c.fail(ErrNotEligible, "period has no working days")
	}

	// Konfigurasi komponen: hanya yang aktif; graf divalidasi defensif (§2.4).
	var comps []ComponentAssignment
	var defs []ComponentDef
	seen := map[string]bool{}
	for _, a := range in.Components {
		if !a.Def.Active {
			c.warn(WarnInactiveComp, a.Def.Code)
			continue
		}
		comps = append(comps, a)
		if !seen[a.Def.Code] {
			seen[a.Def.Code] = true
			defs = append(defs, a.Def)
		}
	}
	order, problem := ValidateGraph(defs)
	if problem != nil {
		code := ErrComponentRef
		if problem.Code == ProblemCycle {
			code = ErrComponentCycle
		}
		return c.fail(code, problem.Component+": "+problem.Detail)
	}
	statutory := map[string]StatutoryRule{}
	for _, r := range rs.Statutory {
		statutory[r.Code] = r
	}
	for _, d := range defs {
		if d.RulesetRef != "" {
			if _, ok := statutory[d.RulesetRef]; !ok {
				return c.fail(ErrComponentRef, d.Code+": ruleset_ref "+d.RulesetRef+" not in ruleset "+rs.Version)
			}
		}
	}
	brackets, ok := rs.TaxBrackets[in.TaxCategory]
	if !ok || len(brackets) == 0 {
		return c.fail(ErrRulesetMissing, "no tax brackets for category "+in.TaxCategory)
	}

	// 2. Gaji pokok per segmen = employment ∩ salary ∩ periode (§6.2).
	salaries := normalizeSalary(in.Salary)
	c.in.Salary = salaries // salaryOn (lembur, absensi) memakai rentang yang sama
	covered, allZero := 0, true
	for _, a := range c.active {
		for _, s := range salaries {
			seg, ok := a.Intersect(s.Range)
			if !ok {
				continue
			}
			days := c.basisDays(seg)
			if days == 0 {
				continue
			}
			covered += days
			if !s.BaseSalary.IsZero() {
				allZero = false
			}
			dd := decimal.NewFromInt(int64(days))
			exact := div(s.BaseSalary.Mul(dd), c.periodBasis)
			c.addLine(Line{Code: "BASE_SALARY", Name: "Gaji pokok", Category: Earning, Stage: StageEarning,
				Amount: round(exact, HalfUp), Taxable: true, Rounding: HalfUp,
				Basis: map[string]string{"salary": s.BaseSalary.String(), "days": dd.String(), "period_days": c.periodBasis.String(),
					"basis": c.in.Policy.ProrationBasis, "from": seg.From.String(), "to": seg.To.String()},
				Explanation: fmt.Sprintf("Gaji pokok %s × %s/%s %s (%s)", rupiah(s.BaseSalary, 0), dd, c.periodBasis, c.basisName(), rangeText(seg)),
			}, true)
		}
	}
	if covered == 0 {
		return c.fail(ErrNoSalary, "no salary assignment overlaps the employment days of the period")
	}
	activeBasis := 0
	for _, a := range c.active {
		activeBasis += c.basisDays(a)
	}
	if covered < activeBasis {
		c.warn(WarnSalaryGap, fmt.Sprintf("%d of %d %s without salary", activeBasis-covered, activeBasis, c.basisName()))
	}
	if allZero {
		c.warn(WarnZeroBaseSalary, "")
	}

	// 3. Komponen tenant (stage EARNING): FIXED/PER_DAY lalu PERCENT_OF dalam urutan topologis.
	// Urutan baris di stage EARNING (= urutan di slip): FIXED, PER_DAY, lalu PERCENT_OF dalam urutan
	// topologis. FIXED/PER_DAY tidak punya dependensi, jadi urutan topologis tetap terjaga.
	typeRank := map[CalcType]int{Fixed: 0, PerDay: 1, PercentOf: 2, PerHourMultiplier: 3}
	rank := map[string]int{}
	for i, code := range order {
		rank[code] = i
	}
	sort.SliceStable(comps, func(i, j int) bool {
		if ti, tj := typeRank[comps[i].Def.CalcType], typeRank[comps[j].Def.CalcType]; ti != tj {
			return ti < tj
		}
		if rank[comps[i].Def.Code] != rank[comps[j].Def.Code] {
			return rank[comps[i].Def.Code] < rank[comps[j].Def.Code]
		}
		return comps[i].Range.From.Before(comps[j].Range.From)
	})
	fixedEarnings := zero
	for _, a := range comps {
		if a.Def.Stage != StageEarning {
			continue
		}
		if l, ok := c.earningLine(a, fixedEarnings); ok {
			if a.Def.CalcType == Fixed && l.Category == Earning {
				fixedEarnings = fixedEarnings.Add(l.Amount)
			}
			c.addLine(l, a.Def.BPJSBase)
		}
	}

	// 4. Lembur (§7): tarif per jam dari gaji yang berlaku PADA tanggal lembur.
	c.overtime(comps)

	// 5. Absensi (§8): daily_rate = gaji segmen hari itu / hari kerja periode.
	c.absence()

	// 6. Statutory (§9): BPJS dari ruleset, base = Σ baris bpjs_base yang sudah dibulatkan.
	bpjsWage := zero
	for i, l := range c.res.Lines {
		if c.bpjsLine[i] {
			bpjsWage = bpjsWage.Add(l.Amount)
		}
	}
	for _, a := range comps {
		if a.Def.Stage != StageStatutory {
			continue
		}
		rule := statutory[a.Def.RulesetRef]
		base := c.baseFor(a.Def.BaseRef, bpjsWage, fixedEarnings)
		capped := base
		if rule.CapMax != nil && capped.GreaterThan(*rule.CapMax) {
			capped = *rule.CapMax
		}
		capped = applyCaps(capped, a.Def)
		exact := rule.Rate.Mul(capped)
		expl := fmt.Sprintf("%s × upah BPJS %s", percent(rule.Rate), rupiah(base, 0))
		if !capped.Equal(base) {
			expl = fmt.Sprintf("%s × %s (upah BPJS %s, dibatasi cap)", percent(rule.Rate), rupiah(capped, 0), rupiah(base, 0))
		}
		amt := round(exact, rule.Rounding)
		if amt.IsZero() {
			continue // upah BPJS 0 (contoh gaji 0): tidak ada baris 0 di slip
		}
		c.addLine(Line{Code: a.Def.Code, Name: a.Def.Name, Category: Deduction, Stage: StageStatutory,
			Amount: amt, Rounding: rule.Rounding,
			Basis:       map[string]string{"rate": rule.Rate.String(), "base": base.String(), "capped_base": capped.String(), "ruleset_ref": a.Def.RulesetRef},
			Explanation: expl}, false)
	}

	// 7. Pajak (§10): TAXABLE_GROSS = Σ earning taxable − Σ absensi + adjustment taxable.
	adjustments := append([]Adjustment(nil), in.Adjustments...)
	sort.Slice(adjustments, func(i, j int) bool {
		if adjustments[i].OriginPeriod != adjustments[j].OriginPeriod {
			return adjustments[i].OriginPeriod < adjustments[j].OriginPeriod
		}
		return adjustments[i].ID < adjustments[j].ID
	})
	taxable := zero
	for _, l := range c.res.Lines {
		switch {
		case l.Category == Earning && l.Taxable:
			taxable = taxable.Add(l.Amount)
		case l.Stage == StageAbsence:
			taxable = taxable.Sub(l.Amount)
		}
	}
	for _, adj := range adjustments {
		if adj.Taxable {
			taxable = taxable.Add(round(adj.Amount.Abs(), HalfUp).Mul(decimal.NewFromInt(int64(adj.Amount.Sign()))))
		}
	}
	if taxable.IsNegative() {
		taxable = zero
	}
	c.tax(taxable, brackets)

	// 8. Adjustment (§11): koreksi periode lalu, dalam rupiah eksplisit.
	for _, adj := range adjustments {
		amt := round(adj.Amount.Abs(), HalfUp)
		if amt.IsZero() {
			continue
		}
		cat := Earning
		if adj.Amount.IsNegative() {
			cat = Deduction
		}
		c.addLine(Line{Code: "ADJUSTMENT", Name: "Koreksi", Category: cat, Stage: StageAdjustment, Amount: amt,
			Taxable: adj.Taxable, Rounding: HalfUp,
			Basis:       map[string]string{"adjustment_id": adj.ID, "origin_period": adj.OriginPeriod, "amount": adj.Amount.String()},
			Explanation: fmt.Sprintf("Koreksi periode %s: %s (%s)", adj.OriginPeriod, adj.Reason, adj.ID)}, false)
	}

	// 9. Net = Σ earning − Σ deduction atas baris yang sudah dibulatkan (PI-05, eksak).
	for _, l := range c.res.Lines {
		if l.Category == Earning {
			c.res.Gross = c.res.Gross.Add(l.Amount)
		} else {
			c.res.Deductions = c.res.Deductions.Add(l.Amount)
		}
	}
	c.res.Net = c.res.Gross.Sub(c.res.Deductions)
	if c.res.Net.IsNegative() {
		// Baris tetap ada untuk diagnosis (§13); carry-over tidak didukung di V1.
		c.res.Errors = append(c.res.Errors, Issue{Code: ErrNegativeNet, Detail: "deductions exceed earnings by " + rupiah(c.res.Net.Abs(), 0)})
		c.res.Status = StatusError
	}
	return c.res
}

// mergeRanges menggabungkan rentang yang beririsan/bersambung. Span kerja di DB tidak pernah beririsan
// (EXCLUDE), tetapi engine defensif: hari yang sama tidak boleh dibayar dua kali (ditemukan property test).
func mergeRanges(rs []DateRange) []DateRange {
	if len(rs) == 0 {
		return nil
	}
	sorted := append([]DateRange(nil), rs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].From.Before(sorted[j].From) })
	out := []DateRange{sorted[0]}
	for _, r := range sorted[1:] {
		last := &out[len(out)-1]
		if last.To != nil && last.To.Before(r.From) {
			out = append(out, r)
			continue
		}
		if last.To != nil && (r.To == nil || last.To.Before(*r.To)) {
			last.To = r.To
		}
	}
	return out
}

// normalizeSalary: urut menurut tanggal mulai; span yang beririsan dipotong di awal span berikutnya
// (yang lebih baru berlaku), sehingga satu hari tepat satu gaji. Di DB dijaga EXCLUDE (PI-09).
func normalizeSalary(in []SalarySpan) []SalarySpan {
	out := append([]SalarySpan(nil), in...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Range.From.Before(out[j].Range.From) })
	for i := 0; i+1 < len(out); i++ {
		next := out[i+1].Range.From
		if out[i].Range.To == nil || next.Before(*out[i].Range.To) {
			n := next
			out[i].Range.To = &n
		}
	}
	return out
}

func applyCaps(base decimal.Decimal, d ComponentDef) decimal.Decimal {
	if d.CapMin != nil && base.LessThan(*d.CapMin) {
		base = *d.CapMin
	}
	if d.CapMax != nil && base.GreaterThan(*d.CapMax) {
		base = *d.CapMax
	}
	return base
}

func (c *calc) baseFor(ref string, bpjsWage, fixedEarnings decimal.Decimal) decimal.Decimal {
	switch ref {
	case BaseSalaryRef:
		return c.byCode["BASE_SALARY"]
	case FixedAllowancesRef:
		return fixedEarnings
	case BPJSWageRef:
		return bpjsWage
	}
	return c.byCode[ref]
}

// effective: rentang assignment ∩ hari aktif.
func (c *calc) effective(a ComponentAssignment) []DateRange {
	var out []DateRange
	for _, act := range c.active {
		if r, ok := act.Intersect(a.Range); ok {
			out = append(out, r)
		}
	}
	return out
}

func (c *calc) earningLine(a ComponentAssignment, fixedEarnings decimal.Decimal) (Line, bool) {
	d := a.Def
	eff := c.effective(a)
	if len(eff) == 0 {
		return Line{}, false
	}
	amount := d.DefaultAmount
	if a.Amount != nil {
		amount = a.Amount
	}
	l := Line{Code: d.Code, Name: d.Name, Category: d.Category, Stage: StageEarning, Taxable: d.Taxable, Rounding: d.Rounding}
	switch d.CalcType {
	case Fixed:
		if amount == nil {
			return Line{}, false
		}
		days := 0
		for _, r := range eff {
			days += c.basisDays(r)
		}
		if days == 0 {
			return Line{}, false
		}
		dd := decimal.NewFromInt(int64(days))
		l.Amount = round(div(amount.Mul(dd), c.periodBasis), d.Rounding)
		l.Basis = map[string]string{"amount": amount.String(), "days": dd.String(), "period_days": c.periodBasis.String()}
		if dd.Equal(c.periodBasis) {
			l.Explanation = fmt.Sprintf("%s %s (aktif penuh)", d.Name, rupiah(*amount, 0))
		} else {
			l.Explanation = fmt.Sprintf("%s %s × %s/%s %s", d.Name, rupiah(*amount, 0), dd, c.periodBasis, c.basisName())
		}
	case PerDay:
		if amount == nil {
			return Line{}, false
		}
		days := 0
		for _, r := range eff {
			r.Days(func(day Date) {
				if c.isWorkDay(day) && (d.DaySource != "PRESENT" || !c.notPresent(day)) {
					days++
				}
			})
		}
		dd := decimal.NewFromInt(int64(days))
		src := "hari kerja"
		if d.DaySource == "PRESENT" {
			src = "hari hadir"
		}
		l.Amount = round(amount.Mul(dd), d.Rounding)
		l.Basis = map[string]string{"rate": amount.String(), "days": dd.String(), "day_source": d.DaySource}
		l.Explanation = fmt.Sprintf("%s %s × %s %s", d.Name, rupiah(*amount, 0), dd, src)
	case PercentOf:
		rate := d.Rate
		if a.Rate != nil {
			rate = a.Rate
		}
		if rate == nil {
			return Line{}, false
		}
		base := applyCaps(c.baseFor(d.BaseRef, zero, fixedEarnings), d)
		l.Amount = round(rate.Mul(base), d.Rounding)
		l.Basis = map[string]string{"rate": rate.String(), "base_ref": d.BaseRef, "base": base.String()}
		l.Explanation = fmt.Sprintf("%s × %s %s", percent(*rate), strings.ToLower(strings.ReplaceAll(d.BaseRef, "_", " ")), rupiah(base, 0))
	default:
		return Line{}, false
	}
	if l.Amount.IsZero() {
		return Line{}, false
	}
	return l, true
}

// weightedHours membagi jam satu hari ke tier: tier i berlaku untuk jam [FromHour_i, FromHour_i+1).
func weightedHours(hours decimal.Decimal, tiers []Tier) (decimal.Decimal, []string) {
	if len(tiers) == 0 {
		return hours, []string{num(hours) + "×1"}
	}
	ts := append([]Tier(nil), tiers...)
	sort.Slice(ts, func(i, j int) bool { return ts[i].FromHour.LessThan(ts[j].FromHour) })
	total := zero
	var parts []string
	for i, t := range ts {
		upper := hours
		if i+1 < len(ts) && ts[i+1].FromHour.LessThan(upper) {
			upper = ts[i+1].FromHour
		}
		h := upper.Sub(t.FromHour)
		if !h.IsPositive() {
			continue
		}
		total = total.Add(h.Mul(t.Multiplier))
		parts = append(parts, num(h)+"×"+num(t.Multiplier))
	}
	return total, parts
}

func (c *calc) overtime(comps []ComponentAssignment) {
	entries := append([]OvertimeEntry(nil), c.in.Overtime...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Date.Before(entries[j].Date) })
	exact, hoursTotal, weightedTotal := zero, zero, zero
	var days []string
	bases := map[string]bool{}
	var lastBasis decimal.Decimal
	for _, e := range entries {
		if !c.isActive(e.Date) || !e.Hours.IsPositive() {
			continue
		}
		basis, ok := c.salaryOn(e.Date)
		if !ok {
			c.warn(WarnSalaryGap, "overtime on "+e.Date.String()+" without salary")
			continue
		}
		if c.in.Policy.OvertimeBasis == "BASE_PLUS_FIXED" {
			for _, a := range comps {
				if a.Def.Stage == StageEarning && a.Def.CalcType == Fixed && a.Def.Category == Earning && a.Range.Contains(e.Date) {
					amt := a.Def.DefaultAmount
					if a.Amount != nil {
						amt = a.Amount
					}
					if amt != nil {
						basis = basis.Add(*amt)
					}
				}
			}
		}
		if max := c.in.Policy.OvertimeMaxHoursPerDay; max != nil && e.Hours.GreaterThan(*max) {
			c.warn(WarnOvertimeOver, fmt.Sprintf("%s: %s hours > %s", e.Date, num(e.Hours), num(*max)))
		}
		w, parts := weightedHours(e.Hours, c.rs.OvertimeTiers)
		exact = exact.Add(div(w.Mul(basis), c.rs.OvertimeDivisor))
		hoursTotal, weightedTotal = hoursTotal.Add(e.Hours), weightedTotal.Add(w)
		bases[basis.String()] = true
		lastBasis = basis
		day := fmt.Sprintf("%s %sj: %s", shortDate(e.Date), num(e.Hours), strings.Join(parts, " + "))
		days = append(days, day)
	}
	if hoursTotal.IsZero() {
		return
	}
	var expl string
	if len(bases) == 1 {
		hourly := div(lastBasis, c.rs.OvertimeDivisor)
		expl = fmt.Sprintf("Lembur %s jam (%s) = %s jam berbobot × %s/jam (%s / %s)", num(hoursTotal), strings.Join(days, "; "),
			num(weightedTotal), rupiah(hourly, 2), rupiah(lastBasis, 0), num(c.rs.OvertimeDivisor))
	} else {
		expl = fmt.Sprintf("Lembur %s jam (%s) = %s jam berbobot; tarif per jam = gaji yang berlaku pada tanggal lembur / %s",
			num(hoursTotal), strings.Join(days, "; "), num(weightedTotal), num(c.rs.OvertimeDivisor))
	}
	c.addLine(Line{Code: "OVERTIME", Name: "Lembur", Category: Earning, Stage: StageOvertime, Amount: round(exact, HalfUp),
		Taxable: true, Rounding: HalfUp, Explanation: expl,
		Basis: map[string]string{"hours": hoursTotal.String(), "weighted_hours": weightedTotal.String(), "divisor": c.rs.OvertimeDivisor.String()}}, false)
}

func (c *calc) absence() {
	type acc struct {
		exact decimal.Decimal
		days  []string
	}
	unpaid, unexcused := &acc{exact: zero}, &acc{exact: zero}
	incomplete := 0
	c.period.Days(func(d Date) {
		if !c.isActive(d) || !c.isWorkDay(d) {
			return
		}
		st := c.attendance[d]
		if st == IncompleteRecord {
			incomplete++
		}
		var target *acc
		switch {
		case st == UnpaidLeave:
			target = unpaid
		case st == AbsentUnexcused, st == IncompleteRecord && c.in.Policy.IncompletePolicy == "COUNT_AS_ABSENT":
			target = unexcused
		default:
			return
		}
		salary, _ := c.salaryOn(d)
		target.exact = target.exact.Add(div(salary, c.periodWork))
		target.days = append(target.days, shortDate(d)+" "+rupiah(div(salary, c.periodWork), 0))
	})
	if incomplete > 0 {
		c.warn(WarnIncomplete, fmt.Sprintf("%d day(s) with incomplete attendance, counted per policy %s", incomplete, c.in.Policy.IncompletePolicy))
	}
	for _, x := range []struct {
		code, name string
		a          *acc
	}{{"ABSENCE_UNPAID_LEAVE", "Cuti tidak dibayar", unpaid}, {"ABSENCE_UNEXCUSED", "Absen tanpa keterangan", unexcused}} {
		if len(x.a.days) == 0 {
			continue
		}
		c.addLine(Line{Code: x.code, Name: x.name, Category: Deduction, Stage: StageAbsence, Amount: round(x.a.exact, HalfUp),
			Rounding: HalfUp, Basis: map[string]string{"days": fmt.Sprint(len(x.a.days)), "period_working_days": c.periodWork.String()},
			Explanation: fmt.Sprintf("%s %d hari × gaji harian (gaji / %s hari kerja): %s", x.name, len(x.a.days), c.periodWork, strings.Join(x.a.days, ", "))}, false)
	}
}

func (c *calc) tax(base decimal.Decimal, brackets []Bracket) {
	var chosen *Bracket
	lower := zero
	for i := range brackets {
		b := brackets[i]
		if b.Max == nil || !base.GreaterThan(*b.Max) {
			chosen = &b
			break
		}
		lower = *b.Max
	}
	if chosen == nil { // base di atas bracket terakhir yang berhingga: pakai bracket terakhir
		chosen = &brackets[len(brackets)-1]
	}
	amt := round(chosen.Rate.Mul(base), c.rs.TaxRounding)
	if amt.IsZero() {
		return
	}
	rangeTxt := "> " + rupiah(lower, 0)
	if chosen.Max != nil {
		rangeTxt = rupiah(lower, 0) + "–" + rupiah(*chosen.Max, 0)
	}
	c.addLine(Line{Code: "PPH21", Name: "PPh 21 (simplified)", Category: Deduction, Stage: StageTax, Amount: amt, Rounding: c.rs.TaxRounding,
		Basis:       map[string]string{"taxable_gross": base.String(), "rate": chosen.Rate.String(), "category": c.in.TaxCategory},
		Explanation: fmt.Sprintf("Kategori %s: penghasilan kena pajak %s × %s (bracket %s)", c.in.TaxCategory, rupiah(base, 0), percent(chosen.Rate), rangeTxt)}, false)
}
