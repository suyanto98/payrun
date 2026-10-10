package mail

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

// fakeSMTP adalah server SMTP minimal (tanpa STARTTLS/AUTH) yang merekam transaksi.
func fakeSMTP(t *testing.T) (addr string, got chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got = make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		r, w := bufio.NewReader(conn), conn
		reply := func(s string) { _, _ = w.Write([]byte(s + "\r\n")) }
		reply("220 fake")
		var tr strings.Builder
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			tr.WriteString(line)
			switch {
			case inData && line == ".\r\n":
				inData = false
				reply("250 queued")
			case inData:
			case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
				reply("250 fake")
			case strings.HasPrefix(line, "DATA"):
				inData = true
				reply("354 go")
			case strings.HasPrefix(line, "QUIT"):
				reply("221 bye")
				got <- tr.String()
				return
			default:
				reply("250 ok")
			}
		}
	}()
	return ln.Addr().String(), got
}

func TestSMTP_SendsMessage(t *testing.T) {
	addr, got := fakeSMTP(t)
	s := SMTP{Addr: addr, From: "PayRun <no-reply@payrun.test>"}
	if err := s.Send(context.Background(), Message{To: "dewi@alpha.test", Subject: "Verify", Text: "line1\nline2"}); err != nil {
		t.Fatal(err)
	}
	tr := <-got
	for _, want := range []string{"MAIL FROM:<no-reply@payrun.test>", "RCPT TO:<dewi@alpha.test>", "Subject: Verify\r\n", "line1\r\nline2"} {
		if !strings.Contains(tr, want) {
			t.Errorf("transcript missing %q:\n%s", want, tr)
		}
	}
}

func TestSMTP_RejectsHeaderInjection(t *testing.T) {
	s := SMTP{Addr: "127.0.0.1:1", From: "x@y"}
	err := s.Send(context.Background(), Message{To: "a@b\r\nBcc: victim@x", Subject: "s"})
	if !errors.Is(err, errHeaderInjection) {
		t.Fatalf("want header injection error before dialing, got %v", err)
	}
}
