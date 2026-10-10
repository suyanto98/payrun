<script setup lang="ts">
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import AppIcon from './components/AppIcon.vue'
import { canEditEmployees, canListEmployees, canReadPayroll, rolesText } from './access'
import { useAuth } from './stores/auth'

const auth = useAuth()
const route = useRoute()
const router = useRouter()

// Halaman tamu punya layout sendiri; halaman lain memakai sidebar (desktop) atau app bar (mobile).
const guest = computed(() => route.meta.guest === true || !auth.loggedIn)
const employeesLink = computed(() => (canListEmployees(auth.roles) ? '/employees' : auth.employeeId ? `/employees/${auth.employeeId}` : ''))

function back() {
  if (window.history.state?.back) router.back()
  else router.push('/')
}

async function logout() {
  await auth.logout()
  router.push('/login')
}
</script>

<template>
  <RouterView v-if="guest" />
  <div v-else class="shell">
    <aside class="sidebar">
      <RouterLink to="/" class="brand" aria-label="PayRun, ke beranda">
        <span class="brand-mark"><AppIcon name="logo" :stroke="2.2" /></span>
        <span>PayRun</span>
      </RouterLink>
      <div class="sidebar-body">
        <div v-if="auth.tenant" class="nav-group">
          <span class="nav-title">{{ auth.tenant.name }}</span>
          <span class="tenant-roles">{{ rolesText(auth.roles) }}</span>
        </div>
        <nav class="nav" aria-label="Menu utama">
          <RouterLink v-if="employeesLink" :to="employeesLink" class="nav-link" :class="{ active: route.path.startsWith('/employees') }">
            <AppIcon name="users" />{{ canListEmployees(auth.roles) ? 'Karyawan' : 'Profil saya' }}
          </RouterLink>
          <RouterLink v-if="canReadPayroll(auth.roles)" to="/payroll" class="nav-link" :class="{ active: route.path.startsWith('/payroll') }">
            <AppIcon name="bank" />Payroll
          </RouterLink>
          <RouterLink v-if="canReadPayroll(auth.roles)" to="/adjustments" class="nav-link" :class="{ active: route.path === '/adjustments' }">
            <AppIcon name="undo" />Adjustment
          </RouterLink>
          <RouterLink v-if="auth.employeeId" to="/payslips" class="nav-link" :class="{ active: route.path === '/payslips' }">
            <AppIcon name="download" />Slip gaji
          </RouterLink>
          <RouterLink v-if="auth.employeeId || canEditEmployees(auth.roles)" to="/attendance" class="nav-link" :class="{ active: route.path === '/attendance' }">
            <AppIcon name="clock" />Absensi
          </RouterLink>
          <RouterLink v-if="auth.employeeId" to="/leave" class="nav-link" :class="{ active: route.path === '/leave' }">
            <AppIcon name="list" />Cuti
          </RouterLink>
          <RouterLink v-if="auth.employeeId" to="/overtime" class="nav-link" :class="{ active: route.path === '/overtime' }">
            <AppIcon name="chart" />Lembur
          </RouterLink>
          <RouterLink v-if="auth.tenant" to="/approvals" class="nav-link" :class="{ active: route.path === '/approvals' }">
            <AppIcon name="check" />Approval
          </RouterLink>
          <RouterLink to="/select-tenant" class="nav-link" :class="{ active: route.path === '/select-tenant' }">
            <AppIcon name="building" />Ganti perusahaan
          </RouterLink>
        </nav>
        <div class="sidebar-spacer" />
        <button type="button" class="nav-link" data-test="logout" @click="logout"><AppIcon name="logout" />Keluar</button>
      </div>
    </aside>

    <header class="appbar">
      <button type="button" class="icon-btn bare appbar-back" aria-label="Kembali" @click="back"><AppIcon name="back" :size="22" :stroke="2" /></button>
      <span class="appbar-title">{{ route.meta.title }}</span>
      <!-- Mobile: sidebar tersembunyi, jadi ganti perusahaan dan keluar ada di app bar. -->
      <RouterLink to="/select-tenant" class="icon-btn bare" aria-label="Ganti perusahaan"><AppIcon name="building" /></RouterLink>
      <button type="button" class="icon-btn bare" aria-label="Keluar" @click="logout"><AppIcon name="logout" /></button>
    </header>

    <main class="content">
      <p v-if="!auth.emailVerified" class="alert error" role="status">
        Email kamu belum diverifikasi. Buka link di email untuk mengaktifkan perusahaan dan menerima undangan.
      </p>
      <RouterView />
    </main>
  </div>
</template>

<style scoped>
.tenant-roles { font-size: 12px; color: var(--on-navy-muted, #b9c3dc); padding: 0 12px; }
.appbar-title { flex: 1; }
</style>
