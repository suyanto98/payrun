package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"payrun/internal/platform/apperr"
)

// password_hash_concurrency_bounded (C-45): 12 verifikasi paralel tidak pernah menjalankan lebih
// dari 3 hash sekaligus.
func TestHasher_ConcurrencyBounded(t *testing.T) {
	h := NewHasher(TestArgon2, 3, 5*time.Second)
	hash, err := h.Hash(context.Background(), "secret password")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, err := h.Verify(context.Background(), "secret password", hash); err != nil || !ok {
				t.Errorf("verify: ok=%v err=%v", ok, err)
			}
		}()
	}
	wg.Wait()
	if p := h.Peak(); p > 3 || p < 1 {
		t.Fatalf("peak concurrent hashes = %d, want 1..3", p)
	}
}

// Slot penuh lebih lama dari maxWait → 503 SERVICE_UNAVAILABLE (aman diulang), bukan antre tanpa batas.
func TestHasher_BusyReturnsUnavailable(t *testing.T) {
	h := NewHasher(TestArgon2, 1, 20*time.Millisecond)
	release, err := h.acquire(context.Background()) // tahan satu-satunya slot
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_, err = h.Verify(context.Background(), "x", DummyHash(TestArgon2))
	if !errors.Is(err, apperr.ErrUnavailable) {
		t.Fatalf("want SERVICE_UNAVAILABLE when all slots are busy, got %v", err)
	}
}
