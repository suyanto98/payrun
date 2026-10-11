<script setup lang="ts">
import { errorText } from '../errors'
import { onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { api, ApiError } from '../api'
import AppIcon from '../components/AppIcon.vue'
import PayslipCard from '../components/PayslipCard.vue'
import type { Payslip } from '../types'

// S4 (PI-17): OWNER/HR_ADMIN/PAYROLL_ADMIN membuka slip karyawan. SETIAP pembukaan dan unduhan tercatat
// di audit log; halaman ini tidak memuat ulang otomatis supaya tidak membuat baris audit tanpa niat.
const route = useRoute()
const slip = ref<Payslip | null>(null)
const error = ref('')

onMounted(async () => {
  try {
    slip.value = await api<Payslip>(`/payslips/${route.params.id}`)
  } catch (e) {
    error.value = e instanceof ApiError && e.status === 404 ? 'Slip tidak ditemukan.' : errorText(e, 'Gagal memuat slip')
  }
})
</script>

<template>
  <div class="stack">
    <div class="page-head">
      <div>
        <RouterLink v-if="slip" :to="`/payroll/${slip.run_id}`" class="back"><AppIcon name="back" :size="16" />Kembali ke payroll</RouterLink>
        <h1>Slip gaji karyawan</h1>
      </div>
    </div>
    <p class="note"><AppIcon name="shield" />Akses ke slip orang lain tercatat di audit log (siapa, kapan, slip siapa).</p>
    <p v-if="error" class="alert error" role="alert">{{ error }}</p>
    <PayslipCard v-if="slip" :slip="slip" />
  </div>
</template>

<style scoped>
.back { display: inline-flex; align-items: center; gap: 4px; font-size: 13px; margin-bottom: 4px; }
</style>
