package metrics

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"time"
)

// OutboundTransport membungkus RoundTripper untuk menghitung panggilan keluar per target
// (contoh object_storage) dan outcome.
func OutboundTransport(target string, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return roundTrip(func(r *http.Request) (*http.Response, error) {
		start := time.Now() //nolint:forbidigo // durasi teknis
		resp, err := base.RoundTrip(r)
		OutboundDuration.WithLabelValues(target).Observe(Since(start))
		Outbound.WithLabelValues(target, outcome(resp, err)).Inc()
		return resp, err
	})
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func outcome(resp *http.Response, err error) string {
	if err != nil {
		var ne net.Error
		switch {
		case strings.Contains(err.Error(), "not allowed"): // egress guard
			return "blocked"
		case errors.As(err, &ne) && ne.Timeout():
			return "timeout"
		default:
			return "conn_error"
		}
	}
	switch {
	case resp.StatusCode >= 500:
		return "http_5xx"
	case resp.StatusCode >= 400:
		return "http_4xx"
	}
	return "ok"
}
