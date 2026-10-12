package sub_test

import (
	"testing"

	"example.com/fx/hub/sub"
)

func TestOnlyMoved(t *testing.T) {
	if sub.Data() == "" {
		t.Fatal()
	}
}
