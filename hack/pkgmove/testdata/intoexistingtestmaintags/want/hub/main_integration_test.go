//go:build integration

package hub

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) { os.Setenv("HUB_MODE", "integration"); os.Exit(m.Run()) }
