package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ---------------------------------------------------------------- S6: absensi, cuti, lembur → engine

func (e env) leaveTypeID(t *testing.T, tenant uuid.UUID, code string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	e.inTenant(t, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT id FROM leave_types WHERE code = $1`, code).Scan(&id)
	})
	return id
}

func (e env) submitLeave(t *testing.T, token string, typeID uuid.UUID, start, end string) (result, map[string]any) {
	t.Helper()
	return e.doH(t, http.MethodPost, "/api/v1/leave-requests", token, key(), map[string]any{"leave_type_id": typeID, "start_date": start, "end_date": end})
}

func (e env) submitOvertime(t *testing.T, token, day, from, to string) (result, map[string]any) {
	t.Helper()
	return e.doH(t, http.MethodPost, "/api/v1/overtime-requests", token, key(), map[string]any{
		"work_date": day, "start_at": day + "T" + from + ":00+07:00", "end_at": day + "T" + to + ":00+07:00", "reason": "Stok opname"})
}

// advance: jam palsu maju; access token (15 menit) diterbitkan ulang untuk semua role fixture.
func (e env) advance(t *testing.T, f *tenantFx, d time.Duration) {
	t.Helper()
	e.clk.Advance(d)
	for _, role := range allRoleNames {
		f.tokens[role], _, _ = e.login(t, f.emails[role])
	}
}

func approvalOf(t *testing.T, b map[string]any) uuid.UUID {
	t.Helper()
	a, ok := b["approval"].(map[string]any)
	if !ok {
		t.Fatalf("response without approval: %v", b)
	}
	return uuid.MustParse(a["id"].(string))
}

func (e env) balance(t *testing.T, token string, year int, code string) map[string]any {
	t.Helper()
	_, b := e.do(t, http.MethodGet, "/api/v1/me/leave-balances?year="+itoa(year), token, nil)
	for _, x := range b["data"].([]any) {
		m := x.(map[string]any)
		if m["leave_type"].(map[string]any)["code"] == code {
			return m
		}
	}
	t.Fatalf("no balance for %s: %v", code, b)
	return nil
}

// TestAttendance_ClockInOut (PF-08 AC1/AC2): satu catatan per hari; clock-in kedua dan clock-out kedua
// idempoten tanpa menimpa waktu pertama; clock-out tanpa clock-in → 422; shift melewati tengah malam
// ditutup pada catatan hari kemarin; daftar dibatasi scope; ringkasan periode.
func TestAttendance_ClockInOut(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	emp := f.tokens[roleE]

	r1, in1 := e.do(t, http.MethodPost, "/api/v1/attendance/clock-in", emp, nil)
	r2, in2 := e.do(t, http.MethodPost, "/api/v1/attendance/clock-in", emp, nil)
	if r1.StatusCode != http.StatusCreated || in1["status"] != "INCOMPLETE" || in1["work_date"] != "2026-10-06" ||
		r2.StatusCode != http.StatusOK || in2["id"] != in1["id"] || in2["clock_in_at"] != in1["clock_in_at"] {
		t.Fatalf("clock-in twice: %d %v / %d %v", r1.StatusCode, in1, r2.StatusCode, in2)
	}
	e.advance(t, f, 2*time.Hour)
	emp = f.tokens[roleE]
	_, out1 := e.do(t, http.MethodPost, "/api/v1/attendance/clock-out", emp, nil)
	e.advance(t, f, time.Hour)
	emp = f.tokens[roleE]
	r, out2 := e.do(t, http.MethodPost, "/api/v1/attendance/clock-out", emp, nil)
	if out1["status"] != "COMPLETE" || r.StatusCode != http.StatusOK || out2["clock_out_at"] != out1["clock_out_at"] {
		t.Fatalf("clock-out twice: %v / %v", out1, out2)
	}
	if r, b := e.do(t, http.MethodPost, "/api/v1/attendance/clock-out", f.tokens[roleM], nil); r.StatusCode != http.StatusUnprocessableEntity || b["code"] != "CLOCK_OUT_WITHOUT_CLOCK_IN" {
		t.Fatalf("clock-out without clock-in: %d %v", r.StatusCode, b)
	}
	if r, b := e.do(t, http.MethodPost, "/api/v1/attendance/clock-in", f.tokens[roleO], nil); r.StatusCode != http.StatusUnprocessableEntity || b["code"] != "EMPLOYEE_NOT_ACTIVE" {
		t.Fatalf("user without employee: %d %v", r.StatusCode, b)
	}

	// Shift malam MANAGER: clock-in 22:00 WIB 6 Okt, clock-out 06:00 WIB 7 Okt → catatan 6 Okt COMPLETE.
	e.advance(t, f, 3*time.Hour) // 22:00 WIB
	_, night := e.do(t, http.MethodPost, "/api/v1/attendance/clock-in", f.tokens[roleM], nil)
	e.advance(t, f, 8*time.Hour) // 06:00 WIB besok
	_, closed := e.do(t, http.MethodPost, "/api/v1/attendance/clock-out", f.tokens[roleM], nil)
	emp = f.tokens[roleE]
	if closed["id"] != night["id"] || closed["work_date"] != "2026-10-06" || closed["status"] != "COMPLETE" {
		t.Fatalf("night shift: %v → %v", night, closed)
	}

	_, mine := e.do(t, http.MethodGet, "/api/v1/attendance?period=2026-10", emp, nil)
	if n := len(mine["data"].([]any)); n != 1 {
		t.Fatalf("EMPLOYEE sees only own records, got %d", n)
	}
	_, sum := e.do(t, http.MethodGet, "/api/v1/attendance?period=2026-10&employee_id="+f.empSelf.String(), emp, nil)
	if s := sum["summary"].(map[string]any); s["present_days"] != float64(1) || s["working_days"] != float64(22) {
		t.Fatalf("summary: %v", s)
	}
	if r, _ := e.do(t, http.MethodGet, "/api/v1/attendance?period=2026-10&employee_id="+f.empOther.String(), emp, nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("other employee's attendance: want 404, got %d", r.StatusCode)
	}
	_, team := e.do(t, http.MethodGet, "/api/v1/attendance?period=2026-10", f.tokens[roleM], nil)
	if n := len(team["data"].([]any)); n != 2 {
		t.Fatalf("MANAGER sees self + direct report, got %d", n)
	}
}

// TestAttendanceEdit_FinalizedPeriod_PeriodLocked (DoD S6, PF-08 AC3, PI-14): HR mengoreksi catatan
// dengan alasan + If-Match (audit before/after); setelah periodenya FINALIZED koreksi → 409
// PERIOD_LOCKED, dan INSERT langsung tanpa PeriodGuard ditolak trigger.
func TestAttendanceEdit_FinalizedPeriod_PeriodLocked(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	m := &matrixRunner{e: e, f: f, items: map[uuid.UUID]uuid.UUID{}}
	rec := m.target(t, "attendance_other", true) // work_date Februari 2026
	path := "/api/v1/attendance/" + rec.String()
	body := map[string]any{"clock_out_at": "2026-01-01T09:00:00Z", "correction_reason": "Lupa clock out"}

	if r, _ := e.do(t, http.MethodPatch, path, f.tokens[roleH], body); r.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("without If-Match: want 428, got %d", r.StatusCode)
	}
	if r, b := e.doH(t, http.MethodPatch, path, f.tokens[roleH], map[string]string{"If-Match": `"1"`}, map[string]any{"correction_reason": " "}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty reason: want 400, got %d %v", r.StatusCode, b)
	}
	r, got := e.doH(t, http.MethodPatch, path, f.tokens[roleH], map[string]string{"If-Match": `"1"`}, body)
	if r.StatusCode != http.StatusOK || got["source"] != "CORRECTION" || got["status"] != "COMPLETE" || got["version"] != float64(2) {
		t.Fatalf("correction: %d %v", r.StatusCode, got)
	}
	if r, _ := e.doH(t, http.MethodPatch, path, f.tokens[roleH], map[string]string{"If-Match": `"1"`}, body); r.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match: want 412, got %d", r.StatusCode)
	}
	var diff string
	e.inTenant(t, f.id, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT diff::text FROM audit_log WHERE action = 'attendance.corrected'`).Scan(&diff)
	})
	var d map[string]map[string]any
	if err := json.Unmarshal([]byte(diff), &d); err != nil || d["before"]["clock_out_at"] != nil || d["after"]["clock_out_at"] == nil {
		t.Fatalf("audit must hold before/after: %s", diff)
	}

	rec2 := m.target(t, "attendance_other", true) // dibuat sebelum periodenya final
	run, _ := e.calculatedRun(t, f.tokens[roleP], "2026-02")
	e.finalizeRun(t, run)
	if r, b := e.doH(t, http.MethodPatch, "/api/v1/attendance/"+rec2.String(), f.tokens[roleH], map[string]string{"If-Match": `"1"`}, body); r.StatusCode != http.StatusConflict || b["code"] != "PERIOD_LOCKED" {
		t.Fatalf("correction in a finalized period: want 409 PERIOD_LOCKED, got %d %v", r.StatusCode, b)
	}
	var pgErr *pgconn.PgError
	err := e.tryInTenant(f.id, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `INSERT INTO attendance_records (tenant_id, id, employee_id, work_date, clock_in_at, status, source)
			VALUES ($1, $2, $3, '2026-02-27', now(), 'INCOMPLETE', 'CLOCK')`, f.id, newID(), f.empSelf)
		return err
	})
	if !errors.As(err, &pgErr) || pgErr.Code != "P0002" {
		t.Fatalf("trigger must reject attendance in a finalized period, got %v", err)
	}
}

// TestLeave_SubmitApproveRejectCancel (PF-09): request PENDING ke atasan; saldo tidak berubah sampai
// approve; tumpang tindih → 409; lintas tahun / tanpa hari kerja → 400; approve memotong saldo; saldo
// tidak cukup → 422 + saldo, approval tetap PENDING; reject; cancel oleh pengaju; jenis tanpa kuota.
func TestLeave_SubmitApproveRejectCancel(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	emp, mgr := f.tokens[roleE], f.tokens[roleM]
	annual := e.leaveTypeID(t, f.id, "ANNUAL")

	r, lv := e.submitLeave(t, emp, annual, "2026-10-12", "2026-10-14")
	if r.StatusCode != http.StatusCreated || lv["days"] != "3.0" || lv["status"] != "PENDING" ||
		lv["approval"].(map[string]any)["approver"].(map[string]any)["id"] != f.users[roleM].String() {
		t.Fatalf("submit: want 3 days pending at the direct manager, got %d %v", r.StatusCode, lv)
	}
	if b := e.balance(t, emp, 2026, "ANNUAL"); b["balance_days"] != "12.0" || b["pending_days"] != "3.0" {
		t.Fatalf("balance before approval: %v", b)
	}
	if r, b := e.submitLeave(t, emp, annual, "2026-10-14", "2026-10-15"); r.StatusCode != http.StatusConflict || b["code"] != "OVERLAPPING_LEAVE" {
		t.Fatalf("overlap: want 409 OVERLAPPING_LEAVE, got %d %v", r.StatusCode, b)
	}
	if r, _ := e.submitLeave(t, emp, annual, "2026-12-31", "2027-01-04"); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("crossing the year: want 400, got %d", r.StatusCode)
	}
	if r, _ := e.submitLeave(t, emp, annual, "2026-10-17", "2026-10-18"); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("weekend only: want 400, got %d", r.StatusCode)
	}
	_, inbox := e.do(t, http.MethodGet, "/api/v1/approvals/inbox?subject_type=LEAVE_REQUEST", mgr, nil)
	if items := inbox["data"].([]any); len(items) != 1 ||
		items[0].(map[string]any)["subject"].(map[string]any)["summary"] != "Cuti tahunan 12 Okt 2026 – 14 Okt 2026 · 3 hari · Tono Saputra" {
		t.Fatalf("manager inbox: %v", inbox)
	}
	r, dec := e.approve(t, mgr, approvalOf(t, lv))
	if r.StatusCode != http.StatusOK || dec["subject"].(map[string]any)["status"] != "APPROVED" || dec["subject"].(map[string]any)["approved_at"] == nil {
		t.Fatalf("approve: %d %v", r.StatusCode, dec)
	}
	if b := e.balance(t, emp, 2026, "ANNUAL"); b["balance_days"] != "9.0" || b["pending_days"] != "0.0" {
		t.Fatalf("balance after approval: %v", b)
	}

	_, big := e.submitLeave(t, emp, annual, "2026-10-19", "2026-10-30") // 10 hari kerja > saldo 9
	ap := approvalOf(t, big)
	if r, b := e.approve(t, mgr, ap); r.StatusCode != http.StatusUnprocessableEntity || b["code"] != "INSUFFICIENT_LEAVE_BALANCE" || b["balance"] != "9.0" {
		t.Fatalf("insufficient: want 422 with balance 9.0, got %d %v", r.StatusCode, b)
	}
	if a, _ := e.approvalStatusAny(t, f.id, ap); a != "PENDING" {
		t.Fatalf("approval must stay PENDING after a failed effect, got %s", a)
	}
	if r, b := e.do(t, http.MethodPost, "/api/v1/approvals/"+ap.String()+"/reject", mgr, map[string]any{"note": "Saldo tidak cukup"}); r.StatusCode != http.StatusOK ||
		b["subject"].(map[string]any)["status"] != "REJECTED" {
		t.Fatalf("reject: %d %v", r.StatusCode, b)
	}

	_, pend := e.submitLeave(t, emp, annual, "2026-11-02", "2026-11-02")
	path := "/api/v1/leave-requests/" + pend["id"].(string) + "/cancel"
	if r, _ := e.do(t, http.MethodPost, path, mgr, map[string]any{}); r.StatusCode != http.StatusNotFound {
		t.Fatalf("cancel by someone else: want 404, got %d", r.StatusCode)
	}
	if r, b := e.do(t, http.MethodPost, path, emp, map[string]any{}); r.StatusCode != http.StatusOK || b["status"] != "CANCELLED" || b["approval"].(map[string]any)["status"] != "CANCELLED" {
		t.Fatalf("cancel: %d %v", r.StatusCode, b)
	}
	if r, _ := e.do(t, http.MethodPost, path, emp, map[string]any{}); r.StatusCode != http.StatusConflict {
		t.Fatalf("cancel twice: want 409, got %d", r.StatusCode)
	}

	_, sick := e.submitLeave(t, emp, e.leaveTypeID(t, f.id, "SICK"), "2026-11-03", "2026-11-04")
	if r, _ := e.approve(t, mgr, approvalOf(t, sick)); r.StatusCode != http.StatusOK {
		t.Fatalf("sick leave approve: %d", r.StatusCode)
	}
	if b := e.balance(t, emp, 2026, "SICK"); b["balance_days"] != nil {
		t.Fatalf("leave type without quota has no balance: %v", b)
	}
	if b := e.balance(t, emp, 2026, "ANNUAL"); b["balance_days"] != "9.0" {
		t.Fatalf("annual balance must be unchanged: %v", b)
	}
}

func (e env) approvalStatusAny(t *testing.T, tenant, id uuid.UUID) (string, string) {
	t.Helper()
	var st, subject string
	e.inTenant(t, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT status, subject_type FROM approval_requests WHERE id = $1`, id).Scan(&st, &subject)
	})
	return st, subject
}

// TestLeave_ParallelApproveRespectsBalance (DoD leave_parallel_approve_respects_balance, PI-10, race (a)):
// lima cuti 3 hari (15 hari) atas saldo 12 di-approve bersamaan → tepat empat lolos, satu 422, saldo 0.
func TestLeave_ParallelApproveRespectsBalance(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	annual := e.leaveTypeID(t, f.id, "ANNUAL")
	var approvals []uuid.UUID
	for _, mon := range []string{"2026-11-02", "2026-11-09", "2026-11-16", "2026-11-23", "2026-11-30"} {
		start, _ := time.Parse(time.DateOnly, mon)
		_, b := e.submitLeave(t, f.tokens[roleE], annual, mon, start.AddDate(0, 0, 2).Format(time.DateOnly))
		approvals = append(approvals, approvalOf(t, b))
	}
	statuses := make([]int, len(approvals))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, ap := range approvals {
		wg.Go(func() {
			<-start
			r, _ := e.approve(t, f.tokens[roleM], ap)
			statuses[i] = r.StatusCode
		})
	}
	close(start)
	wg.Wait()
	ok, short := 0, 0
	for _, st := range statuses {
		switch st {
		case http.StatusOK:
			ok++
		case http.StatusUnprocessableEntity:
			short++
		default:
			t.Fatalf("unexpected status %d (%v)", st, statuses)
		}
	}
	if ok != 4 || short != 1 {
		t.Fatalf("want 4 approved and 1 INSUFFICIENT_LEAVE_BALANCE, got %v", statuses)
	}
	if b := e.balance(t, f.tokens[roleE], 2026, "ANNUAL"); b["balance_days"] != "0.0" {
		t.Fatalf("balance must be exactly 0.0, got %v", b)
	}
}

// TestOvertime_SubmitRules (PF-10): jam dibulatkan 0,25; > batas harian → warning (bukan blokir);
// > 12 jam → 400; tumpang tindih jam → 409; work_date harus tanggal lokal start_at; cancel pengaju.
func TestOvertime_SubmitRules(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	emp := f.tokens[roleE]
	r, ot := e.submitOvertime(t, emp, "2026-10-12", "18:00", "21:00")
	if r.StatusCode != http.StatusCreated || ot["hours"] != "3.00" || len(ot["warnings"].([]any)) != 0 {
		t.Fatalf("submit: %d %v", r.StatusCode, ot)
	}
	_, long := e.submitOvertime(t, emp, "2026-10-13", "17:00", "22:10")
	if long["hours"] != "5.25" || len(long["warnings"].([]any)) != 1 || long["warnings"].([]any)[0] != "OVERTIME_OVER_LIMIT" {
		t.Fatalf("over the daily limit is a warning, got %v", long)
	}
	if r, _ := e.submitOvertime(t, emp, "2026-10-14", "06:00", "19:30"); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("over 12 hours: want 400, got %d", r.StatusCode)
	}
	if r, b := e.submitOvertime(t, emp, "2026-10-12", "20:00", "22:00"); r.StatusCode != http.StatusConflict || b["code"] != "OVERLAPPING_PERIOD" {
		t.Fatalf("overlap: want 409 OVERLAPPING_PERIOD, got %d %v", r.StatusCode, b)
	}
	if r, _ := e.doH(t, http.MethodPost, "/api/v1/overtime-requests", emp, key(), map[string]any{"work_date": "2026-10-12",
		"start_at": "2026-10-13T18:00:00+07:00", "end_at": "2026-10-13T19:00:00+07:00", "reason": "x"}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("work_date mismatch: want 400, got %d", r.StatusCode)
	}
	if r, b := e.approve(t, f.tokens[roleM], approvalOf(t, ot)); r.StatusCode != http.StatusOK || b["subject"].(map[string]any)["status"] != "APPROVED" {
		t.Fatalf("approve: %d %v", r.StatusCode, b)
	}
	if r, b := e.do(t, http.MethodPost, "/api/v1/overtime-requests/"+long["id"].(string)+"/cancel", emp, map[string]any{}); r.StatusCode != http.StatusOK || b["status"] != "CANCELLED" {
		t.Fatalf("cancel: %d %v", r.StatusCode, b)
	}
}

// TestApprover_FallbackAndNoApprover (ADR-P09 + amandemen, file 17 §3.2): tanpa atasan → HR_ADMIN; tidak
// ada siapa pun selain pengaju → 422 NO_APPROVER_AVAILABLE (self-approval cuti tidak pernah boleh).
func TestApprover_FallbackAndNoApprover(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	_, lv := e.submitLeave(t, f.tokens[roleM], e.leaveTypeID(t, f.id, "ANNUAL"), "2026-10-12", "2026-10-12")
	if lv["approval"].(map[string]any)["approver"].(map[string]any)["id"] != f.users[roleH].String() {
		t.Fatalf("manager without a manager: approver must be HR_ADMIN, got %v", lv["approval"])
	}

	owner := e.createUser(t, "solo@solo.test")
	e.verifyUser(t, owner)
	solo := e.createTenant(t, "solo", owner)
	tok, _, _ := e.login(t, "solo@solo.test")
	emp := e.createEmployee(t, tok, "S-1", "Solo", "3174012304950777")
	e.inTenant(t, solo, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE employees SET user_id = $1 WHERE id = $2`, owner, emp)
		return err
	})
	if r, b := e.submitLeave(t, tok, e.leaveTypeID(t, solo, "ANNUAL"), "2026-10-12", "2026-10-12"); r.StatusCode != http.StatusUnprocessableEntity || b["code"] != "NO_APPROVER_AVAILABLE" {
		t.Fatalf("nobody else to approve: want 422 NO_APPROVER_AVAILABLE, got %d %v", r.StatusCode, b)
	}
}

// TestInputProvider_AttendanceLeaveOvertimeFlowIntoEngine (DoD S6: cuti tidak dibayar dan lembur dari
// input provider): cuti tidak dibayar 1 hari → ABSENCE_UNPAID_LEAVE = gaji/22 hari kerja Oktober; cuti
// dibayar → tanpa potongan; lembur 4 jam → (1×1,5 + 3×2) × gaji/173; catatan INCOMPLETE → warning sesuai
// kebijakan (COUNT_AS_PRESENT: tanpa potongan). Snapshot memuat input harian dan run bisa diverifikasi.
func TestInputProvider_AttendanceLeaveOvertimeFlowIntoEngine(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	emp, mgr := f.tokens[roleE], f.tokens[roleM]
	e.doH(t, http.MethodPost, "/api/v1/employees/"+f.empSelf.String()+"/salary-changes", f.tokens[roleO], key(),
		map[string]any{"base_salary": "6000000.00", "valid_from": "2026-01-05"})
	e.do(t, http.MethodPost, "/api/v1/attendance/clock-in", emp, nil) // 6 Okt, tanpa clock-out → INCOMPLETE
	for _, x := range []struct{ code, day string }{{"UNPAID", "2026-10-12"}, {"ANNUAL", "2026-10-13"}} {
		_, lv := e.submitLeave(t, emp, e.leaveTypeID(t, f.id, x.code), x.day, x.day)
		if r, b := e.approve(t, mgr, approvalOf(t, lv)); r.StatusCode != http.StatusOK {
			t.Fatalf("approve %s: %d %v", x.code, r.StatusCode, b)
		}
	}
	_, ot := e.submitOvertime(t, emp, "2026-10-14", "18:00", "22:00")
	e.approve(t, mgr, approvalOf(t, ot))
	_, pending := e.submitOvertime(t, emp, "2026-10-15", "18:00", "22:00") // PENDING: tidak dihitung
	_ = pending

	run, _ := e.calculatedRun(t, f.tokens[roleP], "2026-10")
	var item string
	for _, it := range e.runItems(t, f.tokens[roleP], run) {
		if it["employee_id"] == f.empSelf.String() {
			item = it["id"].(string)
			if ws := it["warning_codes"].([]any); len(ws) != 1 || ws[0] != "INCOMPLETE_ATTENDANCE" {
				t.Fatalf("warnings: %v", ws)
			}
		}
	}
	_, detail := e.do(t, http.MethodGet, "/api/v1/payroll-runs/"+run.String()+"/items/"+item, f.tokens[roleP], nil)
	lines := map[string]string{}
	for _, l := range detail["lines"].([]any) {
		m := l.(map[string]any)
		lines[m["code"].(string)] = m["amount"].(string)
	}
	if lines["ABSENCE_UNPAID_LEAVE"] != "272727.00" || lines["OVERTIME"] != "260116.00" || lines["ABSENCE_UNEXCUSED"] != "" {
		t.Fatalf("lines from the input provider: %v", lines)
	}
	var snap string
	e.inTenant(t, f.id, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT jsonb_build_object('a', input->'attendance', 'o', input->'overtime')::text
			FROM payroll_item_snapshots WHERE item_id = $1`, item).Scan(&snap)
	})
	want := `{"a": [{"date": "2026-10-06", "status": "INCOMPLETE"}, {"date": "2026-10-12", "status": "UNPAID_LEAVE"}, {"date": "2026-10-13", "status": "PAID_LEAVE"}], "o": [{"date": "2026-10-14", "hours": "4"}]}`
	if snap != want {
		t.Fatalf("snapshot daily input:\n got %s\nwant %s", snap, want)
	}
	if rep := e.verifyRun(t, f.id, run); !rep.OK() {
		t.Fatalf("verify-run: %+v", rep)
	}
}

// TestCutoff_ApprovedAfterCutoff_NextPeriod (PI-08, BR6): lembur Oktober yang disetujui SETELAH cut-off
// periode Oktober (2 Nov 23:59 WIB) tidak masuk run Oktober; yang disetujui sebelumnya masuk. Koreksinya
// lewat adjustment di periode berikutnya (S7).
func TestCutoff_ApprovedAfterCutoff_NextPeriod(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	emp, mgr := f.tokens[roleE], f.tokens[roleM]
	e.doH(t, http.MethodPost, "/api/v1/employees/"+f.empSelf.String()+"/salary-changes", f.tokens[roleO], key(),
		map[string]any{"base_salary": "6000000.00", "valid_from": "2026-01-05"})
	run := e.createRun(t, f.tokens[roleP], "2026-10") // cutoff_at dibekukan sekarang
	_, early := e.submitOvertime(t, emp, "2026-10-14", "18:00", "19:00")
	_, late := e.submitOvertime(t, emp, "2026-10-15", "18:00", "22:00")
	e.approve(t, mgr, approvalOf(t, early))
	e.advance(t, f, 28*24*time.Hour) // 3 Nov 2026: lewat cut-off Oktober
	if r, b := e.approve(t, f.tokens[roleM], approvalOf(t, late)); r.StatusCode != http.StatusOK {
		t.Fatalf("late approve: %d %v", r.StatusCode, b)
	}
	if r, b := e.doH(t, http.MethodPost, "/api/v1/payroll-runs/"+run.String()+"/calculate", f.tokens[roleP], key(), nil); r.StatusCode != http.StatusAccepted {
		t.Fatalf("calculate: %d %v", r.StatusCode, b)
	}
	e.drainJobs(t)
	_ = mgr
	var hours string
	e.inTenant(t, f.id, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT s.input->'overtime'::text FROM payroll_item_snapshots s
			JOIN payroll_items i ON i.id = s.item_id WHERE i.run_id = $1 AND i.employee_id = $2`, run, f.empSelf).Scan(&hours)
	})
	if hours != `[{"date": "2026-10-14", "hours": "1"}]` {
		t.Fatalf("only overtime approved before the cut-off counts, got %s", hours)
	}
}
