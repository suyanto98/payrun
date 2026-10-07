import { createRouter, createWebHistory } from 'vue-router'
import { canListEmployees } from './access'
import { useAuth } from './stores/auth'
import LoginView from './pages/LoginView.vue'
import SelectTenantView from './pages/SelectTenantView.vue'
import EmployeesView from './pages/EmployeesView.vue'
import EmployeeDetailView from './pages/EmployeeDetailView.vue'

declare module 'vue-router' {
  interface RouteMeta {
    guest?: boolean
    /** Halaman yang butuh tenant aktif (token ber-`tid`). */
    tenant?: boolean
    title?: string
  }
}

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/login', component: LoginView, meta: { guest: true } },
    { path: '/select-tenant', component: SelectTenantView, meta: { title: 'Pilih perusahaan' } },
    { path: '/', redirect: '/employees' },
    { path: '/employees', component: EmployeesView, meta: { tenant: true, title: 'Karyawan' } },
    { path: '/employees/:id', component: EmployeeDetailView, meta: { tenant: true, title: 'Detail karyawan' } },
  ],
})

router.beforeEach(async (to) => {
  const auth = useAuth()
  if (!auth.ready) {
    await auth.refresh() // sesi dipulihkan dari cookie; access token tidak pernah disimpan
    auth.ready = true
  }
  if (!to.meta.guest && !auth.loggedIn) return { path: '/login', query: { next: to.fullPath } }
  if (to.meta.guest && auth.loggedIn) return '/'
  if (to.meta.tenant && !auth.tenant) return '/select-tenant'
  // EMPLOYEE tanpa akses daftar (W1 → 403): langsung ke profilnya sendiri.
  if (to.path === '/employees' && auth.tenant && !canListEmployees(auth.roles)) {
    return auth.employeeId ? `/employees/${auth.employeeId}` : '/select-tenant'
  }
})
