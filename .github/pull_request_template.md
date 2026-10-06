## Apa & kenapa
<!-- Ringkas perubahan dan alasannya. Rujuk fitur (PF-xx), ADR, atau issue. -->

## Invariant / state machine yang tersentuh
<!-- PI-xx yang dipengaruhi (contoh PI-01 isolasi tenant, PI-04 run final immutable), dan bagaimana tetap terjaga. Tulis "tidak ada" jika tidak relevan. -->

## Checklist (Definition of Done fitur — docs/design/31-definition-of-done.md §3)
- [ ] Tabel tenant baru: ENABLE + FORCE RLS + policy di migrasi yang SAMA; composite FK `(tenant_id, id)`
- [ ] Akses DB lewat `WithTenantTx` (tenant dari claim `tid`, bukan path/header/body)
- [ ] OpenAPI diperbarui + contract test (jika endpoint berubah); baris baru di authorization matrix
- [ ] Cross-tenant test (ID tenant lain → 404) + authz matrix test
- [ ] Validasi + error code dari katalog (`docs/design/15-api-error-catalogue.md`), dimasukkan ke katalog DULU
- [ ] Transaction boundary + urutan lock sesuai `docs/design/22-tx-concurrency.md`; tidak ada network call di dalam tx
- [ ] Migrasi expand-only (contract di deploy terpisah)
- [ ] Unit + integration test; invariant checker lolos
- [ ] Log (dengan `tenant_id`, tanpa PII) / metrik ditambahkan bila perlu
- [ ] Dokumen/ADR diperbarui jika perilaku berubah

## Cara menguji
<!-- Perintah yang dijalankan reviewer. -->
