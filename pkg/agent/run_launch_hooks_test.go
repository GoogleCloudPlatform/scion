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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStart_PassesLaunchHooksToRun: StartOptions' async-launch hooks reach
// the runtime's RunConfig unchanged.
func TestStart_PassesLaunchHooksToRun(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	f.writeGlobalSettings(t, "")

	var steps []string
	var handles []api.ResourceHandle
	ran := false
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return "kubernetes" },
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			ran = true
			require.NotNil(t, config.Checkpoint, "RunConfig.Checkpoint")
			require.NotNil(t, config.OnResourceCreated, "RunConfig.OnResourceCreated")
			require.NoError(t, config.Checkpoint(ctx, "secrets"))
			config.OnResourceCreated(api.ResourceHandle{Kind: api.ResourceKindPod, Name: "test-agent", UID: "u-1"})
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: f.projectScionDir,
		NoAuth:      true,
		Checkpoint: func(ctx context.Context, step string) error {
			steps = append(steps, step)
			return nil
		},
		OnResourceCreated: func(h api.ResourceHandle) { handles = append(handles, h) },
	})
	require.NoError(t, err)
	require.True(t, ran, "Runtime.Run was not called")
	assert.Equal(t, []string{"secrets"}, steps)
	assert.Equal(t, []api.ResourceHandle{{Kind: api.ResourceKindPod, Name: "test-agent", UID: "u-1"}}, handles)
}

// TestStart_CheckpointsBeforeDeletingAnExistingAgent: Start's delete of an
// existing same-named agent (a new task was given) is preceded by a
// checkpoint on an async launch, and a checkpoint error means no delete.
func TestStart_CheckpointsBeforeDeletingAnExistingAgent(t *testing.T) {
	errEnded := errors.New("launch ended at the hub")
	for _, tc := range []struct {
		name        string
		checkpoint  func(context.Context, string) error
		wantDeletes int
		wantErr     error
	}{
		{"checkpoint error", func(context.Context, string) error { return errEnded }, 0, errEnded},
		{"checkpoint ok", func(context.Context, string) error { return nil }, 1, nil},
		{"sync path", nil, 1, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSharedDirStorageRunFixture(t)
			f.writeGlobalSettings(t, "")

			var events []string
			mockRT := &runtime.MockRuntime{
				NameFunc: func() string { return "kubernetes" },
				ListFunc: func(ctx context.Context, labels map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{{Name: "test-agent", ContainerID: "old-id", Phase: string(state.PhaseRunning)}}, nil
				},
				DeleteFunc: func(ctx context.Context, id string) error {
					events = append(events, "delete:"+id)
					return nil
				},
				RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
					return "mock-id", nil
				},
			}
			opts := api.StartOptions{
				Name:        "test-agent",
				ProjectPath: f.projectScionDir,
				NoAuth:      true,
				Task:        "new task",
			}
			if tc.checkpoint != nil {
				opts.Checkpoint = func(ctx context.Context, step string) error {
					events = append(events, "checkpoint:"+step)
					return tc.checkpoint(ctx, step)
				}
			}
			_, err := NewManager(mockRT).Start(context.Background(), opts)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			deletes := 0
			for i, e := range events {
				if e == "delete:old-id" {
					deletes++
					if tc.checkpoint != nil {
						require.True(t, i > 0 && events[i-1] == "checkpoint:"+runtime.CheckpointStepPreClean,
							"delete not immediately preceded by a pre_clean checkpoint: %v", events)
					}
				}
			}
			assert.Equal(t, tc.wantDeletes, deletes, "events: %v", events)
		})
	}
}
