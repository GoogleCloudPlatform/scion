/*
Copyright 2025 The Scion Authors.
*/

package procreap

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

func TestIsZombieStat(t *testing.T) {
	tests := []struct {
		name string
		stat string
		want bool
	}{
		{
			name: "running process",
			stat: "1234 (git) R 1 1234 1234 0 -1 4194304 100 0 0 0 0 0 0 0 20 0 1 0 12345 0 0",
			want: false,
		},
		{
			name: "zombie process",
			stat: "1234 (git) Z 1 1234 1234 0 -1 4194304 100 0 0 0 0 0 0 0 20 0 1 0 12345 0 0",
			want: true,
		},
		{
			name: "comm field contains spaces and parens, still parses state after last )",
			stat: "1234 (my (weird) proc name) Z 1 1234",
			want: true,
		},
		{
			name: "malformed line with no closing paren",
			stat: "1234 git Z 1 1234",
			want: false,
		},
		{
			name: "empty input",
			stat: "",
			want: false,
		},
		{
			name: "truncated right after the paren",
			stat: "1234 (git)",
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isZombieStat(tc.stat); got != tc.want {
				t.Errorf("isZombieStat(%q) = %v, want %v", tc.stat, got, tc.want)
			}
		})
	}
}

func TestManagedPIDRegistry(t *testing.T) {
	const pid = 999999 // arbitrary PID unlikely to be a real process
	if isManagedPID(pid) {
		t.Fatal("pid should not be managed before registration")
	}
	tok := RegisterManagedPID(pid)
	if !isManagedPID(pid) {
		t.Fatal("pid should be managed after RegisterManagedPID")
	}
	UnregisterManagedPID(pid, tok)
	if isManagedPID(pid) {
		t.Fatal("pid should not be managed after UnregisterManagedPID")
	}
}

// TestManagedPIDRegistry_StaleTokenDoesNotStealNewRegistration is the
// regression test for the PID-reuse hazard: once a token has been
// unregistered (or superseded), presenting it again must never delete a
// different, newer registration for the same pid — which is exactly what
// happens when a PID is reused by a fresh process before the previous
// owner's deferred Unregister call runs.
func TestManagedPIDRegistry_StaleTokenDoesNotStealNewRegistration(t *testing.T) {
	const pid = 999998 // arbitrary PID unlikely to be a real process

	staleTok := RegisterManagedPID(pid)
	UnregisterManagedPID(pid, staleTok) // pid is now unregistered

	newTok := RegisterManagedPID(pid) // simulates the pid being reused
	if !isManagedPID(pid) {
		t.Fatal("pid should be managed after the new registration")
	}

	// A late/duplicate unregister with the OLD token must be a no-op.
	UnregisterManagedPID(pid, staleTok)
	if !isManagedPID(pid) {
		t.Fatal("a stale token's Unregister call deleted a newer registration for the reused pid")
	}

	// The real owner's unregister (with the current token) still works.
	UnregisterManagedPID(pid, newTok)
	if isManagedPID(pid) {
		t.Fatal("pid should not be managed after the current token's Unregister call")
	}
}
