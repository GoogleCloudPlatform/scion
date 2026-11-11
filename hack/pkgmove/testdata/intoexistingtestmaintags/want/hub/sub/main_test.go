//go:build !integration

package sub

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) { os.Setenv("HUB_MODE", "unit"); os.Exit(m.Run()) }
