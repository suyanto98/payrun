// Package ratelimit: rate limiter token bucket in-memory per tier (C-07, docs/design/14-api-conventions.md §7).
//
// V1 in-memory per instance (ADR-007): cukup untuk satu VM. Dengan banyak instance, batas efektif
// = batas × jumlah instance; versi terdistribusi (Redis) dicatat sebagai future migration.
//
// Rate limit adalah PERLINDUNGAN (brute force, abuse, DoS ringan), bukan mekanisme correctness.
// Kebenaran uang tetap dijaga idempotency + lock.
package ratelimit

import (
	"math"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"payrun/internal/platform/apperr"
	"payrun/internal/platform/clock"
	"payrun/internal/platform/httpx"
	"payrun/internal/platform/metrics"
)

// Rule: Events per Per, dengan burst = Events (boleh menghabiskan kuota sekaligus, lalu mengisi ulang rata).
type Rule struct {
	Events int
	Per    time.Duration
}

// Tier adalah satu atau lebih aturan yang semuanya harus lolos (contoh auth-strict: 5/menit DAN 20/jam).
type Tier struct {
	Name  string
	Rules []Rule
}

// Tiers adalah konfigurasi semua tier yang dipakai API.
type Tiers struct {
	AuthStrict, AuthIP, Write, Read Tier
	// Heavy* dikunci per TENANT (bukan per user): melindungi worker bersama dari satu tenant
	// yang memicu kalkulasi/import/finalize berulang (konvensi API §7, PF-29).
	HeavyCalculate, HeavyImport, HeavyFinalize Tier
	// PIIView: full view PII (W6) per USER, di atas tier read: pengurasan PII massal oleh akun yang
	// sah tetap terbatas dan memicu alert (T-PR-10).
	PIIView Tier
}

// DefaultTiers: angka awal dari konvensi API §7, ditinjau ulang setelah load test (Sprint 9).
// Unlimited: semua tier dengan batas yang tidak pernah tercapai (HANYA perf env, lihat config).
func Unlimited() Tiers {
	t := DefaultTiers()
	for _, tier := range []*Tier{&t.AuthStrict, &t.AuthIP, &t.Write, &t.Read, &t.HeavyCalculate, &t.HeavyImport, &t.HeavyFinalize, &t.PIIView} {
		tier.Rules = []Rule{{1_000_000_000, time.Minute}}
	}
	return t
}

func DefaultTiers() Tiers {
	return Tiers{
		AuthStrict:     Tier{"auth-strict", []Rule{{5, time.Minute}, {20, time.Hour}}},
		AuthIP:         Tier{"auth-ip", []Rule{{30, time.Minute}}},
		Write:          Tier{"write", []Rule{{60, time.Minute}}},
		Read:           Tier{"read", []Rule{{300, time.Minute}}},
		HeavyCalculate: Tier{"heavy-calculate", []Rule{{10, time.Hour}}},
		HeavyImport:    Tier{"heavy-import", []Rule{{10, time.Hour}}},
		HeavyFinalize:  Tier{"heavy-finalize", []Rule{{5, time.Hour}}},
		PIIView:        Tier{"pii-view", []Rule{{30, time.Hour}}},
	}
}

type entry struct {
	limiters []*rate.Limiter
	lastSeen time.Time
}

// Limiter menyimpan bucket per (tier, key). Entri yang lama tidak dipakai dibersihkan supaya memori
// tidak tumbuh tanpa batas oleh key acak (contoh: email acak di login).
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*entry
	clock   clock.Clock
	lastGC  time.Time
}

func New(c clock.Clock) *Limiter {
	return &Limiter{buckets: map[string]*entry{}, clock: c}
}

const idleTTL = 2 * time.Hour // > periode aturan terpanjang (1 jam), jadi menghapus entri tidak "mereset" kuota yang masih berjalan

// Allow mengonsumsi satu token dari setiap aturan tier untuk key. Kalau ditolak, retryAfter adalah
// waktu tunggu terpendek sampai request berikutnya bisa lolos.
func (l *Limiter) Allow(t Tier, key string) (ok bool, retryAfter time.Duration) {
	now := l.clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gc(now)
	k := t.Name + "|" + key
	e := l.buckets[k]
	if e == nil {
		e = &entry{}
		for _, r := range t.Rules {
			e.limiters = append(e.limiters, rate.NewLimiter(rate.Every(r.Per/time.Duration(r.Events)), r.Events))
		}
		l.buckets[k] = e
	}
	e.lastSeen = now
	// Reservasi semua aturan dulu; kalau salah satu menolak, batalkan semuanya supaya request yang
	// ditolak tidak ikut menghabiskan kuota aturan lain.
	reservations := make([]*rate.Reservation, 0, len(e.limiters))
	for _, lim := range e.limiters {
		reservations = append(reservations, lim.ReserveN(now, 1))
	}
	var wait time.Duration
	for _, res := range reservations {
		if d := res.DelayFrom(now); d > wait {
			wait = d
		}
	}
	if wait == 0 {
		return true, 0
	}
	for _, res := range reservations {
		res.CancelAt(now)
	}
	return false, wait
}

func (l *Limiter) gc(now time.Time) {
	if now.Sub(l.lastGC) < time.Minute {
		return
	}
	l.lastGC = now
	for k, e := range l.buckets {
		if now.Sub(e.lastSeen) > idleTTL {
			delete(l.buckets, k)
		}
	}
}

// Reject menulis 429 RATE_LIMITED + Retry-After.
func Reject(w http.ResponseWriter, r *http.Request, tier string, retryAfter time.Duration) {
	metrics.RateLimited.WithLabelValues(tier).Inc()
	httpx.WriteError(w, r, apperr.ErrRateLimited.WithRetryAfter(int(math.Ceil(retryAfter.Seconds()))))
}

// Middleware membatasi per key yang dihitung dari request (contoh: IP client, user ID, tenant ID).
func (l *Limiter) Middleware(t Tier, keyFn func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ok, wait := l.Allow(t, keyFn(r)); !ok {
				Reject(w, r, t.Name, wait)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ByIP adalah key function berdasarkan IP client (setelah middleware httpx.ClientIP).
func ByIP(r *http.Request) string { return httpx.ClientIPFrom(r).String() }
