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
	"testing"

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/runtime"
)

func TestMessage(t *testing.T) {
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{
					ContainerID:     "agent-1",
					Name:            "test-agent",
					ContainerStatus: "Up 2 minutes",
					Labels:          map[string]string{"scion.name": "test-agent", "scion.tmux": "true"},
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

	ctx := context.Background()
	err := mgr.Message(ctx, "test-agent", "hello world", true)
	if err != nil {
		t.Fatalf("Message failed: %v", err)
	}

	expectedCmds := []string{
		"tmux send-keys -t scion C-c",
		"tmux send-keys -t scion hello world Enter",
		"tmux send-keys -t scion Enter",
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
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{
					ContainerID:     "agent-1",
					Name:            "test-agent-1",
					ContainerStatus: "Up 2 minutes",
					Labels:          map[string]string{"scion.name": "test-agent-1", "scion.tmux": "true"},
				},
				{
					ContainerID:     "agent-2",
					Name:            "test-agent-2",
					ContainerStatus: "Up 1 minute",
					Labels:          map[string]string{"scion.name": "test-agent-2", "scion.tmux": "true"},
				},
			}, nil
		},
	}

	var capturedCalls []string
	mockRT.ExecFunc = func(ctx context.Context, id string, cmd []string) (string, error) {
		capturedCalls = append(capturedCalls, fmt.Sprintf("%s: %s", id, strings.Join(cmd, " ")))
		return "", nil
	}

	mgr := &AgentManager{
		Runtime: mockRT,
	}

	ctx := context.Background()
	// Broad cast is handled by CLI loop usually, but let's test mgr.Message on both
	err := mgr.Message(ctx, "test-agent-1", "hello", false)
	if err != nil {
		t.Fatalf("Message 1 failed: %v", err)
	}
	err = mgr.Message(ctx, "test-agent-2", "hello", false)
	if err != nil {
		t.Fatalf("Message 2 failed: %v", err)
	}

	expectedCalls := []string{
		"agent-1: tmux send-keys -t scion hello Enter",
		"agent-1: tmux send-keys -t scion Enter",
		"agent-2: tmux send-keys -t scion hello Enter",
		"agent-2: tmux send-keys -t scion Enter",
	}

	if len(capturedCalls) != len(expectedCalls) {
		t.Fatalf("Expected %d calls, got %d", len(expectedCalls), len(capturedCalls))
	}

	for i, call := range capturedCalls {
		if call != expectedCalls[i] {
			t.Errorf("Expected call %d to be '%s', got '%s'", i, expectedCalls[i], call)
		}
	}
}
