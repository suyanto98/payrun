package payroll

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"payrun/internal/modules/payroll/engine"
	"payrun/internal/modules/payroll/store"
	"payrun/internal/platform/db"
)

// VerifyRun (PI-12, ADR-P07 "reproducibility test", perintah ops `payrun verify-run`): untuk setiap
// item versi terkini, engine.Calculate(snapshot input, ruleset versi yang tersimpan) harus identik
// dengan hasil yang tersimpan (status, total, kode issue, setiap baris), dan fingerprint snapshot harus
// cocok dengan input-nya. Tidak membaca data operasional saat ini: bukti bahwa angka final bisa
// dihitung ulang dari jejaknya sendiri, kapan pun.

// Mismatch: satu perbedaan. Reason: MISSING_SNAPSHOT, UNSUPPORTED_SNAPSHOT, RULESET_MISSING,
// FINGERPRINT_MISMATCH, RESULT_MISMATCH, LINE_MISMATCH.
type Mismatch struct {
	ItemID     uuid.UUID `json:"item_id"`
	EmployeeNo string    `json:"employee_no"`
	Reason     string    `json:"reason"`
	Detail     string    `json:"detail"`
}

type VerifyReport struct {
	RunID        uuid.UUID  `json:"run_id"`
	Status       string     `json:"status"`
	CalcVersion  int32      `json:"calc_version"`
	ItemsChecked int        `json:"items_checked"`
	Mismatches   []Mismatch `json:"mismatches"`
}

// OK: tidak ada perbedaan.
func (r VerifyReport) OK() bool { return len(r.Mismatches) == 0 }

// VerifyRun membaca di transaksi tenant tenantID (dipanggil perintah ops dan test; bukan endpoint API).
func (s *Service) VerifyRun(ctx context.Context, tenantID, runID uuid.UUID) (VerifyReport, error) {
	rep := VerifyReport{RunID: runID, Mismatches: []Mismatch{}}
	err := db.WithTenantTxForOptions(ctx, s.pool, tenantID, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		q := store.New(tx)
		run, err := q.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		if run.CalcVersion == 0 {
			return fmt.Errorf("payroll: run %s was never calculated", runID)
		}
		rep.Status, rep.CalcVersion = run.Status, run.CalcVersion
		items, err := q.VerifyItems(ctx, store.VerifyItemsParams{RunID: runID, CalcVersion: run.CalcVersion})
		if err != nil {
			return err
		}
		lines, err := q.VerifyLines(ctx, store.VerifyLinesParams{RunID: runID, CalcVersion: run.CalcVersion})
		if err != nil {
			return err
		}
		byItem := map[uuid.UUID][]store.VerifyLinesRow{}
		for _, l := range lines {
			byItem[l.ItemID] = append(byItem[l.ItemID], l)
		}
		rulesets := map[string]*engine.Ruleset{}
		for _, it := range items {
			rep.ItemsChecked++
			if m := s.verifyItem(ctx, q, it, byItem[it.ID], rulesets); m != nil {
				rep.Mismatches = append(rep.Mismatches, *m)
			}
		}
		return nil
	})
	return rep, db.MapError(err)
}

func (s *Service) verifyItem(ctx context.Context, q *store.Queries, it store.VerifyItemsRow, stored []store.VerifyLinesRow, cache map[string]*engine.Ruleset) *Mismatch {
	bad := func(reason, format string, args ...any) *Mismatch {
		return &Mismatch{ItemID: it.ID, EmployeeNo: it.EmployeeNo, Reason: reason, Detail: fmt.Sprintf(format, args...)}
	}
	if it.SnapshotItemID == nil || it.RulesetCode == nil || it.RulesetVersion == nil {
		return bad("MISSING_SNAPSHOT", "item has no input snapshot")
	}
	if it.SnapshotVersion == nil || *it.SnapshotVersion != snapshotVersion {
		return bad("UNSUPPORTED_SNAPSHOT", "snapshot_version %v (supported: %d)", it.SnapshotVersion, snapshotVersion)
	}
	var in engine.Input
	if err := json.Unmarshal(it.Input, &in); err != nil {
		return bad("FINGERPRINT_MISMATCH", "snapshot input is not valid: %v", err)
	}
	// jsonb menormalisasi spasi dan urutan key; fingerprint dihitung atas serialisasi kanonik struct
	// engine, sama seperti saat kalkulasi.
	raw, err := json.Marshal(in)
	if err != nil {
		return bad("FINGERPRINT_MISMATCH", "re-encode: %v", err)
	}
	fp := fingerprint(raw, *it.RulesetCode, *it.RulesetVersion)
	if !bytes.Equal(fp, it.SnapshotFingerprint) || !bytes.Equal(fp, it.InputFingerprint) {
		return bad("FINGERPRINT_MISMATCH", "snapshot input does not match the stored fingerprint")
	}
	key := *it.RulesetCode + "@" + *it.RulesetVersion
	rs, ok := cache[key]
	if !ok {
		payload, err := q.RulesetByVersion(ctx, store.RulesetByVersionParams{Code: *it.RulesetCode, Version: *it.RulesetVersion})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return bad("RULESET_MISSING", "%v", err)
		}
		if err == nil {
			rs = &engine.Ruleset{}
			if err := json.Unmarshal(payload, rs); err != nil {
				return bad("RULESET_MISSING", "ruleset %s payload: %v", key, err)
			}
		}
		cache[key] = rs
	}
	if rs == nil && *it.RulesetCode != "" {
		return bad("RULESET_MISSING", "ruleset %s not found", key)
	}
	var ruleset engine.Ruleset
	if rs != nil {
		ruleset = *rs
	}
	res := engine.Calculate(in, ruleset)

	status := it.Status
	if status == "EXCLUDED" { // exclude adalah keputusan manusia atas item ERROR, bukan hasil engine
		status = string(engine.StatusError)
	}
	if string(res.Status) != status || !slices.Equal(codes(res.Errors), it.ErrorCodes) || !slices.Equal(codes(res.Warnings), it.WarningCodes) {
		return bad("RESULT_MISMATCH", "status/issues: engine %s %v %v, stored %s %v %v", res.Status, codes(res.Errors), codes(res.Warnings),
			it.Status, it.ErrorCodes, it.WarningCodes)
	}
	if len(res.Lines) > 0 || res.Status == engine.StatusOK {
		for name, pair := range map[string][2]any{"gross": {res.Gross, toDec(it.Gross)}, "deductions": {res.Deductions, toDec(it.Deductions)}, "net": {res.Net, toDec(it.Net)}} {
			want, got := pair[0].(decimal.Decimal), pair[1].(*decimal.Decimal)
			if got == nil || !want.Equal(*got) {
				return bad("RESULT_MISMATCH", "%s: engine %s, stored %v", name, want.StringFixed(2), got)
			}
		}
	}
	if len(res.Lines) != len(stored) {
		return bad("LINE_MISMATCH", "engine %d lines, stored %d", len(res.Lines), len(stored))
	}
	for i, l := range res.Lines {
		st := stored[i]
		amount := toDec(st.Amount)
		var basis map[string]string
		_ = json.Unmarshal(st.Basis, &basis)
		if basis == nil {
			basis = map[string]string{}
		}
		lb := l.Basis
		if lb == nil {
			lb = map[string]string{}
		}
		if l.Code != st.Code || string(l.Category) != st.Category || string(l.Stage) != st.Stage || amount == nil || !l.Amount.Equal(*amount) ||
			l.Explanation != st.Explanation || l.Taxable != st.Taxable || string(l.Rounding) != st.Rounding || !maps.Equal(lb, basis) {
			return bad("LINE_MISMATCH", "line %d (%s): engine %s, stored %s %v", i+1, l.Code, l.Amount.StringFixed(2), st.Code, amount)
		}
	}
	return nil
}
