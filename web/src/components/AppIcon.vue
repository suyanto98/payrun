<script setup lang="ts">
import { computed } from 'vue'

// Ikon garis (stroke) sederhana; warna mengikuti currentColor.
const props = withDefaults(defineProps<{ name: keyof typeof icons; size?: number; stroke?: number }>(), { size: 20, stroke: 1.9 })

const icons = {
  logo: ['M4 4v16h16', 'M8 14l4-4 3 3 5-5'],
  home: ['M3 11 12 4l9 7v9a1 1 0 0 1-1 1h-5v-6H9v6H4a1 1 0 0 1-1-1z'],
  transfer: ['M7 17 17 7', 'M8 7h9v9'],
  plus: ['M12 5v14', 'M5 12h14'],
  user: ['M12 4a4 4 0 1 0 0 8 4 4 0 0 0 0-8z', 'M4 21a8 8 0 0 1 16 0'],
  back: ['M15 18l-6-6 6-6'],
  chevron: ['M9 18l6-6-6-6'],
  bank: ['M3 10h18', 'M5 10v10', 'M19 10v10', 'M9 10v10', 'M15 10v10', 'M3 20h18', 'M12 3l9 5H3z'],
  list: ['M8 6h13', 'M8 12h13', 'M8 18h13', 'M3 6h.01', 'M3 12h.01', 'M3 18h.01'],
  shield: ['M12 3l8 3v6c0 5-3.5 8-8 9-4.5-1-8-4-8-9V6z'],
  logout: ['M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4', 'M16 17l5-5-5-5', 'M21 12H9'],
  eye: ['M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7S2 12 2 12z', 'M12 9a3 3 0 1 0 0 6 3 3 0 0 0 0-6z'],
  eyeOff: ['M3 3l18 18', 'M10.6 5.1A10 10 0 0 1 12 5c6.5 0 10 7 10 7a17 17 0 0 1-3 3.9', 'M6.6 6.6A17 17 0 0 0 2 12s3.5 7 10 7a9.7 9.7 0 0 0 5.4-1.6', 'M9.9 9.9a3 3 0 0 0 4.2 4.2'],
  check: ['M5 12.5l4.5 4.5L19 7.5'],
  x: ['M6 6l12 12', 'M18 6 6 18'],
  info: ['M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18z', 'M12 8h.01', 'M11 12h1v4h1'],
  clock: ['M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18z', 'M12 7v5l3 2'],
  lock: ['M7 11h10a2 2 0 0 1 2 2v6a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2v-6a2 2 0 0 1 2-2z', 'M8 11V8a4 4 0 0 1 8 0v3'],
  refresh: ['M21 12a9 9 0 1 1-3-6.7L21 8', 'M21 3v5h-5'],
  chart: ['M4 4v16h16', 'M8 14l4-4 3 3 5-5'],
  arrowIn: ['M17 7 7 17', 'M16 17H7V8'],
  cart: ['M3 4h2l2.4 11.2a2 2 0 0 0 2 1.6h7.7a2 2 0 0 0 2-1.5L21 8H6', 'M10 21h.01', 'M17 21h.01'],
  undo: ['M9 14 4 9l5-5', 'M4 9h11a5 5 0 0 1 0 10h-3'],
  download: ['M12 3v12', 'M7 10l5 5 5-5', 'M5 21h14'],
  users: ['M9 4a4 4 0 1 0 0 8 4 4 0 0 0 0-8z', 'M2 21a7 7 0 0 1 14 0', 'M16 4.1a4 4 0 0 1 0 7.8', 'M22 21a7 7 0 0 0-4-6.3'],
  building: ['M4 21V5a2 2 0 0 1 2-2h8a2 2 0 0 1 2 2v16', 'M16 9h2a2 2 0 0 1 2 2v10', 'M3 21h18', 'M8 7h4', 'M8 11h4', 'M8 15h4'],
  search: ['M11 4a7 7 0 1 0 0 14 7 7 0 0 0 0-14z', 'M21 21l-4.3-4.3'],
} as const

const paths = computed(() => icons[props.name])
</script>

<template>
  <svg :width="size" :height="size" viewBox="0 0 24 24" fill="none" stroke="currentColor" :stroke-width="stroke" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <path v-for="d in paths" :key="d" :d="d" />
  </svg>
</template>
