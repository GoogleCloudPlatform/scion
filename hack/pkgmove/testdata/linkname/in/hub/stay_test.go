package hub

import "testing"

func TestOff(t *testing.T) {
	if Off() != 8 {
		t.Fatal(Off())
	}
}
