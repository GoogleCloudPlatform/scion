package hub

import "testing"

func TestLimit(t *testing.T) {
	if limit() == 0 {
		t.Fatal("limit")
	}
}
