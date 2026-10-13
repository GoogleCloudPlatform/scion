package hub

import "testing"

func helperB() int { return B() }

func TestB(t *testing.T) {
	if helperA() != 1 {
		t.Fatal()
	}
}
