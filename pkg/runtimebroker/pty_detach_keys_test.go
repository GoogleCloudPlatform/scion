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

package runtimebroker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeExecRuntime writes a fake runtime binary named name into a temp dir.
// It answers "tmux has-session" with success, and for anything else records
// its arguments (one per line) to the returned log file and then sleeps so
// the PTY exec stays up until the test kills it.
func fakeExecRuntime(t *testing.T, name string) (runtimeCmd, argLog string) {
	t.Helper()
	dir := t.TempDir()
	argLog = filepath.Join(dir, "args.log")
	runtimeCmd = filepath.Join(dir, name)
	script := `#!/bin/sh
for a in "$@"; do
	if [ "$a" = "has-session" ]; then exit 0; fi
done
for a in "$@"; do printf '%s\n' "$a" >> '` + argLog + `'; done
exec sleep 30
`
	require.NoError(t, os.WriteFile(runtimeCmd, []byte(script), 0o700))
	return runtimeCmd, argLog
}

func waitForArgLog(t *testing.T, argLog string) []string {
	t.Helper()
	var lines []string
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(argLog)
		if err != nil || !strings.Contains(string(data), "attach-session") {
			return false
		}
		lines = strings.Split(strings.TrimSpace(string(data)), "\n")
		return true
	}, 5*time.Second, 20*time.Millisecond, "the attach exec should run")
	return lines
}

// TestStartDockerExec_DetachKeys checks that the control-channel attach exec
// (StreamPTYHandler) and the direct-attach exec (LocalPTYSession) both pass
// --detach-keys for docker and podman, so the runtime's default Ctrl-p Ctrl-q
// detach sequence does not hold back Ctrl-p inside the session.
func TestStartDockerExec_DetachKeys(t *testing.T) {
	tests := []struct {
		runtime string
		want    string
	}{
		{"docker", `--detach-keys=ctrl-\,ctrl-^`},
		{"podman", "--detach-keys="},
	}
	for _, tc := range tests {
		t.Run("stream/"+tc.runtime, func(t *testing.T) {
			runtimeCmd, argLog := fakeExecRuntime(t, tc.runtime)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h := &StreamPTYHandler{
				containerID: "c1",
				runtimeCmd:  runtimeCmd,
				execUser:    "scion",
				cols:        80,
				rows:        24,
				ctx:         ctx,
				cancel:      cancel,
			}
			require.NoError(t, h.startDockerExec())
			t.Cleanup(func() {
				_ = h.cmd.Process.Kill()
				_, _ = h.cmd.Process.Wait()
				_ = h.ptyMaster.Close()
			})
			args := waitForArgLog(t, argLog)
			require.GreaterOrEqual(t, len(args), 3)
			require.Equal(t, []string{"exec", "-it", tc.want}, args[:3])
		})
		t.Run("local/"+tc.runtime, func(t *testing.T) {
			runtimeCmd, argLog := fakeExecRuntime(t, tc.runtime)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := &LocalPTYSession{
				containerID: "c1",
				runtimeCmd:  runtimeCmd,
				execUser:    "scion",
				cols:        80,
				rows:        24,
				ctx:         ctx,
				cancel:      cancel,
			}
			require.NoError(t, s.startDockerExec())
			t.Cleanup(func() {
				_ = s.cmd.Process.Kill()
				_, _ = s.cmd.Process.Wait()
				_ = s.ptyMaster.Close()
			})
			args := waitForArgLog(t, argLog)
			require.GreaterOrEqual(t, len(args), 3)
			require.Equal(t, []string{"exec", "-it", tc.want}, args[:3])
		})
	}
}

// TestStartDockerExec_OtherRuntimesGetNoDetachKeys checks that runtimes other
// than docker and podman (here a test adapter) get no extra flag.
func TestStartDockerExec_OtherRuntimesGetNoDetachKeys(t *testing.T) {
	runtimeCmd, argLog := fakeExecRuntime(t, "runtime-exec")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &StreamPTYHandler{containerID: "c1", runtimeCmd: runtimeCmd, execUser: "scion", cols: 80, rows: 24, ctx: ctx, cancel: cancel}
	require.NoError(t, h.startDockerExec())
	t.Cleanup(func() {
		_ = h.cmd.Process.Kill()
		_, _ = h.cmd.Process.Wait()
		_ = h.ptyMaster.Close()
	})
	args := waitForArgLog(t, argLog)
	for _, a := range args {
		require.False(t, strings.HasPrefix(a, "--detach-keys"), "unexpected %q in %q", a, args)
	}
}
