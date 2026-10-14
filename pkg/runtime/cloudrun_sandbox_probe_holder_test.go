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
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Probe output holders (ptone/scion#5003): a process the sandbox CLI
// leaves behind may inherit and hold the CLI's stdout and stderr. The
// liveness probes (start and reconcile) capture the output in a file, so
// they return when the CLI exits or is killed, not when the holder exits.
// The holders below run 15 s or more; the elapsed-time bounds leave wide
// margins on both sides (a loaded runner, and the holder's lifetime).

// Fault check P1: the reconcile probe captures its output through a pipe
// (runSimpleCommand) again. Each case then waits for the 15 s holder and
// exceeds the bound.
func TestSandboxReconcile_ProbeOutputHolder(t *testing.T) {
	prevTimeout := sandboxReconcileProbeTimeout
	sandboxReconcileProbeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { sandboxReconcileProbeTimeout = prevTimeout })

	cases := []struct {
		name string
		tail string // runs after the holder is started
		kept bool
	}{
		{"not-found exit leaves a holder", `echo "Error: sandbox \"$2\" not found" >&2; exit 1`, false},
		{"passing probe leaves a holder", "exit 0", true},
		{"probe hangs past the deadline with a holder", "exec sleep 60", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "holder.pid")
			t.Cleanup(func() { killPIDFile(pidFile) })
			bin := writeMockBin(t, fmt.Sprintf("sleep 15 & echo $! > %q\n%s", pidFile, tc.tail))
			ss := newSandboxStateStore(filepath.Join(t.TempDir(), "state.json"))
			ss.add(&sandboxStateEntry{SandboxName: "sb-live", AgentID: "dev"})

			done := make(chan struct{})
			begin := time.Now()
			go func() {
				ss.reconcile(bin)
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("reconcile did not return within 30 s")
			}
			if d := time.Since(begin); d > 5*time.Second {
				t.Errorf("reconcile took %v, want well under the holder's 15 s: the probe waited on the held output", d)
			}
			if got := ss.get("sb-live") != nil; got != tc.kept {
				t.Errorf("entry kept = %v, want %v", got, tc.kept)
			}
		})
	}
}

// Fault check P2: the start probe captures its output through a pipe
// (runSimpleCommand) again. `exec` exits 0 leaving a 15 s holder; the
// start then waits for the holder and exceeds the bound.
func TestCloudRunSandboxStarting_ProbeOutputHolder(t *testing.T) {
	rt, env := newStartingRuntime(t, startScripts{exec: `sleep 15 & echo $! > "$PIDFILE"; exit 0`})
	begin := time.Now()
	err := recvErr(t, runSandboxAsync(context.Background(), rt, startingCfg(t, "run-a")), "Run")
	if err != nil {
		t.Fatalf("Run = %v, want success", err)
	}
	// The first probe runs after 0.5 s.
	if d := time.Since(begin); d > 8*time.Second {
		t.Errorf("Run took %v, want well under the holder's 15 s: the probe waited on the held output", d)
	}
	wantDeletes(t, env.log, 0)
	if e := fileEntry(t, env.state); e == nil || e.Starting {
		t.Errorf("entry = %+v, want the promoted entry", e)
	}
}

// Fault checks D1 and D2: the cleanup delete captures its output through a
// pipe again, D1 in runDeleteWithTimeout (workaround on: the wait then
// ends only at the 10 s workaround timeout) and D2 in runDeletePlain
// (workaround off: it ends when the 15 s holder exits). Every probe fails
// at once; `delete` exits 0 leaving the holder. The start returns its
// dead-on-arrival error after the 3.5 s probe ladder, well within the
// 10 s cleanup budget on top, with one delete and no entry left.
func TestCloudRunSandboxStarting_DeadOnArrivalCleanupHolder(t *testing.T) {
	for _, workaround := range []bool{true, false} {
		t.Run(fmt.Sprintf("workaround=%v", workaround), func(t *testing.T) {
			withDeleteWorkaround(t, workaround)
			rt, env := newStartingRuntime(t, startScripts{
				exec: `echo "sandbox not running"; exit 1`,
				del:  `sleep 15 & echo $! > "$PIDFILE"; exit 0`,
			})
			begin := time.Now()
			err := recvErr(t, runSandboxAsync(context.Background(), rt, startingCfg(t, "run-a")), "Run")
			if err == nil || !strings.Contains(err.Error(), "dead on arrival") {
				t.Fatalf("Run = %v, want the dead-on-arrival error", err)
			}
			if d := time.Since(begin); d > 9*time.Second {
				t.Errorf("Run took %v, want well under 3.5 s + the 10 s cleanup budget: the cleanup delete waited on the held output", d)
			}
			wantDeletes(t, env.log, 1)
			if e := fileEntry(t, env.state); e != nil {
				t.Errorf("entry = %+v, want none", e)
			}
		})
	}
}
