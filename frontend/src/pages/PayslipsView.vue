<script setup lang="ts">
import { errorText } from '../errors'
import { onMounted, ref, watch } from 'vue'
import { api, ApiError } from '../api'
import AppIcon from '../components/AppIcon.vue'
import PayslipCard from '../components/PayslipCard.vue'
import { formatDate, formatMoney, formatPeriod, pdfStatusLabel, pdfStatusPill } from '../access'
import { useAuth } from '../stores/auth'
import { useDemo } from '../stores/demo'
import type { Page, Payslip, PayslipSummary } from '../types'

// S1/S2 (PF-15): slip milik sendiri untuk run FINALIZED. Akses pemilik tidak diaudit; slip orang lain → 404.
const auth = useAuth()
const year = ref(String(new Date().getFullYear()))
const slips = ref<PayslipSummary[]>([])
const page = ref<Page>({ next_cursor: null, has_more: false })
const selected = ref<Payslip | null>(null)
const error = ref('')
const loading = ref(false)

async function load(append = false) {
  if (!auth.employeeId) return
  loading.value = true
  error.value = ''
  try {
    const params = new URLSearchParams({ limit: '12' })
    if (year.value) params.set('year', year.value)
    if (append && page.value.next_cursor) params.set('cursor', page.value.next_cursor)
    const res = await api<{ data: PayslipSummary[]; page: Page }>('/me/payslips?' + params)
    slips.value = append ? [...slips.value, ...res.data] : res.data
    page.value = res.page
    if (!append && !selected.value && res.data.length) await open(res.data[0].id)
  } catch (e) {
    error.value = errorText(e, 'Gagal memuat slip gaji')
  } finally {
    loading.value = false
  }
}

async function open(id: string) {
  try {
    selected.value = await api<Payslip>(`/me/payslips/${id}`)
    useDemo().mark('payslip') // tur demo: slip sendiri sudah dibuka
  } catch (e) {
    error.value = errorText(e, 'Gagal memuat slip')
  }
}

watch(year, () => { selected.value = null; load() })
onMounted(() => load())
</script>

<template>
  <div class="stack">
    <div class="page-head">
      <div>
        <h1>Slip gaji</h1>
        <p class="sub">Slip tersedia setelah payroll bulan itu difinalisasi.</p>
      </div>
      <select v-model="year" class="select sm" aria-label="Tahun">
        <option v-for="y in [0, 1, 2].map((d) => String(new Date().getFullYear() - d))" :key="y" :value="y">{{ y }}</option>
      </select>
    </div>
    <p v-if="error" class="alert error" role="alert">{{ error }}</p>
    <p v-if="!auth.employeeId" class="note"><AppIcon name="info" />Akunmu belum tertaut ke data karyawan, jadi belum ada slip gaji.</p>

    <div v-else class="split">
      <div class="card card-flush list">
        <ul class="slip-list">
          <li v-if="!loading && slips.length === 0" class="empty">Belum ada slip di tahun ini.</li>
          <li v-for="(s, i) in slips" :key="s.id">
            <button type="button" class="slip-row" :data-spot="i === 0 ? 'payslip' : undefined" :class="{ active: selected?.id === s.id }" :aria-current="selected?.id === s.id" @click="open(s.id)">
              <span>
                <strong>{{ formatPeriod(s.period) }}</strong>
                <span class="muted">Final {{ formatDate(s.finalized_at) }}</span>
              </span>
              <span class="right">
                <span class="num">{{ formatMoney(s.net) }}</span>
                <span class="pill" :class="pdfStatusPill[s.pdf_status]">{{ pdfStatusLabel[s.pdf_status] }}</span>
              </span>
            </button>
          </li>
        </ul>
        <div v-if="page.has_more" class="table-foot"><button type="button" class="btn sm ghost" @click="load(true)">Muat lagi</button></div>
      </div>
      <PayslipCard v-if="selected" :slip="selected" />
    </div>
  </div>
</template>

<style scoped>
.split { display: grid; grid-template-columns: minmax(0, 320px) minmax(0, 1fr); gap: 16px; align-items: start; }
@media (max-width: 800px) { .split { grid-template-columns: minmax(0, 1fr); } }
.slip-list { list-style: none; margin: 0; padding: 0; }
.slip-list .empty { padding: 16px 24px; }
.slip-row { width: 100%; display: flex; justify-content: space-between; gap: 12px; padding: 12px 20px; background: none; border: 0;
  border-bottom: 1px solid var(--border); text-align: left; cursor: pointer; font: inherit; color: inherit; }
.slip-row > span { display: flex; flex-direction: column; gap: 2px; }
.slip-row .right { align-items: flex-end; }
.slip-row.active, .slip-row:hover { background: var(--hover, rgba(0, 0, 0, .04)); }
</style>
