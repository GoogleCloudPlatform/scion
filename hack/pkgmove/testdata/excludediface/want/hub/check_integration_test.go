//go:build integration

package hub

import "testing"

func TestCanRun(t *testing.T) {
	if !CanRun(Use()) {
		t.Fatal("worker no longer satisfies interface{ run() string }")
	}
}
