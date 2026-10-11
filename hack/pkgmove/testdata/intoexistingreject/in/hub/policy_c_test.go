package hub

import (
	"testing"

	"example.com/fx/consumer"
)

// TestCycle imports a package that imports the target, directly and
// through an alias.
func TestCycle(t *testing.T) {
	_ = view{}
	if consumer.Name == "" {
		t.Fatal("name")
	}
}
