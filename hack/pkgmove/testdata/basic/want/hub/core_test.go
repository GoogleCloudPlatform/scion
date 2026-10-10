package hub

import "testing"

func TestCore(t *testing.T) {
	if onlyForTests() != "t" || Lookup("z") != 1 {
		t.Fatal("core")
	}
}
