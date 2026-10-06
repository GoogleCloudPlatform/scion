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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Tests for ptone/scion#3242: Start records its run as the owner of the
// agent's files only once step 0's list has succeeded, so the previous
// run's container is known to be gone. When the list fails, a previous
// run's container may remain, the create collides on the name, and the hub
// may settle on that previous run; the files must still be removable by a
// delete of it.

// filesRemovedByDeleteOf reports whether a run-scoped delete for runID
// removes the agent's files. It mirrors only the broker's
// agentFilesRunOwner check (the files are left alone when another run is
// recorded as their owner), not its other-run-in-flight check.
func filesRemovedByDeleteOf(agentName, projectScionDir, runID string) bool {
	owner := GetSavedRunID(agentName, projectScionDir)
	return owner == "" || owner == runID
}

// errNameInUse is the create failure of a name still held by an old
// container that no pre-clean removed.
var errNameInUse = errors.New(`conflict: the container name "/tz-agent" is already in use`)

func writeRun1AgentInfo(t *testing.T, agentDir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(agentDir, "home", "agent-info.json"),
		[]byte(`{"name":"tz-agent","phase":"stopped","runId":"run-1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The list failing leaves run-1 recorded on an already provisioned agent,
// so a delete of run-1 (the run the hub settles on after the name
// collision) still removes the files.
func TestStart_ListFailureKeepsPreviousRun(t *testing.T) {
	projectScionDir, agentDir := startTZFixture(t, "", `""`)
	writeRun1AgentInfo(t, agentDir)
	rt := &runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return nil, errors.New("runtime unavailable")
		},
		DeleteFunc: func(context.Context, runtime.RunRef) error {
			t.Error("Start deleted a runtime entry without a successful list")
			return nil
		},
		RunFunc: func(context.Context, runtime.RunConfig) (string, error) {
			if got := GetSavedRunID("tz-agent", projectScionDir); got != "run-1" {
				t.Errorf("recorded run at create = %q, want run-1 kept", got)
			}
			return "", errNameInUse
		},
	}
	if _, err := NewManager(rt).Start(context.Background(), api.StartOptions{
		Name: "tz-agent", ProjectPath: projectScionDir, BrokerMode: true, NoAuth: true, RunID: "run-2",
	}); err == nil {
		t.Fatal("Start succeeded although the create collided on the name")
	}
	if got := GetSavedRunID("tz-agent", projectScionDir); got != "run-1" {
		t.Errorf("recorded run = %q, want run-1 kept", got)
	}
	if !filesRemovedByDeleteOf("tz-agent", projectScionDir, "run-1") {
		t.Error("a delete of run-1 would leave the agent's files behind")
	}
}

// A fresh provision reached after the list failed records no run while
// the create collides, so a delete of whichever run the hub settles on
// removes the files.
func TestStart_ListFailureFreshProvisionRecordsNoRun(t *testing.T) {
	projectScionDir, agentDir := startTZFixture(t, "", `""`)
	if err := os.RemoveAll(agentDir); err != nil {
		t.Fatal(err)
	}
	ran := false
	rt := &runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return nil, errors.New("runtime unavailable")
		},
		RunFunc: func(context.Context, runtime.RunConfig) (string, error) {
			ran = true
			return "", errNameInUse
		},
	}
	if _, err := NewManager(rt).Start(context.Background(), api.StartOptions{
		Name: "tz-agent", ProjectPath: projectScionDir, BrokerMode: true, NoAuth: true, RunID: "run-2",
		Template: "default",
	}); err == nil {
		t.Fatal("Start succeeded although the create collided on the name")
	}
	if !ran {
		t.Fatal("Start did not reach the create; the test does not exercise the fresh provision")
	}
	if _, err := os.Stat(filepath.Join(agentDir, "home", "agent-info.json")); err != nil {
		t.Fatalf("fresh provision wrote no agent-info.json: %v", err)
	}
	if got := GetSavedRunID("tz-agent", projectScionDir); got != "" {
		t.Errorf("recorded run = %q, want none", got)
	}
	if !filesRemovedByDeleteOf("tz-agent", projectScionDir, "run-1") {
		t.Error("a delete of the old run would leave the agent's files behind")
	}
}

// With a successful list the previous run's container is removed, so the
// new run is recorded even if the create then fails: the hub keeps it, and
// a delete of it must remove the files. A fresh provision records it too.
func TestStart_ListSuccessRecordsNewRunEvenIfCreateFails(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		name := "provisioned"
		if fresh {
			name = "fresh provision"
		}
		t.Run(name, func(t *testing.T) {
			projectScionDir, agentDir := startTZFixture(t, "", `""`)
			var entries []api.AgentInfo
			if fresh {
				if err := os.RemoveAll(agentDir); err != nil {
					t.Fatal(err)
				}
			} else {
				writeRun1AgentInfo(t, agentDir)
				entries = []api.AgentInfo{{
					Name: "tz-agent", ContainerID: "cid-run-1", RunID: "run-1", Phase: "stopped",
					Labels: map[string]string{"scion.name": "tz-agent", api.LabelRunID: "run-1"},
				}}
			}
			var deleted []string
			rt := &runtime.MockRuntime{
				ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return entries, nil },
				DeleteFunc: func(_ context.Context, ref runtime.RunRef) error {
					deleted = append(deleted, ref.ID)
					return nil
				},
				RunFunc: func(context.Context, runtime.RunConfig) (string, error) { return "", errNameInUse },
			}
			if _, err := NewManager(rt).Start(context.Background(), api.StartOptions{
				Name: "tz-agent", ProjectPath: projectScionDir, BrokerMode: true, NoAuth: true, RunID: "run-2",
				Template: "default",
			}); err == nil {
				t.Fatal("Start succeeded although the create failed")
			}
			if len(deleted) != len(entries) {
				t.Fatalf("pre-clean deletes = %v, want %d", deleted, len(entries))
			}
			if got := GetSavedRunID("tz-agent", projectScionDir); got != "run-2" {
				t.Errorf("recorded run = %q, want run-2", got)
			}
			if !filesRemovedByDeleteOf("tz-agent", projectScionDir, "run-2") {
				t.Error("a delete of run-2 would leave the agent's files behind")
			}
		})
	}
}

// A running entry and no task returns that entry as is: its run keeps the
// files.
func TestStart_RunningEntryKeepsRecordedRun(t *testing.T) {
	projectScionDir, agentDir := startTZFixture(t, "", `""`)
	writeRun1AgentInfo(t, agentDir)
	rt := &runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{{
				Name: "tz-agent", ContainerID: "cid-run-1", RunID: "run-1", Phase: "running",
				Labels: map[string]string{"scion.name": "tz-agent", api.LabelRunID: "run-1"},
			}}, nil
		},
		RunFunc: func(context.Context, runtime.RunConfig) (string, error) {
			t.Error("Start created a container although the agent is running")
			return "", nil
		},
	}
	info, err := NewManager(rt).Start(context.Background(), api.StartOptions{
		Name: "tz-agent", ProjectPath: projectScionDir, BrokerMode: true, NoAuth: true, RunID: "run-2",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if info.ContainerID != "cid-run-1" {
		t.Errorf("Start returned %q, want the running cid-run-1", info.ContainerID)
	}
	if got := GetSavedRunID("tz-agent", projectScionDir); got != "run-1" {
		t.Errorf("recorded run = %q, want run-1 kept", got)
	}
}

// A failed list followed by a successful create (review R1) records the
// new run once the create succeeds: the name was free, so the failed list
// hid no previous container and the hub keeps the new run. A delete of the
// new run removes the files; a late delete of the old run does not.
func TestStart_ListFailureThenCreateRecordsNewRun(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		name := "provisioned"
		if fresh {
			name = "fresh provision"
		}
		t.Run(name, func(t *testing.T) {
			projectScionDir, agentDir := startTZFixture(t, "", `""`)
			if fresh {
				if err := os.RemoveAll(agentDir); err != nil {
					t.Fatal(err)
				}
			} else {
				writeRun1AgentInfo(t, agentDir)
			}
			lists := 0
			rt := &runtime.MockRuntime{
				ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
					lists++
					if lists == 1 {
						return nil, errors.New("runtime unavailable")
					}
					return []api.AgentInfo{{
						Name: "tz-agent", ContainerID: "cid-run-2", RunID: "run-2", Phase: "running",
						Labels: map[string]string{"scion.name": "tz-agent", api.LabelRunID: "run-2"},
					}}, nil
				},
				DeleteFunc: func(context.Context, runtime.RunRef) error {
					t.Error("Start deleted a runtime entry")
					return nil
				},
				RunFunc: func(context.Context, runtime.RunConfig) (string, error) { return "cid-run-2", nil },
			}
			if _, err := NewManager(rt).Start(context.Background(), api.StartOptions{
				Name: "tz-agent", ProjectPath: projectScionDir, BrokerMode: true, NoAuth: true, RunID: "run-2",
				Template: "default",
			}); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if got := GetSavedRunID("tz-agent", projectScionDir); got != "run-2" {
				t.Errorf("recorded run after a successful create = %q, want run-2", got)
			}
			if !filesRemovedByDeleteOf("tz-agent", projectScionDir, "run-2") {
				t.Error("a delete of the live run-2 would leave the agent's files behind")
			}
			if filesRemovedByDeleteOf("tz-agent", projectScionDir, "run-1") {
				t.Error("a late delete of run-1 would remove the live run-2's files")
			}
		})
	}
}
