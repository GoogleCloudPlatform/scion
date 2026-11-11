package sub_test

import (
	"testing"

	"example.com/fx/hub"
	"example.com/fx/hub/sub"
)

func TestData(t *testing.T) {
	if sub.Data() == "" || hub.Kept() != 1 {
		t.Fatal()
	}
}
