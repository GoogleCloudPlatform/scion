package hub_test

import (
	"testing"

	"example.com/fx/hub"
)

func TestOnlyMoved(t *testing.T) {
	if hub.Data() == "" {
		t.Fatal()
	}
}
