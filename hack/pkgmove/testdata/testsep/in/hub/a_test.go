package hub

import "testing"

func helperA() int { return A() }

func TestA(t *testing.T) {
	if helperB() != 2 {
		t.Fatal()
	}
}
