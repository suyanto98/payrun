// Package clock memisahkan "waktu sekarang" dari kode bisnis supaya bisa dipalsukan di test
// (contoh: token kedaluwarsa tanpa benar-benar menunggu 15 menit). Waktu bisnis selalu dari
// server, tidak pernah dari client (C-31).
package clock

import (
	"sync"
	"time"
)

// Clock mengembalikan waktu sekarang dalam UTC.
type Clock interface {
	Now() time.Time
}

// System memakai jam sistem.
type System struct{}

func (System) Now() time.Time { return time.Now().UTC() }

// Fake adalah jam yang bisa diatur manual. Aman dipakai bersamaan dari beberapa goroutine.
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

func NewFake(t time.Time) *Fake { return &Fake{now: t.UTC()} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance memajukan jam.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}
