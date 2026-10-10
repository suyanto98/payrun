//go:build !demo

// Package migrationsdemo: di build release tidak ada migrasi demo (lihat demo.go).
package migrationsdemo

import "context"

// MigrateUp tidak melakukan apa pun di build release.
func MigrateUp(context.Context, string) error { return nil }
