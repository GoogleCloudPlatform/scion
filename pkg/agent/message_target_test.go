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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// newMessageTargetManager builds a manager whose buffered deliveries are
// counted instead of executed, so tests can tell whether Message accepted a
// message into the buffer.
func newMessageTargetManager(t *testing.T, list func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error)) (*AgentManager, *atomic.Int32, *[]map[string]string) {
	t.Helper()
	var filters []map[string]string
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			cp := make(map[string]string, len(filter))
			for k, v := range filter {
				cp[k] = v
			}
			filters = append(filters, cp)
			return list(ctx, filter)
		},
	}
	var delivered atomic.Int32
	mgr := &AgentManager{Runtime: mockRT}
	mgr.msgBuffer = NewMessageBuffer(time.Hour, func(agentID, projectID, message string, interrupt bool) error {
		delivered.Add(1)
		return nil
	})
	t.Cleanup(mgr.msgBuffer.Close)
	return mgr, &delivered, &filters
}

// flushCount flushes the buffer and returns how many deliveries ran.
func flushCount(mgr *AgentManager, delivered *atomic.Int32) int32 {
	mgr.msgBuffer.Close()
	return delivered.Load()
}

func TestMessage_NoContainerFailsVisibly(t *testing.T) {
	mgr, delivered, filters := newMessageTargetManager(t, func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
		return nil, nil
	})

	err := mgr.Message(context.Background(), "Worker-1", "proj-1", "hello", false)
	if err == nil {
		t.Fatal("expected an error for an agent with no container")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q should contain %q so the broker maps it to 404", err, "not found")
	}
	if got := flushCount(mgr, delivered); got != 0 {
		t.Errorf("message must not be buffered when the container is missing, delivered=%d", got)
	}

	// One scoped lookup: name and project labels, no unscoped list.
	if len(*filters) != 1 {
		t.Fatalf("expected exactly one runtime List call, got %d", len(*filters))
	}
	f := (*filters)[0]
	if f["scion.name"] != "worker-1" || f["scion.project_id"] != "proj-1" {
		t.Errorf("unexpected lookup filter: %v", f)
	}
}

func TestMessage_StoppedContainerFailsVisibly(t *testing.T) {
	for _, phase := range []state.Phase{state.PhaseStopped, state.PhaseError} {
		t.Run(string(phase), func(t *testing.T) {
			mgr, _, _ := newMessageTargetManager(t, func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{{
					ContainerID: "c1",
					Name:        "worker",
					Phase:       string(phase),
					Labels:      map[string]string{"scion.name": "worker"},
				}}, nil
			})
			if err := mgr.Message(context.Background(), "worker", "", "hello", false); err == nil {
				t.Fatalf("expected an error for a %s container", phase)
			}
		})
	}
}

func TestMessage_OtherAgentContainerIgnored(t *testing.T) {
	// A running container for a different agent must not satisfy the check.
	mgr, _, _ := newMessageTargetManager(t, func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
		return []api.AgentInfo{{
			ContainerID: "c1",
			Name:        "other",
			Phase:       string(state.PhaseRunning),
			Labels:      map[string]string{"scion.name": "other"},
		}}, nil
	})
	if err := mgr.Message(context.Background(), "worker", "", "hello", false); err == nil {
		t.Fatal("expected an error when only another agent's container exists")
	}
}

func TestMessage_LookupErrorFallsBackToBuffer(t *testing.T) {
	mgr, delivered, _ := newMessageTargetManager(t, func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
		return nil, errors.New("api unavailable")
	})
	if err := mgr.Message(context.Background(), "worker", "proj-1", "hello", false); err != nil {
		t.Fatalf("a failed lookup must not fail the send, got %v", err)
	}
	if got := flushCount(mgr, delivered); got != 1 {
		t.Errorf("expected the message to be buffered, delivered=%d", got)
	}
}

func TestMessage_RunningContainerBuffered(t *testing.T) {
	mgr, delivered, _ := newMessageTargetManager(t, func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
		return []api.AgentInfo{
			{
				ContainerID: "old",
				Name:        "worker",
				Phase:       string(state.PhaseStopped),
				Labels:      map[string]string{"scion.name": "worker"},
			},
			{
				ContainerID: "c1",
				Name:        "worker",
				Phase:       string(state.PhaseRunning),
				Labels:      map[string]string{"scion.name": "worker"},
			},
		}, nil
	})
	if err := mgr.Message(context.Background(), "worker", "proj-1", "hello", false); err != nil {
		t.Fatalf("Message to a running container failed: %v", err)
	}
	if got := flushCount(mgr, delivered); got != 1 {
		t.Errorf("expected the message to be buffered, delivered=%d", got)
	}
}
