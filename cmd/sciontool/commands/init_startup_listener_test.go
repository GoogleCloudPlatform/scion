/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/startuplistener"
)

// TestRunInit_OpensStartupListenerWhileHarnessRuns proves RunInit opens the
// startup listener when the runtime sets its env var: the port accepts a TCP
// connection while the harness child is still running. The child waits for a
// flag file, which the test writes once the dial attempts are done.
func TestRunInit_OpensStartupListenerWhileHarnessRuns(t *testing.T) {
	agentHome := t.TempDir()
	setupRunInitAsRootlessScion(t, agentHome)

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := strconv.Itoa(probe.Addr().(*net.TCPAddr).Port)
	_ = probe.Close()
	t.Setenv(startuplistener.EnvPort, port)

	flag := filepath.Join(t.TempDir(), "done")
	dialed := make(chan bool, 1)
	go func() {
		ok := false
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 500*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				ok = true
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		_ = os.WriteFile(flag, nil, 0o644)
		dialed <- ok
	}()

	got := RunInit([]string{"sh", "-c", "while [ ! -e '" + flag + "' ]; do sleep 0.05; done"},
		InitRunOptions{DisableTermSignalForwarding: true})
	if got != 0 {
		t.Fatalf("RunInit() = %d, want 0", got)
	}
	if !<-dialed {
		t.Fatalf("startup listener never accepted a connection on port %s while the harness ran", port)
	}
}

// TestRunInit_StartupListenerBindFailureKeepsRunning proves a startup
// listener that cannot bind does not stop init: the harness still runs to
// completion and RunInit returns its exit code.
func TestRunInit_StartupListenerBindFailureKeepsRunning(t *testing.T) {
	agentHome := t.TempDir()
	setupRunInitAsRootlessScion(t, agentHome)

	busy, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = busy.Close() }()
	t.Setenv(startuplistener.EnvPort, strconv.Itoa(busy.Addr().(*net.TCPAddr).Port))

	out := filepath.Join(t.TempDir(), "ran")
	got := RunInit([]string{"sh", "-c", "touch '" + out + "'"}, InitRunOptions{DisableTermSignalForwarding: true})
	if got != 0 {
		t.Fatalf("RunInit() = %d, want 0 (a bind failure must not stop init)", got)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("harness did not run after the bind failure: %v", err)
	}
}

// runInitPORT runs RunInit with the startup listener on a free port and PORT
// set to port (or the listener port when port is ""), and returns the PORT
// value the harness child saw ("unset" when absent).
func runInitPORT(t *testing.T, port string) (listenerPort, seen string) {
	t.Helper()
	agentHome := t.TempDir()
	setupRunInitAsRootlessScion(t, agentHome)

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	listenerPort = strconv.Itoa(probe.Addr().(*net.TCPAddr).Port)
	_ = probe.Close()
	t.Setenv(startuplistener.EnvPort, listenerPort)
	if port == "" {
		port = listenerPort
	}
	t.Setenv("PORT", port)

	out := filepath.Join(t.TempDir(), "port")
	got := RunInit([]string{"sh", "-c", `printf '%s' "${PORT-unset}" > '` + out + `'`},
		InitRunOptions{DisableTermSignalForwarding: true})
	if got != 0 {
		t.Fatalf("RunInit() = %d, want 0", got)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading harness output: %v", err)
	}
	return listenerPort, string(raw)
}

// TestRunInit_StartupListenerDropsMatchingPORT proves the harness does not
// inherit a PORT that names the port the startup listener holds, so a
// workload that honours PORT does not try to bind it.
func TestRunInit_StartupListenerDropsMatchingPORT(t *testing.T) {
	listenerPort, seen := runInitPORT(t, "")
	if seen != "unset" {
		t.Fatalf("harness saw PORT=%q, want it unset (the startup listener holds port %s)", seen, listenerPort)
	}
}

// TestRunInit_StartupListenerKeepsOtherPORT proves a PORT that names some
// other port is passed through unchanged.
func TestRunInit_StartupListenerKeepsOtherPORT(t *testing.T) {
	_, seen := runInitPORT(t, "3000")
	if seen != "3000" {
		t.Fatalf("harness saw PORT=%q, want 3000 (only the startup listener's port is dropped)", seen)
	}
}
