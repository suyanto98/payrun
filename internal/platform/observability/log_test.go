package observability

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func capture(fn func(l *slog.Logger)) string {
	var buf bytes.Buffer
	fn(NewLogger(&buf, "debug", "api", "test", "dev"))
	return buf.String()
}

// log_redaction_auth_body: kredensial yang tidak sengaja di-log tetap tertutup.
func TestLogRedaction_AuthBody(t *testing.T) {
	out := capture(func(l *slog.Logger) {
		l.Info("auth.debug", "email", "dewi@alpha.test", "password", "correct horse battery",
			slog.Group("body", "password", "hunter2hunter2", "refresh_token", "abc"))
	})
	for _, leak := range []string{"correct horse battery", "hunter2hunter2", `"abc"`} {
		if strings.Contains(out, leak) {
			t.Fatalf("secret %q leaked into the log: %s", leak, out)
		}
	}
	if !strings.Contains(out, "dewi@alpha.test") {
		t.Fatalf("non-sensitive fields must stay: %s", out)
	}
}

// log_redaction_headers: header Authorization/Cookie tertutup walaupun nama atributnya berbeda huruf.
func TestLogRedaction_Headers(t *testing.T) {
	out := capture(func(l *slog.Logger) {
		l.Warn("http.debug", "Authorization", "Bearer eyJ.secret", "Cookie", "pr_refresh=xyz")
	})
	if strings.Contains(out, "eyJ.secret") || strings.Contains(out, "xyz") {
		t.Fatalf("header leaked: %s", out)
	}
}

// log_redaction_pii_types: Sensitive[T] tertutup di log, fmt, dan JSON, tetapi bisa dibuka eksplisit.
func TestLogRedaction_PIITypes(t *testing.T) {
	nik := NewSensitive("3174010101900001")
	out := capture(func(l *slog.Logger) { l.Info("employee.debug", "employee_nik_value", nik) })
	for _, s := range []string{out, fmt.Sprint(nik), fmt.Sprintf("%v %+v %#v", nik, nik, nik)} {
		if strings.Contains(s, "3174010101900001") {
			t.Fatalf("NIK leaked: %s", s)
		}
	}
	if nik.Reveal() != "3174010101900001" {
		t.Fatal("Reveal must return the original value")
	}
}
