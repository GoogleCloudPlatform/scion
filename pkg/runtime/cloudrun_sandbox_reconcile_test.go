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

// Start-up reconcile of the sandbox state store (ptone/scion#3738): an
// entry is dropped only when the liveness probe answers that the sandbox
// does not exist. A slow or otherwise failing probe keeps the entry, so a
// live sandbox is not orphaned from List, Stop and Delete.

func TestSandboxReconcile_ProbeOutcomes(t *testing.T) {
	prevTimeout := sandboxReconcileProbeTimeout
	sandboxReconcileProbeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { sandboxReconcileProbeTimeout = prevTimeout })

	cases := []struct {
		name     string
		script   string
		kept     bool
		wantWarn bool
	}{
		{"probe succeeds", "exit 0", true, false},
		{"pattern not found", `echo "Error: sandbox \"$2\" not found" >&2; exit 1`, false, false},
		{"pattern no such", `echo "error: no such sandbox: $2" >&2; exit 1`, false, false},
		{"pattern does not exist", `echo "Sandbox $2 does not exist"; exit 2`, false, false},
		{"dead sandbox, non-matching output", `echo "exec: control socket closed" >&2; exit 1`, true, true},
		{"probe times out", "exec sleep 5", true, true},
		{"ambiguous failure", `echo "connection refused" >&2; exit 1`, true, true},
		{"killed by a signal", `kill -9 $$`, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			prevLog := runtimeLog
			runtimeLog = slog.New(slog.NewTextHandler(&logs, nil))
			t.Cleanup(func() { runtimeLog = prevLog })

			bin := writeMockBin(t, tc.script)
			statePath := filepath.Join(t.TempDir(), "state.json")
			ss := newSandboxStateStore(statePath)
			ss.add(&sandboxStateEntry{SandboxName: "sb-live", AgentID: "dev"})
			ss.add(&sandboxStateEntry{SandboxName: "sb-stopped", AgentID: "old", Stopped: true})

			ss.reconcile(bin)

			if got := ss.get("sb-live") != nil; got != tc.kept {
				t.Errorf("entry kept = %v, want %v (logs: %s)", got, tc.kept, logs.String())
			}
			if ss.get("sb-stopped") != nil {
				t.Errorf("stopped entry kept, want removed")
			}
			// The persisted state agrees with memory.
			reloaded := newSandboxStateStore(statePath)
			if got := reloaded.get("sb-live") != nil; got != tc.kept {
				t.Errorf("persisted entry kept = %v, want %v", got, tc.kept)
			}
			warned := strings.Contains(logs.String(), "level=WARN") &&
				strings.Contains(logs.String(), "keeping the entry")
			if warned != tc.wantWarn {
				t.Errorf("warn logged = %v, want %v (logs: %s)", warned, tc.wantWarn, logs.String())
			}
		})
	}
}
