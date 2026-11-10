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
	"os/exec"
	"strings"
	"testing"
	"time"
)

// runDeleteScript runs script under sh with ctx and returns its combined
// output and exec error, as a real `sandbox delete --force` would.
func runDeleteScript(ctx context.Context, script string) (string, error) {
	out, err := exec.CommandContext(ctx, "sh", "-c", script).CombinedOutput()
	return string(out), err
}

func TestClassifySandboxDelete(t *testing.T) {
	notFound := `echo 'Error: sandbox "sb" not found' >&2`
	tests := []struct {
		name     string
		script   string
		timeout  time.Duration // ctx deadline for the run; 0 means none
		cancel   bool          // cancel ctx after the run, before classifying
		timedOut bool
		want     sandboxDeleteResult
	}{
		{name: "zero exit", script: "exit 0", want: sandboxDeleteDeleted},
		{name: "exit 0 while ctx cancelled", script: "exit 0", cancel: true, want: sandboxDeleteDeleted},
		// ptone/scion#4776: the CLI has no verified "already gone" answer
		// on delete (a missing sandbox's delete exits 0), so no output
		// turns a non-zero exit into anything but failed.
		{name: "not found", script: notFound + "; exit 1", want: sandboxDeleteFailed},
		{name: "does not exist", script: "echo 'sandbox does not exist'; exit 2", want: sandboxDeleteFailed},
		{name: "generic no such file or directory", script: "echo 'exec: /opt/sandbox/root: no such file or directory' >&2; exit 1", want: sandboxDeleteFailed},
		{name: "not running wording on delete", script: "echo 'Error: sandbox sb is not running' >&2; exit 1", want: sandboxDeleteFailed},
		{name: "other failure", script: "echo 'permission denied' >&2; exit 1", want: sandboxDeleteFailed},
		{name: "non-zero exit, no output", script: "exit 1", want: sandboxDeleteFailed},
		{name: "not found, killed by a signal", script: notFound + "; kill -9 $$", want: sandboxDeleteFailed},
		{name: "not found, killed by ctx cancellation", script: notFound + "; exec sleep 5", timeout: 200 * time.Millisecond, want: sandboxDeleteFailed},
		{name: "not found exit while ctx cancelled", script: notFound + "; exit 1", cancel: true, want: sandboxDeleteFailed},
		{name: "workaround timeout", script: "exit 0", timedOut: true, want: sandboxDeleteTimedOut},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.timeout > 0 {
				ctx, cancel = context.WithTimeout(ctx, tt.timeout)
				defer cancel()
			}
			out, err := runDeleteScript(ctx, tt.script)
			if tt.cancel {
				cancel()
			}
			if got := classifySandboxDelete(err, tt.timedOut); got != tt.want {
				t.Errorf("classifySandboxDelete(out=%q, err=%v) = %s, want %s", out, err, got, tt.want)
			}
		})
	}
}

func TestSandboxCommandOutputForLog(t *testing.T) {
	short := "Error: sandbox not found"
	if got := sandboxCommandOutputForLog(short); got != short {
		t.Errorf("sandboxCommandOutputForLog(short) = %q, want it unchanged", got)
	}
	long := strings.Repeat("x", maxSandboxCommandOutput)
	got := sandboxCommandOutputForLog(long)
	if want := strings.Repeat("x", maxSandboxCommandOutputLog) + "...(truncated)"; got != want {
		t.Errorf("sandboxCommandOutputForLog(64 KiB) has length %d, want %d", len(got), len(want))
	}
}

// withDeleteWorkaround sets deleteWorkaroundEnabled for one test.
func withDeleteWorkaround(t *testing.T, enabled bool) {
	t.Helper()
	prev := deleteWorkaroundEnabled
	deleteWorkaroundEnabled = enabled
	t.Cleanup(func() { deleteWorkaroundEnabled = prev })
}

// S0h: with the workaround off, a delete of an already-gone sandbox exits
// 0 on the real CLI (ptone/scion#4776), which is a success that removes
// the entry.
func TestCloudRunSandboxDeletePlain_GoneIsSuccess(t *testing.T) {
	withDeleteWorkaround(t, false)
	rt := newWorkaroundTestRuntime(t, writeMockBin(t, `exit 0`))
	rt.state.add(&sandboxStateEntry{SandboxName: "sb-gone", AgentID: "agent-gone"})

	if err := rt.Delete(context.Background(), RunRef{ID: "sb-gone"}); err != nil {
		t.Fatalf("Delete() error = %v, want nil (already gone counts as deleted)", err)
	}
	if rt.state.get("sb-gone") != nil {
		t.Error("state entry still present after deleting an already-gone sandbox")
	}
}

// ptone/scion#4776: with the workaround off, a delete that fails with a
// generic "no such file or directory" (a missing runtime binary or root
// directory, not a missing sandbox) is a failure: Delete returns an error
// and the entry is kept, so a live sandbox is not left untracked. The same
// holds for the old unverified not-found wording.
func TestCloudRunSandboxDeletePlain_GenericNoSuchFileIsFailed(t *testing.T) {
	for _, tc := range []struct{ name, script string }{
		{"no such file or directory", `echo "exec: /opt/sandbox/root: no such file or directory" >&2; exit 1`},
		{"unverified not found wording", `echo "Error: sandbox \"$3\" not found" >&2; exit 1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withDeleteWorkaround(t, false)
			rt := newWorkaroundTestRuntime(t, writeMockBin(t, tc.script))
			rt.state.add(&sandboxStateEntry{SandboxName: "sb-live", AgentID: "agent-live"})

			if err := rt.Delete(context.Background(), RunRef{ID: "sb-live"}); err == nil {
				t.Fatal("Delete() error = nil, want the delete failure")
			}
			if rt.state.get("sb-live") == nil {
				t.Error("state entry removed after a delete that failed with a generic error")
			}
		})
	}
}

func TestCloudRunSandboxDeletePlain_FailedStillErrors(t *testing.T) {
	withDeleteWorkaround(t, false)
	rt := newWorkaroundTestRuntime(t, writeMockBin(t, `echo "permission denied" >&2; exit 1`))
	rt.state.add(&sandboxStateEntry{SandboxName: "sb-fail", AgentID: "agent-fail"})

	if err := rt.Delete(context.Background(), RunRef{ID: "sb-fail"}); err == nil {
		t.Fatal("Delete() error = nil, want the delete failure")
	}
	if rt.state.get("sb-fail") == nil {
		t.Error("state entry removed after a failed delete")
	}
}

func TestCloudRunSandboxDeletePlain_CancelledIsFailed(t *testing.T) {
	withDeleteWorkaround(t, false)
	rt := newWorkaroundTestRuntime(t, writeMockBin(t, `echo "Error: sandbox \"$3\" not found" >&2; exec sleep 5`))
	rt.state.add(&sandboxStateEntry{SandboxName: "sb-ctx", AgentID: "agent-ctx"})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := rt.Delete(ctx, RunRef{ID: "sb-ctx"}); err == nil {
		t.Fatal("Delete() error = nil, want an error for a delete killed by cancellation")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("Delete() took %v, want it to return soon after cancellation", elapsed)
	}
	if rt.state.get("sb-ctx") == nil {
		t.Error("state entry removed after a cancelled delete")
	}
}

// The workaround path's results are unchanged: a non-zero exit, whatever
// its output, still removes the entry (only its logged result changes).
func TestCloudRunSandboxDeleteWithTimeout_GoneOutputUnchanged(t *testing.T) {
	withDeleteWorkaround(t, true)
	rt := newWorkaroundTestRuntime(t, writeMockBin(t, `echo "Error: sandbox \"$3\" not found" >&2; exit 1`))
	rt.state.add(&sandboxStateEntry{SandboxName: "sb-gone", AgentID: "agent-gone"})

	if err := rt.Delete(context.Background(), RunRef{ID: "sb-gone"}); err != nil {
		t.Fatalf("Delete() error = %v, want nil", err)
	}
	if rt.state.get("sb-gone") != nil {
		t.Error("state entry still present after a not-found delete on the workaround path")
	}
}
