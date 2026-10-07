package config

import (
	"strings"
	"testing"
	"time"
)

// validDev adalah konfigurasi dev minimum yang lolos ValidateAPI.
func validDev() Config {
	return Config{
		AppEnv:             "dev",
		DatabaseURL:        "postgres://payrun_app@localhost/payrun",
		JWTSigningKeysJSON: `{"k1":"cGF5cnVuLWRldi1vbmx5LWp3dC1zaWduaW5nLWtleSE="}`,
		JWTActiveKID:       "k1",
		RefreshTTL:         168 * time.Hour,
		RefreshFamilyTTL:   720 * time.Hour,
		AllowedOrigins:     []string{"http://localhost:5173"},
		PublicBaseURL:      "http://localhost:5173",
		PIIKeyringJSON:     `{"v1":"cGF5cnVuLWRldi1vbmx5LXBpaS1rZXktMzItYnl0ZXM="}`,
		PIIActiveVersion:   "v1",
		SMTPAddr:           "localhost:1025",
		MailFrom:           "PayRun <no-reply@payrun.local>",
	}
}

func TestValidateAPI(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string // "" = harus lolos
	}{
		{"dev defaults pass", func(*Config) {}, ""},
		{"production with localhost origin rejected", func(c *Config) {
			c.AppEnv, c.CookieSecure, c.PublicBaseURL = "production", true, "https://payrun.example.com"
		}, "ALLOWED_ORIGINS"},
		{"production with localhost base url rejected", func(c *Config) {
			c.AppEnv, c.CookieSecure, c.AllowedOrigins = "production", true, []string{"https://payrun.example.com"}
		}, "PUBLIC_BASE_URL"},
		{"production with plain http rejected", func(c *Config) {
			c.AppEnv, c.CookieSecure = "production", true
			c.AllowedOrigins, c.PublicBaseURL = []string{"http://payrun.example.com"}, "https://payrun.example.com"
		}, "harus https"},
		{"production with real https urls passes", func(c *Config) {
			c.AppEnv, c.CookieSecure = "production", true
			c.AllowedOrigins, c.PublicBaseURL = []string{"https://payrun.example.com"}, "https://payrun.example.com"
		}, ""},
		{"origin with path rejected", func(c *Config) { c.AllowedOrigins = []string{"http://localhost:5173/app"} }, "scheme://host"},
		{"empty origins rejected", func(c *Config) { c.AllowedOrigins = nil }, "ALLOWED_ORIGINS wajib"},
		{"refresh ttl longer than family rejected", func(c *Config) { c.RefreshTTL = 800 * time.Hour }, "REFRESH_TTL"},
		{"smtp required", func(c *Config) { c.SMTPAddr = "" }, "SMTP_ADDR"},
		{"mail from header injection rejected", func(c *Config) { c.MailFrom = "a@b\r\nBcc: x@y" }, "MAIL_FROM"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validDev()
			tc.mutate(&c)
			err := c.ValidateAPI()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
