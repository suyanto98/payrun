package db

import "testing"

func TestQueryName(t *testing.T) {
	for stmt, want := range map[string]string{
		"-- name: LockWalletAccounts :many\nSELECT id FROM accounts": "LockWalletAccounts",
		"  -- name:GetTopup :one\nSELECT 1":                          "GetTopup",
		"begin":                                                      "BEGIN",
		"COMMIT":                                                     "COMMIT",
		"SELECT balance FROM accounts WHERE id = $1":                 "SELECT",
	} {
		if got := QueryName(stmt); got != want {
			t.Errorf("QueryName(%q) = %q, want %q", stmt, got, want)
		}
	}
}
