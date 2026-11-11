package hub

import "testing"

func TestHook(t *testing.T) {
	if !IsDefault() || Run() != "rd" {
		t.Fatal("identity")
	}
	if HookName() != "example.com/fx/hub.defaultHook" {
		t.Fatal(HookName())
	}
}
