//go:build !demo

package app

import (
	"time"

	"payrun/internal/platform/jobs"
)

// registerDemoRoutes: build release tidak punya endpoint demo sama sekali (lapis guard "build",
// docs/design/27-docker-compose.md §9.1). DEMO_MODE=true di binary ini tidak berpengaruh.
func registerDemoRoutes(demoWiring, Deps) {}

// demoMaintenanceKinds: build release tidak punya job demo (tidak ada tabel demo_sandboxes).
func demoMaintenanceKinds(Deps) ([]jobs.Kind, map[string]time.Duration) { return nil, nil }
