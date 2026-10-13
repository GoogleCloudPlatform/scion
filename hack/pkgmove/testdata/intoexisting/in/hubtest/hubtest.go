// Package hubtest is the shared test harness of package hub.
package hubtest

import (
	"os"
	"testing"
)

// RunTestMain marks the harness as active, runs the tests and returns the
// exit code.
func RunTestMain(m *testing.M) int {
	os.Setenv("HUB_HARNESS", "1")
	return m.Run()
}
