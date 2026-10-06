package httpx

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestClientIP(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	cases := []struct {
		name, remote, xff, want string
	}{
		{"direct client ignores spoofed XFF", "203.0.113.9:5555", "1.2.3.4", "203.0.113.9"},
		{"via trusted proxy uses XFF", "10.0.0.2:443", "203.0.113.9", "203.0.113.9"},
		{"client-prepended junk is ignored: rightmost untrusted wins", "10.0.0.2:443", "1.2.3.4, 203.0.113.9", "203.0.113.9"},
		{"chain of trusted proxies", "10.0.0.2:443", "203.0.113.9, 10.0.0.5", "203.0.113.9"},
		{"trusted proxy without XFF", "10.0.0.2:443", "", "10.0.0.2"},
	}
	for _, c := range cases {
		var got netip.Addr
		h := ClientIP(trusted)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = ClientIPFrom(r) }))
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
		if got.String() != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}
