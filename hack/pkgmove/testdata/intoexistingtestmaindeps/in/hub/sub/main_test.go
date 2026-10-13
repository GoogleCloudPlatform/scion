package sub

import (
	"os"
	"testing"
)

// setup has the same name as hub's, but does nothing.
func setup() {}

func TestMain(m *testing.M) { setup(); os.Exit(m.Run()) }
