<script setup lang="ts">
import { errorText } from '../errors'
import { onMounted, ref, watch } from 'vue'
import { api, ApiError, newIdempotencyKey } from '../api'
import AppIcon from '../components/AppIcon.vue'
import { formatDate, requestStatusLabel, requestStatusPill } from '../access'
import { useAuth } from '../stores/auth'
import type { LeaveBalance, LeaveRequest, LeaveType, Page } from '../types'

// L1 jenis, L2 saldo saya, L3 ajukan (Idempotency-Key per isi form), L4 batalkan (pengaju, PENDING), L5 daftar
// (karyawan: miliknya; manajer: + bawahan; HR/Owner: semua).
const auth = useAuth()
const year = ref(new Date().getFullYear())
const types = ref<LeaveType[]>([])
const balances = ref<LeaveBalance[]>([])
const requests = ref<LeaveRequest[]>([])
const page = ref<Page>({ next_cursor: null, has_more: false })
const error = ref('')
const notice = ref('')
const busy = ref(false)

const form = ref({ leave_type_id: '', start_date: '', end_date: '', reason: '' })
let submitKey = newIdempotencyKey()
watch(form, () => { submitKey = newIdempotencyKey() }, { deep: true })

async function load(append = false) {
  error.value = ''
  try {
    const params = new URLSearchParams({ limit: '20' })
    if (append && page.value.next_cursor) params.set('cursor', page.value.next_cursor)
    const [t, l] = await Promise.all([
      api<{ data: LeaveType[] }>('/leave-types'),
      api<{ data: LeaveRequest[]; page: Page }>('/leave-requests?' + params),
    ])
    types.value = t.data.filter((x) => x.status === 'ACTIVE')
    requests.value = append ? [...requests.value, ...l.data] : l.data
    page.value = l.page
    if (!form.value.leave_type_id && types.value.length) form.value.leave_type_id = types.value[0].id
    if (auth.employeeId) balances.value = (await api<{ data: LeaveBalance[] }>(`/me/leave-balances?year=${year.value}`)).data
  } catch (e) {
    error.value = errorText(e, 'Gagal memuat cuti')
  }
}

async function submit() {
  busy.value = true
  error.value = ''
  notice.value = ''
  try {
    const body: Record<string, string> = { leave_type_id: form.value.leave_type_id, start_date: form.value.start_date, end_date: form.value.end_date }
    if (form.value.reason.trim()) body.reason = form.value.reason.trim()
    const r = await api<LeaveRequest>('/leave-requests', { method: 'POST', body, idempotencyKey: submitKey })
    notice.value = `Diajukan: ${r.days} hari kerja, menunggu ${r.approval?.approver.display_name ?? 'approver'}.`
    form.value = { ...form.value, start_date: '', end_date: '', reason: '' }
    await load()
  } catch (e) {
    if (e instanceof ApiError && e.code === 'OVERLAPPING_LEAVE') error.value = 'Tanggal bertabrakan dengan cuti lain yang menunggu atau sudah disetujui.'
    else if (e instanceof ApiError && e.code === 'NO_APPROVER_AVAILABLE') error.value = 'Tidak ada orang lain di perusahaan yang bisa menyetujui cuti ini.'
    else error.value = errorText(e, 'Gagal mengajukan cuti')
  } finally {
    busy.value = false
  }
}

async function cancel(r: LeaveRequest) {
  busy.value = true
  error.value = ''
  try {
    await api(`/leave-requests/${r.id}/cancel`, { method: 'POST', body: {} })
    await load()
  } catch (e) {
    error.value = errorText(e, 'Gagal membatalkan')
  } finally {
    busy.value = false
  }
}

watch(year, () => load())
onMounted(() => load())
</script>

<template>
  <div class="stack">
    <div class="page-head">
      <div>
        <h1>Cuti</h1>
        <p class="sub">Saldo berkurang saat cuti disetujui, bukan saat diajukan.</p>
      </div>
    </div>
    <p v-if="error" class="alert error" role="alert">{{ error }}</p>
    <p v-if="notice" class="note" role="status"><AppIcon name="check" />{{ notice }}</p>

    <div v-if="auth.employeeId" class="row-wrap">
      <div v-for="b in balances" :key="b.leave_type.id" class="card balance">
        <span class="muted">{{ b.leave_type.name }} {{ b.year }}</span>
        <strong class="num">{{ b.balance_days ?? '—' }}</strong>
        <span class="muted">{{ b.balance_days === null ? 'tanpa kuota' : 'hari tersisa' }}<template v-if="b.pending_days !== '0.0'"> · {{ b.pending_days }} menunggu</template></span>
      </div>
    </div>

    <form v-if="auth.employeeId" class="card form" @submit.prevent="submit">
      <h2>Ajukan cuti</h2>
      <div class="row-wrap">
        <div class="field">
          <label for="lt">Jenis</label>
          <select id="lt" v-model="form.leave_type_id" class="select" required>
            <option v-for="t in types" :key="t.id" :value="t.id">{{ t.name }}{{ t.is_paid ? '' : ' (tidak dibayar)' }}</option>
          </select>
        </div>
        <div class="field">
          <label for="start">Mulai</label>
          <input id="start" v-model="form.start_date" class="input" type="date" required />
        </div>
        <div class="field">
          <label for="end">Sampai (termasuk)</label>
          <input id="end" v-model="form.end_date" class="input" type="date" :min="form.start_date" required />
        </div>
      </div>
      <div class="field">
        <label for="reason">Alasan (opsional)</label>
        <input id="reason" v-model="form.reason" class="input" maxlength="500" />
      </div>
      <button type="submit" class="btn" :disabled="busy">Ajukan</button>
    </form>
    <p v-else class="note"><AppIcon name="info" />Akunmu belum tertaut ke data karyawan, jadi tidak bisa mengajukan cuti.</p>

    <div class="card card-flush">
      <div class="card-head"><h2>Permintaan cuti</h2></div>
      <div class="table-scroll">
        <table class="table">
          <thead><tr><th scope="col">Jenis</th><th scope="col">Tanggal</th><th scope="col" class="right">Hari</th><th scope="col">Status</th><th scope="col">Approver</th><th scope="col"><span class="sr-only">Aksi</span></th></tr></thead>
          <tbody>
            <tr v-if="requests.length === 0"><td colspan="6" class="empty">Belum ada permintaan.</td></tr>
            <tr v-for="r in requests" :key="r.id">
              <td>{{ r.leave_type.name }}</td>
              <td>{{ formatDate(r.start_date) }}<template v-if="r.end_date !== r.start_date"> – {{ formatDate(r.end_date) }}</template></td>
              <td class="right num">{{ r.days }}</td>
              <td><span class="pill" :class="requestStatusPill[r.status]">{{ requestStatusLabel[r.status] ?? r.status }}</span></td>
              <td class="muted">{{ r.approval?.approver.display_name ?? '—' }}</td>
              <td class="right">
                <button v-if="r.status === 'PENDING' && r.employee_id === auth.employeeId" type="button" class="btn-row" :disabled="busy" @click="cancel(r)">Batalkan</button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <div v-if="page.has_more" class="table-foot">
        <button type="button" class="btn sm ghost" @click="load(true)">Muat lagi</button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.balance { display: flex; flex-direction: column; gap: 4px; flex: 1 1 180px; }
.balance strong { font-size: 28px; font-weight: 800; }
.row-wrap .field { flex: 1 1 180px; }
</style>
