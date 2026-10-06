//go:build !testhooks

// Package testhook menyediakan titik bernama di kode produksi tempat test concurrency bisa
// menyisipkan jeda atau error, untuk mengatur urutan dua transaksi secara deterministik
// (docs/design/19-tx-concurrency.md §7.1).
//
// Di build biasa (tanpa tag `testhooks`) Pause adalah fungsi kosong yang di-inline compiler:
// tidak ada biaya, dan tidak ada cara memasang hook (fungsi Set tidak ikut ter-compile).
package testhook

import "context"

// Pause adalah titik hook bernama. Di build produksi selalu mengembalikan nil.
func Pause(context.Context, string) error { return nil }
