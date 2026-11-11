package hub

import "testing"

func TestCheck(t *testing.T) {
	if !Check() {
		t.Fatal("box no longer satisfies the interface")
	}
}
