package crypto

import (
	"bytes"
	"testing"
)

func TestBlindIndex_ScopedAndDeterministic(t *testing.T) {
	b, err := ParseBlindIndexKey(key(7))
	if err != nil {
		t.Fatal(err)
	}
	a1 := b.Sum("tenant-a:employees.nik", "3174012304950001")
	if !bytes.Equal(a1, b.Sum("tenant-a:employees.nik", "3174012304950001")) {
		t.Fatal("same input must give the same index (uniqueness constraint relies on it)")
	}
	if bytes.Equal(a1, b.Sum("tenant-b:employees.nik", "3174012304950001")) {
		t.Fatal("the same NIK in another tenant must not be correlatable")
	}
	if bytes.Equal(a1, b.Sum("tenant-a:employees.npwp", "3174012304950001")) {
		t.Fatal("index must be bound to the field")
	}
	other, _ := ParseBlindIndexKey(key(8))
	if bytes.Equal(a1, other.Sum("tenant-a:employees.nik", "3174012304950001")) {
		t.Fatal("index must depend on the key")
	}
	if _, err := ParseBlindIndexKey("c2hvcnQ="); err == nil {
		t.Fatal("short key must be rejected")
	}
}
