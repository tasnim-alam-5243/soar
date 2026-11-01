package domain

import (
	"testing"
	"time"
)

func TestIDs(t *testing.T) {
	a := NewID(PrefixExecution)
	if !HasPrefix(a, PrefixExecution) || HasPrefix(a, PrefixCase) {
		t.Fatalf("bad id %q", a)
	}
	// ULIDs sort by time.
	t1 := NewULID(time.UnixMilli(1_700_000_000_000))
	t2 := NewULID(time.UnixMilli(1_700_000_000_001))
	if !(t1 < t2) {
		t.Fatalf("ULIDs must sort by time: %s !< %s", t1, t2)
	}
	seen := map[string]bool{}
	for i := 0; i < 10000; i++ {
		id := NewID(PrefixAlert)
		if seen[id] {
			t.Fatal("duplicate id")
		}
		seen[id] = true
	}
}
