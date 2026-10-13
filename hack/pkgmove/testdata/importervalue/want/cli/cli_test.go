package cli

import "testing"

func TestCheck(t *testing.T) {
	if !Check() {
		t.Fatal("hub.DefaultHook no longer identical to the default")
	}
}
