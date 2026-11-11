package hub

import "testing"

func TestCore(t *testing.T) {
	if onlyForTests() != "t" || Lookup("z") != 1 {
		t.Fatal("core")
	}
}

func TestSafe(t *testing.T) {
	if err := Safe(func() { panic("boom") }); err == nil {
		t.Fatal("panic not recovered")
	}
}
