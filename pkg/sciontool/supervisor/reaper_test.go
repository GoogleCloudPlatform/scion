/*
Copyright 2025 The Scion Authors.
*/

package supervisor

import (
	"os"
	"runtime"
	"testing"
)

func TestSnapshotProcessNames(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping test on non-linux platform")
	}
	names := snapshotProcessNames()

	// We should find at least our own process in the snapshot.
	myPID := os.Getpid()
	name, ok := names[myPID]
	if !ok {
		t.Fatalf("snapshotProcessNames() did not include current process (pid %d)", myPID)
	}
	if name == "" {
		t.Fatal("snapshotProcessNames() returned empty name for current process")
	}
	t.Logf("current process (pid %d) name: %s", myPID, name)
}

func TestSnapshotProcessNames_PID1Excluded(t *testing.T) {
	names := snapshotProcessNames()
	if _, ok := names[1]; ok {
		t.Error("snapshotProcessNames() should exclude PID 1")
	}
}
