<script setup lang="ts">
import { errorText } from '../errors'
import { computed, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { api, ApiError, newIdempotencyKey } from '../api'
import AppIcon from '../components/AppIcon.vue'
import { canRunPayroll, formatMoney, formatPeriod, runStatusLabel, runStatusPill } from '../access'
import { useAuth } from '../stores/auth'
import { useDemo } from '../stores/demo'
import type { Page, PayrollRun, PayrollRunListItem } from '../types'

// Y3 daftar run (termasuk CANCELLED untuk riwayat) + Y2 buat run (OWNER/PAYROLL_ADMIN).
const auth = useAuth()
const router = useRouter()
const canRun = computed(() => canRunPayroll(auth.roles))
const items = ref<PayrollRunListItem[]>([])
const page = ref<Page>({ next_cursor: null, has_more: false })
const loading = ref(false)
const error = ref('')

const now = new Date()
const period = ref(`${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, '0')}`)
const creating = ref(false)
const createError = ref('')
// Satu niat = satu key: klik ganda / retry memakai key yang sama (replay 201), key baru saat periode berubah.
let createKey = newIdempotencyKey()
watch(period, () => { createKey = newIdempotencyKey() })

async function load(append = false) {
  loading.value = true
  error.value = ''
  const params = new URLSearchParams({ limit: '20' })
  if (append && page.value.next_cursor) params.set('cursor', page.value.next_cursor)
  try {
    const res = await api<{ data: PayrollRunListItem[]; page: Page }>('/payroll-runs?' + params)
    items.value = append ? [...items.value, ...res.data] : res.data
    page.value = res.page
  } catch (e) {
    error.value = errorText(e, 'Gagal memuat payroll')
  } finally {
    loading.value = false
  }
}

async function create() {
  creating.value = true
  createError.value = ''
  try {
    const run = await api<PayrollRun>('/payroll-runs', { method: 'POST', body: { period: period.value }, idempotencyKey: createKey })
    router.push(`/payroll/${run.id}${useDemo().sandbox ? '#crash' : ''}`) // demo: sorot "Pasang gangguan"
  } catch (e) {
    if (e instanceof ApiError && e.code === 'ACTIVE_RUN_EXISTS' && typeof e.extra.existing_resource_id === 'string') {
      router.push(`/payroll/${e.extra.existing_resource_id}`) // admin lain sudah membuatnya: buka run itu
      return
    }
    createError.value = errorText(e, 'Gagal membuat payroll')
  } finally {
    creating.value = false
  }
}

onMounted(() => load())
</script>

<template>
  <div class="stack">
    <div class="page-head">
      <div>
        <h1>Payroll</h1>
        <p class="sub">Gaji dihitung otomatis di latar belakang. Periksa hasilnya dulu sebelum meminta persetujuan.</p>
      </div>
    </div>

    <form v-if="canRun" class="card filters" @submit.prevent="create">
      <div class="field">
        <label for="period">Periode</label>
        <input id="period" v-model="period" class="input sm" type="month" required />
      </div>
      <button type="submit" class="btn sm" :disabled="creating" data-spot="create"><AppIcon name="plus" />Buat payroll</button>
      <p v-if="createError" class="alert error" role="alert">{{ createError }}</p>
    </form>

    <p v-if="error" class="alert error" role="alert">{{ error }}</p>
    <div class="card card-flush">
      <div class="table-scroll">
        <table class="table">
          <thead>
            <tr>
              <th scope="col">Periode</th>
              <th scope="col">Status</th>
              <th scope="col" class="right">Hitungan</th>
              <th scope="col" class="right">Total net</th>
              <th scope="col"><span class="sr-only">Buka</span></th>
            </tr>
          </thead>
          <tbody>
            <tr v-if="!loading && items.length === 0"><td colspan="5" class="empty">Belum ada payroll.</td></tr>
            <tr v-for="r in items" :key="r.id">
              <td><RouterLink :to="`/payroll/${r.id}`">{{ formatPeriod(r.period) }}</RouterLink></td>
              <td><span class="pill" :class="runStatusPill[r.status]">{{ runStatusLabel[r.status] ?? r.status }}</span></td>
              <td class="right num">{{ r.calc_version }}</td>
              <td class="right num">{{ formatMoney(r.total_net) }}</td>
              <td class="right"><RouterLink :to="`/payroll/${r.id}`" class="icon-btn bare" :aria-label="`Buka payroll ${r.period}`"><AppIcon name="chevron" /></RouterLink></td>
            </tr>
          </tbody>
        </table>
      </div>
      <div v-if="page.has_more" class="table-foot">
        <span class="muted">{{ items.length }} payroll dimuat</span>
        <button type="button" class="btn sm ghost" :disabled="loading" @click="load(true)">Muat lagi</button>
      </div>
    </div>
  </div>
</template>
