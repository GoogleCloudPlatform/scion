package hub_test

import (
	"testing"

	"example.com/fx/hub"
)

func TestData(t *testing.T) {
	if hub.Data() == "" || hub.Kept() != 1 {
		t.Fatal()
	}
}
