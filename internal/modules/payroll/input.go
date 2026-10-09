package payroll

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/shopspring/decimal"

	"payrun/internal/modules/payroll/engine"
	"payrun/internal/modules/payroll/store"
)

// InputProvider (Step 3 `06` §1): membangun engine.Input per employee dari data operasional, di dalam
// txRead chunk (REPEATABLE READ READ ONLY → satu snapshot konsisten untuk seluruh chunk). Query per
// jenis data, untuk seluruh employee chunk sekaligus (tanpa N+1).
//
// Belum diisi (sumbernya dibuat di sprint berikutnya): absensi, cuti, lembur (S6), adjustment (S7).
// Engine menganggap hari kerja aktif tanpa catatan = hadir.

const snapshotVersion = 1

func toDec(n pgtype.Numeric) *decimal.Decimal {
	if !n.Valid || n.Int == nil {
		return nil
	}
	v := decimal.NewFromBigInt(n.Int, n.Exp)
	return &v
}

func toNum(d decimal.Decimal) pgtype.Numeric {
	return pgtype.Numeric{Int: d.Coefficient(), Exp: d.Exponent(), Valid: true}
}

// fixed: numeric → string 2 desimal (JSON API); NULL → nil.
func fixed(n pgtype.Numeric) *string {
	d := toDec(n)
	if d == nil {
		return nil
	}
	v := d.StringFixed(2)
	return &v
}

func date(t time.Time) engine.Date { return engine.NewDate(t.Year(), t.Month(), t.Day()) }

func dateRange(from time.Time, openEnded bool, to time.Time) engine.DateRange {
	r := engine.DateRange{From: date(from)}
	if !openEnded {
		d := date(to)
		r.To = &d
	}
	return r
}

// monthRange: [tanggal 1, tanggal 1 bulan berikutnya).
func monthRange(year, month int) (time.Time, time.Time) {
	from := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
	return from, from.AddDate(0, 1, 0)
}

// empMeta: kolom denormalisasi item (R3-4), diambil saat kalkulasi.
type empMeta struct {
	no, name string
	dept     *uuid.UUID
}

type chunkData struct {
	rs        engine.Ruleset
	rsCode    string
	rsVersion string
	inputs    map[uuid.UUID]engine.Input
	meta      map[uuid.UUID]empMeta
}

// loadRuleset: versi ruleset yang berlaku pada tanggal 1 periode. Tidak ada → Ruleset kosong; engine
// menandai setiap item ERROR RULESET_MISSING (bukan run gagal diam-diam).
func loadRuleset(ctx context.Context, q *store.Queries, code string, from time.Time) (engine.Ruleset, error) {
	r, err := q.InputRuleset(ctx, store.InputRulesetParams{Code: code, OnDate: from})
	if errors.Is(err, pgx.ErrNoRows) {
		return engine.Ruleset{}, nil
	}
	if err != nil {
		return engine.Ruleset{}, err
	}
	var rs engine.Ruleset
	if err := json.Unmarshal(r.Payload, &rs); err != nil {
		return engine.Ruleset{}, err
	}
	return rs, nil
}

func loadChunk(ctx context.Context, q *store.Queries, tenantID uuid.UUID, year, month int, ids []uuid.UUID) (chunkData, error) {
	from, to := monthRange(year, month)
	set, err := q.InputSettings(ctx, tenantID)
	if err != nil {
		return chunkData{}, err
	}
	rs, err := loadRuleset(ctx, q, set.RulesetCode, from)
	if err != nil {
		return chunkData{}, err
	}
	out := chunkData{rs: rs, rsCode: rs.Code, rsVersion: rs.Version, inputs: map[uuid.UUID]engine.Input{}, meta: map[uuid.UUID]empMeta{}}

	policy := engine.Policy{ProrationBasis: set.ProrationBasis, OvertimeBasis: set.OvertimeBasis, IncompletePolicy: set.IncompleteAttendancePolicy}
	if set.OvertimeMax != "" {
		v, err := decimal.NewFromString(set.OvertimeMax)
		if err != nil {
			return chunkData{}, err
		}
		policy.OvertimeMaxHoursPerDay = &v
	}
	workDays := make([]int, len(set.WorkDays))
	for i, w := range set.WorkDays {
		workDays[i] = int(w)
	}
	hols, err := q.InputHolidays(ctx, store.InputHolidaysParams{FromDate: from, ToDate: to})
	if err != nil {
		return chunkData{}, err
	}
	holidays := make([]engine.Date, len(hols))
	for i, h := range hols {
		holidays[i] = date(h)
	}
	period := engine.DateRange{From: date(from)}
	pt := date(to)
	period.To = &pt

	statutory, err := q.InputStatutory(ctx)
	if err != nil {
		return chunkData{}, err
	}
	emps, err := q.InputEmployees(ctx, store.InputEmployeesParams{FromDate: from, ToDate: to, Ids: ids})
	if err != nil {
		return chunkData{}, err
	}
	for _, e := range emps {
		in := engine.Input{SnapshotVersion: snapshotVersion, EmployeeID: e.ID.String(), Period: period, WorkDays: workDays,
			Holidays: holidays, Employment: []engine.DateRange{}, Salary: []engine.SalarySpan{}, Components: []engine.ComponentAssignment{},
			Attendance: []engine.DayStatus{}, Overtime: []engine.OvertimeEntry{}, Adjustments: []engine.Adjustment{},
			TaxCategory: engine.TaxCategoryForPTKP(e.PtkpStatus), Policy: policy}
		for _, s := range statutory {
			def := engine.ComponentDef{Code: s.Code, Name: s.Name, Category: engine.Category(s.Category), Stage: engine.Stage(s.Stage),
				CalcType: engine.CalcType(s.CalcType), Rounding: engine.Rounding(s.Rounding), Active: s.Status == "ACTIVE"}
			if s.RulesetRef != nil {
				def.RulesetRef = *s.RulesetRef
			}
			if s.BaseRef != nil {
				def.BaseRef = *s.BaseRef
			}
			if def.Active {
				in.Components = append(in.Components, engine.ComponentAssignment{Def: def, Range: period})
			}
		}
		out.inputs[e.ID] = in
		m := empMeta{no: e.EmployeeNo, name: e.FullName}
		if e.DepartmentID != uuid.Nil {
			id := e.DepartmentID
			m.dept = &id
		}
		out.meta[e.ID] = m
	}
	spans, err := q.InputSpans(ctx, store.InputSpansParams{Ids: ids, FromDate: from, ToDate: to})
	if err != nil {
		return chunkData{}, err
	}
	for _, s := range spans {
		in := out.inputs[s.EmployeeID]
		in.Employment = append(in.Employment, dateRange(s.FromDate, s.OpenEnded, s.ToDate))
		out.inputs[s.EmployeeID] = in
	}
	sal, err := q.InputSalary(ctx, store.InputSalaryParams{Ids: ids, FromDate: from, ToDate: to})
	if err != nil {
		return chunkData{}, err
	}
	for _, s := range sal {
		in := out.inputs[s.EmployeeID]
		in.Salary = append(in.Salary, engine.SalarySpan{Range: dateRange(s.FromDate, s.OpenEnded, s.ToDate), BaseSalary: *toDec(s.BaseSalary)})
		out.inputs[s.EmployeeID] = in
	}
	comps, err := q.InputComponents(ctx, store.InputComponentsParams{Ids: ids, FromDate: from, ToDate: to})
	if err != nil {
		return chunkData{}, err
	}
	for _, c := range comps {
		def := engine.ComponentDef{Code: c.Code, Name: c.Name, Category: engine.Category(c.Category), Stage: engine.Stage(c.Stage),
			CalcType: engine.CalcType(c.CalcType), DefaultAmount: toDec(c.DefaultAmount), Rate: toDec(c.DefRate), CapMin: toDec(c.CapMin),
			CapMax: toDec(c.CapMax), Taxable: c.Taxable, BPJSBase: c.BpjsBase, Rounding: engine.Rounding(c.Rounding), Active: c.Status == "ACTIVE"}
		if c.RulesetRef != nil {
			def.RulesetRef = *c.RulesetRef
		}
		if c.BaseRef != nil {
			def.BaseRef = *c.BaseRef
		}
		if c.DaySource != nil {
			def.DaySource = *c.DaySource
		}
		in := out.inputs[c.EmployeeID]
		in.Components = append(in.Components, engine.ComponentAssignment{Def: def, Amount: toDec(c.Amount), Rate: toDec(c.Rate),
			Range: dateRange(c.FromDate, c.OpenEnded, c.ToDate)})
		out.inputs[c.EmployeeID] = in
	}
	return out, nil
}

// fingerprint (ADR-P07): SHA-256(canonical_json(input) || ruleset). json.Marshal atas struct engine
// deterministik: urutan field tetap, slice diurutkan query, decimal sebagai string ternormalisasi.
func fingerprint(input []byte, rsCode, rsVersion string) []byte {
	h := sha256.New()
	h.Write(input)
	h.Write([]byte{0})
	h.Write([]byte(rsCode + "-" + rsVersion))
	return h.Sum(nil)
}

// runFingerprint = SHA-256(concat(fingerprint item, urut employee_id)).
func runFingerprint(items [][]byte) []byte {
	h := sha256.New()
	for _, f := range items {
		h.Write(f)
	}
	return h.Sum(nil)
}
