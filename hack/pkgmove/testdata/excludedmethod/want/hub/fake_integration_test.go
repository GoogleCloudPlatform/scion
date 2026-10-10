//go:build integration

package hub

import "testing"

func TestFakeRuns(t *testing.T) {
	if !FakeRuns {
		t.Fatal("fake no longer satisfies runner")
	}
}
