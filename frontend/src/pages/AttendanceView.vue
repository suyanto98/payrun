<script setup lang="ts">
import { errorText } from '../errors'
import { computed, onMounted, ref, watch } from 'vue'
import { api, ApiError } from '../api'
import AppIcon from '../components/AppIcon.vue'
import { canEditEmployees, formatDate, formatTime } from '../access'
import { useAuth } from '../stores/auth'
import type { AttendanceRecord, AttendanceSummary, Page } from '../types'

// D1/D2 clock in/out (diri sendiri), D3 catatan bulan ini + ringkasan; OWNER/HR: rekap harian + koreksi D4.
const auth = useAuth()
const canCorrect = computed(() => canEditEmployees(auth.roles))
const now = new Date()
const period = ref(`${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, '0')}`)
const mine = ref<AttendanceRecord[]>([])
const summary = ref<AttendanceSummary | null>(null)
const error = ref('')
const notice = ref('')
const busy = ref(false)

const today = computed(() => mine.value.find((r) => r.status === 'INCOMPLETE') ?? null)

async function loadMine() {
  if (!auth.employeeId) return
  try {
    const res = await api<{ data: AttendanceRecord[]; summary?: AttendanceSummary }>(
      `/attendance?period=${period.value}&employee_id=${auth.employeeId}&limit=62`)
    mine.value = res.data
    summary.value = res.summary ?? null
  } catch (e) {
    error.value = errorText(e, 'Gagal memuat absensi')
  }
}

async function clock(kind: 'in' | 'out') {
  busy.value = true
  error.value = ''
  notice.value = ''
  try {
    const r = await api<AttendanceRecord>(`/attendance/clock-${kind}`, { method: 'POST' })
    notice.value = kind === 'in' ? `Clock in ${formatTime(r.clock_in_at)} tercatat.` : `Clock out ${formatTime(r.clock_out_at)} tercatat.`
    await loadMine()
  } catch (e) {
    error.value = errorText(e, 'Gagal menyimpan')
  } finally {
    busy.value = false
  }
}

// ---------------------------------------------------------------- rekap harian (OWNER/HR) + koreksi
const day = ref(now.toISOString().slice(0, 10))
const daily = ref<AttendanceRecord[]>([])
const dailyPage = ref<Page>({ next_cursor: null, has_more: false })
const editing = ref<string | null>(null)
const fixIn = ref('')
const fixOut = ref('')
const fixReason = ref('')

async function loadDaily() {
  if (!canCorrect.value) return
  try {
    const res = await api<{ data: AttendanceRecord[]; page: Page }>(`/attendance?date=${day.value}&limit=200`)
    daily.value = res.data
    dailyPage.value = res.page
  } catch (e) {
    error.value = errorText(e, 'Gagal memuat rekap')
  }
}

function startEdit(r: AttendanceRecord) {
  editing.value = r.id
  fixIn.value = r.clock_in_at ? formatTime(r.clock_in_at) : ''
  fixOut.value = r.clock_out_at ? formatTime(r.clock_out_at) : ''
  fixReason.value = ''
}

async function saveFix(r: AttendanceRecord) {
  busy.value = true
  error.value = ''
  const body: Record<string, string> = { correction_reason: fixReason.value }
  // Jam lokal pada tanggal kerja; clock-out sebelum clock-in = shift malam → keesokan harinya.
  if (fixIn.value) body.clock_in_at = new Date(`${r.work_date}T${fixIn.value}:00`).toISOString()
  if (fixOut.value) {
    const out = new Date(`${r.work_date}T${fixOut.value}:00`)
    if (fixIn.value && fixOut.value <= fixIn.value) out.setDate(out.getDate() + 1)
    body.clock_out_at = out.toISOString()
  }
  try {
    await api(`/attendance/${r.id}`, { method: 'PATCH', body, ifMatch: `"${r.version}"` })
    editing.value = null
    await Promise.all([loadDaily(), loadMine()])
  } catch (e) {
    error.value = errorText(e, 'Gagal menyimpan koreksi')
  } finally {
    busy.value = false
  }
}

watch(period, loadMine)
watch(day, loadDaily)
onMounted(() => Promise.all([loadMine(), loadDaily()]))
const pill: Record<string, string> = { COMPLETE: 'ok', INCOMPLETE: 'warn' }
</script>

<template>
  <div class="stack">
    <div class="page-head">
      <div>
        <h1>Absensi</h1>
        <p class="sub">Waktu dicatat oleh server; tanggal kerja mengikuti zona waktu perusahaan.</p>
      </div>
    </div>
    <p v-if="error" class="alert error" role="alert">{{ error }}</p>
    <p v-if="notice" class="note" role="status"><AppIcon name="check" />{{ notice }}</p>

    <div v-if="auth.employeeId" class="card clock">
      <div>
        <strong>{{ today ? `Masuk ${formatTime(today.clock_in_at)}` : 'Belum clock in hari ini' }}</strong>
        <p class="muted">Lupa clock out? Catatan tetap INCOMPLETE dan bisa dikoreksi HR.</p>
      </div>
      <div class="btn-group">
        <button type="button" class="btn sm" :disabled="busy" @click="clock('in')"><AppIcon name="clock" />Clock in</button>
        <button type="button" class="btn sm ghost" :disabled="busy" @click="clock('out')">Clock out</button>
      </div>
    </div>
    <p v-else class="note"><AppIcon name="info" />Akunmu belum tertaut ke data karyawan, jadi tidak bisa clock in.</p>

    <div v-if="auth.employeeId" class="card card-flush">
      <div class="card-head">
        <h2>Catatan saya</h2>
        <input v-model="period" class="input sm" type="month" aria-label="Periode" />
      </div>
      <p v-if="summary" class="muted pad">Hadir {{ summary.present_days }} dari {{ summary.working_days }} hari kerja · belum lengkap {{ summary.incomplete_days }}</p>
      <div class="table-scroll">
        <table class="table">
          <thead><tr><th scope="col">Tanggal</th><th scope="col">Masuk</th><th scope="col">Keluar</th><th scope="col">Status</th></tr></thead>
          <tbody>
            <tr v-if="mine.length === 0"><td colspan="4" class="empty">Belum ada catatan.</td></tr>
            <tr v-for="r in mine" :key="r.id">
              <td>{{ formatDate(r.work_date) }}</td>
              <td class="num">{{ formatTime(r.clock_in_at) }}</td>
              <td class="num">{{ formatTime(r.clock_out_at) }}</td>
              <td><span class="pill" :class="pill[r.status]">{{ r.status }}</span><span v-if="r.source === 'CORRECTION'" class="muted"> · dikoreksi</span></td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <div v-if="canCorrect" class="card card-flush">
      <div class="card-head">
        <h2>Rekap harian</h2>
        <input v-model="day" class="input sm" type="date" aria-label="Tanggal" />
      </div>
      <div class="table-scroll">
        <table class="table">
          <thead><tr><th scope="col">Karyawan</th><th scope="col">Masuk</th><th scope="col">Keluar</th><th scope="col">Status</th><th scope="col"><span class="sr-only">Aksi</span></th></tr></thead>
          <tbody>
            <tr v-if="daily.length === 0"><td colspan="5" class="empty">Tidak ada catatan di tanggal ini.</td></tr>
            <template v-for="r in daily" :key="r.id">
              <tr>
                <td><RouterLink :to="`/employees/${r.employee_id}`" class="mono">{{ r.employee_id.slice(0, 8) }}</RouterLink></td>
                <td class="num">{{ formatTime(r.clock_in_at) }}</td>
                <td class="num">{{ formatTime(r.clock_out_at) }}</td>
                <td><span class="pill" :class="pill[r.status]">{{ r.status }}</span></td>
                <td class="right"><button type="button" class="btn-row" @click="editing === r.id ? (editing = null) : startEdit(r)">Koreksi</button></td>
              </tr>
              <tr v-if="editing === r.id">
                <td colspan="5">
                  <form class="fix" @submit.prevent="saveFix(r)">
                    <label>Masuk <input v-model="fixIn" class="input sm" type="time" /></label>
                    <label>Keluar <input v-model="fixOut" class="input sm" type="time" /></label>
                    <input v-model="fixReason" class="input sm reason" maxlength="500" required placeholder="Alasan koreksi (wajib)" aria-label="Alasan koreksi" />
                    <button type="submit" class="btn sm" :disabled="busy">Simpan</button>
                  </form>
                </td>
              </tr>
            </template>
          </tbody>
        </table>
      </div>
    </div>
  </div>
</template>

<style scoped>
.clock { display: flex; flex-wrap: wrap; justify-content: space-between; gap: 16px; align-items: center; }
.pad { padding: 0 24px; }
.fix { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; }
.fix label { display: flex; gap: 6px; align-items: center; font-size: 13px; }
.fix .reason { flex: 1 1 240px; }
</style>
