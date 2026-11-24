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
	"os"
	"path/filepath"
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
	var progress []string
	ran := false
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return "kubernetes" },
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			ran = true
			require.NotNil(t, config.Checkpoint, "RunConfig.Checkpoint")
			require.NotNil(t, config.OnResourceCreated, "RunConfig.OnResourceCreated")
			require.NotNil(t, config.Progress, "RunConfig.Progress")
			require.NoError(t, config.Checkpoint(ctx, "secrets"))
			config.Progress(api.LaunchStepScheduling, "waiting to be scheduled")
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
		Progress:          func(step, message string) { progress = append(progress, step+": "+message) },
	})
	require.NoError(t, err)
	require.True(t, ran, "Runtime.Run was not called")
	assert.Equal(t, []string{"scheduling: waiting to be scheduled"}, progress)
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
					return []api.AgentInfo{{Name: "test-agent", ContainerID: "old-id", Phase: string(state.PhaseRunning), ProjectID: identityProjB}}, nil
				},
				DeleteFunc: func(ctx context.Context, ref runtime.RunRef) error {
					events = append(events, "delete:"+ref.ID)
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
				Env:         map[string]string{"SCION_PROJECT_ID": identityProjB},
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

// TestStart_DockerImagePullReportsStepAndTypedLaunchFailure covers
// ptone/scion#4986: an async Start on the Docker runtime whose image must be
// pulled reports api.LaunchStepImagePull ("pulling the image") and classifies
// pull failures (missing tag / unreachable image vs malformed reference) as
// typed *runtime.LaunchFailure errors in step image_pull.
func TestStart_DockerImagePullReportsStepAndTypedLaunchFailure(t *testing.T) {
	cases := []struct {
		name       string
		image      string
		pullStderr string
		wantCode   string
	}{
		{
			name:       "missing tag",
			image:      "example.com/scion/agent:missing-tag",
			pullStderr: "Error response from daemon: manifest for example.com/scion/agent:missing-tag not found: manifest unknown: manifest unknown",
			wantCode:   api.LaunchCodeImagePullFailed,
		},
		{
			name:       "malformed image reference",
			image:      "INVALID::IMAGE",
			pullStderr: "invalid reference format",
			wantCode:   api.LaunchCodeInvalidImage,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSharedDirStorageRunFixture(t)
			f.writeGlobalSettings(t, "")

			mockDocker := filepath.Join(t.TempDir(), "mock-docker")
			script := "#!/bin/sh\n" +
				"case \"$1\" in\n" +
				"  ps) exit 0 ;;\n" +
				"  image)\n" +
				"    echo 'Error: No such image: '\"$3\" >&2\n" +
				"    exit 1 ;;\n" +
				"  pull)\n" +
				"    echo \"" + tc.pullStderr + "\" >&2\n" +
				"    exit 1 ;;\n" +
				"  *) exit 1 ;;\n" +
				"esac\n"
			require.NoError(t, os.WriteFile(mockDocker, []byte(script), 0o755))

			var progress []string
			rt := &runtime.DockerRuntime{Command: mockDocker}
			_, err := NewManager(rt).Start(context.Background(), api.StartOptions{
				Name:        "test-agent",
				ProjectPath: f.projectScionDir,
				NoAuth:      true,
				Image:       tc.image,
				Progress: func(step, message string) {
					progress = append(progress, step+": "+message)
				},
			})
			require.Error(t, err)

			var lf *runtime.LaunchFailure
			require.ErrorAs(t, err, &lf, "expected *runtime.LaunchFailure, got %v", err)
			assert.Equal(t, tc.wantCode, lf.Code)
			assert.Equal(t, api.LaunchStepImagePull, lf.Step)
			assert.Empty(t, lf.Reason)
			assert.Equal(t, []string{api.LaunchStepImagePull + ": pulling the image"}, progress)
		})
	}

	t.Run("cached local image skips image_pull step", func(t *testing.T) {
		f := newSharedDirStorageRunFixture(t)
		f.writeGlobalSettings(t, "")

		mockDocker := filepath.Join(t.TempDir(), "mock-docker")
		script := "#!/bin/sh\n" +
			"case \"$1\" in\n" +
			"  ps) exit 0 ;;\n" +
			"  image) exit 0 ;;\n" +
			"  pull) echo 'unexpected pull' >&2; exit 1 ;;\n" +
			"  *) exit 0 ;;\n" +
			"esac\n"
		require.NoError(t, os.WriteFile(mockDocker, []byte(script), 0o755))

		errAtLaunching := errors.New("reached launching checkpoint")
		var progress []string
		var checkpoints []string
		rt := &runtime.DockerRuntime{Command: mockDocker}
		_, err := NewManager(rt).Start(context.Background(), api.StartOptions{
			Name:        "test-agent",
			ProjectPath: f.projectScionDir,
			NoAuth:      true,
			Image:       "example.com/scion/agent:v1",
			Checkpoint: func(ctx context.Context, step string) error {
				checkpoints = append(checkpoints, step)
				return errAtLaunching
			},
			Progress: func(step, message string) {
				progress = append(progress, step+": "+message)
			},
		})
		require.ErrorIs(t, err, errAtLaunching)
		assert.Equal(t, []string{runtime.CheckpointStepLaunching}, checkpoints)
		assert.Empty(t, progress)
	})
}

// TestStart_LocalRuntimes_ImagePullAndRuntimeUnavailable covers
// ptone/scion#8951: Podman and Apple Container report LaunchStepImagePull and
// typed image_pull_failed / invalid_image failures matching Docker, and all
// three local runtimes surface runtime_unavailable without emitting a spurious
// image_pull step when the local container daemon is stopped.
func TestStart_LocalRuntimes_ImagePullAndRuntimeUnavailable(t *testing.T) {
	t.Run("podman and apple container pull failures", func(t *testing.T) {
		cases := []struct {
			name        string
			makeRT      func(cmd string) runtime.Runtime
			notFoundMsg string
			pullSubcmd  string
			pullStderr  string
			image       string
			wantCode    string
		}{
			{
				name:        "podman missing tag",
				makeRT:      func(cmd string) runtime.Runtime { return &runtime.PodmanRuntime{Command: cmd} },
				notFoundMsg: "Error: image not known",
				pullSubcmd:  "pull",
				pullStderr:  "Error: initializing source docker://example.com/scion/agent:missing: manifest unknown",
				image:       "example.com/scion/agent:missing",
				wantCode:    api.LaunchCodeImagePullFailed,
			},
			{
				name:        "podman invalid image",
				makeRT:      func(cmd string) runtime.Runtime { return &runtime.PodmanRuntime{Command: cmd} },
				notFoundMsg: "Error: image not known",
				pullSubcmd:  "pull",
				pullStderr:  "Error: invalid reference format",
				image:       "INVALID::IMAGE",
				wantCode:    api.LaunchCodeInvalidImage,
			},
			{
				name:        "apple container missing tag",
				makeRT:      func(cmd string) runtime.Runtime { return &runtime.AppleContainerRuntime{Command: cmd} },
				notFoundMsg: "Error: image not found",
				pullSubcmd:  "image",
				pullStderr:  "Error: failed to fetch manifest: manifest unknown",
				image:       "example.com/scion/agent:missing",
				wantCode:    api.LaunchCodeImagePullFailed,
			},
			{
				name:        "apple container invalid image",
				makeRT:      func(cmd string) runtime.Runtime { return &runtime.AppleContainerRuntime{Command: cmd} },
				notFoundMsg: "Error: image not found",
				pullSubcmd:  "image",
				pullStderr:  "Error: invalid image reference: INVALID::IMAGE",
				image:       "INVALID::IMAGE",
				wantCode:    api.LaunchCodeInvalidImage,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				f := newSharedDirStorageRunFixture(t)
				f.writeGlobalSettings(t, "")

				mockRT := filepath.Join(t.TempDir(), "mock-rt")
				script := "#!/bin/sh\n" +
					"case \"$1:$2\" in\n" +
					"  ps:*|list:*) echo '[]'; exit 0 ;;\n" +
					"  image:inspect)\n" +
					"    echo '" + tc.notFoundMsg + "' >&2\n" +
					"    exit 1 ;;\n" +
					"  pull:*|image:pull)\n" +
					"    echo '" + tc.pullStderr + "' >&2\n" +
					"    exit 1 ;;\n" +
					"  *) exit 1 ;;\n" +
					"esac\n"
				require.NoError(t, os.WriteFile(mockRT, []byte(script), 0o755))

				var progress []string
				_, err := NewManager(tc.makeRT(mockRT)).Start(context.Background(), api.StartOptions{
					Name:        "test-agent",
					ProjectPath: f.projectScionDir,
					NoAuth:      true,
					Image:       tc.image,
					Progress: func(step, message string) {
						progress = append(progress, step+": "+message)
					},
				})
				require.Error(t, err)

				var lf *runtime.LaunchFailure
				require.ErrorAs(t, err, &lf, "expected *runtime.LaunchFailure, got %v", err)
				assert.Equal(t, tc.wantCode, lf.Code)
				assert.Equal(t, api.LaunchStepImagePull, lf.Step)
				assert.Empty(t, lf.Reason)
				assert.Equal(t, []string{api.LaunchStepImagePull + ": pulling the image"}, progress)
			})
		}
	})

	t.Run("stopped local runtime short-circuits with runtime_unavailable", func(t *testing.T) {
		cases := []struct {
			name         string
			makeRT       func(cmd string) runtime.Runtime
			daemonStderr string
		}{
			{
				name:         "docker",
				makeRT:       func(cmd string) runtime.Runtime { return &runtime.DockerRuntime{Command: cmd} },
				daemonStderr: "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?",
			},
			{
				name:         "podman",
				makeRT:       func(cmd string) runtime.Runtime { return &runtime.PodmanRuntime{Command: cmd} },
				daemonStderr: "Cannot connect to Podman. Is the podman machine running?",
			},
			{
				name:         "apple container",
				makeRT:       func(cmd string) runtime.Runtime { return &runtime.AppleContainerRuntime{Command: cmd} },
				daemonStderr: "Error: container system is not running. Run container system start.",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				f := newSharedDirStorageRunFixture(t)
				f.writeGlobalSettings(t, "")

				mockRT := filepath.Join(t.TempDir(), "mock-rt")
				script := "#!/bin/sh\n" +
					"echo '" + tc.daemonStderr + "' >&2\n" +
					"exit 1\n"
				require.NoError(t, os.WriteFile(mockRT, []byte(script), 0o755))

				var progress []string
				_, err := NewManager(tc.makeRT(mockRT)).Start(context.Background(), api.StartOptions{
					Name:        "test-agent",
					ProjectPath: f.projectScionDir,
					NoAuth:      true,
					Image:       "example.com/scion/agent:v1",
					Progress: func(step, message string) {
						progress = append(progress, step+": "+message)
					},
				})
				require.Error(t, err)

				var lf *runtime.LaunchFailure
				require.ErrorAs(t, err, &lf, "expected *runtime.LaunchFailure, got %v", err)
				assert.Equal(t, api.LaunchCodeRuntimeUnavailable, lf.Code)
				assert.Equal(t, api.LaunchStepLaunching, lf.Step)
				assert.Empty(t, lf.Reason)
				assert.Empty(t, progress, "stopped runtime must not report image_pull progress")
			})
		}
	})
}
