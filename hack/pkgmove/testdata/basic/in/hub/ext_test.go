package hub_test

import (
	"testing"

	"example.com/fx/hub"
)

func TestExt(t *testing.T) {
	if hub.Exported() == nil {
		t.Fatal("nil")
	}
}
