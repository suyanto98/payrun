package ratelimit

import (
	"testing"
	"time"

	"payrun/internal/platform/clock"
)

func TestAllow_BurstThenRefill(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
	l := New(clk)
	tier := Tier{"t", []Rule{{5, time.Minute}}}
	for i := range 5 {
		if ok, _ := l.Allow(tier, "k"); !ok {
			t.Fatalf("request %d must pass", i+1)
		}
	}
	ok, wait := l.Allow(tier, "k")
	if ok || wait <= 0 || wait > 12*time.Second {
		t.Fatalf("6th must be rejected with a wait ≤ 12s (1 token / 12s), got ok=%v wait=%v", ok, wait)
	}
	if ok, _ := l.Allow(tier, "other"); !ok {
		t.Fatal("keys are independent")
	}
	clk.Advance(12 * time.Second)
	if ok, _ := l.Allow(tier, "k"); !ok {
		t.Fatal("one token must refill after 12s")
	}
}

// Request yang ditolak oleh satu aturan tidak boleh menghabiskan kuota aturan lain.
func TestAllow_MultiRuleRejectDoesNotConsume(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
	l := New(clk)
	tier := Tier{"strict", []Rule{{2, time.Minute}, {3, time.Hour}}}
	l.Allow(tier, "k")
	l.Allow(tier, "k")
	for range 10 { // ditolak aturan per-menit; kuota per-jam (sisa 1) tidak boleh ikut habis
		if ok, _ := l.Allow(tier, "k"); ok {
			t.Fatal("per-minute rule must reject")
		}
	}
	clk.Advance(time.Minute)
	if ok, _ := l.Allow(tier, "k"); !ok {
		t.Fatal("hourly quota must still have 1 token left")
	}
	if ok, _ := l.Allow(tier, "k"); ok {
		t.Fatal("hourly quota (3) must now be exhausted")
	}
}
