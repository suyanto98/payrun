// Sorotan tur demo: tautan tur membawa #penanda di URL; elemen [data-spot="penanda"] digulir ke tengah layar dan
// diberi cincin berdenyut beberapa detik, supaya pengunjung langsung tahu harus menekan apa. Elemen sering baru
// muncul setelah data dimuat, jadi dicari ulang setiap 150 ms, maksimal 5 detik; tidak ketemu = diam saja.
export function spotlight(name: string) {
  const started = Date.now()
  const tick = () => {
    const el = document.querySelector<HTMLElement>(`[data-spot="${CSS.escape(name)}"]`)
    if (el && el.getClientRects().length > 0) {
      document.querySelectorAll('.spot').forEach((o) => o.classList.remove('spot')) // satu sorotan pada satu waktu
      const reduce = window.matchMedia('(prefers-reduced-motion: reduce)').matches
      el.scrollIntoView({ behavior: reduce ? 'auto' : 'smooth', block: 'center' })
      el.classList.remove('spot')
      void el.offsetWidth // mulai ulang animasi kalau penanda yang sama diklik lagi
      el.classList.add('spot')
      window.setTimeout(() => el.classList.remove('spot'), 4500)
      return
    }
    if (Date.now() - started < 5000) window.setTimeout(tick, 150)
  }
  tick()
}
