// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// newWorkaroundTestRuntime creates a CloudRunSandboxRuntime wired to a mock
// shell-script binary. The deleteTimeout is set short so tests complete
// quickly.
func newWorkaroundTestRuntime(t *testing.T, bin string) *CloudRunSandboxRuntime {
	t.Helper()
	stateFile := filepath.Join(t.TempDir(), "state.json")
	return &CloudRunSandboxRuntime{
		bin:          bin,
		state:        newSandboxStateStore(stateFile),
		rootDir:      t.TempDir(),
		watchCancels: make(map[string]context.CancelFunc),
	}
}

// writeMockBin writes a shell script as a mock sandbox binary and returns
// the path.
func writeMockBin(t *testing.T, script string) string {
	t.Helper()
	tmpDir := t.TempDir()
	mockBin := filepath.Join(tmpDir, "sandbox")
	if err := os.WriteFile(mockBin, []byte("#!/bin/sh\n"+script+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return mockBin
}

// -----------------------------------------------------------------------
// deleteWithTimeout tests
// -----------------------------------------------------------------------

func TestCloudRunSandboxDeleteWithTimeout_NormalCompletion(t *testing.T) {
	mockBin := writeMockBin(t, "exit 0")
	rt := newWorkaroundTestRuntime(t, mockBin)
	rt.state.add(&sandboxStateEntry{SandboxName: "sb-ok", AgentID: "agent-ok"})

	err := rt.deleteWithTimeout(context.Background(), "sb-ok")
	if err != nil {
		t.Fatalf("deleteWithTimeout() error = %v, want nil", err)
	}

	// State should be cleaned up.
	if entry := rt.state.get("sb-ok"); entry != nil {
		t.Error("state entry still present after normal completion")
	}
}

func TestCloudRunSandboxDeleteWithTimeout_Timeout(t *testing.T) {
	mockBin := writeMockBin(t, "sleep 60")
	rt := newWorkaroundTestRuntime(t, mockBin)
	rt.deleteTimeout = 200 * time.Millisecond
	rt.state.add(&sandboxStateEntry{SandboxName: "sb-hang", AgentID: "agent-hang"})

	start := time.Now()
	err := rt.deleteWithTimeout(context.Background(), "sb-hang")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("deleteWithTimeout() error = %v, want nil (timeout treated as success)", err)
	}

	// Should complete near the timeout.
	if elapsed > 5*time.Second {
		t.Errorf("deleteWithTimeout() took %v, expected ~200ms (timeout)", elapsed)
	}

	// State should be cleaned up.
	if entry := rt.state.get("sb-hang"); entry != nil {
		t.Error("state entry still present after timeout")
	}
}

func TestCloudRunSandboxDeleteWithTimeout_ContextCancellation(t *testing.T) {
	mockBin := writeMockBin(t, "sleep 60")
	rt := newWorkaroundTestRuntime(t, mockBin)
	rt.state.add(&sandboxStateEntry{SandboxName: "sb-ctx", AgentID: "agent-ctx"})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := rt.deleteWithTimeout(ctx, "sb-ctx")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("deleteWithTimeout() error = nil, want context error")
	}
	if err != context.DeadlineExceeded {
		t.Errorf("deleteWithTimeout() error = %v, want %v", err, context.DeadlineExceeded)
	}

	// Should complete quickly after context cancellation.
	if elapsed > 5*time.Second {
		t.Errorf("deleteWithTimeout() took %v, expected ~200ms (context timeout)", elapsed)
	}
}

func TestCloudRunSandboxDeleteWithTimeout_ProcessErrorNonFatal(t *testing.T) {
	mockBin := writeMockBin(t, "exit 1")
	rt := newWorkaroundTestRuntime(t, mockBin)
	rt.state.add(&sandboxStateEntry{SandboxName: "sb-err", AgentID: "agent-err"})

	err := rt.deleteWithTimeout(context.Background(), "sb-err")
	if err != nil {
		t.Fatalf("deleteWithTimeout() error = %v, want nil (process error is non-fatal)", err)
	}

	// State should still be cleaned up.
	if entry := rt.state.get("sb-err"); entry != nil {
		t.Error("state entry still present after non-zero exit")
	}
}

func TestCloudRunSandboxDeleteWithTimeout_CancelsWatcher(t *testing.T) {
	mockBin := writeMockBin(t, "exit 0")
	rt := newWorkaroundTestRuntime(t, mockBin)
	rt.state.add(&sandboxStateEntry{SandboxName: "sb-watch", AgentID: "agent-watch"})

	// Register a mock watcher cancel.
	cancelled := false
	var mu sync.Mutex
	rt.watchMu.Lock()
	rt.watchCancels["sb-watch"] = func() {
		mu.Lock()
		cancelled = true
		mu.Unlock()
	}
	rt.watchMu.Unlock()

	// deleteOrWorkaround cancels the watcher before dispatching.
	err := rt.deleteOrWorkaround(context.Background(), "sb-watch")
	if err != nil {
		t.Fatalf("deleteOrWorkaround() error = %v", err)
	}

	mu.Lock()
	wasCancelled := cancelled
	mu.Unlock()
	if !wasCancelled {
		t.Error("watcher cancel function was not called")
	}

	// Watcher cancel should be removed from the map.
	rt.watchMu.Lock()
	_, present := rt.watchCancels["sb-watch"]
	rt.watchMu.Unlock()
	if present {
		t.Error("watcher cancel still in map after deleteOrWorkaround()")
	}
}

func TestCloudRunSandboxDeleteWithTimeout_DefaultTimeout(t *testing.T) {
	mockBin := writeMockBin(t, "exit 0")
	rt := newWorkaroundTestRuntime(t, mockBin)
	if rt.deleteTimeout != 0 {
		t.Fatalf("deleteTimeout = %v, want 0 (zero value)", rt.deleteTimeout)
	}

	// Verify the constant exists and is reasonable.
	if DefaultDeleteTimeout != 10*time.Second {
		t.Errorf("DefaultDeleteTimeout = %v, want 10s", DefaultDeleteTimeout)
	}
}

// -----------------------------------------------------------------------
// killProcessGroup tests
// -----------------------------------------------------------------------

func TestKillProcessGroup_NilProcess(t *testing.T) {
	cmd := &exec.Cmd{}
	// Should not panic when Process is nil.
	killProcessGroup(cmd)
}

func TestKillProcessGroup_RunningProcess(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start sleep: %v", err)
	}

	killProcessGroup(cmd)

	// The process should eventually exit.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-done:
		// OK — process was killed.
	case <-time.After(5 * time.Second):
		t.Fatal("killProcessGroup did not kill the process within 5s")
	}
}

// -----------------------------------------------------------------------
// reapOrphanedRunsc tests
// -----------------------------------------------------------------------

func TestReapOrphanedRunsc_NoProc(t *testing.T) {
	// On a system with /proc, this should be a no-op for a non-existent
	// sandbox name. On a system without /proc, it should gracefully no-op.
	// Either way, it should not panic.
	reapOrphanedRunsc("nonexistent-sandbox-name-12345")
}

// -----------------------------------------------------------------------
// isOrphanedRunscProcess tests
// -----------------------------------------------------------------------

func TestIsOrphanedRunscProcess(t *testing.T) {
	const sandbox = "my-sandbox"

	tests := []struct {
		name        string
		cmdline     []byte
		sandboxName string
		want        bool
	}{
		{
			// Real argv captured from a Cloud Run Instance, recorded in
			// defect-sandbox-delete-hang.md section 3.
			name: "genuine captured orphan",
			cmdline: []byte(
				"/usr/local/gcp/bin/runsc\x00" +
					"--platform=xemu\x00" +
					"--platform_device_path=/dev/xemu\x00" +
					"--root=/tmp/runsc-root\x00" +
					"--ignore-cgroups\x00" +
					"--TESTONLY-unsafe-nonroot\x00" +
					"--overlay2=root:memory\x00" +
					"--network=none\x00" +
					"delete\x00" +
					"--force\x00" +
					"my-sandbox\x00"),
			sandboxName: sandbox,
			want:        true,
		},
		{
			name: "near-miss: sandbox name is substring of final arg",
			cmdline: []byte(
				"/usr/local/gcp/bin/runsc\x00" +
					"--platform=xemu\x00" +
					"--root=/tmp/runsc-root\x00" +
					"delete\x00" +
					"--force\x00" +
					"my-sandbox-worker\x00"),
			sandboxName: sandbox,
			want:        false,
		},
		{
			name: "near-miss: sandbox name as flag value not final arg",
			cmdline: []byte(
				"/usr/local/gcp/bin/runsc\x00" +
					"--platform=xemu\x00" +
					"--root=/tmp/runsc-root\x00" +
					"--network=none\x00" +
					"delete\x00" +
					"--force\x00" +
					"--some-flag=my-sandbox\x00" +
					"other-sandbox\x00"),
			sandboxName: sandbox,
			want:        false,
		},
		{
			name: "unrelated runsc process: create not delete",
			cmdline: []byte(
				"/usr/local/gcp/bin/runsc\x00" +
					"--root=/tmp/runsc-root\x00" +
					"create\x00" +
					"--bundle=/tmp/bundle\x00" +
					"my-sandbox\x00"),
			sandboxName: sandbox,
			want:        false,
		},
		{
			name: "short cmdline",
			cmdline: []byte(
				"/usr/local/gcp/bin/runsc\x00" +
					"--help\x00"),
			sandboxName: "anything",
			want:        false,
		},
		{
			name:        "empty cmdline",
			cmdline:     []byte{},
			sandboxName: "anything",
			want:        false,
		},
		{
			name: "non-runsc binary with delete and matching name",
			cmdline: []byte(
				"/usr/bin/some-tool\x00" +
					"delete\x00" +
					"--force\x00" +
					"my-sandbox\x00"),
			sandboxName: sandbox,
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isOrphanedRunscProcess(tt.cmdline, tt.sandboxName)
			if got != tt.want {
				t.Errorf("isOrphanedRunscProcess(%q, %q) = %v, want %v",
					tt.cmdline, tt.sandboxName, got, tt.want)
			}
		})
	}
}
