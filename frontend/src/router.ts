import { createRouter, createWebHistory } from 'vue-router'
import { canListEmployees, canReadPayroll } from './access'
import { useAuth } from './stores/auth'
import LoginView from './pages/LoginView.vue'
import SelectTenantView from './pages/SelectTenantView.vue'
import EmployeesView from './pages/EmployeesView.vue'
import EmployeeDetailView from './pages/EmployeeDetailView.vue'
import InviteView from './pages/InviteView.vue'
import PayrollRunsView from './pages/PayrollRunsView.vue'
import PayrollRunView from './pages/PayrollRunView.vue'
import ApprovalsView from './pages/ApprovalsView.vue'
import VerifyEmailView from './pages/VerifyEmailView.vue'
import AttendanceView from './pages/AttendanceView.vue'
import LeaveView from './pages/LeaveView.vue'
import OvertimeView from './pages/OvertimeView.vue'
import PayslipsView from './pages/PayslipsView.vue'
import PayslipView from './pages/PayslipView.vue'
import AdjustmentsView from './pages/AdjustmentsView.vue'
import WelcomeView from './pages/WelcomeView.vue'

declare module 'vue-router' {
  interface RouteMeta {
    guest?: boolean
    /** Boleh dibuka dengan atau tanpa login (halaman undangan). */
    public?: boolean
    /** Halaman yang butuh tenant aktif (token ber-`tid`). */
    tenant?: boolean
    title?: string
  }
}

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/welcome', component: WelcomeView, meta: { public: true } },
    { path: '/login', component: LoginView, meta: { guest: true } },
    { path: '/invite', component: InviteView, meta: { public: true, title: 'Undangan' } },
    { path: '/verify-email', component: VerifyEmailView, meta: { public: true, title: 'Verifikasi email' } },
    { path: '/select-tenant', component: SelectTenantView, meta: { title: 'Pilih perusahaan' } },
    { path: '/', redirect: '/employees' },
    { path: '/employees', component: EmployeesView, meta: { tenant: true, title: 'Karyawan' } },
    { path: '/employees/:id', component: EmployeeDetailView, meta: { tenant: true, title: 'Detail karyawan' } },
    { path: '/payroll', component: PayrollRunsView, meta: { tenant: true, title: 'Payroll' } },
    { path: '/payroll/:id', component: PayrollRunView, meta: { tenant: true, title: 'Payroll' } },
    { path: '/approvals', component: ApprovalsView, meta: { tenant: true, title: 'Persetujuan' } },
    { path: '/attendance', component: AttendanceView, meta: { tenant: true, title: 'Absensi' } },
    { path: '/leave', component: LeaveView, meta: { tenant: true, title: 'Cuti' } },
    { path: '/overtime', component: OvertimeView, meta: { tenant: true, title: 'Lembur' } },
    { path: '/payslips', component: PayslipsView, meta: { tenant: true, title: 'Slip gaji' } },
    { path: '/payslips/:id', component: PayslipView, meta: { tenant: true, title: 'Slip gaji karyawan' } },
    { path: '/adjustments', component: AdjustmentsView, meta: { tenant: true, title: 'Koreksi gaji' } },
  ],
})

router.beforeEach(async (to) => {
  const auth = useAuth()
  if (!auth.ready) {
    await auth.refresh() // sesi dipulihkan dari cookie; access token tidak pernah disimpan
    auth.ready = true
  }
  // Undangan: rahasia di fragment dipindah ke memori dan dihapus dari URL OLEH ROUTER (redirect tanpa
  // hash). replaceState manual tidak cukup: navigasi awal router menulis ulang URL lengkap.
  if (to.path === '/invite' && to.hash.length > 1) {
    auth.pendingInvite = decodeURIComponent(to.hash.slice(1))
    return { path: '/invite', hash: '', replace: true }
  }
  // Verifikasi email: token di query (link email, A7) dipindah ke memori dan dihapus dari URL, supaya
  // tidak tertinggal di riwayat browser atau terkirim sebagai Referer.
  if (to.path === '/verify-email' && typeof to.query.token === 'string' && to.query.token !== '') {
    auth.pendingVerify = to.query.token
    return { path: '/verify-email', query: {}, replace: true }
  }
  if (to.meta.public) return
  // `next` memakai path + query saja, TANPA hash: fragment bisa berisi rahasia (undangan).
  // Pengunjung baru di "/" (redirect ke /employees) melihat landing; halaman lain tetap ke login dan kembali (?next=).
  if (!to.meta.guest && !auth.loggedIn) return to.redirectedFrom?.path === '/' ? '/welcome' : { path: '/login', query: { next: to.path } }
  if (to.meta.guest && auth.loggedIn) return '/'
  if (to.meta.tenant && !auth.tenant) return '/select-tenant'
  // EMPLOYEE tanpa akses daftar (W1 → 403): langsung ke profilnya sendiri.
  if (to.path === '/employees' && auth.tenant && !canListEmployees(auth.roles)) {
    return auth.employeeId ? `/employees/${auth.employeeId}` : '/select-tenant'
  }
  // Payroll hanya untuk OWNER/HR_ADMIN/PAYROLL_ADMIN (Y3 → 403, Y4 → 404 untuk role lain).
  if ((to.path.startsWith('/payroll') || to.path.startsWith('/adjustments') || to.path.startsWith('/payslips/')) &&
    auth.tenant && !canReadPayroll(auth.roles)) return '/'
})

/** Hanya path internal ("/x"), bukan "//host" atau URL absolut (open redirect). */
export function safeNext(next: unknown): string {
  return typeof next === 'string' && next.startsWith('/') && !next.startsWith('//') && !next.includes('\\') ? next : '/'
}

