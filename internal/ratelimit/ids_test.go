package ratelimit

import (
	"testing"

	tb "github.com/tigerbeetle/tigerbeetle-go"
)

func TestDeriveAccountID(t *testing.T) {
	a := DeriveAccountID("key-a")
	b := DeriveAccountID("key-b")
	aAgain := DeriveAccountID("key-a")

	if a != aAgain {
		t.Fatalf("DeriveAccountID must be deterministic: got %v and %v for the same key", a, aAgain)
	}
	if a == b {
		t.Fatalf("DeriveAccountID must differ for different keys")
	}
	if a == tb.ToUint128(0) {
		t.Fatalf("DeriveAccountID must never return zero")
	}
}
