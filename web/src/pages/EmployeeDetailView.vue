<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { api, ApiError } from '../api'
import { canListEmployees, formatDate, statusLabel } from '../access'
import { useAuth } from '../stores/auth'
import type { Employee } from '../types'

const route = useRoute()
const auth = useAuth()
const emp = ref<Employee | null>(null)
const error = ref('')
const notFound = ref(false)

async function load(id: string) {
  emp.value = null
  error.value = ''
  notFound.value = false
  try {
    emp.value = await api<Employee>(`/employees/${id}`)
  } catch (e) {
    // 404 juga untuk karyawan perusahaan lain atau di luar scope (tidak dibedakan, file 17 §1.3).
    if (e instanceof ApiError && e.status === 404) notFound.value = true
    else error.value = e instanceof ApiError ? e.message : 'Gagal memuat karyawan'
  }
}
watch(() => route.params.id as string, load, { immediate: true })

// Versi lengkap (OWNER/HR_ADMIN/PAYROLL_ADMIN) memuat PTKP dan PII ter-mask; versi dasar tidak.
const full = computed(() => emp.value !== null && emp.value.ptkp_status !== undefined)
const isSelf = computed(() => emp.value !== null && emp.value.id === auth.employeeId)
</script>

<template>
  <div class="stack narrow">
    <p v-if="canListEmployees(auth.roles)" class="crumb"><RouterLink to="/employees">Karyawan</RouterLink> / Detail</p>
    <p v-if="notFound" class="card empty">Karyawan tidak ditemukan.</p>
    <p v-else-if="error" class="alert error" role="alert">{{ error }}</p>
    <template v-else-if="emp">
      <div class="page-head">
        <div>
          <h1>{{ emp.full_name }}</h1>
          <p class="sub"><span class="mono">{{ emp.employee_no }}</span><span v-if="isSelf"> · Profil saya</span></p>
        </div>
        <span class="pill" :class="{ ok: emp.status === 'ACTIVE', warn: emp.status === 'RESIGNED', bad: emp.status === 'TERMINATED' }">{{ statusLabel[emp.status] ?? emp.status }}</span>
      </div>
      <section class="card">
        <h2>Kepegawaian</h2>
        <dl class="dl">
          <div><dt>Email kerja</dt><dd>{{ emp.work_email ?? '—' }}</dd></div>
          <div><dt>Mulai kerja</dt><dd>{{ formatDate(emp.hire_date) }}</dd></div>
          <div v-if="emp.end_date"><dt>Hari kerja terakhir</dt><dd>{{ formatDate(emp.end_date) }}</dd></div>
          <div v-if="full"><dt>Status PTKP</dt><dd>{{ emp.ptkp_status }}</dd></div>
        </dl>
      </section>
      <section v-if="full" class="card">
        <h2>Data pribadi</h2>
        <p class="muted">Disimpan terenkripsi dan selalu ditampilkan ter-mask.</p>
        <dl class="dl">
          <div><dt>NIK</dt><dd class="mono">{{ emp.nik_masked ?? '—' }}</dd></div>
          <div><dt>NPWP</dt><dd class="mono">{{ emp.npwp_masked ?? '—' }}</dd></div>
          <div><dt>Bank</dt><dd>{{ emp.bank?.name ?? '—' }}</dd></div>
          <div><dt>Nomor rekening</dt><dd class="mono">{{ emp.bank?.account_masked ?? '—' }}</dd></div>
          <div><dt>Atas nama</dt><dd>{{ emp.bank?.holder ?? '—' }}</dd></div>
        </dl>
      </section>
    </template>
    <p v-else class="muted">Memuat…</p>
  </div>
</template>
