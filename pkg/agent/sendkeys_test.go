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
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// runningAgent returns a minimal, running, identity-bound fixture for
// SendKeys tests: slug "test-agent", project "proj-1", agent_id "agent-abc".
func runningAgent() api.AgentInfo {
	return api.AgentInfo{
		ContainerID: "container-1",
		Name:        "test-agent",
		ProjectID:   "proj-1",
		Phase:       string(state.PhaseRunning),
		Labels: map[string]string{
			"scion.name": "test-agent",
			"agent_id":   "agent-abc",
		},
	}
}

// execRecord captures one call into a mock Runtime, whichever method made
// it: argv is always populated (joined by spaces); stdin is only non-empty
// for an ExecWithStdin call.
type execRecord struct {
	argv  string
	stdin string
}

// newSendKeysMock builds a MockRuntime that answers List with agents and
// records every Exec/ExecWithStdin call into captured, succeeding every call
// unless execErr is non-nil.
func newSendKeysMock(agents []api.AgentInfo, captured *[]execRecord, execErr error) *runtime.MockRuntime {
	var mu sync.Mutex
	record := func(cmd []string, stdin string) {
		mu.Lock()
		*captured = append(*captured, execRecord{argv: strings.Join(cmd, " "), stdin: stdin})
		mu.Unlock()
	}
	return &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return agents, nil
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			record(cmd, "")
			if execErr != nil {
				return "", execErr
			}
			return "", nil
		},
		ExecWithStdinFunc: func(ctx context.Context, id string, cmd []string, stdin io.Reader) (string, error) {
			data, _ := io.ReadAll(stdin)
			record(cmd, string(data))
			if execErr != nil {
				return "", execErr
			}
			return "", nil
		},
	}
}

// TestSendKeys_ArgvExactness covers AC "Enter, Escape, C-c, Unicode, spaces
// and literal @text reach the exact expected argv with no added Enter", and
// the adapter argv/stdin spy for the stdin-based delivery mechanism: SendKeys
// must call "tmux has-session -t scion:0" (the readiness probe, a plain
// Exec) followed by ExecWithStdin with the fixed argv "tmux source-file -"
// and a stdin payload built by sendKeysScript — never trimmed, split,
// tokenized, or followed by an implicit Enter (contrast deliverImmediate's
// message path, which does add one).
func TestSendKeys_ArgvExactness(t *testing.T) {
	cases := []struct {
		name string
		keys string
	}{
		{"Enter", "Enter"},
		{"Escape", "Escape"},
		{"C-c", "C-c"},
		{"spaces_not_a_sequence", "Up Up Enter"},
		{"literal_at_text", "@builder do the thing"},
		{"unicode", "héllo 世界 🎉"},
		{"whitespace_only", "   "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var captured []execRecord
			mock := newSendKeysMock([]api.AgentInfo{runningAgent()}, &captured, nil)
			mgr := &AgentManager{Runtime: mock}

			err := mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", tc.keys)
			if err != nil {
				t.Fatalf("SendKeys failed: %v", err)
			}

			if len(captured) == 0 {
				t.Fatalf("expected at least one call, got none")
			}
			last := captured[len(captured)-1]
			wantArgv := "tmux source-file -"
			if last.argv != wantArgv {
				t.Errorf("argv = %q, want %q", last.argv, wantArgv)
			}
			wantStdin := sendKeysScript(keysTarget, tc.keys)
			if last.stdin != wantStdin {
				t.Errorf("stdin = %q, want %q", last.stdin, wantStdin)
			}
			// No step must add a trailing Enter (unlike deliverImmediate's
			// message path), and the payload — raw or in any reversible
			// encoding of it — must never appear as a process argument, on
			// any recorded call including the readiness probe: the transport
			// requirement is "no payload, and no reversible encoding of it, in
			// argv," not merely "no raw payload."
			encoded := tmuxOctalEscape(tc.keys)
			script := sendKeysScript(keysTarget, tc.keys)
			for _, c := range captured {
				if c.argv == "tmux send-keys -t scion:0 Enter" {
					t.Errorf("SendKeys must never send an implicit Enter, but got: %v", captured)
				}
				if strings.Contains(c.argv, tc.keys) && tc.keys != "" {
					t.Errorf("payload must not appear in argv, got: %q", c.argv)
				}
				if strings.Contains(c.argv, encoded) && encoded != "" {
					t.Errorf("octal-escaped payload must not appear in argv, got: %q", c.argv)
				}
				if strings.Contains(c.argv, script) && script != "" {
					t.Errorf("generated tmux command must not appear in argv, got: %q", c.argv)
				}
			}
		})
	}
}

// TestSendKeys_InvalidKeysShapeNeverExecutes covers the AC "empty/NUL/
// oversize/invalid shapes never execute" at the primitive level itself
// (renamed from
// TestSendKeys_EmptyKeysPassedThroughVerbatim, which asserted the opposite
// of what the test now does): SendKeys calls agentkeys.ValidateKeys before
// any resolution or Exec attempt, so a local-mode caller invoking this
// primitive directly — without going through the runtimebroker's own
// ValidateKeys enforcement (handlers_keys_test.go's
// TestSendKeys_HTTP_InvalidKeysShape table) — still gets the same
// guarantee, never a silent "empty becomes Enter" (contrast
// deliverImmediate's message path, which does have that special case for
// its own, unrelated reasons). Includes an invalid-UTF-8 case that the JSON
// broker route can never actually reach (encoding/json repairs invalid
// UTF-8 to U+FFFD on decode — see TestSendKeys_HTTP_InvalidKeysShape's doc
// comment) but a direct primitive caller can.
func TestSendKeys_InvalidKeysShapeNeverExecutes(t *testing.T) {
	cases := []struct {
		name string
		keys string
	}{
		{"empty", ""},
		{"nul_byte", "abc\x00def"},
		{"oversize", strings.Repeat("a", agentkeys.MaxBytes+1)},
		{"invalid_utf8", "abc\xff"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var captured []execRecord
			mock := newSendKeysMock([]api.AgentInfo{runningAgent()}, &captured, nil)
			mgr := &AgentManager{Runtime: mock}

			err := mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", tc.keys)
			if err == nil {
				t.Fatal("expected an error")
			}
			if _, ok := agentkeys.AsValidationError(err); !ok {
				t.Errorf("SendKeys error = %v, want an *agentkeys.ValidationError", err)
			}
			if len(captured) != 0 {
				t.Errorf("expected zero Exec calls for invalid keys shape %q, got %v", tc.name, captured)
			}
		})
	}
}

// TestSendKeys_TargetNotFound_NoMatch covers the "missing" case of AK-45/46:
// no container matches (projectID, agentSlug) at all.
func TestSendKeys_TargetNotFound_NoMatch(t *testing.T) {
	var captured []execRecord
	mock := newSendKeysMock(nil, &captured, nil)
	mgr := &AgentManager{Runtime: mock}

	err := mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", "C-c")
	if !errors.Is(err, agentkeys.ErrTargetNotFound) {
		t.Fatalf("SendKeys error = %v, want agentkeys.ErrTargetNotFound", err)
	}
	if len(captured) != 0 {
		t.Fatalf("expected no Exec calls when target is not found, got %v", captured)
	}
}

// TestSendKeys_TargetNotFound_AgentIDMismatch covers AK-45: a container
// resolves by slug/project, but its "agent_id" label does not match the
// caller's expected ID — e.g. a same-slug agent recreated inside the
// execute-before window. Must fail closed to not_found, never fall back to
// the slug-only match.
func TestSendKeys_TargetNotFound_AgentIDMismatch(t *testing.T) {
	agent := runningAgent()
	agent.Labels["agent_id"] = "some-other-agent"

	var captured []execRecord
	mock := newSendKeysMock([]api.AgentInfo{agent}, &captured, nil)
	mgr := &AgentManager{Runtime: mock}

	err := mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", "C-c")
	if !errors.Is(err, agentkeys.ErrTargetNotFound) {
		t.Fatalf("SendKeys error = %v, want agentkeys.ErrTargetNotFound", err)
	}
	if len(captured) != 0 {
		t.Fatalf("expected no Exec calls on an agent_id mismatch, got %v", captured)
	}
}

// TestSendKeys_TargetNotFound_MissingLabel covers AK-46: a container started
// outside the Hub's dispatch path (or before SCION_AGENT_ID injection
// existed) has no "agent_id" label at all. Must fail closed, never a
// slug-only match.
func TestSendKeys_TargetNotFound_MissingLabel(t *testing.T) {
	agent := runningAgent()
	delete(agent.Labels, "agent_id")

	var captured []execRecord
	mock := newSendKeysMock([]api.AgentInfo{agent}, &captured, nil)
	mgr := &AgentManager{Runtime: mock}

	err := mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", "C-c")
	if !errors.Is(err, agentkeys.ErrTargetNotFound) {
		t.Fatalf("SendKeys error = %v, want agentkeys.ErrTargetNotFound", err)
	}
	if len(captured) != 0 {
		t.Fatalf("expected no Exec calls on a missing agent_id label, got %v", captured)
	}
}

// TestSendKeys_CrossProjectSameSlugNeverConfused covers "Two same-name
// agents in different projects cannot be confused": a container matching
// the requested slug exists, but only in a different project than the one
// requested. The other project's container must never be selected.
func TestSendKeys_CrossProjectSameSlugNeverConfused(t *testing.T) {
	other := runningAgent()
	other.ProjectID = "proj-2"
	other.ContainerID = "container-2"
	other.Labels = map[string]string{"scion.name": "test-agent", "agent_id": "agent-abc", "scion.project_id": "proj-2"}

	var capturedCmd []string
	// The runtime's List is expected to be called with a "scion.project_id"
	// filter; a real runtime backend would exclude proj-2's container from
	// the result for a proj-1 query, so the mock replicates that filtering
	// rather than returning both indiscriminately (which would hide a bug
	// where SendKeys forgot to pass the filter at all).
	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			wantProject, ok := filter["scion.project_id"]
			if !ok {
				t.Errorf("List called without a scion.project_id filter: %v", filter)
				return []api.AgentInfo{other}, nil
			}
			if other.ProjectID == wantProject {
				return []api.AgentInfo{other}, nil
			}
			return nil, nil
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			capturedCmd = append(capturedCmd, strings.Join(cmd, " "))
			return "", nil
		},
	}
	mgr := &AgentManager{Runtime: mock}

	err := mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", "C-c")
	if !errors.Is(err, agentkeys.ErrTargetNotFound) {
		t.Fatalf("SendKeys error = %v, want agentkeys.ErrTargetNotFound (proj-2's agent must not be selected for a proj-1 request)", err)
	}
	if len(capturedCmd) != 0 {
		t.Fatalf("expected no Exec calls against the wrong project's container, got %v", capturedCmd)
	}
}

// TestSendKeys_AgentNotRunning covers AK-26: a resolved, identity-confirmed
// target that is not running must fail with ErrAgentNotRunning, with no
// Exec attempt (no wake/start).
func TestSendKeys_AgentNotRunning(t *testing.T) {
	agent := runningAgent()
	agent.Phase = string(state.PhaseStopped)

	var captured []execRecord
	mock := newSendKeysMock([]api.AgentInfo{agent}, &captured, nil)
	mgr := &AgentManager{Runtime: mock}

	err := mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", "C-c")
	if !errors.Is(err, agentkeys.ErrAgentNotRunning) {
		t.Fatalf("SendKeys error = %v, want agentkeys.ErrAgentNotRunning", err)
	}
	if len(captured) != 0 {
		t.Fatalf("expected no Exec calls when agent is not running, got %v", captured)
	}
}

// TestSendKeys_TerminalNotReady covers AK-27: a running, identity-confirmed
// target whose tmux session is not accepting input (has-session fails) must
// fail with ErrTerminalNotReady, and the send-keys call itself must never
// run.
func TestSendKeys_TerminalNotReady(t *testing.T) {
	agent := runningAgent()

	var capturedCmd []string
	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{agent}, nil
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			capturedCmd = append(capturedCmd, strings.Join(cmd, " "))
			if len(cmd) >= 2 && cmd[1] == "has-session" {
				return "", errors.New("no such session")
			}
			return "", nil
		},
	}
	mgr := &AgentManager{Runtime: mock}

	err := mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", "C-c")
	if !errors.Is(err, agentkeys.ErrTerminalNotReady) {
		t.Fatalf("SendKeys error = %v, want agentkeys.ErrTerminalNotReady", err)
	}
	for _, c := range capturedCmd {
		if strings.Contains(c, "send-keys") {
			t.Fatalf("send-keys must not run when the terminal is not ready, but got: %v", capturedCmd)
		}
	}
}

// TestSendKeys_PlainErrorOnAmbiguousExecFailure covers "Any execution error
// with uncertain terminal effect is represented as unknown, not definitely
// undelivered": once resolution/identity/readiness have all passed, a
// failure from the send-keys Exec call itself (which may have partially
// run) must be a plain error, never one of the three sentinels.
func TestSendKeys_PlainErrorOnAmbiguousExecFailure(t *testing.T) {
	agent := runningAgent()
	execErr := errors.New("tmux: some ambiguous failure")

	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{agent}, nil
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			if len(cmd) >= 2 && cmd[1] == "has-session" {
				return "", nil
			}
			return "", execErr
		},
	}
	mgr := &AgentManager{Runtime: mock}

	err := mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", "C-c")
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, agentkeys.ErrTargetNotFound) || errors.Is(err, agentkeys.ErrAgentNotRunning) || errors.Is(err, agentkeys.ErrTerminalNotReady) || errors.Is(err, ErrKeysNotStarted) {
		t.Fatalf("an ambiguous Exec failure must never be reported as one of the proven-before-execution sentinels, got: %v", err)
	}
	// The underlying error's *text* must still be discoverable (for
	// diagnostics), but not via errors.Is/errors.As — see
	// TestSendKeys_PostExecFailureWrappingCtxErrorIsNotErrKeysNotStarted for
	// why the chain is deliberately broken.
	if !strings.Contains(err.Error(), execErr.Error()) {
		t.Fatalf("expected the underlying Exec error's text to be present, got: %v", err)
	}
	if errors.Is(err, execErr) {
		t.Fatalf("the underlying Exec error must not be reachable via errors.Is (chain must be broken), got: %v", err)
	}
}

// TestSendKeys_StdinTransportFailureIsAmbiguous covers a transport failure
// on the delivery mechanism itself (ExecWithStdin, not Exec's fallback to
// it): once the readiness probe has passed, a failure from the stdin-based
// call must be a plain error — never one of the proven-before-execution
// sentinels — matching TestSendKeys_PlainErrorOnAmbiguousExecFailure's
// requirement, exercised against the actual method SendKeys calls for
// delivery rather than MockRuntime's Exec-fallback behavior.
func TestSendKeys_StdinTransportFailureIsAmbiguous(t *testing.T) {
	agent := runningAgent()
	transportErr := errors.New("transport: connection reset")
	var gotStdin string

	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{agent}, nil
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			return "", nil // the has-session probe
		},
		ExecWithStdinFunc: func(ctx context.Context, id string, cmd []string, stdin io.Reader) (string, error) {
			data, _ := io.ReadAll(stdin)
			gotStdin = string(data)
			return "", transportErr
		},
	}
	mgr := &AgentManager{Runtime: mock}

	err := mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", "C-c")
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, agentkeys.ErrTargetNotFound) || errors.Is(err, agentkeys.ErrAgentNotRunning) || errors.Is(err, agentkeys.ErrTerminalNotReady) || errors.Is(err, ErrKeysNotStarted) {
		t.Fatalf("a stdin-transport failure must never be reported as one of the proven-before-execution sentinels, got: %v", err)
	}
	if gotStdin != sendKeysScript(keysTarget, "C-c") {
		t.Fatalf("ExecWithStdin received stdin %q, want the generated script", gotStdin)
	}
}

// TestSendKeys_PostExecFailureWrappingCtxErrorIsNotErrKeysNotStarted is
// the core regression test for this behavior: a send-keys Exec
// failure that itself wraps a context error (e.g. a Kubernetes exec stream
// cancelled mid-call, or os/exec's Wait returning ctx.Err() after a
// successful cancel) must not be classified as "proven not to have
// started" — only SendKeys's own pre-Exec checks may produce that
// classification. Reproduces the exact repro shape that surfaces the bug: the mock
// records that send-keys ran, then returns an error wrapping
// context.DeadlineExceeded.
func TestSendKeys_PostExecFailureWrappingCtxErrorIsNotErrKeysNotStarted(t *testing.T) {
	agent := runningAgent()
	sendKeysRan := false

	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{agent}, nil
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			if len(cmd) >= 2 && cmd[1] == "has-session" {
				return "", nil
			}
			sendKeysRan = true
			// Simulates a backend whose Exec observed the call's own
			// context expiring *during* a genuinely-started call, and
			// wrapped that into its returned error — this must not be
			// mistaken for SendKeys's own pre-Exec expiry signal.
			return "", fmt.Errorf("exec stream: %w", context.DeadlineExceeded)
		},
	}
	mgr := &AgentManager{Runtime: mock}

	err := mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", "C-c")

	if !sendKeysRan {
		t.Fatal("test setup error: send-keys Exec never ran")
	}
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrKeysNotStarted) {
		t.Fatalf("a post-Exec failure that wraps a context error must never match ErrKeysNotStarted (it is not proven to have failed before Exec began), got: %v", err)
	}
	// SendKeys must not join the send-keys Exec error
	// with %w, so the mock's own context.DeadlineExceeded is not reachable
	// via errors.Is on SendKeys's returned value either — the handler must
	// be unable to reach this by accident even if it (incorrectly) checked
	// for context.DeadlineExceeded/Canceled directly instead of matching
	// ErrKeysNotStarted's identity.
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SendKeys's returned error must not wrap context.DeadlineExceeded via errors.Is, got: %v", err)
	}
	// SendKeys deliberately breaks the error chain here (%v, not %w — see
	// ErrKeysNotStarted's doc comment), so errors.Is(err,
	// context.DeadlineExceeded) is correctly false on the returned error
	// itself; that is the fix, not a gap. What must still hold is that the
	// mock's own crafted error really did wrap context.DeadlineExceeded
	// (confirming this test reproduces the intended repro shape) and that
	// its text survives into SendKeys's returned error for diagnostics.
	mockErr := fmt.Errorf("exec stream: %w", context.DeadlineExceeded)
	if !errors.Is(mockErr, context.DeadlineExceeded) {
		t.Fatal("test setup error: mock's own error does not wrap context.DeadlineExceeded")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("expected the underlying error's text to survive for diagnostics, got: %v", err)
	}
}

// TestSendKeys_ProbeFailureDuringExpiredDeadlineIsNotStarted covers the
// case where the admission deadline fires during the terminal-readiness
// probe itself: the outcome must be "proven not to have started"
// (ErrKeysNotStarted), not ErrTerminalNotReady — the session's actual
// readiness was never established either way, but only one of these two
// outcomes is honest about why the probe failed.
func TestSendKeys_ProbeFailureDuringExpiredDeadlineIsNotStarted(t *testing.T) {
	agent := runningAgent()
	ctx, cancel := context.WithCancel(context.Background())

	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{agent}, nil
		},
		ExecFunc: func(execCtx context.Context, id string, cmd []string) (string, error) {
			if len(cmd) >= 2 && cmd[1] == "has-session" {
				// Simulates the deadline firing while the probe itself was
				// in flight: cancel the caller's ctx before the probe
				// returns its own (unrelated) failure.
				cancel()
				return "", errors.New("no such session")
			}
			return "", nil
		},
	}
	mgr := &AgentManager{Runtime: mock}

	err := mgr.SendKeys(ctx, "proj-1", "test-agent", "agent-abc", "C-c")
	if !errors.Is(err, ErrKeysNotStarted) {
		t.Fatalf("SendKeys error = %v, want an error wrapping ErrKeysNotStarted", err)
	}
	if errors.Is(err, agentkeys.ErrTerminalNotReady) {
		t.Fatalf("a probe failure coinciding with ctx expiry must not be reported as ErrTerminalNotReady, got: %v", err)
	}
}

// TestSendKeys_DeadlineExpiredWhileWaitingForLock is the deadline-expiry-
// after-wait test: SendKeys must not inject once its ctx's deadline has
// passed while it was waiting for the per-target injection lock (modeling
// the contract's "after control-channel semaphore/target-lock wait"
// enforcement point) — even though the lock does eventually become free.
func TestSendKeys_DeadlineExpiredWhileWaitingForLock(t *testing.T) {
	agent := runningAgent()

	var capturedCmd []string
	var mu sync.Mutex
	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{agent}, nil
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			mu.Lock()
			capturedCmd = append(capturedCmd, strings.Join(cmd, " "))
			mu.Unlock()
			return "", nil
		},
	}
	mgr := &AgentManager{Runtime: mock}

	// Hold the injection lock for this target for longer than the deadline
	// SendKeys will be given below, simulating a saturated critical section
	// (e.g. a slow concurrent message delivery). Keyed by the resolved
	// container ID (see injectionLock's doc comment), matching what
	// SendKeys itself will look up once it resolves the fixture above.
	lock := mgr.injectionLock(agent.ContainerID)
	if err := lock.Lock(context.Background()); err != nil {
		t.Fatalf("failed to seed the lock: %v", err)
	}
	release := make(chan struct{})
	go func() {
		<-release
		lock.Unlock()
	}()
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	err := mgr.SendKeys(ctx, "proj-1", "test-agent", "agent-abc", "C-c")
	if err == nil {
		t.Fatal("expected SendKeys to fail once its deadline expired while waiting for the lock")
	}
	if !errors.Is(err, ErrKeysNotStarted) {
		t.Fatalf("SendKeys error = %v, want an error wrapping ErrKeysNotStarted", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(capturedCmd) != 0 {
		t.Fatalf("expected no Exec calls once the admission deadline expired before the lock was acquired, got %v", capturedCmd)
	}
}

// TestSendKeys_ConcurrentWithInterruptMessage_NoInterleave covers "Concurrent
// keys and buffered/interrupt messages cannot interleave manager injection
// sequences": SendKeys and an interrupt Message call for the same target,
// launched concurrently, must never have their tmux Exec calls interleaved —
// one call's full sequence of Exec invocations must complete before the
// other's begins.
//
// This is checked by recording the ordered sequence of caller tags across
// every Exec call (not just whether two calls were ever simultaneously
// "active"): a lock-free A,B,A ordering with no time overlap at all would
// pass an overlap-only check but still prove the two callers' Exec sequences
// were not each contiguous, which is what the AC actually requires. A
// correctly serialized run produces exactly one transition in the recorded
// sequence (all of one caller's Execs, then all of the other's, in either
// order) — this test asserts exactly that, and was confirmed to fail 3/3
// runs with injectionLock's Lock call removed from SendKeys and
// deliverImmediate.
func TestSendKeys_ConcurrentWithInterruptMessage_NoInterleave(t *testing.T) {
	agent := runningAgent()

	var mu sync.Mutex
	var sequence []string
	record := func(who string) {
		mu.Lock()
		sequence = append(sequence, who)
		mu.Unlock()
	}
	leaveDelay := 3 * time.Millisecond

	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{agent}, nil
		},
		// Tags which logical call (SendKeys vs. the interrupt message) is
		// currently inside an Exec invocation via a ctx value set by each
		// goroutine below, recording the tag on every call.
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			record(currentCaller(ctx))
			time.Sleep(leaveDelay)
			return "", nil
		},
	}
	mgr := &AgentManager{Runtime: mock}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ctx := withCaller(context.Background(), "keys")
		if err := mgr.SendKeys(ctx, "proj-1", "test-agent", "agent-abc", "C-c"); err != nil {
			t.Errorf("SendKeys failed: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		ctx := withCaller(context.Background(), "interrupt")
		// projectID is irrelevant to which lock the two calls share now
		// (injectionLock keys on the resolved container ID, not caller
		// strings), but both resolve the same fixture regardless.
		if err := mgr.deliverImmediate(ctx, "test-agent", "proj-1", "", true); err != nil {
			t.Errorf("deliverImmediate failed: %v", err)
		}
	}()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(sequence) < 2 {
		t.Fatalf("expected at least 2 recorded Exec calls (one per caller), got %v", sequence)
	}
	transitions := 0
	for i := 1; i < len(sequence); i++ {
		if sequence[i] != sequence[i-1] {
			transitions++
		}
	}
	if transitions != 1 {
		t.Fatalf("SendKeys and an interrupt message interleaved their tmux Exec calls for the same target: sequence = %v (want exactly one transition between callers, got %d)", sequence, transitions)
	}
}

// TestMessageRaw_ConcurrentWithSendKeys_NoInterleave covers a gap where
// MessageRaw (the legacy raw-keys primitive, pending Phase 4
// removal) previously did not take injectionLock at all, so raw keys
// delivered through it could interleave with a concurrent SendKeys call (or
// a buffered/interrupt message) for the same target. Same
// sequence-contiguity technique as
// TestSendKeys_ConcurrentWithInterruptMessage_NoInterleave.
// TestMessageRaw_ConcurrentWithSendKeys_NoInterleave deterministically
// forces the race the injection lock exists to prevent (an earlier version
// of this test passed 30/30 runs with the
// lock removed from MessageRaw, because MessageRaw's single Exec call
// happened to always run to completion before SendKeys's two calls in
// practice, never actually landing between them).
//
// SendKeys's mocked Exec blocks on its first call — the "has-session"
// readiness probe, which the lock must be held across — after signaling
// that it has entered its critical section. Only once that signal arrives
// is the MessageRaw goroutine started, and only after giving it a fixed
// window to reach (and, with the lock present, block on) its own Exec
// attempt is the probe released to let SendKeys continue to its second
// (send-keys) call. With the lock, MessageRaw's Lock call cannot succeed
// until SendKeys's Unlock — after both of its calls — so the sequence must
// be keys, keys, raw. Without it, MessageRaw's unblocked Exec call lands
// inside that window, producing keys, raw, keys instead. Confirmed to fail
// with the lock removed from MessageRaw, and confirmed again here via a
// temporary revert-and-retest before restoring the fix.
func TestMessageRaw_ConcurrentWithSendKeys_NoInterleave(t *testing.T) {
	agent := runningAgent()

	var mu sync.Mutex
	var sequence []string
	record := func(who string) {
		mu.Lock()
		sequence = append(sequence, who)
		mu.Unlock()
	}

	probeEntered := make(chan struct{})
	releaseProbe := make(chan struct{})

	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{agent}, nil
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			record(currentCaller(ctx))
			if currentCaller(ctx) == "keys" && len(cmd) >= 2 && cmd[1] == "has-session" {
				// SendKeys's readiness probe — its first of two Exec calls,
				// made while (with the lock present) still holding the
				// injection lock. Signal entry, then hold here until the
				// test decides MessageRaw has had its chance to race in.
				close(probeEntered)
				<-releaseProbe
			}
			return "", nil
		},
	}
	mgr := &AgentManager{Runtime: mock}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx := withCaller(context.Background(), "keys")
		if err := mgr.SendKeys(ctx, "proj-1", "test-agent", "agent-abc", "C-c"); err != nil {
			t.Errorf("SendKeys failed: %v", err)
		}
	}()

	<-probeEntered

	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx := withCaller(context.Background(), "raw")
		if err := mgr.MessageRaw(ctx, "test-agent", "", "Escape"); err != nil {
			t.Errorf("MessageRaw failed: %v", err)
		}
	}()

	// Give MessageRaw time to reach its own Exec attempt — with the lock
	// present it blocks there; without it, it completes within this window,
	// landing between SendKeys's two calls.
	time.Sleep(50 * time.Millisecond)
	close(releaseProbe)

	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(sequence) != 4 {
		t.Fatalf("expected exactly 4 recorded Exec calls (3 from SendKeys — version check, probe, send — and 1 from MessageRaw), got %v", sequence)
	}
	want := []string{"keys", "keys", "keys", "raw"}
	for i := range want {
		if sequence[i] != want[i] {
			t.Fatalf("SendKeys and MessageRaw interleaved their tmux Exec calls for the same target: sequence = %v, want %v", sequence, want)
		}
	}
}

// callerCtxKey/withCaller/currentCaller let the concurrency test above tag
// which logical call (SendKeys vs. the interrupt message) is currently
// inside an Exec invocation, without depending on argv shape (SendKeys's
// has-session probe and the interrupt path's send-keys call have different
// argv anyway, but tagging via ctx keeps the test's intent explicit).
type callerCtxKey struct{}

func withCaller(ctx context.Context, who string) context.Context {
	return context.WithValue(ctx, callerCtxKey{}, who)
}

func currentCaller(ctx context.Context) string {
	who, _ := ctx.Value(callerCtxKey{}).(string)
	return who
}

// TestTmuxOctalEscape covers the byte-for-byte octal-escape encoding used to
// embed keys inside the generated tmux command: every byte becomes a
// three-digit octal escape, so no byte — including one that would otherwise
// need individual handling, such as a quote, a backslash, or a trailing
// separator character — is ever written unescaped.
func TestTmuxOctalEscape(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"A", `\101`},
		{";", `\073`},
		{`"`, `\042`},
		{`\`, `\134`},
		{"a;b", `\141\073\142`},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := tmuxOctalEscape(tc.in)
			if got != tc.want {
				t.Errorf("tmuxOctalEscape(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSendKeysScript covers the exact generated command line SendKeys
// supplies on stdin.
func TestSendKeysScript(t *testing.T) {
	got := sendKeysScript("scion:0", "Enter")
	want := "send-keys -t scion:0 -- \"\\105\\156\\164\\145\\162\"\n"
	if got != want {
		t.Errorf("sendKeysScript(...) = %q, want %q", got, want)
	}
}

// TestSendKeys_StdinScriptForSpecialCharacters is the argv/stdin spy for
// inputs that would have needed special-casing under a process-argument
// invocation (a trailing ';') and others that exercise the same encoding
// path (quotes, backslashes, embedded semicolons, newlines) — now handled
// uniformly by tmuxOctalEscape. TestRealTmuxSendKeys separately proves the
// round trip against a real tmux server.
func TestSendKeys_StdinScriptForSpecialCharacters(t *testing.T) {
	cases := []string{";", "abc;", `a"b`, `a\b`, "a;b;c", "line1\nline2"}
	for _, keys := range cases {
		t.Run(keys, func(t *testing.T) {
			var captured []execRecord
			mock := newSendKeysMock([]api.AgentInfo{runningAgent()}, &captured, nil)
			mgr := &AgentManager{Runtime: mock}

			if err := mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", keys); err != nil {
				t.Fatalf("SendKeys failed: %v", err)
			}

			last := captured[len(captured)-1]
			if last.argv != "tmux source-file -" {
				t.Errorf("argv = %q, want the fixed argv carrying no payload", last.argv)
			}
			want := sendKeysScript(keysTarget, keys)
			if last.stdin != want {
				t.Errorf("stdin = %q, want %q", last.stdin, want)
			}
		})
	}
}

// TestSendKeys_UnscopedProjectRejected covers #2193's "reject unscoped
// target fallback": SendKeys must fail closed on an empty projectID rather
// than listing across every project.
func TestSendKeys_UnscopedProjectRejected(t *testing.T) {
	var listCalled bool
	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			listCalled = true
			return []api.AgentInfo{runningAgent()}, nil
		},
	}
	mgr := &AgentManager{Runtime: mock}

	err := mgr.SendKeys(context.Background(), "", "test-agent", "agent-abc", "C-c")
	if !errors.Is(err, agentkeys.ErrTargetNotFound) {
		t.Fatalf("SendKeys error = %v, want agentkeys.ErrTargetNotFound", err)
	}
	if listCalled {
		t.Error("SendKeys must not list agents at all for an empty (unscoped) projectID")
	}
}

// TestSendKeys_UnscopedAgentIDRejected is TestSendKeys_UnscopedProjectRejected's
// counterpart for an empty expectedAgentID: SendKeys must fail closed rather
// than binding to whatever container happens to match the slug/project.
func TestSendKeys_UnscopedAgentIDRejected(t *testing.T) {
	var listCalled bool
	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			listCalled = true
			return []api.AgentInfo{runningAgent()}, nil
		},
	}
	mgr := &AgentManager{Runtime: mock}

	err := mgr.SendKeys(context.Background(), "proj-1", "test-agent", "", "C-c")
	if !errors.Is(err, agentkeys.ErrTargetNotFound) {
		t.Fatalf("SendKeys error = %v, want agentkeys.ErrTargetNotFound", err)
	}
	if listCalled {
		t.Error("SendKeys must not list agents at all for an empty expectedAgentID")
	}
}

// TestSendKeys_UnsupportedBackend covers the case where a manager whose
// runtime backend does not support keys delivery must fail with
// ErrKeysUnsupported before any resolution or Exec attempt.
func TestSendKeys_UnsupportedBackend(t *testing.T) {
	var listCalled bool
	mock := &runtime.MockRuntime{
		NameFunc: func() string { return "cloudrun" },
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			listCalled = true
			return []api.AgentInfo{runningAgent()}, nil
		},
	}
	mgr := &AgentManager{Runtime: mock}

	err := mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", "C-c")
	if !errors.Is(err, ErrKeysUnsupported) {
		t.Fatalf("SendKeys error = %v, want ErrKeysUnsupported", err)
	}
	if listCalled {
		t.Error("SendKeys must not resolve a target at all for a backend that does not support keys delivery")
	}
}

// TestSendKeys_MarksExecCallsSensitive covers the
// SendKeys-level redaction check, adapted to what SendKeys itself is
// responsible for: it performs no logging of its own (there is nothing to
// capture here), and it cannot scrub arbitrary content out of a real
// backend's Exec error without violating "any other failure ... must be a
// plain error" (contract §4.3) — generic string-scrubbing at this layer
// would fight that requirement rather than serve it. What SendKeys does
// control, and what the actual source-level suppression
// (pkg/runtime/common.go's debug logging, the Kubernetes backend's
// stderr-embedding error — see TestRunSimpleCommand_SensitiveExec_
// SuppressesOutputInDebugLog and TestWrapExecStreamError_SensitiveOmitsStderr
// in pkg/runtime) depends on, is marking every Exec call — the version
// check, the readiness probe, and the delivery call itself — via
// runtime.WithSensitiveExec. This test proves that marking happens for all
// three.
func TestSendKeys_MarksExecCallsSensitive(t *testing.T) {
	agent := runningAgent()
	var sawSensitive []bool

	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{agent}, nil
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			sawSensitive = append(sawSensitive, runtime.IsSensitiveExec(ctx))
			return "", nil
		},
	}
	mgr := &AgentManager{Runtime: mock}

	if err := mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", "C-c"); err != nil {
		t.Fatalf("SendKeys failed: %v", err)
	}

	if len(sawSensitive) < 3 {
		t.Fatalf("expected at least 3 Exec calls (version check + readiness probe + delivery), got %d", len(sawSensitive))
	}
	for i, sensitive := range sawSensitive {
		if !sensitive {
			t.Errorf("Exec call %d was not marked sensitive via runtime.WithSensitiveExec", i)
		}
	}
}

// TestParseTmuxVersion covers the numeric major/minor extraction from
// tmux -V's own output format, including the trailing-letter suffix tmux
// itself appends to some releases (e.g. "3.3a").
func TestParseTmuxVersion(t *testing.T) {
	cases := []struct {
		in        string
		wantMajor int
		wantMinor int
		wantErr   bool
	}{
		{"tmux 3.3a", 3, 3, false},
		{"tmux 3.1", 3, 1, false},
		{"tmux 3.0a", 3, 0, false},
		{"tmux 2.9a", 2, 9, false},
		{"tmux 10.2", 10, 2, false},
		{"tmux 3.3a\n", 3, 3, false}, // trailing newline, as CombinedOutput yields
		{"", 0, 0, true},
		{"not tmux at all", 0, 0, true},
		{"tmux next-3.4", 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			major, minor, err := parseTmuxVersion(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseTmuxVersion(%q) = (%d, %d, nil), want an error", tc.in, major, minor)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTmuxVersion(%q) failed: %v", tc.in, err)
			}
			if major != tc.wantMajor || minor != tc.wantMinor {
				t.Fatalf("parseTmuxVersion(%q) = (%d, %d), want (%d, %d)", tc.in, major, minor, tc.wantMajor, tc.wantMinor)
			}
		})
	}
}

// TestTmuxVersionAtLeast covers the major/minor comparison
// checkTmuxVersionSupported uses against the delivery mechanism's floor.
func TestTmuxVersionAtLeast(t *testing.T) {
	cases := []struct {
		major, minor         int
		wantMajor, wantMinor int
		want                 bool
	}{
		// Exercised against the production constants directly, not just
		// arbitrary literals,
		// so a change to the floor itself is reflected here automatically.
		{minTmuxMajor, minTmuxMinor, minTmuxMajor, minTmuxMinor, true},          // exactly the floor
		{minTmuxMajor, minTmuxMinor + 2, minTmuxMajor, minTmuxMinor, true},      // newer minor, same major
		{minTmuxMajor + 1, 0, minTmuxMajor, minTmuxMinor, true},                 // newer major, older minor number
		{minTmuxMajor, minTmuxMinor - 1, minTmuxMajor, minTmuxMinor, false},     // older minor, same major
		{minTmuxMajor - 1, minTmuxMinor + 8, minTmuxMajor, minTmuxMinor, false}, // older major
	}
	for _, tc := range cases {
		got := tmuxVersionAtLeast(tc.major, tc.minor, tc.wantMajor, tc.wantMinor)
		if got != tc.want {
			t.Errorf("tmuxVersionAtLeast(%d, %d, %d, %d) = %v, want %v", tc.major, tc.minor, tc.wantMajor, tc.wantMinor, got, tc.want)
		}
	}
}

// TestSendKeys_TmuxVersionGate is the table-driven pin for the version
// gate's exact floor (an earlier version of this test had only a
// single below-floor case that did not pin the floor itself: a mutation
// lowering minTmuxMinor to 0 survived because tmux 3.0a was never tried).
// tmux 3.0a is the release that has octal escapes (added in 3.0) but cannot
// read "source-file -" from stdin (added in 3.1) — exactly the version the
// gate exists to catch — so it gets its own case rather than relying on
// 2.9a alone. A version at or above the floor must let the readiness probe
// and delivery run; a version below it must never reach either.
func TestSendKeys_TmuxVersionGate(t *testing.T) {
	cases := []struct {
		name          string
		versionOutput string
		wantSupported bool
	}{
		{"2.9a_below_floor", "tmux 2.9a\n", false},
		{"3.0a_below_floor_has_octal_escapes_only", "tmux 3.0a\n", false},
		{"3.1_at_floor", "tmux 3.1\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := runningAgent()
			var capturedCmd []string

			mock := &runtime.MockRuntime{
				ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{agent}, nil
				},
				ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
					capturedCmd = append(capturedCmd, strings.Join(cmd, " "))
					if len(cmd) >= 2 && cmd[1] == "-V" {
						return tc.versionOutput, nil
					}
					return "", nil
				},
			}
			mgr := &AgentManager{Runtime: mock}

			err := mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", "C-c")
			if !tc.wantSupported {
				if !errors.Is(err, ErrKeysUnsupported) {
					t.Fatalf("SendKeys error = %v, want ErrKeysUnsupported for %q", err, tc.versionOutput)
				}
				for _, c := range capturedCmd {
					if strings.Contains(c, "has-session") || strings.Contains(c, "source-file") {
						t.Fatalf("neither the readiness probe nor delivery must run below the tmux version floor, got: %v", capturedCmd)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("SendKeys failed for a supported tmux version %q: %v", tc.versionOutput, err)
			}
			var sawProbe, sawDelivery bool
			for _, c := range capturedCmd {
				if strings.Contains(c, "has-session") {
					sawProbe = true
				}
				if c == "tmux source-file -" {
					sawDelivery = true
				}
			}
			if !sawProbe || !sawDelivery {
				t.Fatalf("expected the readiness probe and delivery to run for a supported tmux version, got: %v", capturedCmd)
			}
		})
	}
}

// TestSendKeys_TmuxVersionGate_BelowFloorNeverCached exists because
// moving the cache Store above the
// floor comparison would let a below-floor result be cached as supported,
// so a second call on the same container would skip the gate entirely and
// reach delivery instead of failing closed again. Two SendKeys calls on the
// same below-floor container must both return ErrKeysUnsupported, and
// "tmux -V" must be queried on both.
func TestSendKeys_TmuxVersionGate_BelowFloorNeverCached(t *testing.T) {
	agent := runningAgent()
	var versionQueries int

	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{agent}, nil
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			if len(cmd) >= 2 && cmd[1] == "-V" {
				versionQueries++
				return "tmux 2.9a\n", nil
			}
			return "", nil
		},
	}
	mgr := &AgentManager{Runtime: mock}

	for i := 0; i < 2; i++ {
		err := mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", "C-c")
		if !errors.Is(err, ErrKeysUnsupported) {
			t.Fatalf("SendKeys call %d error = %v, want ErrKeysUnsupported", i, err)
		}
	}
	if versionQueries != 2 {
		t.Fatalf("expected tmux -V to be queried on both calls (a below-floor result must never be cached), got %d queries", versionQueries)
	}
}

// TestSendKeys_TmuxVersionGate_InconclusiveNeverCached covers the case
// where a version check that cannot be
// determined (here, the first "tmux -V" call fails) must not be cached
// either way, so a later call for the same container queries again rather
// than reusing the earlier, inconclusive result.
func TestSendKeys_TmuxVersionGate_InconclusiveNeverCached(t *testing.T) {
	agent := runningAgent()
	versionQueries := 0
	firstQueryErr := errors.New("exec: could not reach tmux")

	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{agent}, nil
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			if len(cmd) >= 2 && cmd[1] == "-V" {
				versionQueries++
				if versionQueries == 1 {
					return "", firstQueryErr
				}
				return "tmux 3.3a\n", nil
			}
			return "", nil
		},
	}
	mgr := &AgentManager{Runtime: mock}

	for i := 0; i < 2; i++ {
		if err := mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", "C-c"); err != nil {
			t.Fatalf("SendKeys call %d failed: %v", i, err)
		}
	}
	if versionQueries != 2 {
		t.Fatalf("expected tmux -V to be queried on both calls (an inconclusive result must never be cached), got %d queries", versionQueries)
	}
}

// TestSendKeys_TmuxVersionUnparseableProceeds covers the deliberate other
// half of the version gate's behavior (see checkTmuxVersionSupported's doc
// comment): a tmux -V call that cannot be run, or whose output cannot be
// parsed, is inconclusive rather than a proof of incompatibility, so
// SendKeys must still proceed to the readiness probe and delivery rather
// than failing closed to ErrKeysUnsupported on every backend that merely
// doesn't answer "tmux -V" the way a real tmux binary would (e.g. this
// package's own mocks, none of which implement it).
func TestSendKeys_TmuxVersionUnparseableProceeds(t *testing.T) {
	var captured []execRecord
	mock := newSendKeysMock([]api.AgentInfo{runningAgent()}, &captured, nil)
	mgr := &AgentManager{Runtime: mock}

	if err := mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", "C-c"); err != nil {
		t.Fatalf("SendKeys failed: %v", err)
	}
	var sawDelivery bool
	for _, c := range captured {
		if c.argv == "tmux source-file -" {
			sawDelivery = true
		}
	}
	if !sawDelivery {
		t.Fatalf("expected delivery to proceed when the tmux version cannot be determined, got: %v", captured)
	}
}

// TestSendKeys_TmuxVersionCheckCachedPerContainer covers
// checkTmuxVersionSupported's caching: once a container's tmux is
// determined to meet the floor, a second SendKeys call for the same
// container must not repeat the "tmux -V" query.
func TestSendKeys_TmuxVersionCheckCachedPerContainer(t *testing.T) {
	agent := runningAgent()
	var versionQueries int

	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{agent}, nil
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			if len(cmd) >= 2 && cmd[1] == "-V" {
				versionQueries++
				return "tmux 3.3a\n", nil
			}
			return "", nil
		},
	}
	mgr := &AgentManager{Runtime: mock}

	for i := 0; i < 2; i++ {
		if err := mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", "C-c"); err != nil {
			t.Fatalf("SendKeys call %d failed: %v", i, err)
		}
	}
	if versionQueries != 1 {
		t.Fatalf("expected exactly 1 tmux -V query across 2 SendKeys calls for the same container, got %d", versionQueries)
	}
}

// TestSendKeys_TmuxVersionCache_NotReusedAcrossRecreatedContainerName exists
// because ContainerID is not always a fresh identifier
// per container instance — on at least one backend it is a stable, reusable
// name (a Kubernetes pod name) that a later, differently-imaged container
// recreated under that same name would otherwise inherit a cached
// "supported" result from. The cache key pairs ContainerID with Image, so a
// second container sharing a ContainerID but reporting a different Image
// and a too-old tmux must still be gated, not waved through on the first
// container's cached result.
func TestSendKeys_TmuxVersionCache_NotReusedAcrossRecreatedContainerName(t *testing.T) {
	const sharedContainerID = "pod-scion-test-agent"
	first := runningAgent()
	first.ContainerID = sharedContainerID
	first.Image = "agent-image:v1"

	var currentAgent = first
	var versionOutput = "tmux 3.3a\n"
	var capturedCmd []string

	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{currentAgent}, nil
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			capturedCmd = append(capturedCmd, strings.Join(cmd, " "))
			if len(cmd) >= 2 && cmd[1] == "-V" {
				return versionOutput, nil
			}
			return "", nil
		},
	}
	mgr := &AgentManager{Runtime: mock}

	if err := mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", "C-c"); err != nil {
		t.Fatalf("first SendKeys call (supported tmux) failed: %v", err)
	}

	// A second, unrelated container recreated under the same ContainerID
	// (matching the Kubernetes pod-name-as-ContainerID case), with a
	// different image and a too-old tmux.
	second := first
	second.Image = "agent-image:v2"
	currentAgent = second
	versionOutput = "tmux 2.9a\n"
	capturedCmd = nil

	err := mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", "C-c")
	if !errors.Is(err, ErrKeysUnsupported) {
		t.Fatalf("SendKeys error = %v, want ErrKeysUnsupported for the recreated container's too-old tmux (must not reuse the first container's cached result)", err)
	}
	for _, c := range capturedCmd {
		if strings.Contains(c, "has-session") || strings.Contains(c, "source-file") {
			t.Fatalf("neither the readiness probe nor delivery must run for the recreated, below-floor container, got: %v", capturedCmd)
		}
	}
}

// TestSendKeys_CtxExpiresDuringProbe_RecheckBeforeDeliveryCatchesIt covers
// a ctx that expires during the readiness probe, where the probe's own
// Exec call nonetheless returns success (a backend whose Exec does not
// itself observe the cancellation): that case must still be caught by
// SendKeys's own recheck immediately before delivery — the probe
// succeeding must never be treated as license to proceed regardless of
// ctx. Confirmed to fail once that recheck is removed.
func TestSendKeys_CtxExpiresDuringProbe_RecheckBeforeDeliveryCatchesIt(t *testing.T) {
	agent := runningAgent()
	ctx, cancel := context.WithCancel(context.Background())
	var capturedCmd []string

	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{agent}, nil
		},
		ExecFunc: func(execCtx context.Context, id string, cmd []string) (string, error) {
			capturedCmd = append(capturedCmd, strings.Join(cmd, " "))
			if len(cmd) >= 2 && cmd[1] == "has-session" {
				// The probe itself succeeds, but the caller's ctx has
				// expired by the time it returns.
				cancel()
			}
			return "", nil
		},
	}
	mgr := &AgentManager{Runtime: mock}

	err := mgr.SendKeys(ctx, "proj-1", "test-agent", "agent-abc", "C-c")
	if !errors.Is(err, ErrKeysNotStarted) {
		t.Fatalf("SendKeys error = %v, want an error wrapping ErrKeysNotStarted", err)
	}
	for _, c := range capturedCmd {
		if strings.Contains(c, "source-file") {
			t.Fatalf("delivery must not run once ctx expired, even though the probe succeeded, got: %v", capturedCmd)
		}
	}
}

// TestSendKeys_CtxExpiresDuringVersionCheck_RecheckBeforeDeliveryCatchesIt
// is TestSendKeys_CtxExpiresDuringProbe_RecheckBeforeDeliveryCatchesIt's
// counterpart for the tmux version query: the
// "tmux -V" call itself expires the ctx and returns an inconclusive result
// (so checkTmuxVersionSupported proceeds, by design), and the mocked
// readiness probe afterward succeeds regardless of ctx — delivery must
// still never run once the recheck immediately before it observes the
// expired ctx.
func TestSendKeys_CtxExpiresDuringVersionCheck_RecheckBeforeDeliveryCatchesIt(t *testing.T) {
	agent := runningAgent()
	ctx, cancel := context.WithCancel(context.Background())
	var capturedCmd []string

	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{agent}, nil
		},
		ExecFunc: func(execCtx context.Context, id string, cmd []string) (string, error) {
			capturedCmd = append(capturedCmd, strings.Join(cmd, " "))
			if len(cmd) >= 2 && cmd[1] == "-V" {
				cancel()
				// An inconclusive result (unparseable), by design not
				// itself grounds to fail closed to ErrKeysUnsupported.
				return "not a tmux version string", nil
			}
			return "", nil
		},
	}
	mgr := &AgentManager{Runtime: mock}

	err := mgr.SendKeys(ctx, "proj-1", "test-agent", "agent-abc", "C-c")
	if !errors.Is(err, ErrKeysNotStarted) {
		t.Fatalf("SendKeys error = %v, want an error wrapping ErrKeysNotStarted", err)
	}
	for _, c := range capturedCmd {
		if strings.Contains(c, "source-file") {
			t.Fatalf("delivery must not run once ctx expired during the version query, got: %v", capturedCmd)
		}
	}
}

// TestSendKeys_CtxAlreadyExpired_PostLockRecheckRunsBeforeAnyExec covers the
// post-lock recheck specifically (removing either of SendKeys's two
// ctx.Err() checks — the post-lock recheck or the one immediately before
// delivery — independently survived the existing suite on its own).
// injectionMutex.Lock's uncontended fast path succeeds even when ctx is
// already done (a deliberate property: a free lock must not be spuriously
// refused just because ctx happens to already be expired), so passing an
// already-expired ctx to
// SendKeys, with nothing else holding its target's lock, isolates the
// post-lock recheck: if it did not exist, the version query would be the
// very next thing to run despite ctx already being done.
func TestSendKeys_CtxAlreadyExpired_PostLockRecheckRunsBeforeAnyExec(t *testing.T) {
	agent := runningAgent()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already expired before SendKeys is ever called

	var capturedCmd []string
	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{agent}, nil
		},
		ExecFunc: func(execCtx context.Context, id string, cmd []string) (string, error) {
			capturedCmd = append(capturedCmd, strings.Join(cmd, " "))
			return "", nil
		},
	}
	mgr := &AgentManager{Runtime: mock}

	err := mgr.SendKeys(ctx, "proj-1", "test-agent", "agent-abc", "C-c")
	if !errors.Is(err, ErrKeysNotStarted) {
		t.Fatalf("SendKeys error = %v, want an error wrapping ErrKeysNotStarted", err)
	}
	if len(capturedCmd) != 0 {
		t.Fatalf("expected zero Exec calls (including the version query) for an already-expired ctx, got: %v", capturedCmd)
	}
}

// TestSendKeys_TargetRevalidationFailsClosed covers the pre-delivery target
// re-verification (a correctness hardening): if a second resolution
// immediately before delivery no longer matches the original one — here, a
// different resolved ContainerID for the same (projectID, agentSlug,
// expectedAgentID) — SendKeys must fail closed to ErrTargetNotFound and
// never attempt delivery, even though the original resolution, lock,
// version check and readiness probe all succeeded against the first
// container.
func TestSendKeys_TargetRevalidationFailsClosed(t *testing.T) {
	first := runningAgent()
	second := first
	second.ContainerID = "container-2"

	calls := 0
	var capturedCmd []string
	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			calls++
			if calls == 1 {
				return []api.AgentInfo{first}, nil
			}
			return []api.AgentInfo{second}, nil
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			capturedCmd = append(capturedCmd, strings.Join(cmd, " "))
			return "", nil
		},
	}
	mgr := &AgentManager{Runtime: mock}

	err := mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", "C-c")
	if !errors.Is(err, agentkeys.ErrTargetNotFound) {
		t.Fatalf("SendKeys error = %v, want agentkeys.ErrTargetNotFound", err)
	}
	if calls < 2 {
		t.Fatalf("expected at least 2 List calls (the original resolution and the pre-delivery re-verification), got %d", calls)
	}
	for _, c := range capturedCmd {
		if strings.Contains(c, "source-file") {
			t.Fatalf("delivery must not run once the pre-delivery re-verification finds a different target, got: %v", capturedCmd)
		}
	}
}

// TestSendKeys_TargetRevalidation_DifferentAgentIDSameContainerFailsClosed
// covers the case where the second resolution returns the very same
// ContainerID as the first, but a different "agent_id" label and no
// Kubernetes block: resolveKeysTarget's own identity check (matching
// against the caller's original expectedAgentID) must reject this on the
// second call exactly as it would on the first, so SendKeys fails closed to
// ErrTargetNotFound with zero delivery calls — this must hold even for an
// implementation that resolves the re-verification's target identity by
// some means other than a full, independent resolveKeysTarget call.
func TestSendKeys_TargetRevalidation_DifferentAgentIDSameContainerFailsClosed(t *testing.T) {
	first := runningAgent()
	second := first
	second.Labels = map[string]string{
		"scion.name": "test-agent",
		"agent_id":   "agent-other",
	}

	calls := 0
	var capturedCmd []string
	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			calls++
			if calls == 1 {
				return []api.AgentInfo{first}, nil
			}
			return []api.AgentInfo{second}, nil
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			capturedCmd = append(capturedCmd, strings.Join(cmd, " "))
			return "", nil
		},
	}
	mgr := &AgentManager{Runtime: mock}

	err := mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", "C-c")
	if !errors.Is(err, agentkeys.ErrTargetNotFound) {
		t.Fatalf("SendKeys error = %v, want agentkeys.ErrTargetNotFound", err)
	}
	if calls < 2 {
		t.Fatalf("expected at least 2 List calls (the original resolution and the pre-delivery re-verification), got %d", calls)
	}
	for _, c := range capturedCmd {
		if strings.Contains(c, "source-file") {
			t.Fatalf("delivery must not run once the pre-delivery re-verification finds a different agent_id for the same ContainerID, got: %v", capturedCmd)
		}
	}
}

// TestSendKeys_TargetRevalidation_SameKubernetesUIDPasses is
// TestSendKeys_TargetRevalidationFailsClosed's positive counterpart for the
// per-instance identifier comparison: the same ContainerID and the same
// Kubernetes pod UID on both resolutions must pass re-verification and
// proceed to delivery.
func TestSendKeys_TargetRevalidation_SameKubernetesUIDPasses(t *testing.T) {
	agent := runningAgent()
	agent.Kubernetes = &api.AgentK8sMetadata{PodName: agent.ContainerID, UID: "uid-abc-123"}

	var captured []execRecord
	mock := newSendKeysMock([]api.AgentInfo{agent}, &captured, nil)
	mgr := &AgentManager{Runtime: mock}

	if err := mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", "C-c"); err != nil {
		t.Fatalf("SendKeys failed: %v", err)
	}
	var sawDelivery bool
	for _, c := range captured {
		if c.argv == "tmux source-file -" {
			sawDelivery = true
		}
	}
	if !sawDelivery {
		t.Fatalf("expected delivery to proceed when re-verification finds a matching Kubernetes UID, got: %v", captured)
	}
}

// TestSendKeys_TargetRevalidation_DifferentKubernetesUIDFailsClosed covers
// the per-instance identifier comparison's negative case: the same
// ContainerID but a different Kubernetes pod UID on the second resolution
// must fail closed to ErrTargetNotFound, even though ContainerID alone
// still matches.
func TestSendKeys_TargetRevalidation_DifferentKubernetesUIDFailsClosed(t *testing.T) {
	first := runningAgent()
	first.Kubernetes = &api.AgentK8sMetadata{PodName: first.ContainerID, UID: "uid-original"}
	second := first
	second.Kubernetes = &api.AgentK8sMetadata{PodName: first.ContainerID, UID: "uid-recreated"}

	calls := 0
	var capturedCmd []string
	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			calls++
			if calls == 1 {
				return []api.AgentInfo{first}, nil
			}
			return []api.AgentInfo{second}, nil
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			capturedCmd = append(capturedCmd, strings.Join(cmd, " "))
			return "", nil
		},
	}
	mgr := &AgentManager{Runtime: mock}

	err := mgr.SendKeys(context.Background(), "proj-1", "test-agent", "agent-abc", "C-c")
	if !errors.Is(err, agentkeys.ErrTargetNotFound) {
		t.Fatalf("SendKeys error = %v, want agentkeys.ErrTargetNotFound", err)
	}
	for _, c := range capturedCmd {
		if strings.Contains(c, "source-file") {
			t.Fatalf("delivery must not run once the pre-delivery re-verification finds a different Kubernetes UID for the same ContainerID, got: %v", capturedCmd)
		}
	}
}

// TestSendKeys_TargetRevalidationCtxExpiredIsNotStarted covers the case
// where the pre-delivery re-verification's own List call fails while ctx
// has already expired: nothing has been sent at that point, so the outcome
// must be the honest wrapNotStarted classification (matching
// ErrKeysNotStarted), not whatever plain, wrapped error resolveKeysTarget's
// own List call happened to produce for an expired ctx.
func TestSendKeys_TargetRevalidationCtxExpiredIsNotStarted(t *testing.T) {
	agent := runningAgent()
	ctx, cancel := context.WithCancel(context.Background())

	calls := 0
	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			calls++
			if calls == 1 {
				return []api.AgentInfo{agent}, nil
			}
			cancel()
			return nil, errors.New("transient listing failure")
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			return "", nil
		},
	}
	mgr := &AgentManager{Runtime: mock}

	err := mgr.SendKeys(ctx, "proj-1", "test-agent", "agent-abc", "C-c")
	if !errors.Is(err, ErrKeysNotStarted) {
		t.Fatalf("SendKeys error = %v, want an error wrapping ErrKeysNotStarted", err)
	}
	if calls < 2 {
		t.Fatalf("expected at least 2 List calls, got %d", calls)
	}
}

// TestSameTargetInstance covers sameTargetInstance's comparison directly:
// ContainerID must always match; the Kubernetes UID must also match
// whenever either side reports one — an asymmetric result (one side
// reporting a UID, the other not) fails the comparison rather than falling
// back to ContainerID alone. ContainerID alone decides the comparison only
// when neither side reports a UID.
func TestSameTargetInstance(t *testing.T) {
	base := api.AgentInfo{ContainerID: "c1"}
	cases := []struct {
		name string
		a, b api.AgentInfo
		want bool
	}{
		{"identical_no_k8s", base, base, true},
		{"different_container_id", base, api.AgentInfo{ContainerID: "c2"}, false},
		{
			"same_uid",
			api.AgentInfo{ContainerID: "c1", Kubernetes: &api.AgentK8sMetadata{UID: "u1"}},
			api.AgentInfo{ContainerID: "c1", Kubernetes: &api.AgentK8sMetadata{UID: "u1"}},
			true,
		},
		{
			"different_uid",
			api.AgentInfo{ContainerID: "c1", Kubernetes: &api.AgentK8sMetadata{UID: "u1"}},
			api.AgentInfo{ContainerID: "c1", Kubernetes: &api.AgentK8sMetadata{UID: "u2"}},
			false,
		},
		{
			// Fails closed rather than falling back to ContainerID alone:
			// an identity check must not become permissive just because the
			// data is asymmetric, even though both calls resolving through
			// the same List path makes this case unreachable today.
			"one_side_missing_k8s_info_a_has_uid",
			api.AgentInfo{ContainerID: "c1", Kubernetes: &api.AgentK8sMetadata{UID: "u1"}},
			api.AgentInfo{ContainerID: "c1"},
			false,
		},
		{
			"one_side_missing_k8s_info_b_has_uid",
			api.AgentInfo{ContainerID: "c1"},
			api.AgentInfo{ContainerID: "c1", Kubernetes: &api.AgentK8sMetadata{UID: "u1"}},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameTargetInstance(tc.a, tc.b); got != tc.want {
				t.Errorf("sameTargetInstance(%+v, %+v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
