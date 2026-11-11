package hub

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	d, _ := os.MkdirTemp("", "isolated-home")
	os.Setenv("HOME", d)
	code := m.Run()
	os.RemoveAll(d)
	os.Exit(code)
}
