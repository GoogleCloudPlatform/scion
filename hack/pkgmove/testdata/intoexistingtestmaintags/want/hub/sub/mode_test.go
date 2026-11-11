package sub

import (
	"os"
	"testing"
)

func TestMode(t *testing.T) {
	if os.Getenv("HUB_MODE") == "" {
		t.Fatal("not run under a hub TestMain")
	}
}
