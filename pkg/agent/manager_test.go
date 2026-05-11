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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestStop_ResolvesNameToContainerID(t *testing.T) {
	var stoppedID string

	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{Name: "test-agent", ContainerID: "abc123"},
			}, nil
		},
		StopFunc: func(ctx context.Context, id string) error {
			stoppedID = id
			return nil
		},
	}

	mgr := &AgentManager{Runtime: mock}
	if err := mgr.Stop(context.Background(), "test-agent", ""); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}

	if stoppedID != "abc123" {
		t.Errorf("expected runtime.Stop to receive container ID abc123, got %s", stoppedID)
	}
}

func TestStop_FallsBackToRawID(t *testing.T) {
	var stoppedID string

	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		StopFunc: func(ctx context.Context, id string) error {
			stoppedID = id
			return nil
		},
	}

	mgr := &AgentManager{Runtime: mock}
	if err := mgr.Stop(context.Background(), "unknown-agent", ""); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}

	if stoppedID != "unknown-agent" {
		t.Errorf("expected runtime.Stop to receive raw ID unknown-agent, got %s", stoppedID)
	}
}

func TestStop_FallsBackOnListError(t *testing.T) {
	var stoppedID string

	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return nil, fmt.Errorf("list failed")
		},
		StopFunc: func(ctx context.Context, id string) error {
			stoppedID = id
			return nil
		},
	}

	mgr := &AgentManager{Runtime: mock}
	if err := mgr.Stop(context.Background(), "my-agent", ""); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}

	if stoppedID != "my-agent" {
		t.Errorf("expected runtime.Stop to receive raw ID my-agent, got %s", stoppedID)
	}
}

func TestDelete_StopsContainerBeforeRemoving(t *testing.T) {
	var calls []string

	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{Name: "test-agent", ContainerID: "abc123", ContainerStatus: "Up 5 minutes"},
			}, nil
		},
		StopFunc: func(ctx context.Context, id string) error {
			calls = append(calls, "stop:"+id)
			return nil
		},
		DeleteFunc: func(ctx context.Context, id string) error {
			calls = append(calls, "delete:"+id)
			return nil
		},
	}

	mgr := &AgentManager{Runtime: mock}
	_, err := mgr.Delete(context.Background(), "test-agent", false, "", false)
	if err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}

	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d: %v", len(calls), calls)
	}
	if calls[0] != "stop:abc123" {
		t.Errorf("expected first call to be stop:abc123, got %s", calls[0])
	}
	if calls[1] != "delete:abc123" {
		t.Errorf("expected second call to be delete:abc123, got %s", calls[1])
	}
}

func TestDelete_ProceedsWhenStopFails(t *testing.T) {
	var calls []string

	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{Name: "test-agent", ContainerID: "abc123", ContainerStatus: "Exited (0)"},
			}, nil
		},
		StopFunc: func(ctx context.Context, id string) error {
			calls = append(calls, "stop:"+id)
			return fmt.Errorf("container is not running")
		},
		DeleteFunc: func(ctx context.Context, id string) error {
			calls = append(calls, "delete:"+id)
			return nil
		},
	}

	mgr := &AgentManager{Runtime: mock}
	_, err := mgr.Delete(context.Background(), "test-agent", false, "", false)
	if err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}

	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d: %v", len(calls), calls)
	}
	if calls[0] != "stop:abc123" {
		t.Errorf("expected first call to be stop, got %s", calls[0])
	}
	if calls[1] != "delete:abc123" {
		t.Errorf("expected second call to be delete, got %s", calls[1])
	}
}
