package hub

import (
	"bytes"
	"testing"

	"example.com/fx/apierr"
)

func TestUseAll(t *testing.T) {
	var b bytes.Buffer
	if err := useAll(&b); err != nil || b.String() != `"x"not_found: y` {
		t.Fatalf("got %q, %v", b.String(), err)
	}
	b.Reset()
	if err := errorWriter(&b, apierr.CodeNotFound, "v"); err != nil || b.String() != "not_found: v" {
		t.Fatalf("got %q, %v", b.String(), err)
	}
	if now(fixed(3)) != 3 {
		t.Fatal("now")
	}
}
