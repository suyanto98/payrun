// Package engine adalah payroll engine PayRun: fungsi murni Calculate(input, ruleset) → hasil
// (ADR-P03, docs/design/07-payroll-engine.md). Tanpa database, jaringan, jam sistem, angka acak, atau
// float (depguard payroll-engine-pure + TestEngine_NoFloat): input yang sama + ruleset yang sama
// selalu menghasilkan angka yang sama (PI-12), sehingga run final bisa diverifikasi ulang dari snapshot.
//
// Semua tarif pajak, BPJS, cap, dan pembagi lembur datang dari Ruleset. Ruleset bawaan berlabel
// "simplified — not compliance-certified".
package engine

import "github.com/shopspring/decimal"

// Kategori baris: tanda di net.
type Category string

const (
	Earning   Category = "EARNING"
	Deduction Category = "DEDUCTION"
)

// Stage: posisi di pipeline tetap (§2.4, ADR-P03).
type Stage string

const (
	StageEarning    Stage = "EARNING"
	StageOvertime   Stage = "OVERTIME"
	StageAbsence    Stage = "ABSENCE"
	StageStatutory  Stage = "STATUTORY"
	StageTax        Stage = "TAX"
	StageAdjustment Stage = "ADJUSTMENT"
)

// Urutan stage (indeks lebih kecil = lebih awal). Referensi PERCENT_OF hanya boleh ke stage lebih awal
// atau stage yang sama (dengan urutan topologis).
var stageOrder = map[Stage]int{StageEarning: 0, StageOvertime: 1, StageAbsence: 2, StageStatutory: 3, StageTax: 4, StageAdjustment: 5}

type CalcType string

const (
	Fixed             CalcType = "FIXED"
	PercentOf         CalcType = "PERCENT_OF"
	PerDay            CalcType = "PER_DAY"
	PerHourMultiplier CalcType = "PER_HOUR_MULTIPLIER"
)

type Rounding string

const (
	HalfUp Rounding = "HALF_UP"
	Down   Rounding = "DOWN"
	Up     Rounding = "UP"
)

// Base virtual untuk PERCENT_OF (§2.3). Selain ini, base_ref adalah code komponen lain.
const (
	BaseSalaryRef      = "BASE_SALARY"
	FixedAllowancesRef = "FIXED_ALLOWANCES"
	BPJSWageRef        = "BPJS_WAGE"
	TaxableGrossRef    = "TAXABLE_GROSS"
)

// DayStatus untuk tanggal yang BUKAN hadir normal. Hari kerja aktif yang tidak tercantum = PRESENT.
// InputProvider (S6) wajib mencantumkan setiap hari yang tidak hadir normal.
type AttendanceStatus string

const (
	Present          AttendanceStatus = "PRESENT"
	AbsentUnexcused  AttendanceStatus = "ABSENT_UNEXCUSED"
	PaidLeave        AttendanceStatus = "PAID_LEAVE"
	UnpaidLeave      AttendanceStatus = "UNPAID_LEAVE"
	IncompleteRecord AttendanceStatus = "INCOMPLETE"
)

// ComponentDef adalah salinan component_definitions yang dibekukan di snapshot.
type ComponentDef struct {
	Code          string           `json:"code"`
	Name          string           `json:"name"`
	Category      Category         `json:"category"`
	Stage         Stage            `json:"stage"`
	CalcType      CalcType         `json:"calc_type"`
	DefaultAmount *decimal.Decimal `json:"default_amount,omitempty"`
	Rate          *decimal.Decimal `json:"rate,omitempty"`
	RulesetRef    string           `json:"ruleset_ref,omitempty"`
	BaseRef       string           `json:"base_ref,omitempty"`
	DaySource     string           `json:"day_source,omitempty"` // PRESENT | WORKING
	CapMin        *decimal.Decimal `json:"cap_min,omitempty"`
	CapMax        *decimal.Decimal `json:"cap_max,omitempty"`
	Taxable       bool             `json:"taxable"`
	BPJSBase      bool             `json:"bpjs_base"`
	Rounding      Rounding         `json:"rounding"`
	Active        bool             `json:"active"`
}

// ComponentAssignment: komponen yang berlaku untuk employee pada rentang tertentu. Amount/Rate menimpa
// default definisi. Komponen statutory (BPJS) berlaku untuk semua employee; InputProvider memasukkannya
// dengan Range = periode.
type ComponentAssignment struct {
	Def    ComponentDef     `json:"def"`
	Amount *decimal.Decimal `json:"amount,omitempty"`
	Rate   *decimal.Decimal `json:"rate,omitempty"`
	Range  DateRange        `json:"range"`
}

type SalarySpan struct {
	Range      DateRange       `json:"range"`
	BaseSalary decimal.Decimal `json:"base_salary"`
}

type DayStatus struct {
	Date   Date             `json:"date"`
	Status AttendanceStatus `json:"status"`
}

type OvertimeEntry struct {
	Date  Date            `json:"date"`
	Hours decimal.Decimal `json:"hours"`
}

type Adjustment struct {
	ID           string          `json:"id"`
	OriginPeriod string          `json:"origin_period"` // "2026-03"
	Amount       decimal.Decimal `json:"amount"`        // ± rupiah
	Taxable      bool            `json:"taxable"`
	Reason       string          `json:"reason"`
}

// Policy: pengaturan tenant yang memengaruhi hitungan (ikut snapshot).
type Policy struct {
	ProrationBasis         string           `json:"proration_basis"` // WORKING_DAYS | CALENDAR_DAYS
	OvertimeBasis          string           `json:"overtime_basis"`  // BASE | BASE_PLUS_FIXED
	IncompletePolicy       string           `json:"incomplete_policy"`
	OvertimeMaxHoursPerDay *decimal.Decimal `json:"overtime_max_hours_per_day,omitempty"`
}

// Input adalah EmployeePeriodInput (§14): semua yang dibutuhkan untuk menghitung satu employee pada
// satu periode. Diserialisasi apa adanya sebagai input_snapshot (S4).
type Input struct {
	SnapshotVersion int                   `json:"snapshot_version"`
	EmployeeID      string                `json:"employee_id"`
	Period          DateRange             `json:"period"`    // [tgl 1, tgl 1 bulan berikutnya)
	WorkDays        []int                 `json:"work_days"` // ISO weekday hari kerja mingguan
	Holidays        []Date                `json:"holidays"`  // libur tenant di periode
	Employment      []DateRange           `json:"employment"`
	Salary          []SalarySpan          `json:"salary"`
	Components      []ComponentAssignment `json:"components"`
	Attendance      []DayStatus           `json:"attendance"`
	Overtime        []OvertimeEntry       `json:"overtime"`
	Adjustments     []Adjustment          `json:"adjustments"`
	TaxCategory     string                `json:"tax_category"`
	Policy          Policy                `json:"policy"`
}

// Ruleset (§14): nilai regulasi ber-versi. JSON-nya = rulesets.payload.
type Ruleset struct {
	Code            string               `json:"code"`
	Version         string               `json:"version"`
	Label           string               `json:"label"`
	OvertimeDivisor decimal.Decimal      `json:"overtime_divisor"`
	OvertimeTiers   []Tier               `json:"overtime_tiers"`
	Statutory       []StatutoryRule      `json:"statutory"`
	TaxBrackets     map[string][]Bracket `json:"tax_brackets"`
	TaxRounding     Rounding             `json:"tax_rounding"`
}

// Tier: jam lembur per hari mulai FromHour (0-based) dikali Multiplier, sampai tier berikutnya.
type Tier struct {
	FromHour   decimal.Decimal `json:"from_hour"`
	Multiplier decimal.Decimal `json:"multiplier"`
}

type StatutoryRule struct {
	Code     string           `json:"code"`
	Rate     decimal.Decimal  `json:"rate"`
	CapMax   *decimal.Decimal `json:"cap_max,omitempty"`
	Rounding Rounding         `json:"rounding"`
}

// Bracket: base ≤ Max (nil = tak hingga) → Rate untuk SELURUH base (tarif efektif, bukan progresif).
type Bracket struct {
	Max  *decimal.Decimal `json:"max,omitempty"`
	Rate decimal.Decimal  `json:"rate"`
}

type ItemStatus string

const (
	StatusOK    ItemStatus = "OK"
	StatusError ItemStatus = "ERROR"
)

type Issue struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

type Line struct {
	Code        string            `json:"code"`
	Name        string            `json:"name"`
	Category    Category          `json:"category"`
	Stage       Stage             `json:"stage"`
	Amount      decimal.Decimal   `json:"amount"` // sudah dibulatkan (IDR, 0 desimal)
	Basis       map[string]string `json:"basis"`
	Explanation string            `json:"explanation"`
	Taxable     bool              `json:"taxable"`
	Rounding    Rounding          `json:"rounding"`
}

type Result struct {
	Status     ItemStatus      `json:"status"`
	Ruleset    string          `json:"ruleset"`
	Lines      []Line          `json:"lines"`
	Gross      decimal.Decimal `json:"gross"`
	Deductions decimal.Decimal `json:"deductions"`
	Net        decimal.Decimal `json:"net"`
	Errors     []Issue         `json:"errors"`
	Warnings   []Issue         `json:"warnings"`
}

// Kode issue (§13).
const (
	ErrNoSalary        = "NO_SALARY"
	ErrComponentRef    = "COMPONENT_REF_MISSING"
	ErrComponentCycle  = "COMPONENT_CYCLE"
	ErrNegativeNet     = "NEGATIVE_NET"
	ErrNotEligible     = "NOT_ELIGIBLE"
	ErrRulesetMissing  = "RULESET_MISSING"
	WarnZeroBaseSalary = "ZERO_BASE_SALARY"
	WarnOvertimeOver   = "OVERTIME_OVER_LIMIT"
	WarnIncomplete     = "INCOMPLETE_ATTENDANCE"
	WarnInactiveComp   = "INACTIVE_COMPONENT"
	WarnSalaryGap      = "SALARY_GAP"
)
