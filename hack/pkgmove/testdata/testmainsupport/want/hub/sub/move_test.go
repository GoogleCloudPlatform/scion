package sub

import (
	"strings"
	"testing"
)

// Relies on TestMain isolating HOME, without naming anything from main_test.go.
func TestHomeIsolated(t *testing.T) {
	if !strings.Contains(Home(), "isolated-home") {
		t.Fatalf("HOME not isolated: %s", Home())
	}
}
