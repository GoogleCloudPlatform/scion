package hub

import "testing"

func TestOverride(t *testing.T) {
	old := callerAt
	defer func() { callerAt = old }()
	callerAt = func() string { return "fake" }
	if callerAt() != "fake" {
		t.Fatal("override")
	}
}
