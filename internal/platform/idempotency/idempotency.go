// Package idempotency menerapkan ADR-L02 pola (a) untuk endpoint tenant (disalin dari LedgerPay,
// ditambah tenant_id): key idempotency ditulis di transaksi YANG SAMA dengan efek bisnisnya,
// sehingga "key tersimpan" ⇔ "efek terjadi". Tidak ada keadaan setengah jadi.
//
// Alur Execute (satu transaksi tenant):
//
//	INSERT key ON CONFLICT DO NOTHING ─┬─ baris baru → SAVEPOINT → fn(tx) ─┬─ sukses → RELEASE, simpan respons
//	                                   │                                   ├─ 422 domain → ROLLBACK TO SAVEPOINT,
//	                                   │                                   │   simpan problem 422 (tanpa efek)
//	                                   │                                   └─ lainnya → seluruh tx rollback (tidak disimpan)
//	                                   └─ konflik → baca key: kedaluwarsa → hapus, ulangi sebagai baru
//	                                                          hash beda → 422 IDEMPOTENCY_KEY_REUSED
//	                                                          hash sama → replay respons tersimpan
//
// Request duplikat yang datang bersamaan: INSERT kedua menunggu di unique index sampai transaksi
// pertama selesai. Commit → replay; rollback → INSERT kedua berhasil dan diproses sebagai yang
// pertama; menunggu > lock_timeout → 409 REQUEST_IN_PROGRESS.
//
// Scope key: (tenant, user, endpoint, key). User yang sama di dua tenant, atau dua user di tenant
// yang sama, tidak pernah berbagi key.
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"payrun/internal/platform/apperr"
	"payrun/internal/platform/clock"
	"payrun/internal/platform/db"
	"payrun/internal/platform/httpx"
	"payrun/internal/platform/idempotency/store"
	"payrun/internal/platform/idgen"
	"payrun/internal/platform/metrics"
	"payrun/internal/platform/tenantctx"
)

const (
	HeaderKey      = "Idempotency-Key"
	HeaderReplayed = "Idempotent-Replayed"

	// Retention: setelah 7 hari key dianggap baru (dan boleh dihapus job cleanup).
	Retention = 7 * 24 * time.Hour
)

// keyPattern: karakter dibatasi supaya key aman ditulis ke log (tanpa log injection).
var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_\-:.]{8,255}$`)

// KeyFrom membaca dan memvalidasi header Idempotency-Key.
func KeyFrom(r *http.Request) (string, error) {
	k := r.Header.Get(HeaderKey)
	if k == "" {
		return "", apperr.ErrIdempotencyKeyRequired
	}
	if !keyPattern.MatchString(k) {
		return "", apperr.ErrIdempotencyKeyInvalid
	}
	return k, nil
}

// ReadBody membaca body mentah (untuk hash) lalu memasangnya kembali supaya httpx.Decode tetap bisa
// dipakai. Batas ukuran tetap dijaga middleware BodyLimit.
func ReadBody(r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, apperr.ErrPayloadTooLarge
		}
		return nil, apperr.ErrMalformedJSON.WithCause(err)
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b, nil
}

// Hash = SHA-256(method, path, canonical JSON body). Canonical: key objek diurutkan dan whitespace
// dibuang. Angka disalin apa adanya (json.Number) supaya 1.0 dan 1 tidak diam-diam disamakan.
func Hash(method, path string, body []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, apperr.ErrMalformedJSON.WithCause(err)
	}
	canon, err := json.Marshal(v) // encoding/json mengurutkan key map
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write(canon)
	return h.Sum(nil), nil
}

// Request mengidentifikasi satu niat client.
type Request struct {
	ClientID uuid.UUID // user yang memanggil
	Endpoint string    // template rute, contoh "POST /employees"
	Key      string
	Hash     []byte
}

// FromHTTP membangun Request dari header + body (body dipasang kembali untuk Decode).
func FromHTTP(r *http.Request, clientID uuid.UUID, endpoint string) (Request, error) {
	key, err := KeyFrom(r)
	if err != nil {
		return Request{}, err
	}
	body, err := ReadBody(r)
	if err != nil {
		return Request{}, err
	}
	h, err := Hash(r.Method, endpoint, body)
	if err != nil {
		return Request{}, err
	}
	return Request{ClientID: clientID, Endpoint: endpoint, Key: key, Hash: h}, nil
}

// Outcome adalah hasil sukses fn yang akan disimpan dan dikirim.
type Outcome struct {
	Status       int
	Body         any
	Header       map[string]string // header tambahan (contoh ETag, Location); TIDAK ikut disimpan
	ResourceType string
	ResourceID   *uuid.UUID
}

// Response adalah apa yang dikirim ke client (eksekusi pertama atau replay).
type Response struct {
	Status   int
	Body     []byte
	Header   map[string]string
	Replayed bool
}

// Func adalah efek bisnis, berjalan di transaksi tenant (di bawah SAVEPOINT).
type Func func(ctx context.Context, tx pgx.Tx) (Outcome, error)

type Service struct {
	pool  db.Beginner
	ids   idgen.Generator
	clock clock.Clock
}

func NewService(pool db.Beginner, ids idgen.Generator, c clock.Clock) *Service {
	return &Service{pool: pool, ids: ids, clock: c}
}

// Execute menjalankan fn tepat sekali per (tenant, user, endpoint, key). Lihat komentar package.
func (s *Service) Execute(r *http.Request, req Request, fn Func) (Response, error) {
	ctx := r.Context()
	tid, ok := tenantctx.From(ctx)
	if !ok {
		return Response{}, db.ErrNoTenantContext
	}
	var resp Response
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		resp = Response{}
		q := store.New(tx)
		keyID, done, err := s.claim(ctx, q, tid, req, &resp)
		if err != nil || done {
			return err
		}

		// SAVEPOINT: efek bisnis bisa dibatalkan tanpa membatalkan baris key, supaya penolakan domain
		// (422) tetap bisa disimpan untuk replay.
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		out, ferr := fn(ctx, sp)
		if ferr != nil {
			_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT: efek hilang, baris key tetap
			e := apperr.As(ferr)
			if !storable(e) {
				return ferr // 400/404/409/5xx: tidak disimpan, seluruh transaksi rollback, retry aman
			}
			out = Outcome{Status: e.Status, Body: httpx.NewProblem(r, e)}
		} else if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
			return err
		}
		resp, err = complete(ctx, q, tid, keyID, out)
		return err
	})
	return resp, err
}

// claim menyisipkan key. done=true → resp sudah berisi replay.
func (s *Service) claim(ctx context.Context, q *store.Queries, tid uuid.UUID, req Request, resp *Response) (uuid.UUID, bool, error) {
	for attempt := 0; attempt < 2; attempt++ {
		keyID, err := q.InsertKey(ctx, store.InsertKeyParams{
			TenantID: tid, ID: s.ids.New(), ClientID: req.ClientID, Endpoint: req.Endpoint,
			Key: req.Key, RequestHash: req.Hash, ExpiresAt: s.clock.Now().Add(Retention),
		})
		switch {
		case err == nil:
			return keyID, false, nil
		case isLockTimeout(err):
			metrics.DBLockTimeouts.WithLabelValues("idempotency").Inc()
			return uuid.Nil, false, apperr.ErrRequestInProgress.WithCause(err)
		case !errors.Is(err, pgx.ErrNoRows):
			return uuid.Nil, false, db.MapError(err)
		}
		// Key sudah ada DAN sudah commit (INSERT tadi menunggu transaksi pemiliknya selesai).
		row, err := q.GetKey(ctx, store.GetKeyParams{TenantID: tid, ClientID: req.ClientID, Endpoint: req.Endpoint, Key: req.Key})
		if err != nil {
			return uuid.Nil, false, db.MapError(err)
		}
		if !s.clock.Now().Before(row.ExpiresAt) {
			if err := q.DeleteKey(ctx, store.DeleteKeyParams{TenantID: tid, ID: row.ID}); err != nil {
				return uuid.Nil, false, db.MapError(err)
			}
			continue // retensi lewat: diproses sebagai key baru
		}
		if !bytes.Equal(row.RequestHash, req.Hash) {
			metrics.IdemKeyReused.WithLabelValues(req.Endpoint).Inc()
			return uuid.Nil, false, apperr.ErrIdempotencyKeyReused
		}
		if row.ResponseStatus == nil {
			return uuid.Nil, false, apperr.ErrRequestInProgress
		}
		metrics.IdemReplays.WithLabelValues(req.Endpoint).Inc()
		*resp = Response{Status: int(*row.ResponseStatus), Body: row.ResponseBody, Replayed: true}
		return uuid.Nil, true, nil
	}
	return uuid.Nil, false, apperr.ErrRequestInProgress
}

// complete menulis respons ke baris key (di tx pemanggil) dan mengembalikan respons untuk client.
func complete(ctx context.Context, q *store.Queries, tid, keyID uuid.UUID, out Outcome) (Response, error) {
	body, err := json.Marshal(out.Body)
	if err != nil {
		return Response{}, err
	}
	status := int16(out.Status) //nolint:gosec // G115: status HTTP 100–599 muat di int16
	var rtype *string
	if out.ResourceType != "" {
		rtype = &out.ResourceType
	}
	if err := q.CompleteKey(ctx, store.CompleteKeyParams{
		TenantID: tid, ID: keyID, ResponseStatus: &status, ResponseBody: body, ResourceType: rtype, ResourceID: out.ResourceID,
	}); err != nil {
		return Response{}, db.MapError(err)
	}
	return Response{Status: out.Status, Body: body, Header: out.Header}, nil
}

// storable: penolakan domain (422) bersifat deterministik untuk key itu, jadi disimpan (ADR-L02).
func storable(e *apperr.Error) bool { return e.Status == http.StatusUnprocessableEntity }

func isLockTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "55P03"
}

// Write mengirim respons (eksekusi pertama atau replay). Replay diberi header Idempotent-Replayed.
func Write(w http.ResponseWriter, resp Response) {
	ct := "application/json; charset=utf-8"
	if resp.Status >= 400 {
		ct = httpx.ProblemContentType
	}
	for k, v := range resp.Header {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store")
	if resp.Replayed {
		w.Header().Set(HeaderReplayed, "true")
	}
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}
