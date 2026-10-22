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
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Start-up reconcile of the sandbox state store (ptone/scion#3738,
// ptone/scion#4776): a probed entry is never dropped. A failed probe keeps
// the entry and logs a warning, which says whether the CLI reported this
// sandbox not running (the verified wording, for the probed name only) or
// failed some other way. A generic "no such file or directory" is never
// read as a not-running answer.

// Log fragments of the two reconcile warnings.
const (
	reconcileNotRunningLog = "reports the sandbox not running; keeping the entry"
	reconcileGenericLog    = "without a not-running answer; keeping the entry"
)

// Every not-running fragment names the probed sandbox exactly once, as
// "sandbox %s ...": a fragment without the name would match output about
// any sandbox, or generic CLI errors.
func TestSandboxNotRunningOutputs_NameAnchored(t *testing.T) {
	if len(sandboxNotRunningOutputs) == 0 {
		t.Fatal("sandboxNotRunningOutputs is empty")
	}
	for _, frag := range sandboxNotRunningOutputs {
		if n := strings.Count(frag, "%s"); n != 1 || !strings.HasPrefix(frag, "sandbox %s ") {
			t.Errorf("fragment %q: want it to start with \"sandbox %%s \" and hold exactly one %%s (has %d)", frag, n)
		}
		if frag != strings.ToLower(frag) {
			t.Errorf("fragment %q: want lower case", frag)
		}
	}
}

func TestSandboxReconcile_ProbeOutcomes(t *testing.T) {
	prevTimeout := sandboxReconcileProbeTimeout
	sandboxReconcileProbeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { sandboxReconcileProbeTimeout = prevTimeout })

	const (
		none       = ""
		notRunning = reconcileNotRunningLog
		generic    = reconcileGenericLog
	)
	cases := []struct {
		name    string
		script  string
		wantLog string
	}{
		{"probe succeeds", "exit 0", none},
		{"verified not running wording", `echo "Error: sandbox $2 is not running" >&2; exit 1`, notRunning},
		{"not running wording in upper case", `echo "ERROR: SANDBOX SB-LIVE IS NOT RUNNING" >&2; exit 1`, notRunning},
		{"not running wording for another name", `echo "Error: sandbox proj--sb-live is not running" >&2; exit 1`, generic},
		{"not running wording for a longer name", `echo "Error: sandbox sb-live-2 is not running" >&2; exit 1`, generic},
		{"generic no such file or directory", `echo "exec: /opt/sandbox/root: no such file or directory" >&2; exit 1`, generic},
		{"unverified not found", `echo "Error: sandbox \"$2\" not found" >&2; exit 1`, generic},
		{"unverified no such sandbox", `echo "error: no such sandbox: $2" >&2; exit 1`, generic},
		{"unverified does not exist", `echo "Sandbox $2 does not exist"; exit 2`, generic},
		{"dead sandbox, non-matching output", `echo "exec: control socket closed" >&2; exit 1`, generic},
		{"probe times out", "exec sleep 5", generic},
		{"prints not running, then sleeps past the deadline", `echo "Error: sandbox $2 is not running" >&2; exec sleep 5`, generic},
		{"ambiguous failure", `echo "connection refused" >&2; exit 1`, generic},
		{"killed by a signal", `kill -9 $$`, generic},
		{"prints not running, then killed by a signal", `echo "Error: sandbox $2 is not running" >&2; kill -9 $$`, generic},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			setRuntimeLogHandler(t, slog.NewTextHandler(&logs, nil))

			bin := writeMockBin(t, tc.script)
			statePath := filepath.Join(t.TempDir(), "state.json")
			ss := newSandboxStateStore(statePath)
			ss.add(&sandboxStateEntry{SandboxName: "sb-live", AgentID: "dev"})
			ss.add(&sandboxStateEntry{SandboxName: "sb-stopped", AgentID: "old", Stopped: true})

			ss.reconcile(bin)

			if ss.get("sb-live") == nil {
				t.Errorf("probed entry removed, want kept (logs: %s)", logs.String())
			}
			if ss.get("sb-stopped") != nil {
				t.Errorf("stopped entry kept, want removed")
			}
			// The persisted state agrees with memory.
			reloaded := newSandboxStateStore(statePath)
			if reloaded.get("sb-live") == nil {
				t.Errorf("persisted probed entry removed, want kept")
			}
			for _, frag := range []string{notRunning, generic} {
				want := frag == tc.wantLog
				got := strings.Contains(logs.String(), "level=WARN") && strings.Contains(logs.String(), frag)
				if got != want {
					t.Errorf("warning %q logged = %v, want %v (logs: %s)", frag, got, want, logs.String())
				}
			}
		})
	}
}
