<script setup lang="ts">
import { onMounted, ref, watch } from 'vue'
import { api, ApiError } from '../api'
import AppIcon from '../components/AppIcon.vue'
import { formatDate, statusLabel } from '../access'
import type { EmployeeListItem, Page } from '../types'

// W1: daftar tanpa PII; pencarian prefix nama/nomor, filter status, cursor ("Muat lagi").
const items = ref<EmployeeListItem[]>([])
const page = ref<Page>({ next_cursor: null, has_more: false })
const q = ref('')
const status = ref('ACTIVE')
const loading = ref(false)
const error = ref('')

async function load(append = false) {
  loading.value = true
  error.value = ''
  const params = new URLSearchParams({ limit: '20' })
  if (q.value.trim().length >= 2) params.set('q', q.value.trim())
  if (status.value) params.set('status', status.value)
  if (append && page.value.next_cursor) params.set('cursor', page.value.next_cursor)
  try {
    const res = await api<{ data: EmployeeListItem[]; page: Page }>('/employees?' + params)
    items.value = append ? [...items.value, ...res.data] : res.data
    page.value = res.page
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : 'Gagal memuat karyawan'
  } finally {
    loading.value = false
  }
}

let timer: ReturnType<typeof setTimeout> | undefined
watch(q, () => {
  clearTimeout(timer)
  timer = setTimeout(() => load(), 300)
})
watch(status, () => load())
onMounted(() => load())

const pillClass: Record<string, string> = { ACTIVE: 'ok', RESIGNED: 'warn', TERMINATED: 'bad' }
</script>

<template>
  <div class="stack">
    <div class="page-head">
      <div>
        <h1>Karyawan</h1>
        <p class="sub">Data pribadi (NIK, NPWP, rekening) tidak ditampilkan di daftar.</p>
      </div>
    </div>
    <div class="filters">
      <div class="field">
        <label for="q">Cari nama atau nomor</label>
        <input id="q" v-model="q" class="input sm" type="search" placeholder="Minimal 2 huruf" autocomplete="off" />
      </div>
      <div class="field">
        <label for="status">Status</label>
        <select id="status" v-model="status" class="select sm">
          <option value="">Semua</option>
          <option value="ACTIVE">Aktif</option>
          <option value="RESIGNED">Resign</option>
          <option value="TERMINATED">Diberhentikan</option>
        </select>
      </div>
    </div>
    <p v-if="error" class="alert error" role="alert">{{ error }}</p>
    <div class="card card-flush">
      <div class="table-scroll">
        <table class="table emp-table">
          <thead>
            <tr>
              <th scope="col">Nomor</th>
              <th scope="col">Nama</th>
              <th scope="col">Mulai kerja</th>
              <th scope="col">Status</th>
              <th scope="col"><span class="sr-only">Buka</span></th>
            </tr>
          </thead>
          <tbody>
            <tr v-if="!loading && items.length === 0"><td colspan="5" class="empty">Tidak ada karyawan yang cocok.</td></tr>
            <tr v-for="e in items" :key="e.id">
              <td class="mono">{{ e.employee_no }}</td>
              <td><RouterLink :to="`/employees/${e.id}`">{{ e.full_name }}</RouterLink></td>
              <td>{{ formatDate(e.hire_date) }}</td>
              <td><span class="pill" :class="pillClass[e.status]">{{ statusLabel[e.status] ?? e.status }}</span></td>
              <td class="right"><RouterLink :to="`/employees/${e.id}`" class="icon-btn bare" :aria-label="`Buka ${e.full_name}`"><AppIcon name="chevron" /></RouterLink></td>
            </tr>
          </tbody>
        </table>
      </div>
      <div v-if="page.has_more" class="table-foot">
        <span class="muted">{{ items.length }} karyawan dimuat</span>
        <button type="button" class="btn sm ghost" :disabled="loading" @click="load(true)">Muat lagi</button>
      </div>
    </div>
  </div>
</template>

<style scoped>
/* Mobile: baris ringkas (nomor + nama + status), tanpa scroll horizontal. */
@media (max-width: 768px) {
  .emp-table { min-width: 0; }
  .emp-table th:nth-child(3), .emp-table td:nth-child(3), .emp-table th:nth-child(5), .emp-table td:nth-child(5) { display: none; }
  .emp-table td, .emp-table th { padding: 12px 10px; }
}
</style>

