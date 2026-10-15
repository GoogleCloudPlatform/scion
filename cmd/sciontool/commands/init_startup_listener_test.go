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
