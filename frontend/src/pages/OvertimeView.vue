<script setup lang="ts">
import { errorText } from '../errors'
import { onMounted, ref, watch } from 'vue'
import { api, ApiError, newIdempotencyKey } from '../api'
import AppIcon from '../components/AppIcon.vue'
import { formatDate, formatTime, localDateTime, requestStatusLabel, requestStatusPill } from '../access'
import { useAuth } from '../stores/auth'
import type { OvertimeRequest, Page } from '../types'

// V1 ajukan lembur (≤ 12 jam, dibulatkan 0,25), V2 batalkan, V3 daftar. Lembur hanya dibayar bila disetujui
// sebelum cut-off periode.
const auth = useAuth()
const requests = ref<OvertimeRequest[]>([])
const page = ref<Page>({ next_cursor: null, has_more: false })
const error = ref('')
const notice = ref('')
const busy = ref(false)
const form = ref({ work_date: '', start: '18:00', end: '20:00', reason: '' })
let submitKey = newIdempotencyKey()
watch(form, () => { submitKey = newIdempotencyKey() }, { deep: true })

async function load(append = false) {
  try {
    const params = new URLSearchParams({ limit: '20' })
    if (append && page.value.next_cursor) params.set('cursor', page.value.next_cursor)
    const res = await api<{ data: OvertimeRequest[]; page: Page }>('/overtime-requests?' + params)
    requests.value = append ? [...requests.value, ...res.data] : res.data
    page.value = res.page
  } catch (e) {
    error.value = errorText(e, 'Gagal memuat lembur')
  }
}

async function submit() {
  busy.value = true
  error.value = ''
  notice.value = ''
  // Selesai sebelum mulai = melewati tengah malam.
  const endDate = form.value.end <= form.value.start
    ? new Date(new Date(`${form.value.work_date}T00:00:00`).getTime() + 86400000).toISOString().slice(0, 10)
    : form.value.work_date
  try {
    const r = await api<OvertimeRequest>('/overtime-requests', { method: 'POST', idempotencyKey: submitKey, body: {
      work_date: form.value.work_date, reason: form.value.reason,
      start_at: localDateTime(form.value.work_date, form.value.start), end_at: localDateTime(endDate, form.value.end) } })
    notice.value = `Diajukan ${r.hours} jam.` + (r.warnings.includes('OVERTIME_OVER_LIMIT') ? ' Melebihi batas harian perusahaan (tetap bisa disetujui).' : '')
    form.value = { ...form.value, reason: '' }
    await load()
  } catch (e) {
    if (e instanceof ApiError && e.code === 'OVERLAPPING_PERIOD') error.value = 'Jam bertabrakan dengan lembur lain yang menunggu atau sudah disetujui.'
    else error.value = errorText(e, 'Gagal mengajukan lembur')
  } finally {
    busy.value = false
  }
}

async function cancel(r: OvertimeRequest) {
  busy.value = true
  try {
    await api(`/overtime-requests/${r.id}/cancel`, { method: 'POST', body: {} })
    await load()
  } catch (e) {
    error.value = errorText(e, 'Gagal membatalkan')
  } finally {
    busy.value = false
  }
}

onMounted(() => load())
</script>

<template>
  <div class="stack">
    <div class="page-head">
      <div>
        <h1>Lembur</h1>
        <p class="sub">Hanya lembur yang disetujui sebelum cut-off periode yang masuk payroll bulan itu.</p>
      </div>
    </div>
    <p v-if="error" class="alert error" role="alert">{{ error }}</p>
    <p v-if="notice" class="note" role="status"><AppIcon name="check" />{{ notice }}</p>

    <form v-if="auth.employeeId" class="card form" @submit.prevent="submit">
      <h2>Ajukan lembur</h2>
      <div class="row-wrap">
        <div class="field"><label for="d">Tanggal</label><input id="d" v-model="form.work_date" class="input" type="date" required /></div>
        <div class="field"><label for="s">Mulai</label><input id="s" v-model="form.start" class="input" type="time" required /></div>
        <div class="field"><label for="e">Selesai</label><input id="e" v-model="form.end" class="input" type="time" required /></div>
      </div>
      <div class="field"><label for="r">Alasan</label><input id="r" v-model="form.reason" class="input" maxlength="500" required /></div>
      <button type="submit" class="btn" :disabled="busy">Ajukan</button>
    </form>
    <p v-else class="note"><AppIcon name="info" />Akunmu belum tertaut ke data karyawan, jadi tidak bisa mengajukan lembur.</p>

    <div class="card card-flush">
      <div class="card-head"><h2>Permintaan lembur</h2></div>
      <div class="table-scroll">
        <table class="table">
          <thead><tr><th scope="col">Tanggal</th><th scope="col">Jam</th><th scope="col" class="right">Durasi</th><th scope="col">Status</th><th scope="col">Penyetuju</th><th scope="col"><span class="sr-only">Aksi</span></th></tr></thead>
          <tbody>
            <tr v-if="requests.length === 0"><td colspan="6" class="empty">Belum ada permintaan.</td></tr>
            <tr v-for="r in requests" :key="r.id">
              <td>{{ formatDate(r.work_date) }}</td>
              <td class="num">{{ formatTime(r.start_at) }}–{{ formatTime(r.end_at) }}</td>
              <td class="right num">{{ r.hours }} j<span v-if="r.warnings.length" class="pill warn" title="Melebihi batas harian">!</span></td>
              <td><span class="pill" :class="requestStatusPill[r.status]">{{ requestStatusLabel[r.status] ?? r.status }}</span></td>
              <td class="muted">{{ r.approval?.approver.display_name ?? '—' }}</td>
              <td class="right">
                <button v-if="r.status === 'PENDING' && r.employee_id === auth.employeeId" type="button" class="btn-row" :disabled="busy" @click="cancel(r)">Batalkan</button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <div v-if="page.has_more" class="table-foot"><button type="button" class="btn sm ghost" @click="load(true)">Muat lagi</button></div>
    </div>
  </div>
</template>

<style scoped>
.row-wrap .field { flex: 1 1 160px; }
</style>
