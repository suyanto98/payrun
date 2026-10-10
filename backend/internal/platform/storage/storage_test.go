package storage_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"payrun/internal/platform/storage"
)

// payslip_presigned_url_ttl (C-40): URL presigned SigV4 berlaku tepat 5 menit, memakai host publik,
// dan memaksa unduhan sebagai attachment. Presign = komputasi lokal (tanpa server).
func TestPayslipPresignedURLTTL(t *testing.T) {
	s, err := storage.NewS3(storage.Config{Endpoint: "http://storage:8333", PublicEndpoint: "https://files.example.test",
		Region: "us-east-1", Bucket: "payrun-private", AccessKey: "k", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.PresignGet(context.Background(), "tenants/t/payslips/r/e.pdf", storage.PresignTTL, "slip-2026-03.pdf")
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("X-Amz-Expires") != "300" || q.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" || q.Get("X-Amz-Signature") == "" {
		t.Fatalf("presign query: %v", q)
	}
	if u.Host != "files.example.test" || u.Scheme != "https" || u.Path != "/payrun-private/tenants/t/payslips/r/e.pdf" {
		t.Fatalf("presign host/path: %s", u)
	}
	if !strings.HasPrefix(q.Get("response-content-disposition"), "attachment;") {
		t.Fatalf("content disposition: %q", q.Get("response-content-disposition"))
	}
}

// TestS3_SeaweedFS: PUT + presigned GET ke storage nyata. Hanya bila S3_TEST_ENDPOINT diisi (contoh
// http://127.0.0.1:8333 dengan `make up` profil storage); tanpa itu dilewati.
func TestS3_SeaweedFS(t *testing.T) {
	ep := os.Getenv("S3_TEST_ENDPOINT")
	if ep == "" {
		t.Skip("S3_TEST_ENDPOINT not set")
	}
	s, err := storage.NewS3(storage.Config{Endpoint: ep, Region: "us-east-1", Bucket: "payrun-private",
		AccessKey: "payrun-dev-access", SecretKey: "payrun-dev-secret-only-for-local"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := "tenants/test/payslips/run/" + time.Now().Format("150405.000") + ".pdf" //nolint:forbidigo // key unik test
	if err := s.Put(ctx, key, []byte("%PDF-test"), "application/pdf"); err != nil {
		t.Fatal(err)
	}
	u, err := s.PresignGet(ctx, key, time.Minute, "x.pdf")
	if err != nil {
		t.Fatal(err)
	}
	r, err := http.Get(u.String()) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if r.StatusCode != http.StatusOK || string(b) != "%PDF-test" {
		t.Fatalf("presigned GET: %d %q", r.StatusCode, b)
	}
	anon, err := http.Get(ep + "/payrun-private/" + key) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	_ = anon.Body.Close()
	if anon.StatusCode != http.StatusForbidden {
		t.Fatalf("unsigned GET: want 403, got %d", anon.StatusCode)
	}
}
