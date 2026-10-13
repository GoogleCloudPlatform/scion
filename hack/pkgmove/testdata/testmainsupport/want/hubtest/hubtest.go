// Package hubtest is the shared test harness of package hub.
package hubtest

import (
	"os"
	"testing"
)

// RunTestMain isolates HOME, runs the tests and cleans up.
func RunTestMain(m *testing.M) int {
	d, _ := os.MkdirTemp("", "isolated-home")
	os.Setenv("HOME", d)
	code := m.Run()
	os.RemoveAll(d)
	return code
}
