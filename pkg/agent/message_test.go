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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestMessage(t *testing.T) {
	// Interrupt messages bypass the buffer and are delivered immediately.
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{
					ContainerID:     "agent-1",
					Name:            "test-agent",
					ContainerStatus: "Up 2 minutes",
					Labels:          map[string]string{"scion.name": "test-agent"},
				},
			}, nil
		},
	}

	var capturedCmd []string
	mockRT.ExecFunc = func(ctx context.Context, id string, cmd []string) (string, error) {
		capturedCmd = append(capturedCmd, strings.Join(cmd, " "))
		return "", nil
	}

	mgr := &AgentManager{
		Runtime: mockRT,
	}
	// Initialize buffer (not used for interrupt messages, but needed to avoid nil).
	mgr.msgBuffer = NewMessageBuffer(100*time.Millisecond, func(agentID, projectID, message string, interrupt bool) error {
		return mgr.deliverImmediate(context.Background(), agentID, projectID, message, interrupt)
	})
	defer mgr.msgBuffer.Close()

	ctx := context.Background()
	err := mgr.Message(ctx, "test-agent", "", "hello world", true)
	if err != nil {
		t.Fatalf("Message failed: %v", err)
	}

	expectedCmds := []string{
		"tmux copy-mode -q -t scion:0",
		"tmux send-keys -t scion:0 C-c",
		"tmux set-buffer -b scion-msg -- hello world",
		"tmux paste-buffer -d -b scion-msg -t scion:0 -p",
		// -d consumes the buffer, so each retry re-sets it.
		"tmux set-buffer -b scion-submit -- \r",
		"tmux paste-buffer -d -b scion-submit -t scion:0",
		"tmux set-buffer -b scion-submit -- \r",
		"tmux paste-buffer -d -b scion-submit -t scion:0",
		"tmux set-buffer -b scion-submit -- \r",
		"tmux paste-buffer -d -b scion-submit -t scion:0",
	}

	if len(capturedCmd) != len(expectedCmds) {
		t.Fatalf("Expected %d commands, got %d", len(expectedCmds), len(capturedCmd))
	}

	for i, cmd := range capturedCmd {
		if cmd != expectedCmds[i] {
			t.Errorf("Expected cmd %d to be '%s', got '%s'", i, expectedCmds[i], cmd)
		}
	}
}

func TestBroadcast(t *testing.T) {
	// Non-interrupt messages go through the debounce buffer. When sent to
	// different agents, each agent's buffer flushes independently.
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{
					ContainerID:     "agent-1",
					Name:            "test-agent-1",
					ContainerStatus: "Up 2 minutes",
					Labels:          map[string]string{"scion.name": "test-agent-1"},
				},
				{
					ContainerID:     "agent-2",
					Name:            "test-agent-2",
					ContainerStatus: "Up 1 minute",
					Labels:          map[string]string{"scion.name": "test-agent-2"},
				},
			}, nil
		},
	}

	var mu sync.Mutex
	var capturedCalls []string
	done := make(chan struct{}, 6)
	mockRT.ExecFunc = func(ctx context.Context, id string, cmd []string) (string, error) {
		mu.Lock()
		capturedCalls = append(capturedCalls, fmt.Sprintf("%s: %s", id, strings.Join(cmd, " ")))
		// Signal done for each submit paste (one for the message, two for the
		// trailing retries, per agent delivery).
		if cmd[0] == "tmux" && cmd[1] == "paste-buffer" && slices.Contains(cmd, "scion-submit") {
			done <- struct{}{}
		}
		mu.Unlock()
		return "", nil
	}

	mgr := &AgentManager{
		Runtime: mockRT,
	}
	// Use a short buffer delay for testing.
	mgr.msgBuffer = NewMessageBuffer(100*time.Millisecond, func(agentID, projectID, message string, interrupt bool) error {
		return mgr.deliverImmediate(context.Background(), agentID, projectID, message, interrupt)
	})
	defer mgr.msgBuffer.Close()

	ctx := context.Background()
	// Broadcast is handled by CLI loop usually, but let's test mgr.Message on both.
	// Non-interrupt messages are buffered and delivered after the debounce window.
	err := mgr.Message(ctx, "test-agent-1", "", "hello", false)
	if err != nil {
		t.Fatalf("Message 1 failed: %v", err)
	}
	err = mgr.Message(ctx, "test-agent-2", "", "hello", false)
	if err != nil {
		t.Fatalf("Message 2 failed: %v", err)
	}

	// Wait for both buffered deliveries to complete (3 submits per agent × 2 agents).
	for i := 0; i < 6; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for buffered delivery")
		}
	}

	mu.Lock()
	defer mu.Unlock()

	// Per agent: the message paste, its submit, then two retry submits.
	perAgent := func(a string) []string {
		out := []string{
			a + ": tmux set-buffer -b scion-msg -- hello",
			a + ": tmux paste-buffer -d -b scion-msg -t scion:0 -p",
		}
		// One submit for the message, then the two retries. -d consumes the
		// buffer, so each pairs a set with its paste.
		for range 3 {
			out = append(out,
				a+": tmux set-buffer -b scion-submit -- \r",
				a+": tmux paste-buffer -d -b scion-submit -t scion:0")
		}
		return out
	}

	if want := len(perAgent("agent-1")) * 2; len(capturedCalls) != want {
		t.Fatalf("Expected %d calls, got %d: %v", want, len(capturedCalls), capturedCalls)
	}

	// Since buffer delivery is async, agents may flush in either order.
	// Verify each agent's commands appear together and in the right sequence.
	agent1Calls := filterByPrefix(capturedCalls, "agent-1:")
	agent2Calls := filterByPrefix(capturedCalls, "agent-2:")

	for _, tc := range []struct {
		name string
		got  []string
	}{{"agent-1", agent1Calls}, {"agent-2", agent2Calls}} {
		want := perAgent(tc.name)
		if !slices.Equal(tc.got, want) {
			t.Errorf("%s commands = %v, want %v", tc.name, tc.got, want)
		}
	}
}

func TestMessageRaw(t *testing.T) {
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{
					ContainerID:     "agent-1",
					Name:            "test-agent",
					ContainerStatus: "Up 2 minutes",
					Labels:          map[string]string{"scion.name": "test-agent"},
				},
			}, nil
		},
	}

	var capturedCmd []string
	mockRT.ExecFunc = func(ctx context.Context, id string, cmd []string) (string, error) {
		capturedCmd = append(capturedCmd, strings.Join(cmd, " "))
		return "", nil
	}

	mgr := &AgentManager{
		Runtime: mockRT,
	}
	mgr.msgBuffer = NewMessageBuffer(100*time.Millisecond, func(agentID, projectID, message string, interrupt bool) error {
		return mgr.deliverImmediate(context.Background(), agentID, projectID, message, interrupt)
	})
	defer mgr.msgBuffer.Close()

	ctx := context.Background()
	err := mgr.MessageRaw(ctx, "test-agent", "", "Escape")
	if err != nil {
		t.Fatalf("MessageRaw failed: %v", err)
	}

	// Raw should leave copy-mode, then produce exactly one send-keys command
	// with no trailing Enter.
	expectedCmds := []string{
		"tmux copy-mode -q -t scion:0",
		"tmux send-keys -t scion:0 -- Escape",
	}

	if len(capturedCmd) != len(expectedCmds) {
		t.Fatalf("Expected %d commands, got %d: %v", len(expectedCmds), len(capturedCmd), capturedCmd)
	}

	for i, cmd := range capturedCmd {
		if cmd != expectedCmds[i] {
			t.Errorf("Expected cmd %d to be '%s', got '%s'", i, expectedCmds[i], cmd)
		}
	}
}

// TestDeliveryReachesTheHarnessInCopyMode pins how each delivery path survives
// a pane left in copy-mode by a scroll. Real keys are dispatched through the
// mode's key table, so those paths cancel the mode first; the message path
// submits by paste, which bypasses the key table and therefore must NOT cancel
// - that is what keeps a reading operator's scroll position.
func TestDeliveryReachesTheHarnessInCopyMode(t *testing.T) {
	const exitCopyMode = "tmux copy-mode -q -t scion:0"
	ctx := context.Background()

	tests := []struct {
		name string
		// deliver invokes one delivery path.
		deliver func(mgr *AgentManager) error
		// wantExit is whether that path must cancel copy-mode.
		wantExit bool
		// firstInput is the first command of that path that reaches the pane.
		firstInput string
	}{
		{
			name:       "message submits by paste and leaves the mode alone",
			deliver:    func(mgr *AgentManager) error { return mgr.deliverImmediate(ctx, "test-agent", "", "hello", false) },
			wantExit:   false,
			firstInput: "tmux paste-buffer -d -b scion-msg -t scion:0 -p",
		},
		{
			name:       "interrupt sends real keys so it must cancel first",
			deliver:    func(mgr *AgentManager) error { return mgr.deliverImmediate(ctx, "test-agent", "", "hello", true) },
			wantExit:   true,
			firstInput: "tmux send-keys -t scion:0 C-c",
		},
		{
			name:       "empty message is a bare Enter key so it must cancel first",
			deliver:    func(mgr *AgentManager) error { return mgr.deliverImmediate(ctx, "test-agent", "", "", false) },
			wantExit:   true,
			firstInput: "tmux send-keys -t scion:0 Enter",
		},
		{
			name:       "raw keys must cancel first",
			deliver:    func(mgr *AgentManager) error { return mgr.MessageRaw(ctx, "test-agent", "", "Escape") },
			wantExit:   true,
			firstInput: "tmux send-keys -t scion:0 -- Escape",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedCmds []string
			mockRT := &runtime.MockRuntime{
				ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{
						{
							ContainerID:     "agent-1",
							Name:            "test-agent",
							ContainerStatus: "Up 2 minutes",
							Labels:          map[string]string{"scion.name": "test-agent"},
						},
					}, nil
				},
				ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
					capturedCmds = append(capturedCmds, strings.Join(cmd, " "))
					return "", nil
				},
			}

			mgr := &AgentManager{Runtime: mockRT}
			if err := tt.deliver(mgr); err != nil {
				t.Fatalf("delivery failed: %v", err)
			}
			if len(capturedCmds) == 0 {
				t.Fatal("no commands were sent")
			}

			exitIdx := slices.Index(capturedCmds, exitCopyMode)
			if tt.wantExit && exitIdx != 0 {
				t.Errorf("expected cmd 0 to be %q, got %v", exitCopyMode, capturedCmds)
			}
			if !tt.wantExit && exitIdx >= 0 {
				t.Errorf("path must not cancel copy-mode, but did at %d: %v", exitIdx, capturedCmds)
			}

			inputIdx := slices.Index(capturedCmds, tt.firstInput)
			if inputIdx < 0 {
				t.Fatalf("expected %q among the sent commands, got %v", tt.firstInput, capturedCmds)
			}
			if tt.wantExit && inputIdx < exitIdx {
				t.Errorf("%q was sent before copy-mode was left: %v", tt.firstInput, capturedCmds)
			}
		})
	}
}

// TestMessageSubmitsWithoutSendKeys guards the property the paste submit exists
// for: nothing on the plain-message path may go through the mode's key table.
func TestMessageSubmitsWithoutSendKeys(t *testing.T) {
	var capturedCmds [][]string
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{
					ContainerID:     "agent-1",
					Name:            "test-agent",
					ContainerStatus: "Up 2 minutes",
					Labels:          map[string]string{"scion.name": "test-agent"},
				},
			}, nil
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			capturedCmds = append(capturedCmds, cmd)
			return "", nil
		},
	}

	mgr := &AgentManager{Runtime: mockRT}
	if err := mgr.deliverImmediate(context.Background(), "test-agent", "", "hello", false); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}

	for _, cmd := range capturedCmds {
		if len(cmd) > 1 && cmd[1] == "send-keys" {
			t.Errorf("plain message path used send-keys, which copy-mode swallows: %v", cmd)
		}
	}
	if !slices.ContainsFunc(capturedCmds, func(c []string) bool {
		return len(c) > 1 && c[1] == "paste-buffer" && slices.Contains(c, "scion-submit")
	}) {
		t.Error("no submit paste was sent; the message would never be submitted")
	}
}

// filterByPrefix returns entries from calls that start with the given prefix.
func filterByPrefix(calls []string, prefix string) []string {
	var result []string
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			result = append(result, c)
		}
	}
	return result
}

// TestDeliveryToleratesOldTmux pins the fallback for tmux before 3.1, where
// copy-mode -q does not exist: the delivery must still go through rather than
// aborting on a command that is only best-effort.
func TestDeliveryToleratesOldTmux(t *testing.T) {
	ctx := context.Background()

	for _, tt := range []struct {
		name    string
		deliver func(mgr *AgentManager) error
		want    string
	}{
		{
			name:    "raw keys",
			deliver: func(m *AgentManager) error { return m.MessageRaw(ctx, "test-agent", "", "Escape") },
			want:    "tmux send-keys -t scion:0 -- Escape",
		},
		{
			name:    "interrupt",
			deliver: func(m *AgentManager) error { return m.deliverImmediate(ctx, "test-agent", "", "hello", true) },
			want:    "tmux send-keys -t scion:0 C-c",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var captured []string
			mockRT := &runtime.MockRuntime{
				ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{{
						ContainerID:     "agent-1",
						Name:            "test-agent",
						ContainerStatus: "Up 2 minutes",
						Labels:          map[string]string{"scion.name": "test-agent"},
					}}, nil
				},
				ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
					joined := strings.Join(cmd, " ")
					captured = append(captured, joined)
					if strings.Contains(joined, "copy-mode") {
						return "", errors.New("unknown flag: -q")
					}
					return "", nil
				},
			}

			mgr := &AgentManager{Runtime: mockRT}
			if err := tt.deliver(mgr); err != nil {
				t.Fatalf("delivery aborted on an old-tmux copy-mode failure: %v", err)
			}
			if !slices.Contains(captured, tt.want) {
				t.Errorf("expected %q to still be sent, got %v", tt.want, captured)
			}
		})
	}
}
