// Bentuk respons API (subset dari api/openapi.yaml yang dipakai halaman S2).

export type Role = 'OWNER' | 'HR_ADMIN' | 'PAYROLL_ADMIN' | 'MANAGER' | 'EMPLOYEE'

export interface ActiveTenant { id: string; name: string; roles: Role[] }
export interface Membership { tenant_id: string; tenant_name: string; roles: Role[]; status: string }
export interface Page { next_cursor: string | null; has_more: boolean }

export interface Ref { id: string; name: string }

export interface EmployeeListItem {
  id: string; employee_no: string; full_name: string; status: string; hire_date: string
  department: Ref | null; position: Ref | null
}

export interface CurrentOrg {
  department: Ref | null; position: Ref | null; valid_from: string | null
  manager: { id: string; full_name: string } | null
}

export interface SalaryAssignment { id: string; base_salary: string; valid_from: string; valid_to: string | null; change_reason: string | null }
export interface EmployeeComponent {
  id: string; component: { code: string; name: string; calc_type: string }
  amount: string | null; rate: string | null; valid_from: string; valid_to: string | null
}
export interface SalaryHistory { salary: SalaryAssignment[]; components: EmployeeComponent[] }

export interface Employee {
  id: string; employee_no: string; full_name: string; work_email: string | null; status: string
  hire_date: string; end_date: string | null; user_id: string | null; version: number
  current_org: CurrentOrg | null
  // Versi lengkap saja (OWNER/HR_ADMIN/PAYROLL_ADMIN); MANAGER/EMPLOYEE menerima versi dasar.
  ptkp_status?: string; nik_masked?: string | null; npwp_masked?: string | null
  bank?: { name: string | null; account_masked: string | null; holder: string | null } | null
}

// ---------------------------------------------------------------- payroll run & approval (S4–S5)

export type RunStatus = 'DRAFT' | 'CALCULATING' | 'CALCULATED' | 'PENDING_APPROVAL' | 'APPROVED' | 'FINALIZED' | 'FAILED' | 'CANCELLED'

export interface PayrollRun {
  id: string; period: string; status: RunStatus; calc_version: number
  progress: { chunks_done: number; chunks_total: number; eligible_count: number | null }
  totals: { gross: string; deductions: string; net: string } | null
  counts: { ok: number | null; error: number | null; excluded: number | null; warnings: number | null }
  ruleset: { code: string; version: string } | null
  failure_reason: string | null; created_at: string; calc_started_at: string | null; calculated_at: string | null
  cancel_reason: string | null; approval_request_id: string | null
  submitted_by: string | null; submitted_at: string | null; approved_by: string | null; approved_at: string | null
  finalized_by: string | null; finalized_at: string | null
}

export interface PayrollRunListItem { id: string; period: string; status: RunStatus; calc_version: number; total_net: string | null; created_at: string }

export interface PayrollItem {
  id: string; employee_id: string; employee_no: string; employee_name: string; department_id: string | null
  status: 'OK' | 'ERROR' | 'EXCLUDED'; gross: string | null; deductions: string | null; net: string | null
  error_codes: string[]; warning_codes: string[]; excluded_reason: string | null
}

export interface PayrollLine {
  sort_order: number; code: string; name: string; category: 'EARNING' | 'DEDUCTION'; stage: string
  amount: string; basis: Record<string, string>; explanation: string; taxable: boolean; rounding: string
}

export interface PayrollItemDetail extends PayrollItem {
  calc_version: number; lines: PayrollLine[]; issues: { kind: 'error' | 'warning'; code: string; detail: string }[]
}

export interface ApprovalRequest {
  id: string; subject_type: 'PAYROLL_RUN' | 'LEAVE_REQUEST' | 'OVERTIME_REQUEST'; subject_id: string
  subject: { summary: string }
  requester: { id: string; display_name: string | null }; approver: { id: string; display_name: string | null }
  status: 'PENDING' | 'APPROVED' | 'REJECTED' | 'CANCELLED'; decision_note: string | null
  self_approved: boolean; self_approval_reason: string | null; decided_at: string | null; created_at: string
}

/** RUN_STALE.employees_changed */
export interface StaleEmployee { employee_id: string; employee_no?: string; reason: 'INPUT_CHANGED' | 'NEWLY_ELIGIBLE' | 'NO_LONGER_ELIGIBLE' }

// ---------------------------------------------------------------- absensi, cuti, lembur (S6)

export interface AttendanceRecord {
  id: string; employee_id: string; work_date: string; clock_in_at: string | null; clock_out_at: string | null
  status: 'INCOMPLETE' | 'COMPLETE'; source: 'CLOCK' | 'IMPORT' | 'CORRECTION'; correction_reason: string | null; version: number
}
export interface AttendanceSummary { present_days: number; incomplete_days: number; working_days: number }

export interface LeaveTypeRef { id: string; code: string; name: string; is_paid: boolean }
export interface LeaveType extends LeaveTypeRef { annual_quota_days: string | null; max_negative_days: string; status: string }
export interface LeaveBalance { leave_type: LeaveTypeRef; year: number; balance_days: string | null; pending_days: string }
export interface SubjectApproval { id: string; approver: { id: string; display_name: string | null }; status: string }
export interface LeaveRequest {
  id: string; employee_id: string; leave_type: LeaveTypeRef; start_date: string; end_date: string; days: string
  reason: string | null; status: string; approved_at: string | null; approval: SubjectApproval | null
}
export interface OvertimeRequest {
  id: string; employee_id: string; work_date: string; start_at: string; end_at: string; hours: string; reason: string
  status: string; approved_at: string | null; approval: SubjectApproval | null; warnings: string[]
}
