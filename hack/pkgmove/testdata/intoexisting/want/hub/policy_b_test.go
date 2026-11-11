package hub

import "testing"

// This wave stays, and still uses the shared helper.
func TestDecideB(t *testing.T) {
	if !mkPolicy("b").Decide(testAction) {
		t.Fatal("decide")
	}
}
