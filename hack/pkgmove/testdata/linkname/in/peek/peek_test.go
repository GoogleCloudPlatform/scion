package peek

import (
	"testing"

	"example.com/fx/hub"
)

func TestPeek(t *testing.T) {
	hub.Bump()
	if Counter() != 1 {
		t.Fatal(Counter())
	}
}
