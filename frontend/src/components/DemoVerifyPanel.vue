<script setup lang="ts">
import { ref, watch } from 'vue'
import { api, ApiError } from '../api'
import AppIcon from './AppIcon.vue'
import { useDemo } from '../stores/demo'
import type { PayrollRun } from '../types'

// Mode demo, "di balik layar": setiap slip dihitung ulang dari snapshot input + aturan yang tersimpan saat run
// dihitung (PI-12). Bukti bahwa angka final tidak bergantung pada data hari ini dan bisa diaudit kapan pun.
const props = defineProps<{ run: PayrollRun }>()
const demo = useDemo()
type Result = { items_checked: number; mismatches: number; totals_ok: boolean; ok: boolean }
const res = ref<Result | null>(null)
const busy = ref(false)
const error = ref('')
watch(() => [props.run.id, props.run.calc_version], () => (res.value = null))

async function verify() {
  busy.value = true
  error.value = ''
  try {
    res.value = await api<Result>(`/demo/runs/${props.run.id}/verify`, { method: 'POST' })
    if (res.value.ok) demo.mark('verify')
  } catch (e) {
    error.value = e instanceof ApiError && e.code === 'RATE_LIMITED' ? 'Dibatasi 3 kali per menit. Tunggu sebentar.' : 'Pemeriksaan gagal dijalankan. Coba lagi.'
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <section v-if="demo.sandbox && run.calc_version > 0 && run.status !== 'CALCULATING' && run.status !== 'CANCELLED'" class="card stack verify-panel" aria-labelledby="verify-title" data-test="verify-panel">
    <div class="verify-head">
      <span class="verify-icon"><AppIcon name="lock" :size="20" :stroke="2" /></span>
      <div>
        <h2 id="verify-title">Di balik layar: cek ulang hasil</h2>
        <p class="muted">Setiap slip menyimpan salinan data dan aturan yang dipakai saat dihitung. Pemeriksaan ini menghitung ulang semuanya dari salinan itu; hasilnya harus sama persis, berapa lama pun sejak gaji dibayar.</p>
      </div>
    </div>
    <button type="button" class="btn sm ghost" data-test="verify-run" :disabled="busy" @click="verify">
      <AppIcon name="refresh" />{{ busy ? 'Menghitung ulang…' : 'Cek ulang semua slip' }}
    </button>
    <p v-if="error" class="alert error" role="alert">{{ error }}</p>
    <p v-if="res" class="verdict" :class="{ bad: !res.ok }" data-test="verify-verdict">
      <AppIcon :name="res.ok ? 'check' : 'x'" :size="18" :stroke="2.4" />
      <template v-if="res.ok">{{ res.items_checked }} dari {{ res.items_checked }} slip cocok persis, dan totalnya sama.</template>
      <template v-else>{{ res.mismatches }} slip berbeda dari hasil tersimpan{{ res.totals_ok ? '' : ', total tidak sama' }}.</template>
    </p>
  </section>
</template>

<style scoped>
.verify-head { display: flex; gap: 14px; align-items: flex-start; }
.verify-head p { margin-top: 4px; line-height: 1.5; }
.verify-icon { width: 40px; height: 40px; border-radius: 12px; background: var(--ok-bg); color: var(--ok); display: flex; align-items: center; justify-content: center; flex-shrink: 0; }
.verify-panel .btn { align-self: flex-start; }
.verdict { display: flex; gap: 8px; align-items: center; font-weight: 700; color: var(--ok); }
.verdict.bad { color: var(--bad); }
</style>
