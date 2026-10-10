// Package storage: object storage S3-compatible (ADR-012). Hanya API S3 standar (PUT object, presigned
// GET SigV4): dev memakai SeaweedFS, produksi S3/R2 — tanpa API khusus vendor.
//
// Aturan (C-40, T-PR-09):
//   - bucket private; aplikasi hanya PUT dan GET object (tanpa list);
//   - URL presigned dibuat SETELAH otorisasi + audit, TTL 5 menit, tidak pernah di-log atau di-cache;
//   - unduhan memakai Content-Disposition: attachment.
package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// PresignTTL: masa berlaku URL unduhan payslip (PF-22 AC3, C-40).
const PresignTTL = 5 * time.Minute

// Store dipakai modul (payslip PDF). Implementasi: S3 (produksi/dev) dan Memory (test).
type Store interface {
	Put(ctx context.Context, key string, data []byte, contentType string) error
	// PresignGet: URL GET bertanda tangan untuk key, berlaku ttl; filename → Content-Disposition attachment.
	PresignGet(ctx context.Context, key string, ttl time.Duration, filename string) (*url.URL, error)
}

// Config dari env S3_* (docs/design/27-docker-compose.md §5).
type Config struct {
	Endpoint       string // URL internal, contoh http://storage:8333
	PublicEndpoint string // host di URL presigned (yang dijangkau browser); kosong = Endpoint
	Region         string
	Bucket         string
	AccessKey      string
	SecretKey      string
}

// S3: klien untuk PUT (endpoint internal) + klien terpisah untuk menandatangani URL dengan host publik.
// Presign murni komputasi lokal (region ditetapkan → tanpa panggilan jaringan).
type S3 struct {
	put, sign *minio.Client
	bucket    string
}

func NewS3(cfg Config) (*S3, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("storage: S3_ENDPOINT and S3_BUCKET are required")
	}
	if cfg.PublicEndpoint == "" {
		cfg.PublicEndpoint = cfg.Endpoint
	}
	mk := func(raw string) (*minio.Client, error) {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("storage: invalid endpoint %q", raw)
		}
		return minio.New(u.Host, &minio.Options{Creds: credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
			Secure: u.Scheme == "https", Region: cfg.Region, BucketLookup: minio.BucketLookupPath})
	}
	put, err := mk(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	sign, err := mk(cfg.PublicEndpoint)
	if err != nil {
		return nil, err
	}
	return &S3{put: put, sign: sign, bucket: cfg.Bucket}, nil
}

func (s *S3) Put(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := s.put.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{ContentType: contentType})
	return err
}

func (s *S3) PresignGet(ctx context.Context, key string, ttl time.Duration, filename string) (*url.URL, error) {
	params := url.Values{}
	if filename != "" {
		params.Set("response-content-disposition", fmt.Sprintf("attachment; filename=%q", filename))
	}
	return s.sign.PresignedGetObject(ctx, s.bucket, key, ttl, params)
}

// Memory: Store untuk test (tanpa jaringan). URL presigned berformat memory://bucket/key?expires=<detik>.
type Memory struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    int
	// FailPut: bila diisi, dipanggil sebelum setiap Put; error → Put gagal (simulasi storage down).
	FailPut func(key string) error
}

func NewMemory() *Memory { return &Memory{objects: map[string][]byte{}} }

func (m *Memory) Put(_ context.Context, key string, data []byte, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailPut != nil {
		if err := m.FailPut(key); err != nil {
			return err
		}
	}
	m.objects[key] = append([]byte(nil), data...)
	m.puts++
	return nil
}

func (m *Memory) PresignGet(_ context.Context, key string, ttl time.Duration, filename string) (*url.URL, error) {
	q := url.Values{"expires": {fmt.Sprint(int(ttl.Seconds()))}, "filename": {filename}}
	return &url.URL{Scheme: "memory", Host: "bucket", Path: "/" + key, RawQuery: q.Encode()}, nil
}

// Get / Puts: inspeksi test.
func (m *Memory) Get(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[key]
	return b, ok
}

func (m *Memory) Puts() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.puts
}
