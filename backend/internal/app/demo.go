//go:build !demo

package app

import "github.com/go-chi/chi/v5"

// registerDemoRoutes: build release tidak punya endpoint demo sama sekali (lapis guard "build",
// docs/design/27-docker-compose.md §9.1). DEMO_MODE=true di binary ini tidak berpengaruh.
func registerDemoRoutes(chi.Router, Deps) {}
