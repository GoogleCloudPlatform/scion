package sub_test

import (
	"testing"

	"example.com/fx/hub/sub"
)

func TestEvaluateExternal(t *testing.T) {
	if got := sub.Evaluate(sub.Deny("x"), "read"); got != "x: deny read" {
		t.Fatalf("Evaluate = %q", got)
	}
}
