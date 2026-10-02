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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

// Async-launch runtime hooks in the Docker-family runtimes (design
// t1-async-create-v11.md §3.8.3, §3.8.4, §6 "P1b-2 ... Docker is tested the
// same way with a mock client"): a checkpoint immediately before the
// container create, the container ID reported after it, and a delete by ID.

const mockContainerID = "0123456789abcdef"

// writeMockContainerCLI writes a mock docker/podman/container CLI that logs
// every invocation's arguments to a log file. "run" prints mockContainerID;
// "rm" of an ID listed in missing prints a "no such container" error.
func writeMockContainerCLI(t *testing.T, missing string) (cli, logPath string) {
	t.Helper()
	dir := t.TempDir()
	cli = filepath.Join(dir, "mock-cli")
	logPath = filepath.Join(dir, "calls.log")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
case "$1" in
  run) echo %s ;;
  rm)
    for a in "$@"; do
      if [ "$a" = %q ]; then echo "Error response from daemon: No such container: $a" >&2; exit 1; fi
    done ;;
esac
`, logPath, mockContainerID, missing)
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return cli, logPath
}

func readCalls(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func runCalls(calls []string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, "run ") {
			n++
		}
	}
	return n
}

type containerRuntimeCase struct {
	name string
	new  func(cli string) Runtime
}

func containerRuntimeCases() []containerRuntimeCase {
	return []containerRuntimeCase{
		{"docker", func(cli string) Runtime { return &DockerRuntime{Command: cli} }},
		{"podman", func(cli string) Runtime { return &PodmanRuntime{Command: cli} }},
		{"apple", func(cli string) Runtime { return &AppleContainerRuntime{Command: cli} }},
	}
}

func containerHookConfig() RunConfig {
	return RunConfig{
		Harness:      &harness.Generic{},
		Name:         "hook-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		Task:         "hello",
	}
}

func TestContainerRun_CheckpointBeforeCreateAndHandleIsContainerID(t *testing.T) {
	for _, tc := range containerRuntimeCases() {
		t.Run(tc.name, func(t *testing.T) {
			cli, logPath := writeMockContainerCLI(t, "")
			rt := tc.new(cli)
			config := containerHookConfig()

			var steps []string
			var handles []api.ResourceHandle
			config.Checkpoint = func(ctx context.Context, step string) error {
				if n := runCalls(readCalls(t, logPath)); n != 0 {
					t.Errorf("checkpoint called after %d container create(s); it must come before", n)
				}
				steps = append(steps, step)
				return nil
			}
			config.OnResourceCreated = func(h api.ResourceHandle) {
				if n := runCalls(readCalls(t, logPath)); n != 1 {
					t.Errorf("OnResourceCreated called with %d container creates; want after exactly one", n)
				}
				handles = append(handles, h)
			}

			id, err := rt.Run(context.Background(), config)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if id != mockContainerID {
				t.Fatalf("Run returned %q, want %q", id, mockContainerID)
			}
			if len(steps) != 1 || steps[0] != CheckpointStepLaunching {
				t.Fatalf("checkpoint steps = %q, want one %q", steps, CheckpointStepLaunching)
			}
			want := api.ResourceHandle{Kind: api.ResourceKindContainer, Name: "hook-agent", UID: mockContainerID}
			if len(handles) != 1 || handles[0] != want {
				t.Fatalf("handles = %+v, want [%+v]", handles, want)
			}
		})
	}
}

func TestContainerRun_CheckpointErrorStopsTheCreate(t *testing.T) {
	errEnded := errors.New("launch ended at the hub")
	for _, tc := range containerRuntimeCases() {
		t.Run(tc.name, func(t *testing.T) {
			cli, logPath := writeMockContainerCLI(t, "")
			rt := tc.new(cli)
			config := containerHookConfig()
			config.Checkpoint = func(ctx context.Context, step string) error { return errEnded }
			var handles []api.ResourceHandle
			config.OnResourceCreated = func(h api.ResourceHandle) { handles = append(handles, h) }

			_, err := rt.Run(context.Background(), config)
			if !errors.Is(err, errEnded) {
				t.Fatalf("Run error = %v, want the checkpoint error", err)
			}
			if calls := readCalls(t, logPath); len(calls) != 0 {
				t.Fatalf("the CLI ran after a failed checkpoint: %q", calls)
			}
			if len(handles) != 0 {
				t.Fatalf("handles reported with no create: %+v", handles)
			}
		})
	}
}

// TestContainerRun_NoHooks_SyncPathUnchanged: with no hooks (the
// synchronous path), Run issues the same single create and returns the ID.
func TestContainerRun_NoHooks_SyncPathUnchanged(t *testing.T) {
	for _, tc := range containerRuntimeCases() {
		t.Run(tc.name, func(t *testing.T) {
			cli, logPath := writeMockContainerCLI(t, "")
			id, err := tc.new(cli).Run(context.Background(), containerHookConfig())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if id != mockContainerID {
				t.Fatalf("Run returned %q", id)
			}
			if calls := readCalls(t, logPath); len(calls) != 1 || runCalls(calls) != 1 {
				t.Fatalf("calls = %q, want exactly one run", calls)
			}
		})
	}
}

func TestContainerDeleteResource_ByIDNotName(t *testing.T) {
	for _, tc := range containerRuntimeCases() {
		t.Run(tc.name, func(t *testing.T) {
			cli, logPath := writeMockContainerCLI(t, "")
			rt := tc.new(cli).(interface {
				DeleteResource(context.Context, api.ResourceHandle) error
			})
			h := api.ResourceHandle{Kind: api.ResourceKindContainer, Name: "hook-agent", UID: mockContainerID}
			if err := rt.DeleteResource(context.Background(), h); err != nil {
				t.Fatalf("DeleteResource: %v", err)
			}
			var rmCalls []string
			for _, c := range readCalls(t, logPath) {
				if strings.HasPrefix(c, "rm") {
					rmCalls = append(rmCalls, c)
				}
			}
			if len(rmCalls) == 0 {
				t.Fatal("no rm issued")
			}
			for _, c := range rmCalls {
				if !strings.HasSuffix(c, " "+mockContainerID) || strings.Contains(c, "hook-agent") {
					t.Fatalf("rm must target the container ID, never the name: %q", c)
				}
			}
		})
	}
}

func TestContainerDeleteResource_MissingAndInvalidHandles(t *testing.T) {
	for _, tc := range []containerRuntimeCase{containerRuntimeCases()[0], containerRuntimeCases()[1]} {
		t.Run(tc.name, func(t *testing.T) {
			cli, logPath := writeMockContainerCLI(t, "gone-id")
			rt := tc.new(cli).(interface {
				DeleteResource(context.Context, api.ResourceHandle) error
			})
			ctx := context.Background()
			if err := rt.DeleteResource(ctx, api.ResourceHandle{Kind: api.ResourceKindContainer, Name: "x", UID: "gone-id"}); err != nil {
				t.Fatalf("a container that is already gone must not be an error: %v", err)
			}
			before := len(readCalls(t, logPath))
			if err := rt.DeleteResource(ctx, api.ResourceHandle{Kind: api.ResourceKindContainer, Name: "x"}); err == nil {
				t.Fatal("expected an error for a handle with no container ID")
			}
			if err := rt.DeleteResource(ctx, api.ResourceHandle{Kind: api.ResourceKindSecret, Name: "x", UID: "u"}); err == nil {
				t.Fatal("expected an error for a non-container kind")
			}
			if after := len(readCalls(t, logPath)); after != before {
				t.Fatalf("invalid handles reached the CLI (%d new calls)", after-before)
			}
		})
	}
}
