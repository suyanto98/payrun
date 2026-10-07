// Bentuk respons API (subset dari api/openapi.yaml yang dipakai halaman S2).

export type Role = 'OWNER' | 'HR_ADMIN' | 'PAYROLL_ADMIN' | 'MANAGER' | 'EMPLOYEE'

export interface ActiveTenant { id: string; name: string; roles: Role[] }
export interface Membership { tenant_id: string; tenant_name: string; roles: Role[]; status: string }
export interface Page { next_cursor: string | null; has_more: boolean }

export interface EmployeeListItem {
  id: string; employee_no: string; full_name: string; status: string; hire_date: string
  department: null; position: null
}

export interface Employee {
  id: string; employee_no: string; full_name: string; work_email: string | null; status: string
  hire_date: string; end_date: string | null; user_id: string | null; version: number
  // Versi lengkap saja (OWNER/HR_ADMIN/PAYROLL_ADMIN); MANAGER/EMPLOYEE menerima versi dasar.
  ptkp_status?: string; nik_masked?: string | null; npwp_masked?: string | null
  bank?: { name: string | null; account_masked: string | null; holder: string | null } | null
}
