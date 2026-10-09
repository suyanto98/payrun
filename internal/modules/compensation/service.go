// Package compensation: definisi komponen gaji (C1–C4), riwayat dan perubahan gaji pokok (W8, W9), dan
// penugasan komponen ke employee (W10). Gaji dan komponen effective-dated (ADR-P04): perubahan menutup
// record yang berlaku lalu menyisipkan record baru dalam satu transaksi; irisan dicegah EXCLUDE (409
// OVERLAPPING_PERIOD). Graf referensi PERCENT_OF divalidasi dengan engine.ValidateGraph yang sama
// dengan yang dipakai saat kalkulasi.
package compensation

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"payrun/internal/modules/compensation/store"
	"payrun/internal/modules/payroll/engine"
	"payrun/internal/platform/apperr"
	"payrun/internal/platform/audit"
	"payrun/internal/platform/auth"
	"payrun/internal/platform/clock"
	"payrun/internal/platform/db"
	"payrun/internal/platform/idgen"
	"payrun/internal/platform/periodguard"
	"payrun/internal/platform/reqmeta"
	"payrun/internal/platform/tenantctx"
)

const dateLayout = "2006-01-02"

// Employees: otorisasi akses ke data turunan employee + tanggal mulai kerja. Implementasi di modul
// employee, dipasang composition root (ADR-001). mode: 0 = baca gaji (W8), 1 = tulis (W9/W10).
type Employees interface {
	AccessTx(ctx context.Context, tx pgx.Tx, p auth.Principal, id uuid.UUID, write bool) (hireDate time.Time, err error)
}

type Service struct {
	pool      *pgxpool.Pool
	employees Employees
	clock     clock.Clock
	ids       idgen.Generator
	guard     periodguard.Guard
}

// SetPeriodGuard memasang PeriodGuard (modul payroll) untuk semua penulis input gaji/komponen (S5).
func (s *Service) SetPeriodGuard(g periodguard.Guard) { s.guard = g }

// lockInput: PeriodGuard.Lock sebelum baris input pertama ditulis ([1b] → [2] → [6]).
func (s *Service) lockInput(ctx context.Context, tx pgx.Tx, c periodguard.Change) (periodguard.Recorder, error) {
	if s.guard == nil {
		return nil, errors.New("compensation: period guard not wired")
	}
	return s.guard.Lock(ctx, tx, c)
}

func NewService(pool *pgxpool.Pool, employees Employees, c clock.Clock, ids idgen.Generator) *Service {
	return &Service{pool: pool, employees: employees, clock: c, ids: ids}
}

// ---------------------------------------------------------------- decimal ↔ numeric

func toDec(n pgtype.Numeric) *decimal.Decimal {
	if !n.Valid || n.Int == nil {
		return nil
	}
	v := decimal.NewFromBigInt(n.Int, n.Exp)
	return &v
}

func toNum(d *decimal.Decimal) pgtype.Numeric {
	if d == nil {
		return pgtype.Numeric{}
	}
	return pgtype.Numeric{Int: d.Coefficient(), Exp: d.Exponent(), Valid: true}
}

func money(d *decimal.Decimal) *string { return fixed(d, 2) }
func rate(d *decimal.Decimal) *string  { return fixed(d, 6) }

func fixed(d *decimal.Decimal, places int32) *string {
	if d == nil {
		return nil
	}
	s := d.StringFixed(places)
	return &s
}

// reservedCodes: nama base virtual engine dan kode komponen sistem. Komponen tenant dengan nama ini
// akan membayangi base virtual di validasi graf (review S3).
var reservedCodes = map[string]bool{
	engine.BaseSalaryRef: true, engine.FixedAllowancesRef: true, engine.BPJSWageRef: true, engine.TaxableGrossRef: true,
	"TAX_BRACKETS": true, "OVERTIME": true, "ABSENCE_UNPAID_LEAVE": true, "ABSENCE_UNEXCUSED": true,
	"BPJS_JHT_EE": true, "BPJS_JP_EE": true, "BPJS_KES_EE": true, "PPH21": true, "ADJUSTMENT": true,
}

var (
	moneyPattern = regexp.MustCompile(`^[0-9]{1,16}(\.[0-9]{1,2})?$`)
	ratePattern  = regexp.MustCompile(`^(0(\.[0-9]{1,6})?|1(\.0{1,6})?)$`)
	codePattern  = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,40}$`)
)

func parseMoney(field string, s *string, fe *[]apperr.FieldError) *decimal.Decimal {
	if s == nil {
		return nil
	}
	if !moneyPattern.MatchString(*s) {
		*fe = append(*fe, apperr.FieldError{Field: field, Code: "FORMAT", Message: "decimal string ≥ 0 with at most 2 fraction digits"})
		return nil
	}
	v := decimal.RequireFromString(*s)
	return &v
}

func parseRate(field string, s *string, fe *[]apperr.FieldError) *decimal.Decimal {
	if s == nil {
		return nil
	}
	if !ratePattern.MatchString(*s) {
		*fe = append(*fe, apperr.FieldError{Field: field, Code: "FORMAT", Message: "decimal string 0..1 with at most 6 fraction digits"})
		return nil
	}
	v := decimal.RequireFromString(*s)
	return &v
}

func validText(v string, maxLen int) bool {
	n := utf8.RuneCountInString(v)
	return n >= 1 && n <= maxLen && strings.IndexFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0
}

// ---------------------------------------------------------------- C1–C4 component definitions

type ComponentDefinition struct {
	ID            uuid.UUID `json:"id"`
	Code          string    `json:"code"`
	Name          string    `json:"name"`
	Category      string    `json:"category"`
	Stage         string    `json:"stage"`
	CalcType      string    `json:"calc_type"`
	DefaultAmount *string   `json:"default_amount"`
	Rate          *string   `json:"rate"`
	RulesetRef    *string   `json:"ruleset_ref"`
	BaseRef       *string   `json:"base_ref"`
	DaySource     *string   `json:"day_source"`
	CapMin        *string   `json:"cap_min"`
	CapMax        *string   `json:"cap_max"`
	Taxable       bool      `json:"taxable"`
	BPJSBase      bool      `json:"bpjs_base"`
	Rounding      string    `json:"rounding"`
	IsSystem      bool      `json:"is_system"`
	Status        string    `json:"status"`
	Version       int32     `json:"version"`
}

type defRow = store.GetComponentDefRow

func toDef(r defRow) ComponentDefinition {
	return ComponentDefinition{ID: r.ID, Code: r.Code, Name: r.Name, Category: r.Category, Stage: r.Stage, CalcType: r.CalcType,
		DefaultAmount: money(toDec(r.DefaultAmount)), Rate: rate(toDec(r.Rate)), RulesetRef: r.RulesetRef, BaseRef: r.BaseRef,
		DaySource: r.DaySource, CapMin: money(toDec(r.CapMin)), CapMax: money(toDec(r.CapMax)), Taxable: r.Taxable,
		BPJSBase: r.BpjsBase, Rounding: r.Rounding, IsSystem: r.IsSystem, Status: r.Status, Version: r.Version}
}

func toEngineDef(r defRow) engine.ComponentDef {
	d := engine.ComponentDef{Code: r.Code, Name: r.Name, Category: engine.Category(r.Category), Stage: engine.Stage(r.Stage),
		CalcType: engine.CalcType(r.CalcType), DefaultAmount: toDec(r.DefaultAmount), Rate: toDec(r.Rate), CapMin: toDec(r.CapMin),
		CapMax: toDec(r.CapMax), Taxable: r.Taxable, BPJSBase: r.BpjsBase, Rounding: engine.Rounding(r.Rounding), Active: r.Status == "ACTIVE"}
	if r.RulesetRef != nil {
		d.RulesetRef = *r.RulesetRef
	}
	if r.BaseRef != nil {
		d.BaseRef = *r.BaseRef
	}
	if r.DaySource != nil {
		d.DaySource = *r.DaySource
	}
	return d
}

func (s *Service) ListDefinitions(ctx context.Context, status, category string) ([]ComponentDefinition, error) {
	var fe []apperr.FieldError
	if status != "" && status != "ACTIVE" && status != "INACTIVE" {
		fe = append(fe, apperr.FieldError{Field: "status", Code: "INVALID_ENUM"})
	}
	if category != "" && category != "EARNING" && category != "DEDUCTION" {
		fe = append(fe, apperr.FieldError{Field: "category", Code: "INVALID_ENUM"})
	}
	if len(fe) > 0 {
		return nil, apperr.Validation(fe...)
	}
	params := store.ListComponentDefsParams{}
	if status != "" {
		params.Status = &status
	}
	if category != "" {
		params.Category = &category
	}
	out := []ComponentDefinition{}
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		rs, err := store.New(tx).ListComponentDefs(ctx, params)
		if err != nil {
			return err
		}
		for _, r := range rs {
			if err := db.GuardTenant(ctx, "component_definitions", r.TenantID); err != nil {
				return err
			}
			out = append(out, toDef(defRow(r)))
		}
		return nil
	})
	return out, db.MapError(err)
}

// checkGraph memuat semua definisi, menerapkan perubahan (override, nil = tidak ada), lalu menjalankan
// engine.ValidateGraph. Masalah → 422 COMPONENT_CYCLE / INVALID_COMPONENT_REFERENCE.
func checkGraph(ctx context.Context, q *store.Queries, override *engine.ComponentDef) error {
	tid, _ := tenantctx.From(ctx)
	if err := q.LockComponentGraph(ctx, tid.String()); err != nil {
		return err
	}
	rs, err := q.ListAllComponentDefs(ctx)
	if err != nil {
		return err
	}
	var defs []engine.ComponentDef
	for _, r := range rs {
		d := toEngineDef(defRow(r))
		if override != nil && d.Code == override.Code {
			continue
		}
		defs = append(defs, d)
	}
	if override != nil {
		defs = append(defs, *override)
	}
	if _, p := engine.ValidateGraph(defs); p != nil {
		if p.Code == engine.ProblemCycle {
			return apperr.ErrComponentCycle.WithDetail(p.Detail)
		}
		return apperr.ErrInvalidComponentRf.WithDetail(p.Component + ": " + p.Detail)
	}
	return nil
}

type DefinitionInput struct {
	Code          string  `json:"code"`
	Name          string  `json:"name"`
	Category      string  `json:"category"`
	CalcType      string  `json:"calc_type"`
	DefaultAmount *string `json:"default_amount"`
	Rate          *string `json:"rate"`
	BaseRef       *string `json:"base_ref"`
	DaySource     *string `json:"day_source"`
	CapMin        *string `json:"cap_min"`
	CapMax        *string `json:"cap_max"`
	Taxable       *bool   `json:"taxable"`
	BPJSBase      *bool   `json:"bpjs_base"`
	Rounding      *string `json:"rounding"`
}

// validateParams: aturan ck_component_definitions_params dalam bahasa API (400 per field, bukan 500
// dari CHECK). Komponen tenant selalu stage EARNING; PER_HOUR_MULTIPLIER hanya sistem.
func validateParams(calcType string, amount, rt *decimal.Decimal, baseRef, daySource *string, capMin, capMax *decimal.Decimal, fe *[]apperr.FieldError) {
	add := func(field, msg string) {
		*fe = append(*fe, apperr.FieldError{Field: field, Code: "INVALID_FOR_CALC_TYPE", Message: msg})
	}
	switch calcType {
	case "FIXED":
		if rt != nil || baseRef != nil || daySource != nil {
			add("calc_type", "FIXED takes default_amount only")
		}
	case "PER_DAY":
		if daySource == nil || (*daySource != "PRESENT" && *daySource != "WORKING") {
			add("day_source", "PER_DAY requires day_source PRESENT or WORKING")
		}
		if rt != nil || baseRef != nil {
			add("calc_type", "PER_DAY takes default_amount and day_source only")
		}
	case "PERCENT_OF":
		if baseRef == nil || *baseRef == "" {
			add("base_ref", "PERCENT_OF requires base_ref")
		}
		if rt == nil {
			add("rate", "PERCENT_OF requires rate")
		}
		if amount != nil || daySource != nil {
			add("calc_type", "PERCENT_OF takes rate, base_ref and caps only")
		}
	default:
		*fe = append(*fe, apperr.FieldError{Field: "calc_type", Code: "INVALID_ENUM", Message: "FIXED, PER_DAY or PERCENT_OF (PER_HOUR_MULTIPLIER is system-only)"})
	}
	if capMin != nil && capMax != nil && capMin.GreaterThan(*capMax) {
		*fe = append(*fe, apperr.FieldError{Field: "cap_min", Code: "OUT_OF_RANGE", Message: "cap_min must be ≤ cap_max"})
	}
}

func (s *Service) CreateDefinition(ctx context.Context, actor uuid.UUID, in DefinitionInput, meta reqmeta.Meta) (ComponentDefinition, error) {
	in.Code, in.Name = strings.TrimSpace(in.Code), strings.TrimSpace(in.Name)
	var fe []apperr.FieldError
	if !codePattern.MatchString(in.Code) {
		fe = append(fe, apperr.FieldError{Field: "code", Code: "FORMAT", Message: "must match ^[A-Z][A-Z0-9_]{1,40}$"})
	} else if reservedCodes[in.Code] {
		fe = append(fe, apperr.FieldError{Field: "code", Code: "RESERVED", Message: "reserved for system components and engine bases"})
	}
	if in.Category == "DEDUCTION" && in.BPJSBase != nil && *in.BPJSBase {
		fe = append(fe, apperr.FieldError{Field: "bpjs_base", Code: "INVALID", Message: "a deduction cannot be part of the BPJS wage"})
	}
	if !validText(in.Name, 100) {
		fe = append(fe, apperr.FieldError{Field: "name", Code: "LENGTH", Message: "must be 1..100 characters"})
	}
	if in.Category != "EARNING" && in.Category != "DEDUCTION" {
		fe = append(fe, apperr.FieldError{Field: "category", Code: "INVALID_ENUM"})
	}
	rounding := "HALF_UP"
	if in.Rounding != nil {
		rounding = *in.Rounding
	}
	if rounding != "HALF_UP" && rounding != "DOWN" && rounding != "UP" {
		fe = append(fe, apperr.FieldError{Field: "rounding", Code: "INVALID_ENUM"})
	}
	amount := parseMoney("default_amount", in.DefaultAmount, &fe)
	rt := parseRate("rate", in.Rate, &fe)
	capMin, capMax := parseMoney("cap_min", in.CapMin, &fe), parseMoney("cap_max", in.CapMax, &fe)
	validateParams(in.CalcType, amount, rt, in.BaseRef, in.DaySource, capMin, capMax, &fe)
	if len(fe) > 0 {
		return ComponentDefinition{}, apperr.Validation(fe...)
	}
	taxable, bpjs := true, false
	if in.Taxable != nil {
		taxable = *in.Taxable
	}
	if in.BPJSBase != nil {
		bpjs = *in.BPJSBase
	}
	tid, _ := tenantctx.From(ctx)
	var out ComponentDefinition
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		ed := engine.ComponentDef{Code: in.Code, Category: engine.Category(in.Category), Stage: engine.StageEarning,
			CalcType: engine.CalcType(in.CalcType), Active: true}
		if in.BaseRef != nil {
			ed.BaseRef = *in.BaseRef
		}
		if err := checkGraph(ctx, q, &ed); err != nil {
			return err
		}
		id, err := q.CreateComponentDef(ctx, store.CreateComponentDefParams{TenantID: tid, ID: s.ids.New(), Code: in.Code, Name: in.Name,
			Category: in.Category, CalcType: in.CalcType, DefaultAmount: toNum(amount), Rate: toNum(rt), BaseRef: in.BaseRef,
			DaySource: in.DaySource, CapMin: toNum(capMin), CapMax: toNum(capMax), Taxable: taxable, BpjsBase: bpjs, Rounding: rounding, Actor: &actor})
		if err != nil {
			return err
		}
		r, err := q.GetComponentDef(ctx, id)
		if err != nil {
			return err
		}
		out = toDef(r)
		return audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: actor, Action: "component.created",
			TargetType: "component_definition", TargetID: id, Diff: map[string]any{"code": in.Code, "calc_type": in.CalcType}}, meta)
	})
	return out, db.MapError(err)
}

type DefinitionUpdate struct {
	Name          *string `json:"name"`
	DefaultAmount *string `json:"default_amount"`
	Rate          *string `json:"rate"`
	BaseRef       *string `json:"base_ref"`
	CapMin        *string `json:"cap_min"`
	CapMax        *string `json:"cap_max"`
	Taxable       *bool   `json:"taxable"`
	BPJSBase      *bool   `json:"bpjs_base"`
	Rounding      *string `json:"rounding"`
}

func (u DefinitionUpdate) onlyName() bool {
	return u.DefaultAmount == nil && u.Rate == nil && u.BaseRef == nil && u.CapMin == nil && u.CapMax == nil &&
		u.Taxable == nil && u.BPJSBase == nil && u.Rounding == nil
}

func (s *Service) UpdateDefinition(ctx context.Context, actor, id uuid.UUID, version int32, in DefinitionUpdate, meta reqmeta.Meta) (ComponentDefinition, error) {
	var fe []apperr.FieldError
	if in.Name != nil {
		v := strings.TrimSpace(*in.Name)
		in.Name = &v
		if !validText(v, 100) {
			fe = append(fe, apperr.FieldError{Field: "name", Code: "LENGTH", Message: "must be 1..100 characters"})
		}
	}
	if in.Rounding != nil && *in.Rounding != "HALF_UP" && *in.Rounding != "DOWN" && *in.Rounding != "UP" {
		fe = append(fe, apperr.FieldError{Field: "rounding", Code: "INVALID_ENUM"})
	}
	if in.BaseRef != nil && strings.TrimSpace(*in.BaseRef) == "" {
		fe = append(fe, apperr.FieldError{Field: "base_ref", Code: "REQUIRED", Message: "must not be empty"})
	}
	amount, rt := parseMoney("default_amount", in.DefaultAmount, &fe), parseRate("rate", in.Rate, &fe)
	capMin, capMax := parseMoney("cap_min", in.CapMin, &fe), parseMoney("cap_max", in.CapMax, &fe)
	if len(fe) > 0 {
		return ComponentDefinition{}, apperr.Validation(fe...)
	}
	tid, _ := tenantctx.From(ctx)
	var out ComponentDefinition
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		cur, err := q.GetComponentDef(ctx, id)
		if err != nil {
			return err
		}
		if err := db.GuardTenant(ctx, "component_definitions", cur.TenantID); err != nil {
			return err
		}
		if cur.Version != version {
			return apperr.ErrVersionConflict
		}
		if cur.IsSystem && !in.onlyName() {
			return apperr.ErrForbidden.WithDetail("Only the name of a system component can be changed")
		}
		record, err := s.lockInput(ctx, tx, defChange(cur.IsSystem, id))
		if err != nil {
			return err
		}
		// Parameter hasil gabungan harus tetap sah untuk calc_type (dan untuk graf).
		merged := toEngineDef(cur)
		if in.BaseRef != nil {
			merged.BaseRef = *in.BaseRef
		}
		var pe []apperr.FieldError
		if in.BaseRef != nil && cur.CalcType != "PERCENT_OF" {
			pe = append(pe, apperr.FieldError{Field: "base_ref", Code: "INVALID_FOR_CALC_TYPE", Message: "base_ref applies to PERCENT_OF only"})
		}
		if cur.Category == "DEDUCTION" && in.BPJSBase != nil && *in.BPJSBase {
			pe = append(pe, apperr.FieldError{Field: "bpjs_base", Code: "INVALID", Message: "a deduction cannot be part of the BPJS wage"})
		}
		if !cur.IsSystem {
			mAmount, mRate, mCapMin, mCapMax := toDec(cur.DefaultAmount), toDec(cur.Rate), toDec(cur.CapMin), toDec(cur.CapMax)
			if amount != nil {
				mAmount = amount
			}
			if rt != nil {
				mRate = rt
			}
			if capMin != nil {
				mCapMin = capMin
			}
			if capMax != nil {
				mCapMax = capMax
			}
			var base *string
			if merged.BaseRef != "" {
				base = &merged.BaseRef
			}
			validateParams(cur.CalcType, mAmount, mRate, base, cur.DaySource, mCapMin, mCapMax, &pe)
		}
		if len(pe) > 0 {
			return apperr.Validation(pe...)
		}
		if err := checkGraph(ctx, q, &merged); err != nil {
			return err
		}
		if _, err := q.UpdateComponentDef(ctx, store.UpdateComponentDefParams{Name: in.Name, DefaultAmount: toNum(amount), Rate: toNum(rt),
			BaseRef: in.BaseRef, CapMin: toNum(capMin), CapMax: toNum(capMax), Taxable: in.Taxable, BpjsBase: in.BPJSBase,
			Rounding: in.Rounding, Actor: &actor, ID: id, Version: version}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return apperr.ErrVersionConflict
			}
			return err
		}
		r, err := q.GetComponentDef(ctx, id)
		if err != nil {
			return err
		}
		out = toDef(r)
		if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: actor, Action: "component.updated",
			TargetType: "component_definition", TargetID: id, Diff: map[string]any{"fields": changedFields(in)}}, meta); err != nil {
			return err
		}
		return record(ctx)
	})
	return out, db.MapError(err)
}

// defChange: definisi komponen tidak effective-dated (hasil final tetap dari snapshot). Komponen
// sistem dipakai semua karyawan; komponen tenant hanya oleh pemegangnya.
func defChange(system bool, id uuid.UUID) periodguard.Change {
	if system {
		return periodguard.Change{AllEmployees: true, NotEffectiveDated: true}
	}
	return periodguard.Change{ComponentID: &id, NotEffectiveDated: true}
}

func changedFields(u DefinitionUpdate) []string {
	var out []string
	for name, set := range map[string]bool{"name": u.Name != nil, "default_amount": u.DefaultAmount != nil, "rate": u.Rate != nil,
		"base_ref": u.BaseRef != nil, "cap_min": u.CapMin != nil, "cap_max": u.CapMax != nil, "taxable": u.Taxable != nil,
		"bpjs_base": u.BPJSBase != nil, "rounding": u.Rounding != nil} {
		if set {
			out = append(out, name)
		}
	}
	sort.Strings(out) // urutan diff audit deterministik
	return out
}

// DeactivateDefinition (C4): sistem → 403; masih direferensikan komponen aktif → 422; idempoten.
func (s *Service) DeactivateDefinition(ctx context.Context, actor, id uuid.UUID, meta reqmeta.Meta) (ComponentDefinition, error) {
	tid, _ := tenantctx.From(ctx)
	var out ComponentDefinition
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		cur, err := q.GetComponentDef(ctx, id)
		if err != nil {
			return err
		}
		if err := db.GuardTenant(ctx, "component_definitions", cur.TenantID); err != nil {
			return err
		}
		if cur.IsSystem {
			return apperr.ErrForbidden.WithDetail("System components cannot be deactivated")
		}
		if cur.Status == "ACTIVE" {
			record, err := s.lockInput(ctx, tx, defChange(false, id))
			if err != nil {
				return err
			}
			off := toEngineDef(cur)
			off.Active = false
			if err := checkGraph(ctx, q, &off); err != nil {
				return err
			}
			if _, err := q.DeactivateComponentDef(ctx, store.DeactivateComponentDefParams{Actor: &actor, ID: id}); err != nil {
				return err
			}
			if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: actor, Action: "component.deactivated",
				TargetType: "component_definition", TargetID: id}, meta); err != nil {
				return err
			}
			if err := record(ctx); err != nil {
				return err
			}
		}
		r, err := q.GetComponentDef(ctx, id)
		if err != nil {
			return err
		}
		out = toDef(r)
		return nil
	})
	return out, db.MapError(err)
}

// ---------------------------------------------------------------- W8–W10 gaji & komponen employee

type SalaryAssignment struct {
	ID           uuid.UUID  `json:"id"`
	EmployeeID   uuid.UUID  `json:"employee_id"`
	BaseSalary   string     `json:"base_salary"`
	ValidFrom    string     `json:"valid_from"`
	ValidTo      *string    `json:"valid_to"` // eksklusif (half-open)
	ChangeReason *string    `json:"change_reason"`
	CreatedBy    *uuid.UUID `json:"created_by"`
	CreatedAt    time.Time  `json:"created_at"`
}

type ComponentRef struct {
	ID       uuid.UUID `json:"id"`
	Code     string    `json:"code"`
	Name     string    `json:"name"`
	CalcType string    `json:"calc_type"`
}

type EmployeeComponent struct {
	ID         uuid.UUID    `json:"id"`
	EmployeeID uuid.UUID    `json:"employee_id"`
	Component  ComponentRef `json:"component"`
	Amount     *string      `json:"amount"`
	Rate       *string      `json:"rate"`
	ValidFrom  string       `json:"valid_from"`
	ValidTo    *string      `json:"valid_to"`
}

type SalaryHistory struct {
	Salary     []SalaryAssignment  `json:"salary"`
	Components []EmployeeComponent `json:"components"`
}

func validTo(open bool, to time.Time) *string {
	if open {
		return nil
	}
	s := to.Format(dateLayout)
	return &s
}

func (s *Service) History(ctx context.Context, p auth.Principal, employeeID uuid.UUID, asOf string) (SalaryHistory, error) {
	var at *time.Time
	if asOf != "" {
		t, err := time.Parse(dateLayout, asOf)
		if err != nil {
			return SalaryHistory{}, apperr.Validation(apperr.FieldError{Field: "as_of", Code: "FORMAT", Message: "must be YYYY-MM-DD"})
		}
		at = &t
	}
	h := SalaryHistory{Salary: []SalaryAssignment{}, Components: []EmployeeComponent{}}
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := s.employees.AccessTx(ctx, tx, p, employeeID, false); err != nil {
			return err
		}
		q := store.New(tx)
		sal, err := q.ListSalary(ctx, store.ListSalaryParams{EmployeeID: employeeID, AsOf: at})
		if err != nil {
			return err
		}
		for _, r := range sal {
			if err := db.GuardTenant(ctx, "salary_assignments", r.TenantID); err != nil {
				return err
			}
			h.Salary = append(h.Salary, SalaryAssignment{ID: r.ID, EmployeeID: r.EmployeeID, BaseSalary: *money(toDec(r.BaseSalary)),
				ValidFrom: r.ValidFrom.Format(dateLayout), ValidTo: validTo(r.OpenEnded, r.ValidTo), ChangeReason: r.ChangeReason,
				CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt})
		}
		comps, err := q.ListEmployeeComponents(ctx, store.ListEmployeeComponentsParams{EmployeeID: employeeID, AsOf: at})
		if err != nil {
			return err
		}
		for _, r := range comps {
			if err := db.GuardTenant(ctx, "employee_components", r.TenantID); err != nil {
				return err
			}
			h.Components = append(h.Components, EmployeeComponent{ID: r.ID, EmployeeID: r.EmployeeID,
				Component: ComponentRef{ID: r.ComponentID, Code: r.Code, Name: r.Name, CalcType: r.CalcType},
				Amount:    money(toDec(r.Amount)), Rate: rate(toDec(r.Rate)), ValidFrom: r.ValidFrom.Format(dateLayout), ValidTo: validTo(r.OpenEnded, r.ValidTo)})
		}
		return nil
	})
	return h, db.MapError(err)
}

type SalaryChange struct {
	BaseSalary   string  `json:"base_salary"`
	ValidFrom    string  `json:"valid_from"`
	ChangeReason *string `json:"change_reason"`
}

// ChangeSalaryTx (W9) di transaksi idempotency: tutup record yang berlaku di valid_from, sisipkan
// [valid_from, ∞). Perubahan masa depan yang sudah ada → EXCLUDE → 409 OVERLAPPING_PERIOD.
// PeriodGuard [valid_from, ∞): periode final → 409 PERIOD_LOCKED; periode non-final dicatat (O12).
func (s *Service) ChangeSalaryTx(ctx context.Context, tx pgx.Tx, p auth.Principal, employeeID uuid.UUID, in SalaryChange, meta reqmeta.Meta) (SalaryAssignment, error) {
	tid, _ := tenantctx.From(ctx)
	hire, err := s.employees.AccessTx(ctx, tx, p, employeeID, true)
	if err != nil {
		return SalaryAssignment{}, err
	}
	var fe []apperr.FieldError
	amount := parseMoney("base_salary", &in.BaseSalary, &fe)
	from, perr := time.Parse(dateLayout, in.ValidFrom)
	switch {
	case perr != nil:
		fe = append(fe, apperr.FieldError{Field: "valid_from", Code: "FORMAT", Message: "must be YYYY-MM-DD"})
	case from.Before(hire):
		fe = append(fe, apperr.FieldError{Field: "valid_from", Code: "OUT_OF_RANGE", Message: "must not be before hire_date " + hire.Format(dateLayout)})
	case from.After(s.clock.Now().AddDate(0, 0, 366)):
		fe = append(fe, apperr.FieldError{Field: "valid_from", Code: "OUT_OF_RANGE", Message: "must be at most 366 days ahead"})
	}
	if in.ChangeReason != nil {
		v := strings.TrimSpace(*in.ChangeReason)
		in.ChangeReason = &v
		if !validText(v, 200) {
			fe = append(fe, apperr.FieldError{Field: "change_reason", Code: "LENGTH", Message: "must be 1..200 characters"})
		}
	}
	if len(fe) > 0 {
		return SalaryAssignment{}, apperr.Validation(fe...)
	}
	record, err := s.lockInput(ctx, tx, periodguard.Change{Employees: []uuid.UUID{employeeID}, Range: periodguard.From(from)})
	if err != nil {
		return SalaryAssignment{}, err
	}
	out, err := s.insertSalary(ctx, tx, tid, p.UserID, employeeID, *amount, from, in.ChangeReason, "salary.changed", meta)
	if err != nil {
		return SalaryAssignment{}, err
	}
	return out, record(ctx)
}

// CreateInitialSalaryTx: gaji awal dari POST /employees (W2), di transaksi pembuatan employee. PeriodGuard
// sudah diambil pemanggil (employee, rentang [hire_date, ∞)).
func (s *Service) CreateInitialSalaryTx(ctx context.Context, tx pgx.Tx, actor, employeeID uuid.UUID, baseSalary string, from time.Time, meta reqmeta.Meta) error {
	var fe []apperr.FieldError
	amount := parseMoney("salary.base_salary", &baseSalary, &fe)
	if len(fe) > 0 {
		return apperr.Validation(fe...)
	}
	tid, _ := tenantctx.From(ctx)
	_, err := s.insertSalary(ctx, tx, tid, actor, employeeID, *amount, from, nil, "salary.created", meta)
	return err
}

func (s *Service) insertSalary(ctx context.Context, tx pgx.Tx, tid, actor, employeeID uuid.UUID, amount decimal.Decimal, from time.Time,
	reason *string, action string, meta reqmeta.Meta) (SalaryAssignment, error) {
	q := store.New(tx)
	if _, err := q.CloseSalaryAt(ctx, store.CloseSalaryAtParams{ValidFrom: from, Actor: &actor, EmployeeID: employeeID}); err != nil {
		return SalaryAssignment{}, db.MapError(err)
	}
	id, err := q.InsertSalary(ctx, store.InsertSalaryParams{TenantID: tid, ID: s.ids.New(), EmployeeID: employeeID,
		BaseSalary: toNum(&amount), ValidFrom: from, ChangeReason: reason, Actor: &actor})
	if err != nil {
		return SalaryAssignment{}, db.MapError(err)
	}
	if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: actor, Action: action,
		TargetType: "salary_assignment", TargetID: id,
		Diff: map[string]any{"employee_id": employeeID.String(), "base_salary": amount.StringFixed(2), "valid_from": from.Format(dateLayout)}}, meta); err != nil {
		return SalaryAssignment{}, err
	}
	return SalaryAssignment{ID: id, EmployeeID: employeeID, BaseSalary: amount.StringFixed(2), ValidFrom: from.Format(dateLayout),
		ChangeReason: reason, CreatedBy: &actor, CreatedAt: s.clock.Now()}, nil
}

type ComponentAssignmentInput struct {
	ComponentID uuid.UUID `json:"component_id"`
	Amount      *string   `json:"amount"`
	Rate        *string   `json:"rate"`
	ValidFrom   string    `json:"valid_from"`
	ValidTo     *string   `json:"valid_to"`
}

// AssignComponentTx (W10): komponen ACTIVE dan bukan sistem; amount hanya FIXED/PER_DAY, rate hanya
// PERCENT_OF non-statutory. Record yang berlaku untuk komponen yang sama ditutup di valid_from.
func (s *Service) AssignComponentTx(ctx context.Context, tx pgx.Tx, p auth.Principal, employeeID uuid.UUID, in ComponentAssignmentInput, meta reqmeta.Meta) (EmployeeComponent, error) {
	tid, _ := tenantctx.From(ctx)
	hire, err := s.employees.AccessTx(ctx, tx, p, employeeID, true)
	if err != nil {
		return EmployeeComponent{}, err
	}
	q := store.New(tx)
	def, err := q.GetComponentDef(ctx, in.ComponentID)
	if err != nil {
		return EmployeeComponent{}, db.MapError(err)
	}
	if err := db.GuardTenant(ctx, "component_definitions", def.TenantID); err != nil {
		return EmployeeComponent{}, err
	}
	var fe []apperr.FieldError
	if def.IsSystem || def.Status != "ACTIVE" {
		fe = append(fe, apperr.FieldError{Field: "component_id", Code: "NOT_ASSIGNABLE", Message: "component must be ACTIVE and not a system component"})
	}
	amount, rt := parseMoney("amount", in.Amount, &fe), parseRate("rate", in.Rate, &fe)
	switch def.CalcType {
	case "FIXED", "PER_DAY":
		if rt != nil {
			fe = append(fe, apperr.FieldError{Field: "rate", Code: "INVALID_FOR_CALC_TYPE", Message: "rate applies to PERCENT_OF only"})
		}
		if amount == nil && !def.DefaultAmount.Valid {
			fe = append(fe, apperr.FieldError{Field: "amount", Code: "REQUIRED", Message: "component has no default_amount"})
		}
	case "PERCENT_OF":
		if amount != nil {
			fe = append(fe, apperr.FieldError{Field: "amount", Code: "INVALID_FOR_CALC_TYPE", Message: "amount applies to FIXED/PER_DAY only"})
		}
	}
	from, perr := time.Parse(dateLayout, in.ValidFrom)
	if perr != nil {
		fe = append(fe, apperr.FieldError{Field: "valid_from", Code: "FORMAT", Message: "must be YYYY-MM-DD"})
	} else if from.Before(hire) {
		fe = append(fe, apperr.FieldError{Field: "valid_from", Code: "OUT_OF_RANGE", Message: "must not be before hire_date " + hire.Format(dateLayout)})
	}
	var to *time.Time
	if in.ValidTo != nil {
		t, err := time.Parse(dateLayout, *in.ValidTo)
		if err != nil || (perr == nil && !t.After(from)) {
			fe = append(fe, apperr.FieldError{Field: "valid_to", Code: "OUT_OF_RANGE", Message: "must be YYYY-MM-DD after valid_from"})
		}
		to = &t
	}
	if len(fe) > 0 {
		return EmployeeComponent{}, apperr.Validation(fe...)
	}
	record, err := s.lockInput(ctx, tx, periodguard.Change{Employees: []uuid.UUID{employeeID}, Range: periodguard.Range{From: from, To: to}})
	if err != nil {
		return EmployeeComponent{}, err
	}
	// Override sementara [from, to) di tengah record yang berlaku: ekor record lama [to, akhir lama)
	// dipertahankan dengan nilai lama, tidak ikut hilang (review S3).
	prev, perr2 := q.GetEmployeeComponentAt(ctx, store.GetEmployeeComponentAtParams{EmployeeID: employeeID, ComponentID: in.ComponentID, ValidFrom: from})
	hasPrev := perr2 == nil
	if perr2 != nil && !errors.Is(perr2, pgx.ErrNoRows) {
		return EmployeeComponent{}, db.MapError(perr2)
	}
	if _, err := q.CloseEmployeeComponentAt(ctx, store.CloseEmployeeComponentAtParams{ValidFrom: from, Actor: &p.UserID, EmployeeID: employeeID, ComponentID: in.ComponentID}); err != nil {
		return EmployeeComponent{}, db.MapError(err)
	}
	if hasPrev && to != nil && (prev.OpenEnded || to.Before(prev.ToDate)) {
		var tailTo *time.Time
		if !prev.OpenEnded {
			tailTo = &prev.ToDate
		}
		if _, err := q.InsertEmployeeComponent(ctx, store.InsertEmployeeComponentParams{TenantID: tid, ID: s.ids.New(), EmployeeID: employeeID,
			ComponentID: in.ComponentID, Amount: prev.Amount, Rate: prev.Rate, ValidFrom: *to, ToDate: tailTo, Actor: &p.UserID}); err != nil {
			return EmployeeComponent{}, db.MapError(err)
		}
	}
	id, err := q.InsertEmployeeComponent(ctx, store.InsertEmployeeComponentParams{TenantID: tid, ID: s.ids.New(), EmployeeID: employeeID,
		ComponentID: in.ComponentID, Amount: toNum(amount), Rate: toNum(rt), ValidFrom: from, ToDate: to, Actor: &p.UserID})
	if err != nil {
		return EmployeeComponent{}, db.MapError(err)
	}
	if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: p.UserID, Action: "component.assigned",
		TargetType: "employee_component", TargetID: id,
		Diff: map[string]any{"employee_id": employeeID.String(), "component": def.Code, "valid_from": in.ValidFrom,
			"valid_to": in.ValidTo, "closed_previous": hasPrev}}, meta); err != nil {
		return EmployeeComponent{}, err
	}
	if err := record(ctx); err != nil {
		return EmployeeComponent{}, err
	}
	var toStr *string
	if to != nil {
		v := to.Format(dateLayout)
		toStr = &v
	}
	return EmployeeComponent{ID: id, EmployeeID: employeeID, Component: ComponentRef{ID: def.ID, Code: def.Code, Name: def.Name, CalcType: def.CalcType},
		Amount: money(amount), Rate: rate(rt), ValidFrom: from.Format(dateLayout), ValidTo: toStr}, nil
}
