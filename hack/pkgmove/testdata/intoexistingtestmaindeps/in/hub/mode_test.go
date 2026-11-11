package hub

import (
	"os"
	"testing"
)

func TestHarness(t *testing.T) {
	if os.Getenv("HUB_HARNESS") != "1" {
		t.Fatal("not run under the hub harness")
	}
}
