package sub

import (
	"os"
	"testing"
)

func TestParseGolden(t *testing.T) {
	b, err := os.ReadFile("testdata/golden.txt")
	if os.IsNotExist(err) {
		t.Skip("no golden file")
	}
	if Parse(b) != 6 {
		t.Fatal("bad")
	}
}

func TestModuleRoot(t *testing.T) {
	if _, err := os.Getwd(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("../go.mod"); err != nil {
		t.Skip("module root not found")
	}
}
