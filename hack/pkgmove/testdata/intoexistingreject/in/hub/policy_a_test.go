package hub

import (
	"fmt"
	"os"
	"testing"

	"example.com/fx/hub/sub"
)

// describe duplicates the target's helper of the same name.
func describe(p *policy) string { return fmt.Sprint(p.Decide(testAction)) }

func TestDecide(t *testing.T) {
	p := mkPolicy(sub.DefaultName)
	if !p.Decide(testAction) || describe(p) != "true" {
		t.Fatal("decide")
	}
	if newPolicy("x", false).Decide(testAction) {
		t.Fatal("deny")
	}
	if maxRules != 3 || codeNotFound != "not_found" {
		t.Fatal("consts")
	}
	if got := Evaluate(p, testAction); got != "default: allow read" {
		t.Fatalf("Evaluate = %q", got)
	}
	if os.Getenv("HUB_HARNESS") != "1" {
		t.Fatal("not run under the hub harness")
	}
}
