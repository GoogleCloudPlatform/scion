package hub

import (
	"fmt"
	"testing"
)

// TestFormat pins the %+v output, which shows the embedded field's name.
func TestFormat(t *testing.T) {
	got := fmt.Sprintf("%+v", Outer{Inner: inner{Value: 1}, Name: "n"})
	if got != "{inner:{Value:1} Name:n}" {
		t.Fatalf("got %s", got)
	}
}
