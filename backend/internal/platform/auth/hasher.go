package auth

import (
	"context"
	"sync/atomic"
	"time"

	"payrun/internal/platform/apperr"
)

// Hasher membatasi berapa hash argon2id yang boleh berjalan bersamaan (C-45). Setiap hash memakai
// ±64 MiB; tanpa batas, 100 login paralel = 6,4 GiB RAM → proses di-OOM-kill. Login yang tidak
// mendapat slot dalam MaxWait ditolak 503 (aman diulang), bukan mengantre tanpa batas.
type Hasher struct {
	params  Argon2Params
	sem     chan struct{}
	maxWait time.Duration

	inFlight atomic.Int64
	peak     atomic.Int64
}

// NewHasher: concurrency = jumlah hash paralel maksimum (desain: 4 untuk VM 2 vCPU / 4 GiB).
func NewHasher(p Argon2Params, concurrency int, maxWait time.Duration) *Hasher {
	return &Hasher{params: p, sem: make(chan struct{}, concurrency), maxWait: maxWait}
}

func (h *Hasher) acquire(ctx context.Context) (release func(), err error) {
	ctx, cancel := context.WithTimeout(ctx, h.maxWait)
	defer cancel()
	select {
	case h.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, apperr.ErrUnavailable.WithDetail("password hashing busy, retry shortly")
	}
	n := h.inFlight.Add(1)
	for { // catat puncak (dipakai test dan nanti metrik)
		p := h.peak.Load()
		if n <= p || h.peak.CompareAndSwap(p, n) {
			break
		}
	}
	return func() {
		h.inFlight.Add(-1)
		<-h.sem
	}, nil
}

// Hash membuat hash password baru (register).
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	release, err := h.acquire(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	return HashPassword(password, h.params)
}

// Verify memeriksa password terhadap hash (login), dengan batas konkurensi yang sama.
func (h *Hasher) Verify(ctx context.Context, password, encoded string) (bool, error) {
	release, err := h.acquire(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	return VerifyPassword(password, encoded)
}

// Params mengembalikan parameter argon2 (untuk dummy hash).
func (h *Hasher) Params() Argon2Params { return h.params }

// Peak adalah jumlah hash paralel tertinggi yang pernah terjadi.
func (h *Hasher) Peak() int64 { return h.peak.Load() }
