<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import AppIcon from '../components/AppIcon.vue'
import { rolesText } from '../access'
import { useAuth } from '../stores/auth'
import type { Membership } from '../types'

const auth = useAuth()
const router = useRouter()
const items = ref<Membership[]>([])
const loading = ref(true)
const error = ref('')
const busyId = ref('')

onMounted(async () => {
  items.value = await auth.memberships()
  loading.value = false
})

async function choose(m: Membership) {
  busyId.value = m.tenant_id
  error.value = ''
  try {
    await auth.switchTenant(m.tenant_id)
    router.push('/')
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    busyId.value = ''
  }
}
</script>

<template>
  <div class="stack narrow">
    <div class="page-head">
      <div>
        <h1>Pilih perusahaan</h1>
        <p class="sub">Data setiap perusahaan terpisah. Kamu hanya melihat data perusahaan yang sedang dipilih.</p>
      </div>
    </div>
    <p v-if="error" class="alert error" role="alert">{{ error }}</p>
    <div class="card card-flush">
      <p v-if="loading" class="empty">Memuat…</p>
      <p v-else-if="items.length === 0" class="empty">
        Belum ada perusahaan aktif. Kalau baru mendaftar, verifikasi email dulu; kalau diundang, buka link undangannya.
      </p>
      <ul v-else class="tenant-list">
        <li v-for="m in items" :key="m.tenant_id">
          <button type="button" class="tenant-item" :class="{ current: auth.tenant?.id === m.tenant_id }" :disabled="busyId !== ''" @click="choose(m)">
            <span class="brand-mark"><AppIcon name="building" /></span>
            <span class="tenant-text">
              <strong>{{ m.tenant_name }}</strong>
              <span class="muted">{{ rolesText(m.roles) }}</span>
            </span>
            <span v-if="auth.tenant?.id === m.tenant_id" class="pill ok">Aktif</span>
            <AppIcon v-else name="chevron" />
          </button>
        </li>
      </ul>
    </div>
  </div>
</template>

<style scoped>
.tenant-list { list-style: none; margin: 0; padding: 8px; }
.tenant-item { width: 100%; display: flex; align-items: center; gap: 14px; padding: 14px 16px; border: 0; border-radius: 14px; background: transparent; text-align: left; cursor: pointer; color: var(--text); font: inherit; }
.tenant-item:hover:not(:disabled), .tenant-item.current { background: var(--bg); }
.tenant-item .brand-mark { background: var(--blue-tint); color: var(--blue-dark); }
.tenant-text { flex: 1; display: flex; flex-direction: column; gap: 2px; }
</style>
