package sub

import "testing"

func TestCount(t *testing.T) {
	if NewWidget("a").Count() != computed {
		t.Fatal("count")
	}
}
