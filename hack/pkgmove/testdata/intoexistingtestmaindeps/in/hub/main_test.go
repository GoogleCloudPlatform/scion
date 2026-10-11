package hub

import (
	"os"
	"testing"
)

func setup() { os.Setenv("HUB_HARNESS", "1") }

func TestMain(m *testing.M) { setup(); os.Exit(m.Run()) }
