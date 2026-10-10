//go:build !demo

// Package faults: titik injeksi kegagalan untuk DEMO (docs/design/27-docker-compose.md §9, O15/O18).
// Di titik bernama (contoh "payroll.calc_chunk.before_commit") worker demo bisa diminta mati seketika
// (os.Exit(137), seperti kill -9) supaya Demo B live deterministik ("crash di chunk ke-4"), bukan
// bergantung kecepatan tangan.
//
// Build ini (tanpa tag `demo`) adalah build RELEASE: Check kosong dan di-inline, tidak ada kode yang
// membaca tabel demo_faults, dan tidak ada cara mengaktifkannya lewat env (lapis guard "build").
package faults

import (
	"context"

	"github.com/google/uuid"

	"payrun/internal/platform/db"
)

// Titik injeksi yang dikenal (sama dengan ck_demo_faults_point di migrasi demo).
const (
	PointChunkBeforeCommit = "payroll.calc_chunk.before_commit"
	PointPayslipAfterPut   = "payslip.render.after_put"
)

// Check adalah titik injeksi bernama. Di build release tidak melakukan apa pun.
func Check(context.Context, db.DBTX, string, uuid.UUID) {}

// Enabled melaporkan apakah binary ini dibangun dengan tag `demo`.
func Enabled() bool { return false }
