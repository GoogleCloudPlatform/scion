package sub

import (
	"os"
	"testing"
)

// TestMain of the target does not use the hub harness.
func TestMain(m *testing.M) { os.Exit(m.Run()) }
