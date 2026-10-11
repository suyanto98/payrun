// Package periodguard: kontrak PeriodGuard (ADR-P07, PI-13/PI-14, `22-tx-concurrency.md` D1/D3/O12) untuk setiap
// penulis input payroll. Implementasinya ada di modul payroll (pemilik payroll_periods dan
// payroll_input_changes) dan dipasang composition root; modul penulis (employee, compensation,
// tenancy) hanya mengenal interface ini (ADR-001).
//
// Pemakaian di transaksi penulis, SEBELUM mengunci baris miliknya sendiri (urutan lock global [1b] →
// [2] → [6] → [7]):
//
//	record, err := guard.Lock(ctx, tx, periodguard.Change{Employees: ids, Range: r})  // [1b] [2]
//	... tulis data input ...                                                          // [6]
//	err = record(ctx)                                                                 // [7]
//
// Lock: tenant_settings FOR KEY SHARE, lalu periode yang beririsan FOR SHARE (urut year, month).
// Periode FINALIZED → 409 PERIOD_LOCKED (kecuali NotEffectiveDated). record menulis
// payroll_input_changes (changed_at = clock_timestamp(), setelah lock) untuk periode non-final.
package periodguard

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Range adalah rentang tanggal setengah terbuka [From, To); To nil = tanpa batas.
type Range struct {
	From time.Time
	To   *time.Time
}

// From membuat Range [from, ∞).
func From(from time.Time) Range { return Range{From: from} }

// Day membuat Range satu hari [d, d+1).
func Day(d time.Time) Range {
	to := d.AddDate(0, 0, 1)
	return Range{From: d, To: &to}
}

// Change menggambarkan input yang berubah. Tepat satu dari Employees / AllEmployees / ComponentID
// menentukan karyawan yang dicatat.
type Change struct {
	Employees    []uuid.UUID // karyawan tertentu (gaji, komponen, employment, PTKP)
	AllEmployees bool        // seluruh karyawan yang bekerja di periode (libur, komponen sistem)
	ComponentID  *uuid.UUID  // pemegang komponen di periode (definisi komponen tenant berubah)
	Range        Range       // tanggal yang NILAINYA berubah
	// NotEffectiveDated: nilai saat ini tanpa tanggal berlaku (PTKP, definisi komponen). Tidak menolak
	// periode FINALIZED (hasil final tetap dari snapshot); semua periode non-final dicatat.
	NotEffectiveDated bool
}

// Recorder menulis change log; dipanggil SETELAH data input ditulis, di transaksi yang sama.
type Recorder func(ctx context.Context) error

// Guard adalah PeriodGuard.
type Guard interface {
	Lock(ctx context.Context, tx pgx.Tx, c Change) (Recorder, error)
}
