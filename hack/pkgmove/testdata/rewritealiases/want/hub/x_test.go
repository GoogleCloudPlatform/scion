package hub_test

import (
	"bytes"
	"testing"

	"example.com/fx/apierr"
)

func TestWriteJSON(t *testing.T) {
	var b bytes.Buffer
	if err := apierr.WriteJSON(&b, "x"); err != nil || b.String() != `"x"` {
		t.Fatalf("got %q, %v", b.String(), err)
	}
}
