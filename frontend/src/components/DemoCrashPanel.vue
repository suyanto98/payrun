<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { api, ApiError } from '../api'
import AppIcon from './AppIcon.vue'
import { useDemo } from '../stores/demo'
import type { PayrollRun } from '../types'

// Mode demo: "server mati di tengah hitung gaji" sebagai tombol. Gangguan dipasang untuk perhitungan
// berikutnya di perusahaan pengunjung saja; timeline membaca status setiap bagian (chunk) dari server.
const props = defineProps<{ run: PayrollRun; canCalculate: boolean }>()
const demo = useDemo()

type Chunk = { no: number; status: string; attempts: number; failed_once: boolean; simulated: boolean }
type Timeline = { calc_version: number; chunks: Chunk[]; crash_armed: boolean }
const tl = ref<Timeline | null>(null)
const busy = ref(false)
const error = ref('')

let timer: ReturnType<typeof setTimeout> | undefined
async function load() {
  clearTimeout(timer)
  try {
    tl.value = await api<Timeline>(`/demo/runs/${props.run.id}/timeline`)
  } catch {
    tl.value = null
  }
  if (props.run.status === 'CALCULATING') timer = setTimeout(load, 700)
}
watch(() => [props.run.id, props.run.status, props.run.calc_version], load, { immediate: true })
onBeforeUnmount(() => clearTimeout(timer))

async function arm() {
  busy.value = true
  error.value = ''
  try {
    await api('/demo/crash-test', { method: 'POST' })
    await load()
  } catch (e) {
    error.value = e instanceof ApiError && e.code === 'RATE_LIMITED' ? 'Dibatasi 3 kali per menit. Tunggu sebentar.' : 'Gangguan gagal dipasang. Coba lagi.'
  } finally {
    busy.value = false
  }
}

const crashed = computed(() => tl.value?.chunks.some((c) => c.simulated) ?? false)
const finished = computed(() => props.run.status !== 'CALCULATING' && props.run.calc_version > 0)
const okCount = computed(() => props.run.counts.ok ?? 0)
// Tur demo: kesimpulan tampil = langkah "simulasi gangguan" selesai.
watch(() => finished.value && crashed.value, (v) => v && demo.mark('crash'), { immediate: true })
const chunkLabel = (c: Chunk) => {
  if (c.status === 'succeeded') return c.simulated ? `Bagian ${c.no}: mati di tengah → diulang → selesai` : `Bagian ${c.no}: selesai`
  if (c.status === 'running') return `Bagian ${c.no}: menghitung…`
  if (c.simulated || c.failed_once) return `Bagian ${c.no}: mati di tengah, menunggu diulang…`
  return `Bagian ${c.no}: antre`
}
</script>

<template>
  <section v-if="demo.sandbox" class="card stack crash-panel" aria-labelledby="crash-title" data-test="crash-panel">
    <div class="crash-head">
      <span class="crash-icon"><AppIcon name="shield" :size="20" :stroke="2" /></span>
      <div>
        <h2 id="crash-title">Simulasi gangguan</h2>
        <p class="muted">Apa yang terjadi kalau server mati saat gaji sedang dihitung? Pasang gangguan, lalu klik <strong>Hitung</strong>: satu bagian perhitungan akan mati di tengah jalan sebelum tersimpan.</p>
      </div>
    </div>
    <div v-if="canCalculate" class="row-wrap end">
      <button v-if="!tl?.crash_armed" type="button" class="btn sm ghost" data-test="arm-crash" :disabled="busy" @click="arm">
        <AppIcon name="x" />{{ busy ? 'Memasang…' : 'Pasang gangguan' }}
      </button>
      <p v-else class="pill warn" data-test="crash-armed">Gangguan terpasang: satu bagian akan mati di tengah. Sekarang klik Hitung.</p>
    </div>
    <p v-if="error" class="alert error" role="alert">{{ error }}</p>

    <ol v-if="tl && tl.chunks.length" class="chunks" aria-live="polite">
      <li v-for="c in tl.chunks" :key="c.no" :class="{ ok: c.status === 'succeeded', bad: c.status !== 'succeeded' && (c.simulated || c.failed_once), retried: c.simulated }">
        <AppIcon :name="c.status === 'succeeded' ? 'check' : c.simulated || c.failed_once ? 'x' : 'clock'" :size="16" :stroke="2.4" />
        <span>{{ chunkLabel(c) }}</span>
        <span v-if="c.attempts > 1" class="muted small">({{ c.attempts }} percobaan)</span>
      </li>
    </ol>
    <p v-if="finished && crashed" class="verdict" data-test="crash-verdict">
      <AppIcon name="check" :size="18" :stroke="2.4" />
      {{ okCount }} karyawan, {{ okCount }} hasil. Bagian yang mati dihitung ulang dari awal; tidak ada gaji yang dobel atau hilang.
    </p>
  </section>
</template>

<style scoped>
.crash-head { display: flex; gap: 14px; align-items: flex-start; }
.crash-head p { margin-top: 4px; line-height: 1.5; }
.crash-icon { width: 40px; height: 40px; border-radius: 12px; background: var(--warn-bg); color: var(--warn); display: flex; align-items: center; justify-content: center; flex-shrink: 0; }
.chunks { list-style: none; padding: 0; margin: 0; display: flex; flex-direction: column; gap: 6px; font-size: 14px; }
.chunks li { display: flex; align-items: center; gap: 8px; padding: 8px 12px; border-radius: 10px; background: var(--bg); color: var(--muted); }
.chunks li.ok { background: var(--ok-bg); color: var(--ok); }
.chunks li.bad { background: var(--bad-bg); color: var(--bad); }
.chunks li.ok.retried { background: var(--warn-bg); color: var(--warn); }
.verdict { display: flex; gap: 8px; align-items: center; font-weight: 700; color: var(--ok); }
.small { font-size: 12px; }
</style>
