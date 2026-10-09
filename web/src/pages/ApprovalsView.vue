<script setup lang="ts">
import { onMounted, ref, watch } from 'vue'
import { api, ApiError } from '../api'
import AppIcon from '../components/AppIcon.vue'
import { staleReasonLabel } from '../access'
import type { ApprovalRequest, Page, StaleEmployee } from '../types'

// R1 inbox (approval yang di-snapshot untuk saya), R2 approve, R3 reject (catatan wajib). PAYROLL_RUN yang
// inputnya berubah sejak kalkulasi → 409 RUN_STALE: daftar karyawan + alasan ditampilkan.
const status = ref('PENDING')
const items = ref<ApprovalRequest[]>([])
const page = ref<Page>({ next_cursor: null, has_more: false })
const loading = ref(false)
const busy = ref(false)
const error = ref('')
const notice = ref('')
const stale = ref<{ id: string; employees: StaleEmployee[]; count: number } | null>(null)
const rejecting = ref<string | null>(null)
const note = ref('')

async function load(append = false) {
  loading.value = true
  error.value = ''
  const params = new URLSearchParams({ status: status.value, limit: '20' })
  if (append && page.value.next_cursor) params.set('cursor', page.value.next_cursor)
  try {
    const res = await api<{ data: ApprovalRequest[]; page: Page }>('/approvals/inbox?' + params)
    items.value = append ? [...items.value, ...res.data] : res.data
    page.value = res.page
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : 'Gagal memuat approval'
  } finally {
    loading.value = false
  }
}

async function decide(a: ApprovalRequest, approve: boolean) {
  busy.value = true
  error.value = ''
  notice.value = ''
  stale.value = null
  try {
    await api(`/approvals/${a.id}/${approve ? 'approve' : 'reject'}`, { method: 'POST', body: approve ? {} : { note: note.value } })
    notice.value = approve ? 'Disetujui.' : 'Ditolak. Pengaju bisa memperbaiki lalu mengajukan ulang.'
    rejecting.value = null
    note.value = ''
    await load()
  } catch (e) {
    if (e instanceof ApiError && e.code === 'RUN_STALE') {
      stale.value = { id: a.id, employees: (e.extra.employees_changed as StaleEmployee[]) ?? [], count: Number(e.extra.changed_count ?? 0) }
    } else if (e instanceof ApiError && e.code === 'APPROVAL_ALREADY_DECIDED') {
      notice.value = 'Approval ini sudah diputuskan sebelumnya.'
      await load()
    } else {
      error.value = e instanceof ApiError ? e.message : 'Gagal menyimpan keputusan'
    }
  } finally {
    busy.value = false
  }
}

watch(status, () => load())
onMounted(() => load())
const pill: Record<string, string> = { PENDING: 'warn', APPROVED: 'ok', REJECTED: 'bad', CANCELLED: '' }
</script>

<template>
  <div class="stack">
    <div class="page-head">
      <div>
        <h1>Approval</h1>
        <p class="sub">Permintaan yang menunggu keputusan kamu.</p>
      </div>
      <select v-model="status" class="select sm" aria-label="Filter status">
        <option value="PENDING">Menunggu</option>
        <option value="APPROVED">Disetujui</option>
        <option value="REJECTED">Ditolak</option>
      </select>
    </div>

    <p v-if="error" class="alert error" role="alert">{{ error }}</p>
    <p v-if="notice" class="note" role="status"><AppIcon name="info" />{{ notice }}</p>

    <div v-if="!loading && items.length === 0" class="card empty">Tidak ada approval.</div>
    <div v-for="a in items" :key="a.id" class="card approval">
      <div class="approval-head">
        <div>
          <strong>{{ a.subject.summary }}</strong>
          <div class="muted">
            Diajukan {{ a.requester.display_name ?? 'pengguna lain' }}
            <template v-if="a.self_approved"> · disetujui sendiri: {{ a.self_approval_reason }}</template>
          </div>
        </div>
        <span class="pill" :class="pill[a.status]">{{ a.status }}</span>
      </div>
      <p v-if="a.decision_note" class="muted">Catatan: {{ a.decision_note }}</p>

      <div v-if="stale && stale.id === a.id" class="alert error" role="alert">
        Input payroll berubah sejak dihitung ({{ stale.count }} karyawan). Tolak lalu minta pengaju menghitung ulang.
        <ul v-if="stale.employees.length">
          <li v-for="s in stale.employees" :key="s.employee_id">{{ s.employee_no ?? s.employee_id }}: {{ staleReasonLabel[s.reason] ?? s.reason }}</li>
        </ul>
      </div>

      <div v-if="a.status === 'PENDING'" class="btn-group">
        <RouterLink v-if="a.subject_type === 'PAYROLL_RUN'" :to="`/payroll/${a.subject_id}`" class="btn sm ghost">Lihat run</RouterLink>
        <button type="button" class="btn sm" :disabled="busy" @click="decide(a, true)"><AppIcon name="check" />Setujui</button>
        <button type="button" class="btn sm ghost" :disabled="busy" @click="rejecting = rejecting === a.id ? null : a.id"><AppIcon name="x" />Tolak</button>
      </div>
      <form v-if="rejecting === a.id" class="reject" @submit.prevent="decide(a, false)">
        <input v-model="note" class="input sm" maxlength="500" required placeholder="Alasan penolakan (wajib)" aria-label="Alasan penolakan" />
        <button type="submit" class="btn sm" :disabled="busy">Tolak</button>
      </form>
    </div>
    <button v-if="page.has_more" type="button" class="btn sm ghost" :disabled="loading" @click="load(true)">Muat lagi</button>
  </div>
</template>

<style scoped>
.approval { display: flex; flex-direction: column; gap: 12px; }
.approval-head { display: flex; justify-content: space-between; gap: 12px; align-items: flex-start; }
.reject { display: flex; flex-wrap: wrap; gap: 8px; }
.reject .input { flex: 1 1 280px; }
.alert ul { margin: 8px 0 0; padding-left: 18px; font-weight: 500; }
</style>
