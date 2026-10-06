// Package idgen membuat ID entitas. UUIDv7 dipilih karena time-ordered (ramah index B-tree),
// tidak bisa ditebak berurutan, dan bisa dibuat sebelum INSERT (berguna untuk idempotency
// dan outbox). Lihat docs/design/07-db-shared-tables.md dan konvensi Step 4.
package idgen

import "github.com/google/uuid"

// Generator membuat ID baru.
type Generator interface {
	New() uuid.UUID
}

// V7 membuat UUIDv7.
type V7 struct{}

func (V7) New() uuid.UUID { return uuid.Must(uuid.NewV7()) }
