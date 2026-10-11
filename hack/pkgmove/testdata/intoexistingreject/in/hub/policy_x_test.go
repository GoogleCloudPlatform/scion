package hub_test

import (
	"testing"

	"example.com/fx/hub"
)

func TestEvaluateExternal(t *testing.T) {
	if got := hub.Evaluate(hub.Deny("x"), "read"); got != "x: deny read" {
		t.Fatalf("Evaluate = %q", got)
	}
}
