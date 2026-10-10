package hub

import "testing"

func TestBuiltinCount(t *testing.T) {
	if BuiltinCount() != 1 || !registered {
		t.Fatalf("BuiltinCount() = %d, want 1", BuiltinCount())
	}
}
