//go:build demo

package app

// "Simulasi gangguan" untuk pengunjung sandbox: versi tombol dari Demo B (crash di tengah payroll). Bedanya
// dengan Demo B presenter: aksinya `panic` (satu percobaan chunk gagal, worker tetap hidup), BUKAN `exit`.
// Di VM publik, exit mematikan proses worker bersama: perhitungan semua pengunjung ikut berhenti, dan siapa pun
// bisa mengulanginya. Panic dibatasi ke tenant pengunjung dan memakai jalur retry yang sama dengan crash
// sungguhan: transaksi chunk rollback, job dijadwal ulang, chunk dihitung lagi dari snapshot input.

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"payrun/internal/platform/apperr"
	"payrun/internal/platform/auth"
	"payrun/internal/platform/db"
	"payrun/internal/platform/faults"
	"payrun/internal/platform/httpx"
)

// crashAfterChunks: chunk yang KEDUA mencapai titik commit gagal sekali (bukan "chunk nomor 2": chunk berjalan
// paralel, urutan sampainya tidak pasti). Sandbox = 15 karyawan, chunk 10 (demo/prod) → 2 chunk: satu selesai
// normal, satu "mati" sebelum COMMIT lalu diulang.
const crashAfterChunks = 1

// sandboxForTenant: sandbox aktif pemanggil, dan token pemanggil harus ber-tenant sandbox itu.
func (s *sandboxer) sandboxForTenant(ctx context.Context, p auth.Principal) (sandboxRow, error) {
	sb, err := s.sandboxOf(ctx, p.UserID)
	if err != nil {
		return sb, err
	}
	if p.TenantID != sb.tenantID {
		return sb, apperr.ErrNotFound
	}
	return sb, nil
}

// armCrash: pasang satu gangguan untuk perhitungan berikutnya di tenant sandbox (berlaku 15 menit).
func (s *sandboxer) armCrash(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	sb, err := s.sandboxForTenant(r.Context(), p)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	err = db.WithTx(r.Context(), s.d.Pool, func(tx pgx.Tx) error {
		// Satu gangguan aktif per tenant: klik berulang tidak menumpuk crash.
		if _, err := tx.Exec(r.Context(), `DELETE FROM demo_faults WHERE tenant_id = $1 AND point = $2`, sb.tenantID, faults.PointChunkBeforeCommit); err != nil {
			return err
		}
		_, err := tx.Exec(r.Context(), `INSERT INTO demo_faults (id, point, target_kind, tenant_id, after_n, times, action, created_by)
			VALUES ($1, $2, 'payroll.calc_chunk', $3, $4, 1, 'panic', $5)`, s.d.IDs.New(), faults.PointChunkBeforeCommit, sb.tenantID, crashAfterChunks, p.UserID)
		return err
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"armed": true})
}

type timelineChunk struct {
	No         int    `json:"no"`
	Status     string `json:"status"` // queued | running | succeeded | dead
	Attempts   int    `json:"attempts"`
	FailedOnce bool   `json:"failed_once"` // ada percobaan yang gagal (pesan error mentah tidak dikirim ke publik)
	Simulated  bool   `json:"simulated"`   // kegagalan itu gangguan demo
}

// runTimeline: status setiap chunk perhitungan terbaru sebuah run, dari tabel jobs tenant sandbox.
func (s *sandboxer) runTimeline(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	sb, err := s.sandboxForTenant(r.Context(), p)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	runID, err := httpx.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var (
		version int
		chunks  = []timelineChunk{}
		armed   bool
	)
	err = db.WithTenantTxFor(r.Context(), s.d.Pool, sb.tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(r.Context(), `SELECT COALESCE(max((payload->>'calc_version')::int), 0) FROM jobs
			WHERE tenant_id = $1 AND kind = 'payroll.calc_chunk' AND payload->>'run_id' = $2`, sb.tenantID, runID.String()).Scan(&version); err != nil {
			return err
		}
		rows, err := tx.Query(r.Context(), `SELECT (payload->>'chunk_no')::int, status, attempts, COALESCE(last_error, '')
			FROM jobs WHERE tenant_id = $1 AND kind = 'payroll.calc_chunk' AND payload->>'run_id' = $2
			  AND (payload->>'calc_version')::int = $3
			ORDER BY (payload->>'chunk_no')::int`, sb.tenantID, runID.String(), version)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c timelineChunk
			var lastErr string
			if err := rows.Scan(&c.No, &c.Status, &c.Attempts, &lastErr); err != nil {
				return err
			}
			c.FailedOnce = lastErr != "" || (c.Status == "succeeded" && c.Attempts > 1)
			c.Simulated = strings.Contains(lastErr, "demo fault")
			chunks = append(chunks, c)
		}
		return rows.Err()
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, r, err)
		return
	}
	if err := s.d.Pool.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM demo_faults WHERE tenant_id = $1 AND point = $2
		AND expires_at > now() AND fired < after_n + times)`, sb.tenantID, faults.PointChunkBeforeCommit).Scan(&armed); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"run_id": runID, "calc_version": version, "chunks": chunks, "crash_armed": armed})
}

// verifyRun (demo): "Cek ulang hasil" = payroll.VerifyRun (PI-12): setiap item versi terkini dihitung ulang dari
// snapshot input + ruleset yang tersimpan dan harus identik. Di produk ini perintah ops (`payrun verify-run`), bukan
// endpoint API; di sandbox aman dibuka karena 15 karyawan, tenant pengunjung sendiri, dan dibatasi per user.
func (s *sandboxer) verifyRun(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	sb, err := s.sandboxForTenant(r.Context(), p)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	runID, err := httpx.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	rep, err := s.w.payroll.VerifyRun(r.Context(), sb.tenantID, runID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	// Ringkasan untuk publik: jumlah item dan jumlah selisih (alasan per item tidak dikirim).
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"run_id": runID, "status": rep.Status, "calc_version": rep.CalcVersion,
		"items_checked": rep.ItemsChecked, "mismatches": len(rep.Mismatches), "totals_ok": rep.Totals.OK, "ok": rep.OK()})
}
