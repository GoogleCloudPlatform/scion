/*
Copyright 2025 The Scion Authors.
*/

package procreap

import (
	"os/exec"
	"runtime"
	"testing"
)

// TestScanZombies_IncludesNameForZombie verifies scanZombies resolves the
// *actual* process name for a genuine zombie, not just a non-empty
// placeholder: scanZombies defaults an unresolved name to "unknown", so
// asserting only "!= \"\"" would pass even if the /proc/<pid>/comm read
// were silently broken.
func TestScanZombies_IncludesNameForZombie(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping test on non-linux platform")
	}

	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start test process: %v", err)
	}
	pid := cmd.Process.Pid
	// Best-effort cleanup: an error here (e.g. procreap's own reaper won the
	// race and already reaped this child) doesn't affect the assertions
	// below, which all run before this deferred call, so it's intentionally
	// ignored rather than failing the test.
	defer func() { _ = cmd.Wait() }()

	waitUntilZombie(t, pid)

	var found bool
	for _, z := range scanZombies() {
		if z.pid != pid {
			continue
		}
		found = true
		if z.name != "true" {
			t.Errorf("scanZombies() returned name %q for zombie pid %d, want %q", z.name, pid, "true")
		}
	}
	if !found {
		t.Fatalf("scanZombies() did not include zombie pid %d", pid)
	}
}

func TestParseProcPID(t *testing.T) {
	tests := []struct {
		name    string
		wantPID int
		wantOK  bool
	}{
		{name: "2", wantPID: 2, wantOK: true},
		{name: "10", wantPID: 10, wantOK: true},
		{name: "999999", wantPID: 999999, wantOK: true},
		{name: "1", wantOK: false},  // PID 1 (init) is never a reapable child
		{name: "0", wantOK: false},  // not a valid PID
		{name: "-5", wantOK: false}, // Atoi parses this, but it's <= 1
		{name: "abc", wantOK: false},
		{name: "1abc", wantOK: false},
		{name: "", wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pid, ok := parseProcPID(tc.name)
			if ok != tc.wantOK {
				t.Fatalf("parseProcPID(%q) ok = %v, want %v", tc.name, ok, tc.wantOK)
			}
			if ok && pid != tc.wantPID {
				t.Errorf("parseProcPID(%q) = %d, want %d", tc.name, pid, tc.wantPID)
			}
		})
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
	const pid = 999999 // fixed PID unlikely to be a real process
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
	const pid = 999998 // fixed PID unlikely to be a real process

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
