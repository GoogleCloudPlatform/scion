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

package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestRealTmuxBufferedMessageWaitsForDelayedReader runs a private,
// disposable tmux server (never an active agent terminal) whose "scion"
// session stands in for a harness that starts slowly: for its first second
// it discards all input, as a harness does before it reads its prompt, then
// writes the ready marker and starts reading. A buffered message sent right
// after the session starts must arrive at the reader, not be discarded. The
// readiness probe runs the production script, with only the tmux socket and
// the readiness file paths pointed at the test's own.
func TestRealTmuxBufferedMessageWaitsForDelayedReader(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real-tmux integration test in short mode")
	}
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed; skipping real-tmux integration test")
	}
	timeoutPath, err := exec.LookPath("timeout")
	if err != nil {
		t.Skip("timeout(1) not installed; skipping real-tmux integration test")
	}
	requireTmuxVersionFloor(t, tmuxPath)

	dir, err := os.MkdirTemp("", "tmxr")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sock := filepath.Join(dir, "sock")
	outFile := filepath.Join(dir, "out")
	initRun := filepath.Join(dir, "init-run")
	ready := filepath.Join(dir, "ready")
	const nonce = "0123456789abcdef"

	runTmux := func(stdin io.Reader, args ...string) (string, error) {
		cmd := exec.Command(tmuxPath, append([]string{"-S", sock}, args...)...)
		cmd.Stdin = stdin
		out, err := cmd.CombinedOutput()
		if err != nil {
			return string(out), fmt.Errorf("tmux %v failed: %w (%s)", args, err, out)
		}
		return string(out), nil
	}
	t.Cleanup(func() { _, _ = runTmux(nil, "kill-server") })

	if err := os.WriteFile(initRun, []byte(state.FormatInitRun(nonce, state.ReadyModeHook)), 0o644); err != nil {
		t.Fatal(err)
	}

	reader := fmt.Sprintf("%s 1 cat >/dev/null; printf 'nonce=%s\\n' > %s; head -n 1 > %s; sleep 60",
		timeoutPath, nonce, ready, outFile)
	if _, err := runTmux(nil, "-f", "/dev/null", "new-session", "-d", "-s", "scion", "-x", "200", "-y", "50", reader); err != nil {
		t.Fatal(err)
	}

	// Point the production probe script at the private server and files.
	rewriteProbe := func(script string) string {
		script = strings.ReplaceAll(script, "tmux ", tmuxPath+" -S "+sock+" ")
		script = strings.ReplaceAll(script, state.HarnessInitRunFile, initRun)
		return strings.ReplaceAll(script, state.HarnessReadyFile, ready)
	}
	shim := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{{
				ContainerID:     "local",
				Name:            "test-agent",
				ContainerStatus: "Up 1 second",
				Labels:          map[string]string{"scion.name": "test-agent"},
			}}, nil
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			if cmd[0] == "sh" {
				out, err := exec.CommandContext(ctx, "sh", "-c", rewriteProbe(cmd[2])).CombinedOutput()
				return string(out), err
			}
			return runTmux(nil, cmd[1:]...)
		},
		ExecWithStdinFunc: func(ctx context.Context, id string, cmd []string, stdin io.Reader) (string, error) {
			return runTmux(stdin, cmd[1:]...)
		},
	}

	mgr := &AgentManager{Runtime: shim, readyTimings: harnessReadyTimings{
		PollInterval:   100 * time.Millisecond,
		Deadline:       15 * time.Second,
		Settle:         200 * time.Millisecond,
		SettleFallback: 8 * time.Second,
	}}
	mgr.msgBuffer = NewMessageBuffer(20*time.Millisecond, func(agentID, projectID, message string, interrupt bool) error {
		return mgr.deliverImmediate(context.Background(), agentID, projectID, message, interrupt)
	})
	mgr.msgBuffer.waitReady = mgr.waitHarnessReady
	defer mgr.Close()

	const msg = "first task for the new agent"
	if err := mgr.Message(context.Background(), "test-agent", "", msg, false); err != nil {
		t.Fatalf("Message: %v", err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for {
		data, _ := os.ReadFile(outFile)
		if got := strings.TrimSpace(string(data)); got != "" {
			if got != msg {
				t.Fatalf("reader got %q, want %q", got, msg)
			}
			return
		}
		if time.Now().After(deadline) {
			pane, _ := runTmux(nil, "capture-pane", "-p", "-t", "scion:0")
			t.Fatalf("message never reached the delayed reader; pane:\n%s", pane)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
