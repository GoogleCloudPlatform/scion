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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

type MockRuntime struct {
	NameFunc             func() string
	RunFunc              func(ctx context.Context, config RunConfig) (string, error)
	StopFunc             func(ctx context.Context, id string) error
	DeleteFunc           func(ctx context.Context, id string) error
	ListFunc             func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error)
	GetLogsFunc          func(ctx context.Context, id string) (string, error)
	GetLogsSinceFunc     func(ctx context.Context, id string, since time.Time) (string, error)
	InspectFunc          func(ctx context.Context, id string) (ContainerState, error)
	AttachFunc           func(ctx context.Context, id string) error
	ImageExistsFunc      func(ctx context.Context, image string) (bool, error)
	SyncFunc             func(ctx context.Context, id string, direction SyncDirection) error
	ExecFunc             func(ctx context.Context, id string, cmd []string) (string, error)
	GetWorkspacePathFunc func(ctx context.Context, id string) (string, error)
}

func (m *MockRuntime) Name() string {
	if m.NameFunc != nil {
		return m.NameFunc()
	}
	return "mock"
}

func (m *MockRuntime) ExecUser() string {
	return "scion"
}

func (m *MockRuntime) Run(ctx context.Context, config RunConfig) (string, error) {
	if m.RunFunc != nil {
		return m.RunFunc(ctx, config)
	}
	return "mock-id", nil
}

func (m *MockRuntime) Stop(ctx context.Context, id string) error {
	if m.StopFunc != nil {
		return m.StopFunc(ctx, id)
	}
	return nil
}

func (m *MockRuntime) Delete(ctx context.Context, id string) error {
	if m.DeleteFunc != nil {
		return m.DeleteFunc(ctx, id)
	}
	return nil
}

func (m *MockRuntime) List(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
	if m.ListFunc != nil {
		return m.ListFunc(ctx, labelFilter)
	}
	return []api.AgentInfo{}, nil
}

func (m *MockRuntime) GetLogs(ctx context.Context, id string) (string, error) {
	if m.GetLogsFunc != nil {
		return m.GetLogsFunc(ctx, id)
	}
	return "mock logs", nil
}

func (m *MockRuntime) GetLogsSince(ctx context.Context, id string, since time.Time) (string, error) {
	if m.GetLogsSinceFunc != nil {
		return m.GetLogsSinceFunc(ctx, id, since)
	}
	return m.GetLogs(ctx, id)
}

func (m *MockRuntime) Inspect(ctx context.Context, id string) (ContainerState, error) {
	if m.InspectFunc != nil {
		return m.InspectFunc(ctx, id)
	}
	return ContainerState{}, nil
}

func (m *MockRuntime) Attach(ctx context.Context, id string) error {
	if m.AttachFunc != nil {
		return m.AttachFunc(ctx, id)
	}
	return nil
}

func (m *MockRuntime) ImageExists(ctx context.Context, image string) (bool, error) {
	if m.ImageExistsFunc != nil {
		return m.ImageExistsFunc(ctx, image)
	}
	return true, nil
}

func (m *MockRuntime) PullImage(ctx context.Context, image string) error {
	return nil
}

func (m *MockRuntime) Sync(ctx context.Context, id string, direction SyncDirection) error {

	if m.SyncFunc != nil {

		return m.SyncFunc(ctx, id, direction)

	}

	return nil

}

func (m *MockRuntime) Exec(ctx context.Context, id string, cmd []string) (string, error) {
	if m.ExecFunc != nil {
		return m.ExecFunc(ctx, id, cmd)
	}
	return "", nil
}

func (m *MockRuntime) GetWorkspacePath(ctx context.Context, id string) (string, error) {
	if m.GetWorkspacePathFunc != nil {
		return m.GetWorkspacePathFunc(ctx, id)
	}
	return "/mock/workspace", nil
}
