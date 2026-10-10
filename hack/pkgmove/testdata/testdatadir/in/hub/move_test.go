package hub

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
