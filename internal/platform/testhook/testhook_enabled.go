//go:build testhooks

package testhook

import (
	"context"
	"sync"
)

var (
	mu    sync.RWMutex
	hooks = map[string]func(ctx context.Context) error{}
)

// Pause menjalankan hook yang terpasang untuk name (kalau ada). Hook boleh memblok (menunggu
// sinyal dari test) atau mengembalikan error (mensimulasikan kegagalan di titik itu).
func Pause(ctx context.Context, name string) error {
	mu.RLock()
	fn := hooks[name]
	mu.RUnlock()
	if fn == nil {
		return nil
	}
	return fn(ctx)
}

// Set memasang hook untuk name dan mengembalikan fungsi untuk melepasnya (panggil di t.Cleanup).
func Set(name string, fn func(ctx context.Context) error) (reset func()) {
	mu.Lock()
	hooks[name] = fn
	mu.Unlock()
	return func() {
		mu.Lock()
		delete(hooks, name)
		mu.Unlock()
	}
}
