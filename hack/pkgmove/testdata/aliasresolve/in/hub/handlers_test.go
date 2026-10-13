package hub

import (
	"bytes"
	"strings"
	"testing"
)

func TestHandle(t *testing.T) {
	var b bytes.Buffer
	if err := Handle(&b, ""); err != nil || b.String() != "not_found: empty" {
		t.Fatalf("got %q, %v", b.String(), err)
	}
	b.Reset()
	if err := Handle(&b, "x"); err != nil || b.String() != "custom: x" {
		t.Fatalf("got %q, %v", b.String(), err)
	}
	b.Reset()
	if err := HandleJSON(&b, "j"); err != nil || b.String() != `"j"` {
		t.Fatalf("got %q, %v", b.String(), err)
	}
	b.Reset()
	if err := Writer()(&b, codeNotFound, "v"); err != nil || b.String() != "not_found: v" {
		t.Fatalf("got %q, %v", b.String(), err)
	}
	b.Reset()
	if err := Typed(&b); err != nil || b.String() != "not_found: typed" {
		t.Fatalf("got %q, %v", b.String(), err)
	}
	b.Reset()
	if err := Shadowed(&b, "s"); err != nil || b.String() != "not_found: s" {
		t.Fatalf("got %q, %v", b.String(), err)
	}
}

func TestWhere(t *testing.T) {
	if got := Where(); !strings.HasPrefix(got, "handlers.go:") {
		t.Fatalf("Where() = %q, want the call site in handlers.go", got)
	}
}

func TestSafe(t *testing.T) {
	if err := Safe(func() { panic("boom") }); err == nil || err.Error() != "panic: boom" {
		t.Fatalf("Safe = %v", err)
	}
}
