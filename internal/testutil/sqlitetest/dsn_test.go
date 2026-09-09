package sqlitetest

import "testing"

func TestMemoryDSNIsUnique(t *testing.T) {
	first := MemoryDSN()
	second := MemoryDSN()
	if first == second {
		t.Fatalf("expected isolated SQLite DSNs, got %q twice", first)
	}
}
