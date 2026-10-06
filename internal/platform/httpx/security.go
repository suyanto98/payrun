package httpx

import (
	"context"
	"net/http"
	"net/netip"
	"strings"

	"payrun/internal/platform/apperr"
)

// SecurityHeaders memasang header keamanan untuk respons API (C-10, docs/design/17-security-controls.md §2).
// Header yang berlaku untuk SPA (HSTS, CSP SPA, Permissions-Policy) dipasang Caddy di Sprint 7.
// Header dipasang SEBELUM handler berjalan, sehingga ikut ada di respons error dan 404.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'") // JSON tidak memuat resource apa pun
		h.Set("X-Content-Type-Options", "nosniff")                                     // jangan tebak MIME (JSON dirender sebagai HTML)
		h.Set("Referrer-Policy", "no-referrer")                                        // ID resource tidak bocor lewat Referer
		h.Set("X-Frame-Options", "DENY")                                               // cadangan frame-ancestors untuk browser lama
		h.Set("Cache-Control", "no-store")                                             // data finansial tidak di-cache (ADR-008)
		next.ServeHTTP(w, r)
	})
}

type clientIPKey struct{}

// ClientIP menentukan IP asli client. X-Forwarded-For HANYA dipercaya kalau koneksi datang dari
// proxy tepercaya (trusted, contoh Caddy). Tanpa aturan ini, penyerang cukup mengirim header
// X-Forwarded-For palsu untuk mendapat kuota rate limit baru di setiap request (T-SH-15).
//
// Dari XFF diambil alamat paling KANAN yang bukan proxy tepercaya: alamat di kiri ditulis oleh
// client sendiri (bisa dipalsukan), alamat di kanan ditambahkan proxy yang kita kendalikan.
func ClientIP(trusted []netip.Prefix) func(http.Handler) http.Handler {
	isTrusted := func(a netip.Addr) bool {
		for _, p := range trusted {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := remoteAddr(r)
			if ip.IsValid() && isTrusted(ip) {
				hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
				for i := len(hops) - 1; i >= 0; i-- {
					a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
					if err != nil {
						break // hop rusak: berhenti, pakai alamat terakhir yang valid
					}
					ip = a
					if !isTrusted(a) {
						break
					}
				}
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIPKey{}, ip)))
		})
	}
}

// ClientIPFrom mengembalikan IP client yang ditentukan middleware ClientIP (atau RemoteAddr).
func ClientIPFrom(r *http.Request) netip.Addr {
	if ip, ok := r.Context().Value(clientIPKey{}).(netip.Addr); ok {
		return ip
	}
	return remoteAddr(r)
}

func remoteAddr(r *http.Request) netip.Addr {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr().Unmap()
}

// CSRFGuard melindungi endpoint yang diautentikasi lewat COOKIE (refresh, logout; C-08).
// Lapisan:
//  1. cookie SameSite=Strict (dipasang saat login);
//  2. header kustom X-Requested-With: fetch. Form HTML lintas situs tidak bisa memasang header
//     kustom, dan fetch lintas situs dengan header kustom butuh preflight CORS yang kita tolak;
//  3. Origin (kalau dikirim browser) harus termasuk daftar origin yang diizinkan.
//
// Endpoint lain memakai Authorization: Bearer yang tidak dikirim otomatis oleh browser, jadi tidak
// rentan CSRF dan tidak perlu guard ini.
func CSRFGuard(allowedOrigins []string) func(http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range allowedOrigins {
		allowed[strings.TrimRight(o, "/")] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Requested-With") != "fetch" {
				WriteError(w, r, apperr.ErrForbidden.WithDetail("missing X-Requested-With: fetch"))
				return
			}
			if o := r.Header.Get("Origin"); o != "" && !allowed[o] {
				WriteError(w, r, apperr.ErrForbidden.WithDetail("origin not allowed"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
