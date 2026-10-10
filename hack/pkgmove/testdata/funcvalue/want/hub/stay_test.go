package hub

import "testing"

func TestIsDefault(t *testing.T) {
	if !IsDefault() || Run() != "default" {
		t.Fatal("hook identity changed")
	}
}
