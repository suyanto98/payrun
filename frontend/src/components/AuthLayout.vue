<script setup lang="ts">
import AppIcon from './AppIcon.vue'
</script>

<template>
  <div class="auth">
    <section class="auth-hero">
      <span class="brand"><span class="brand-mark"><AppIcon name="logo" :stroke="2.2" /></span>PayRun</span>
      <div class="auth-hero-body">
        <h1>Gaji tepat waktu, data tiap perusahaan tetap terpisah.</h1>
        <ul class="perks">
          <li>
            <span class="perk-icon"><AppIcon name="shield" :size="18" :stroke="2" /></span>
            <div><strong>Terisolasi per perusahaan</strong><span>Setiap query berjalan di bawah row-level security; data perusahaan lain tidak pernah terlihat.</span></div>
          </li>
          <li>
            <span class="perk-icon"><AppIcon name="lock" :size="18" :stroke="2" /></span>
            <div><strong>Data pribadi terenkripsi</strong><span>NIK, NPWP, dan nomor rekening disimpan terenkripsi dan tampil ter-mask.</span></div>
          </li>
          <li>
            <span class="perk-icon"><AppIcon name="check" :size="18" :stroke="2" /></span>
            <div><strong>Setiap perubahan tercatat</strong><span>Siapa mengubah apa dan kapan tersimpan di audit log.</span></div>
          </li>
        </ul>
      </div>
    </section>
    <section class="auth-main">
      <div class="auth-form"><slot /></div>
    </section>
  </div>
</template>
