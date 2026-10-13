package hub

import "testing"

func TestCount(t *testing.T) {
	if newWidget("a").count() != computed {
		t.Fatal("count")
	}
}
