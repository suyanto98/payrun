<script setup lang="ts">
import { errorText } from '../errors'
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { api, ApiError } from '../api'
import AuthLayout from '../components/AuthLayout.vue'
import { useAuth } from '../stores/auth'

// U3. Link undangan: /invite#<tenant_id>.<secret>. Fragment tidak pernah dikirim browser ke server;
// guard router memindahkannya ke memori lalu menghapusnya dari URL (redirect tanpa hash), sehingga
// rahasia tidak masuk riwayat browser, query string, maupun log.
const auth = useAuth()
const router = useRouter()
const error = ref('')
const busy = ref(false)

async function accept() {
  busy.value = true
  error.value = ''
  try {
    const res = await api<{ membership: { tenant_id: string } }>('/invitations/accept', { method: 'POST', body: { token: auth.pendingInvite } })
    auth.pendingInvite = ''
    await auth.switchTenant(res.membership.tenant_id)
    router.push('/')
  } catch (e) {
    if (e instanceof ApiError && e.code === 'INVITATION_INVALID') error.value = 'Undangan tidak valid, sudah dipakai, kedaluwarsa, atau ditujukan ke email lain.'
    else if (e instanceof ApiError && e.status === 403) error.value = 'Verifikasi email akunmu dulu, lalu buka lagi link undangan ini.'
    else if (e instanceof ApiError && e.code === 'MEMBERSHIP_EXISTS') error.value = 'Kamu sudah menjadi anggota perusahaan ini.'
    else error.value = errorText(e, 'Terjadi kesalahan. Coba lagi.')
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <AuthLayout>
    <div class="form">
      <div>
        <h2>Undangan bergabung</h2>
        <p class="lead">Kamu diundang bergabung ke sebuah perusahaan di PayRun.</p>
      </div>
      <p v-if="!auth.pendingInvite" class="alert error" role="alert">Link undangan tidak lengkap. Buka lagi link dari pengundang.</p>
      <template v-else-if="!auth.loggedIn">
        <p class="muted">Masuk dengan akun yang emailnya sama dengan email undangan.</p>
        <RouterLink to="/login" class="btn block">Masuk untuk menerima</RouterLink>
      </template>
      <template v-else>
        <p class="muted">Masuk sebagai <strong>{{ auth.email }}</strong>.</p>
        <p v-if="error" class="alert error" role="alert">{{ error }}</p>
        <button type="button" class="btn block" :disabled="busy" @click="accept">Terima undangan</button>
      </template>
    </div>
  </AuthLayout>
</template>
