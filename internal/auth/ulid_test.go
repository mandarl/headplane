package auth

import (
	"strings"
	"testing"
	"time"
)

func TestNewULIDFormat(t *testing.T) {
	before := time.Now().UnixMilli()
	u := NewULID()
	after := time.Now().UnixMilli()

	if len(u) != 26 {
		t.Fatalf("ULID length = %d, want 26 (%q)", len(u), u)
	}
	for _, c := range u {
		if !strings.ContainsRune(crockford, c) {
			t.Fatalf("ULID %q contains non-Crockford char %q", u, c)
		}
	}

	// The first 10 chars encode the millisecond timestamp (48 bits, the
	// first char holding 3 bits); it must round-trip to roughly now.
	var ms uint64
	for i := 0; i < 10; i++ {
		bits := 5
		if i == 0 {
			bits = 3
		}
		ms = ms<<bits | uint64(strings.IndexByte(crockford, u[i]))
	}
	if int64(ms) < before || int64(ms) > after {
		t.Fatalf("ULID timestamp %d not within [%d, %d]", ms, before, after)
	}
}

func TestNewULIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		u := NewULID()
		if seen[u] {
			t.Fatalf("duplicate ULID %q", u)
		}
		seen[u] = true
	}
}
