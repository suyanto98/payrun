<script setup lang="ts">
import { ref } from 'vue'
import { api, ApiError } from '../api'
import AuthLayout from '../components/AuthLayout.vue'
import { useAuth } from '../stores/auth'

// A7. Link verifikasi: /verify-email?token=<raw>. Guard router memindahkan token ke memori lalu
// menghapusnya dari URL (pola yang sama dengan undangan), sehingga token tidak tertinggal di riwayat
// browser, Referer, atau log. Verifikasi butuh password akun (review S1: pemilik inbox saja tidak cukup
// untuk mengaktifkan akun yang didaftarkan orang lain). Berhasil → tenant PENDING_VERIFICATION aktif.
const auth = useAuth()
const password = ref('')
const show = ref(false)
const busy = ref(false)
const error = ref('')
const done = ref(false)

async function verify() {
  busy.value = true
  error.value = ''
  try {
    await api('/auth/verify-email', { method: 'POST', body: { token: auth.pendingVerify, password: password.value }, retryOnExpired: false })
    auth.pendingVerify = ''
    password.value = ''
    done.value = true
    if (auth.loggedIn) await auth.loadMe() // banner "belum verifikasi" hilang; tenant yang baru aktif bisa dipilih
  } catch (e) {
    if (e instanceof ApiError && e.code === 'EMAIL_VERIFICATION_INVALID') {
      auth.pendingVerify = ''
      error.value = 'Link verifikasi tidak valid, sudah dipakai, atau kedaluwarsa. Minta link baru dari halaman masuk.'
    } else if (e instanceof ApiError && e.code === 'INVALID_CREDENTIALS') {
      error.value = 'Password salah. Percobaan yang salah berulang kali akan mengunci akun sementara.'
    } else if (e instanceof ApiError && e.status === 429) {
      error.value = 'Terlalu banyak percobaan. Tunggu sebentar lalu coba lagi.'
    } else {
      error.value = e instanceof ApiError ? e.message : 'Gagal memverifikasi email'
    }
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <AuthLayout>
    <div class="form">
      <div>
        <h2>Verifikasi email</h2>
        <p class="lead">Konfirmasi bahwa email ini milikmu untuk mengaktifkan akun dan perusahaanmu.</p>
      </div>

      <template v-if="done">
        <p class="note" role="status">Email terverifikasi. Perusahaan yang kamu daftarkan sudah aktif.</p>
        <RouterLink v-if="auth.loggedIn" to="/select-tenant" class="btn block">Pilih perusahaan</RouterLink>
        <RouterLink v-else to="/login" class="btn block">Masuk</RouterLink>
      </template>

      <p v-else-if="!auth.pendingVerify && !error" class="alert error" role="alert">
        Link verifikasi tidak lengkap. Buka lagi link dari email, atau minta link baru setelah masuk.
      </p>

      <form v-else class="form" @submit.prevent="verify">
        <p v-if="error" class="alert error" role="alert">{{ error }}</p>
        <template v-if="auth.pendingVerify">
          <div class="field">
            <label for="password">Password akun</label>
            <div class="input-wrap">
              <input id="password" v-model="password" class="input" :type="show ? 'text' : 'password'" autocomplete="current-password" required maxlength="128" />
            </div>
            <label class="hint"><input v-model="show" type="checkbox" /> Tampilkan password</label>
          </div>
          <button type="submit" class="btn block" :disabled="busy || !password">Verifikasi</button>
        </template>
        <RouterLink v-else to="/login" class="btn block">Ke halaman masuk</RouterLink>
      </form>
    </div>
  </AuthLayout>
</template>
