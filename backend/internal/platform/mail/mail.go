// Package mail mengirim email transaksional (verifikasi email, undangan) lewat SMTP. Dev/demo: Mailpit
// menangkap semua email (final review R2-8). Sprint 1 mengirim langsung SETELAH transaksi commit, dengan
// endpoint "kirim ulang" sebagai pemulihan; Sprint 4 memindahkannya ke outbox job (ADR-006).
package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"

	"payrun/internal/platform/metrics"
)

// Message adalah email teks sederhana.
type Message struct {
	To      string
	Subject string
	Text    string
}

// Sender mengirim satu email. Implementasi test merekam pesan alih-alih mengirim.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// SMTP mengirim lewat server SMTP. STARTTLS dipakai kalau server menawarkannya; auth PLAIN hanya kalau
// Username diisi (net/smtp menolak PLAIN tanpa TLS kecuali ke localhost).
type SMTP struct {
	Addr     string // host:port
	From     string // "PayRun <no-reply@…>"
	Username string
	Password string
	Timeout  time.Duration // 0 → 10 detik
}

var errHeaderInjection = errors.New("mail: header value contains a line break")

func (s SMTP) Send(ctx context.Context, m Message) (err error) {
	start := metrics.Start()
	defer func() {
		outcome := "ok"
		if err != nil {
			outcome = "error"
		}
		metrics.Outbound.WithLabelValues("smtp", outcome).Inc()
		metrics.OutboundDuration.WithLabelValues("smtp").Observe(metrics.Since(start))
	}()
	// Nilai header berasal dari input user (email); CR/LF berarti header injection (Bcc tersembunyi dsb.).
	for _, v := range []string{m.To, m.Subject, s.From} {
		if strings.ContainsAny(v, "\r\n") {
			return errHeaderInjection
		}
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("mail: dial: %w", err)
	}
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline) // net/smtp tidak mengenal context: batasi lewat deadline koneksi
	host, _, _ := net.SplitHostPort(s.Addr)
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("mail: handshake: %w", err)
	}
	defer func() { _ = c.Close() }()
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("mail: starttls: %w", err)
		}
	}
	if s.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password, host)); err != nil {
			return fmt.Errorf("mail: auth: %w", err)
		}
	}
	if err := c.Mail(addrOnly(s.From)); err != nil {
		return fmt.Errorf("mail: MAIL FROM: %w", err)
	}
	if err := c.Rcpt(m.To); err != nil {
		return fmt.Errorf("mail: RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("mail: DATA: %w", err)
	}
	body := "From: " + s.From + "\r\nTo: " + m.To + "\r\nSubject: " + m.Subject +
		"\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n" +
		strings.ReplaceAll(m.Text, "\n", "\r\n")
	if _, err := w.Write([]byte(body)); err != nil {
		return fmt.Errorf("mail: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mail: end data: %w", err)
	}
	return c.Quit()
}

// addrOnly: "PayRun <a@b>" → "a@b" untuk MAIL FROM.
func addrOnly(from string) string {
	if i, j := strings.LastIndex(from, "<"), strings.LastIndex(from, ">"); i >= 0 && j > i {
		return from[i+1 : j]
	}
	return from
}
