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
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests for ptone/scion#1292: the stderr of a sandbox that dies during
// provisioning must survive.

const (
	priorRunOutput = "prior run: all good\n"
	deathOutput    = "sciontool init: provisioning failed: fatal: clone refused\n"
)

// writeDyingSandbox writes a fake sandbox binary whose `run` appends
// deathOutput to logPath and exits (the sandbox dies during provisioning),
// whose `exec` fails (sandbox not running), and whose `wait` reports exit 3.
func writeDyingSandbox(t *testing.T, dir, logPath string) string {
	t.Helper()
	bin := filepath.Join(dir, "sandbox")
	script := `#!/bin/sh
case "$1" in
  run)    printf '%s' '` + deathOutput + `' >> '` + logPath + `'; echo sandbox-ok; exit 0 ;;
  exec)   echo "sandbox not running"; exit 1 ;;
  wait)   echo "sandbox exited" >&2; exit 3 ;;
  delete) exit 0 ;;
  *)      exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// TestTailer_DrainsOnCancel: lines written just before the tailer is
// cancelled (by Delete) are shipped, not dropped while it sleeps.
func TestTailer_DrainsOnCancel(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), entrypointLogFile)
	if err := os.WriteFile(logPath, []byte("provisioning...\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var out safeBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		doTailEntrypointLog(ctx, logPath, "slug", "agent-1", "proj-1", 0, &out)
		close(done)
	}()
	waitForTailerOutput(t, &out, 1, 5*time.Second)

	// The sandbox writes its last words and is deleted at once.
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(deathOutput + "no newline")
	_ = f.Close()
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("tailer did not exit after cancellation")
	}
	got := out.String()
	for _, want := range []string{strings.TrimSpace(deathOutput), "no newline"} {
		if !strings.Contains(got, want) {
			t.Errorf("tailer output missing %q after cancel:\n%s", want, got)
		}
	}
}

// TestCloudRunSandboxRun_DeadOnArrivalReportsThisRunsStderr: the Run error
// carries this run's entrypoint output, not the previous run's.
func TestCloudRunSandboxRun_DeadOnArrivalReportsThisRunsStderr(t *testing.T) {
	tmpDir := t.TempDir()
	rootDir := filepath.Join(tmpDir, "scion")
	agentHome := filepath.Join(rootDir, "agents", "dead-agent", "home")
	if err := os.MkdirAll(agentHome, 0755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(agentHome, entrypointLogFile)
	if err := os.WriteFile(logPath, []byte(priorRunOutput), 0644); err != nil {
		t.Fatal(err)
	}
	homeDir := filepath.Join(tmpDir, "agent-home")
	workspace := filepath.Join(tmpDir, "workspace")
	for _, d := range []string{homeDir, workspace} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}

	rt := &CloudRunSandboxRuntime{
		bin:          writeDyingSandbox(t, tmpDir, logPath),
		state:        newSandboxStateStore(filepath.Join(tmpDir, "state.json")),
		rootDir:      rootDir,
		watchCancels: make(map[string]context.CancelFunc),
	}
	_, err := rt.Run(context.Background(), RunConfig{
		Name:      "dead-agent",
		HomeDir:   homeDir,
		Workspace: workspace,
		Image:     "omni-image",
		Harness:   &mockHarness{command: []string{"claude"}, env: map[string]string{}},
		Labels:    map[string]string{"scion.name": "dead-agent"},
	})
	if err == nil {
		t.Fatal("Run() should fail for a sandbox that died during provisioning")
	}
	if !strings.Contains(err.Error(), strings.TrimSpace(deathOutput)) {
		t.Errorf("Run() error lost this run's stderr: %v", err)
	}
	if strings.Contains(err.Error(), strings.TrimSpace(priorRunOutput)) {
		t.Errorf("Run() error shows the prior run's output as the cause: %v", err)
	}
}

// TestCloudRunSandboxWatch_LogsStderrOnAbnormalExit: when a sandbox exits
// non-zero, the watcher logs this run's entrypoint output, so it survives
// the Delete that removes the state entry GetLogs needs.
func TestCloudRunSandboxWatch_LogsStderrOnAbnormalExit(t *testing.T) {
	tmpDir := t.TempDir()
	agentHome := filepath.Join(tmpDir, "home")
	if err := os.MkdirAll(agentHome, 0755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(agentHome, entrypointLogFile)
	if err := os.WriteFile(logPath, []byte(priorRunOutput+deathOutput), 0644); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	prevLog := runtimeLog
	runtimeLog = slog.New(slog.NewTextHandler(&logs, nil))
	t.Cleanup(func() { runtimeLog = prevLog })

	rt := &CloudRunSandboxRuntime{
		bin:          writeDyingSandbox(t, tmpDir, logPath),
		state:        newSandboxStateStore(filepath.Join(tmpDir, "state.json")),
		watchCancels: make(map[string]context.CancelFunc),
	}
	rt.state.add(&sandboxStateEntry{
		SandboxName:         "dead-agent",
		AgentID:             "dead-agent",
		AgentHome:           agentHome,
		EntrypointLogOffset: int64(len(priorRunOutput)),
	})

	rt.watchSandbox(context.Background(), "dead-agent")

	entry := rt.state.get("dead-agent")
	if entry == nil || !entry.Stopped || entry.ExitCode == nil || *entry.ExitCode != 3 {
		t.Fatalf("state entry not marked stopped with exit 3: %+v", entry)
	}
	got := logs.String()
	if !strings.Contains(got, "sandbox exited abnormally") || !strings.Contains(got, "clone refused") {
		t.Errorf("watcher did not log this run's stderr:\n%s", got)
	}
	if strings.Contains(got, "prior run") {
		t.Errorf("watcher logged the prior run's output:\n%s", got)
	}
}

func TestEntrypointLogTail(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), entrypointLogFile)
	if got := entrypointLogTail(path, 0, 10); got != "" {
		t.Errorf("missing file: got %q, want empty", got)
	}
	if err := os.WriteFile(path, []byte("0123456789abcdef"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		offset int64
		max    int
		want   string
	}{
		{0, 100, "0123456789abcdef"},
		{10, 100, "abcdef"},
		{16, 100, ""},
		{99, 100, "0123456789abcdef"}, // truncated file: read from start
		{0, 4, "...(truncated)\ncdef"},
	} {
		if got := entrypointLogTail(path, tc.offset, tc.max); got != tc.want {
			t.Errorf("entrypointLogTail(offset=%d, max=%d) = %q, want %q", tc.offset, tc.max, got, tc.want)
		}
	}
}
