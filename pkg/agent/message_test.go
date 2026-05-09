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
		"tmux send-keys -t scion:0 C-c",
		"tmux set-buffer -- hello world",
		"tmux paste-buffer -t scion:0 -p",
		"tmux send-keys -t scion:0 Enter",
		"tmux send-keys -t scion:0 Enter",
		"tmux send-keys -t scion:0 Enter",
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
		// Signal done for each bare Enter keypress (two trailing Enters per agent delivery).
		// Bare Enter: ["tmux", "send-keys", "-t", "scion:0", "Enter"] → len 5.
		if len(cmd) == 5 && cmd[0] == "tmux" && cmd[1] == "send-keys" && cmd[4] == "Enter" {
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

	// Wait for both buffered deliveries to complete (3 Enters per agent × 2 agents).
	for i := 0; i < 6; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for buffered delivery")
		}
	}

	mu.Lock()
	defer mu.Unlock()

	expectedCalls := []string{
		"agent-1: tmux set-buffer -- hello",
		"agent-1: tmux paste-buffer -t scion:0 -p",
		"agent-1: tmux send-keys -t scion:0 Enter",
		"agent-1: tmux send-keys -t scion:0 Enter",
		"agent-1: tmux send-keys -t scion:0 Enter",
		"agent-2: tmux set-buffer -- hello",
		"agent-2: tmux paste-buffer -t scion:0 -p",
		"agent-2: tmux send-keys -t scion:0 Enter",
		"agent-2: tmux send-keys -t scion:0 Enter",
		"agent-2: tmux send-keys -t scion:0 Enter",
	}

	if len(capturedCalls) != len(expectedCalls) {
		t.Fatalf("Expected %d calls, got %d: %v", len(expectedCalls), len(capturedCalls), capturedCalls)
	}

	// Since buffer delivery is async, agents may flush in either order.
	// Verify each agent's commands appear together and in the right sequence.
	agent1Calls := filterByPrefix(capturedCalls, "agent-1:")
	agent2Calls := filterByPrefix(capturedCalls, "agent-2:")

	if len(agent1Calls) != 5 || len(agent2Calls) != 5 {
		t.Fatalf("Expected 5 calls per agent, got agent-1=%d agent-2=%d", len(agent1Calls), len(agent2Calls))
	}
	if agent1Calls[0] != "agent-1: tmux set-buffer -- hello" {
		t.Errorf("Unexpected agent-1 call[0]: %s", agent1Calls[0])
	}
	if agent2Calls[0] != "agent-2: tmux set-buffer -- hello" {
		t.Errorf("Unexpected agent-2 call[0]: %s", agent2Calls[0])
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

	// Raw should produce exactly one send-keys command with no trailing Enter
	expectedCmds := []string{
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
