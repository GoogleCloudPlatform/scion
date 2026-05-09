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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestStopProjectContainers_StopsMatchingContainers(t *testing.T) {
	var deletedIDs []string

	allContainers := []api.AgentInfo{
		{
			ContainerID: "container-1",
			Name:        "agent-a",
			Labels:      map[string]string{"scion.name": "agent-a", "scion.grove": "mygrove"},
		},
		{
			ContainerID: "container-2",
			Name:        "agent-b",
			Labels:      map[string]string{"scion.name": "agent-b", "scion.grove": "mygrove"},
		},
		{
			ContainerID: "container-3",
			Name:        "other-agent",
			Labels:      map[string]string{"scion.name": "other-agent", "scion.grove": "mygrove"},
		},
	}

	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			// The initial List call uses grove filter; Delete's internal List
			// uses scion.name filter to find the container by ID.
			if name, ok := filter["scion.name"]; ok {
				for _, c := range allContainers {
					if c.ContainerID == name || c.Name == name {
						return []api.AgentInfo{c}, nil
					}
				}
				return nil, nil
			}
			return allContainers, nil
		},
		DeleteFunc: func(ctx context.Context, id string) error {
			deletedIDs = append(deletedIDs, id)
			return nil
		},
	}

	mgr := NewManager(mock)
	stopped := StopProjectContainers(context.Background(), mgr, "mygrove", []string{"agent-a", "agent-b"})

	if len(stopped) != 2 {
		t.Fatalf("expected 2 stopped, got %d: %v", len(stopped), stopped)
	}
	if len(deletedIDs) != 2 {
		t.Fatalf("expected 2 deletes, got %d: %v", len(deletedIDs), deletedIDs)
	}

	// Verify container-3 was NOT deleted (not in agent names list)
	for _, id := range deletedIDs {
		if id == "container-3" {
			t.Error("should not have deleted container-3 (not in agent names)")
		}
	}
}

func TestStopProjectContainers_NoContainers(t *testing.T) {
	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
	}

	mgr := NewManager(mock)
	stopped := StopProjectContainers(context.Background(), mgr, "empty-project", []string{"agent-a"})

	if len(stopped) != 0 {
		t.Errorf("expected 0 stopped, got %d", len(stopped))
	}
}

func TestStopProjectContainers_SkipsEmptyContainerID(t *testing.T) {
	deleteCount := 0
	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{
					ContainerID: "", // no container ID (created but never started)
					Name:        "agent-a",
					Labels:      map[string]string{"scion.name": "agent-a"},
				},
			}, nil
		},
		DeleteFunc: func(ctx context.Context, id string) error {
			deleteCount++
			return nil
		},
	}

	mgr := NewManager(mock)
	stopped := StopProjectContainers(context.Background(), mgr, "myproject", []string{"agent-a"})

	if len(stopped) != 0 {
		t.Errorf("expected 0 stopped, got %d", len(stopped))
	}
	if deleteCount != 0 {
		t.Errorf("expected 0 delete calls, got %d", deleteCount)
	}
}
