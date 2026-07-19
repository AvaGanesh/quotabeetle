package ratelimit

import (
	"crypto/sha256"

	tb "github.com/tigerbeetle/tigerbeetle-go"
)

// DeriveAccountID deterministically maps an API key to a TigerBeetle account
// ID, so no separate key->ID mapping table needs to be persisted.
func DeriveAccountID(apiKey string) tb.Uint128 {
	sum := sha256.Sum256([]byte(apiKey))
	var b [16]byte
	copy(b[:], sum[:16])

	// TigerBeetle rejects account IDs that are zero or all-ones (int max);
	// nudge away from those two reserved values on the off chance a hash lands there.
	zero, allFF := true, true
	for _, x := range b {
		if x != 0 {
			zero = false
		}
		if x != 0xff {
			allFF = false
		}
	}
	if zero {
		b[15] = 0x01
	}
	if allFF {
		b[15] = 0xfe
	}

	return tb.BytesToUint128(b)
}
