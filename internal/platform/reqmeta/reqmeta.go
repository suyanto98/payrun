// Package reqmeta: metadata request untuk audit dan daftar sesi (request id, IP client, user agent).
// Dipakai lintas modul tanpa membuat modul saling meng-import (ADR-001).
package reqmeta

import (
	"net/http"
	"net/netip"

	"payrun/internal/platform/httpx"
)

type Meta struct {
	RequestID string
	UserAgent string
	IP        *netip.Addr
}

// From mengambil metadata dari request (setelah middleware RequestID dan ClientIP).
func From(r *http.Request) Meta {
	m := Meta{RequestID: httpx.RequestIDFrom(r.Context()), UserAgent: r.UserAgent()}
	if ip := httpx.ClientIPFrom(r); ip.IsValid() {
		m.IP = &ip
	}
	return m
}
