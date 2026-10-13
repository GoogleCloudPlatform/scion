package hub

import "testing"

// A local shadows the bare target name of an alias.
func TestShadowedBare(t *testing.T) {
	NewPolicy := "local"
	_ = NewPolicy
	_ = newPolicy("a", true)
}
