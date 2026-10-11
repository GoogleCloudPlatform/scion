package sub

import (
	"os"
	"testing"

	"example.com/fx/apierr"
)

func TestDecide(t *testing.T) {
	p := mkPolicy(DefaultName)
	if !p.Decide(testAction) || describe(p) != "true" {
		t.Fatal("decide")
	}
	if NewPolicy("x", false).Decide(testAction) {
		t.Fatal("deny")
	}
	if MaxRules != 3 || apierr.CodeNotFound != "not_found" {
		t.Fatal("consts")
	}
	if got := Evaluate(p, testAction); got != "default: allow read" {
		t.Fatalf("Evaluate = %q", got)
	}
	if os.Getenv("HUB_HARNESS") != "1" {
		t.Fatal("not run under the hub harness")
	}
}
