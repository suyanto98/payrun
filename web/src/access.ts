import type { Role } from './types'

// Hanya untuk menampilkan menu/tombol; server tetap menegakkan otorisasi (api/authz_matrix.yaml).
export const roleLabel: Record<Role, string> = {
  OWNER: 'Pemilik', HR_ADMIN: 'Admin HR', PAYROLL_ADMIN: 'Admin Payroll', MANAGER: 'Manajer', EMPLOYEE: 'Karyawan',
}

/** W1: OWNER/HR_ADMIN/PAYROLL_ADMIN (seluruh tenant), MANAGER (bawahan). EMPLOYEE → 403. */
export function canListEmployees(roles: Role[]): boolean {
  return roles.some((r) => r === 'OWNER' || r === 'HR_ADMIN' || r === 'PAYROLL_ADMIN' || r === 'MANAGER')
}

/** W2/W4/W5: hanya OWNER dan HR_ADMIN. */
export function canEditEmployees(roles: Role[]): boolean {
  return roles.includes('OWNER') || roles.includes('HR_ADMIN')
}

export const statusLabel: Record<string, string> = { ACTIVE: 'Aktif', RESIGNED: 'Resign', TERMINATED: 'Diberhentikan' }

/** Tanggal API (YYYY-MM-DD) → "5 Jan 2026" tanpa pergeseran zona waktu. */
export function formatDate(iso: string | null | undefined): string {
  if (!iso) return '—'
  const [y, m, d] = iso.split('-').map(Number)
  const months = ['Jan', 'Feb', 'Mar', 'Apr', 'Mei', 'Jun', 'Jul', 'Agu', 'Sep', 'Okt', 'Nov', 'Des']
  return `${d} ${months[m - 1]} ${y}`
}

/** "Admin HR · Karyawan" untuk daftar role membership. */
export function rolesText(roles: Role[]): string {
  return roles.map((r) => roleLabel[r]).join(' · ')
}

/** "6600000.00" → "Rp6.600.000" (string dari API; tidak pernah lewat float untuk aritmetika). */
export function formatMoney(v: string | null | undefined): string {
  if (!v) return '—'
  const [int, frac] = v.split('.')
  const grouped = int.replace(/\B(?=(\d{3})+(?!\d))/g, '.')
  return 'Rp' + grouped + (frac && /[1-9]/.test(frac) ? ',' + frac : '')
}


/** Y3/Y4/Y6: OWNER, HR_ADMIN (baca), PAYROLL_ADMIN. */
export function canReadPayroll(roles: Role[]): boolean {
  return roles.some((r) => r === 'OWNER' || r === 'HR_ADMIN' || r === 'PAYROLL_ADMIN')
}

/** Y2/Y5/Y8/Y10/Y12: hanya OWNER dan PAYROLL_ADMIN (HR_ADMIN membaca saja, pemisahan tugas). */
export function canRunPayroll(roles: Role[]): boolean {
  return roles.includes('OWNER') || roles.includes('PAYROLL_ADMIN')
}

export const runStatusLabel: Record<string, string> = {
  DRAFT: 'Draf', CALCULATING: 'Menghitung', CALCULATED: 'Terhitung', PENDING_APPROVAL: 'Menunggu approval',
  APPROVED: 'Disetujui', FINALIZED: 'Final', FAILED: 'Gagal', CANCELLED: 'Dibatalkan',
}
export const runStatusPill: Record<string, string> = {
  DRAFT: '', CALCULATING: 'warn', CALCULATED: 'ok', PENDING_APPROVAL: 'warn', APPROVED: 'ok', FINALIZED: 'ok', FAILED: 'bad', CANCELLED: 'bad',
}

/** Aksi yang tersedia untuk status run (Step 3 `08`: state machine ADR-P05). */
export function runActions(status: string): { calculate: boolean; submit: boolean; cancel: boolean } {
  return {
    calculate: status === 'DRAFT' || status === 'CALCULATED' || status === 'FAILED',
    submit: status === 'CALCULATED',
    cancel: status === 'DRAFT' || status === 'CALCULATED' || status === 'FAILED',
  }
}

export const staleReasonLabel: Record<string, string> = {
  INPUT_CHANGED: 'input berubah', NEWLY_ELIGIBLE: 'baru masuk periode', NO_LONGER_ELIGIBLE: 'tidak lagi bekerja di periode',
}

/** "2026-03" → "Maret 2026". */
export function formatPeriod(p: string): string {
  const [y, m] = p.split('-').map(Number)
  const months = ['Januari', 'Februari', 'Maret', 'April', 'Mei', 'Juni', 'Juli', 'Agustus', 'September', 'Oktober', 'November', 'Desember']
  return `${months[m - 1]} ${y}`
}
