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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// postRunListRuntime is a MockRuntime whose Run returns id and whose List
// returns nothing before Run (no pre-clean) and after it the entries.
// Every delete is recorded.
func postRunListRuntime(id string, after []api.AgentInfo, deleted *[]runtime.RunRef) *runtime.MockRuntime {
	started := false
	rt := noContainers()
	rt.ListFunc = func(context.Context, map[string]string) ([]api.AgentInfo, error) {
		if !started {
			return nil, nil
		}
		return after, nil
	}
	rt.RunFunc = func(context.Context, runtime.RunConfig) (string, error) {
		started = true
		return id, nil
	}
	rt.DeleteFunc = func(_ context.Context, ref runtime.RunRef) error {
		*deleted = append(*deleted, ref)
		return nil
	}
	return rt
}

// resetPostRunTestEnv clears the host variables Start reads, so the
// test's own environment cannot change the start.
func resetPostRunTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SCION_HOST_UID", "")
	t.Setenv("SCION_PROJECT", "")
}

// postRunEntryShapes are ended entries in the shape each runtime's List
// reports, with no run ID (a legacy entry). The post-Run check reads only
// the entry, never the runtime name.
var postRunEntryShapes = []struct {
	runtime string
	id      string
	entry   api.AgentInfo
}{
	{"docker", "3f2a9c1b7d4e", api.AgentInfo{Name: "tz-agent", ContainerID: "3f2a9c1b7d4e", Phase: "stopped", ContainerStatus: "Exited (1) 2 seconds ago"}},
	{"kubernetes", "tz-agent", api.AgentInfo{Name: "tz-agent", ContainerID: "tz-agent", Phase: "error", ContainerStatus: "Failed"}},
	{"cloudrun-sandbox", "tz-agent-sbx", api.AgentInfo{Name: "tz-agent", ContainerID: "tz-agent-sbx", Phase: "stopped", ContainerStatus: "stopped"}},
	{"cloudrun", "agent-tz-agent-0123456789", api.AgentInfo{Name: "tz-agent", ContainerID: "agent-tz-agent-0123456789", Phase: "stopped", ContainerStatus: "CONDITION_FAILED"}},
}

// A legacy entry (no run ID) that has already ended still fails the start
// and is deleted with this run, on every runtime's entry shape.
func TestStart_PostRunCheck_LegacyEntryStillMatches(t *testing.T) {
	for _, tc := range postRunEntryShapes {
		t.Run(tc.runtime, func(t *testing.T) {
			resetPostRunTestEnv(t)
			projectScionDir, _ := startTZFixture(t, "", `""`)
			entry := tc.entry
			entry.ProjectPath = projectScionDir
			var deleted []runtime.RunRef
			rt := postRunListRuntime(tc.id, []api.AgentInfo{entry}, &deleted)

			err := startTZ(t, rt, projectScionDir, "run-2")
			if err == nil || !strings.Contains(err.Error(), "exited immediately") {
				t.Fatalf("Start error = %v, want the exited-immediately error", err)
			}
			if len(deleted) != 1 || deleted[0].ID != tc.id || deleted[0].RunID != "run-2" {
				t.Errorf("deletes = %+v, want one delete of %s with run-2", deleted, tc.id)
			}
		})
	}
}

// An ended entry of another run, listed under the same name and ID right
// after Run (the previous run's stopped or soft-deleted Cloud Run
// instance), is not this start's container: it is never deleted and the
// start does not fail.
func TestStart_PostRunCheck_OtherRunEntryNeverDeletedOrFailed(t *testing.T) {
	for _, tc := range postRunEntryShapes {
		t.Run(tc.runtime, func(t *testing.T) {
			resetPostRunTestEnv(t)
			projectScionDir, _ := startTZFixture(t, "", `""`)
			entry := tc.entry
			entry.ProjectPath = projectScionDir
			entry.RunID = "run-1"
			entry.Labels = map[string]string{"scion.name": "tz-agent", api.LabelRunID: "run-1"}
			var deleted []runtime.RunRef
			rt := postRunListRuntime(tc.id, []api.AgentInfo{entry}, &deleted)

			if err := startTZ(t, rt, projectScionDir, "run-2"); err != nil {
				t.Fatalf("Start: %v, want success: the ended entry is another run's", err)
			}
			if len(deleted) != 0 {
				t.Errorf("deletes = %+v, want none", deleted)
			}
			if got := rawHostState(t, projectScionDir, "tz-agent")["startFailedRunId"]; got != "" {
				t.Errorf("host state startFailedRunId = %q, want none", got)
			}
		})
	}
}

// The Cloud Run soft-deleted shape of the previous run (delete_time set,
// reported as stopped) listed next to this run's running entry: Start
// reports this run's entry and deletes nothing.
func TestStart_PostRunCheck_SkipsStaleEntryAndReportsThisRun(t *testing.T) {
	resetPostRunTestEnv(t)
	projectScionDir, _ := startTZFixture(t, "", `""`)
	const id = "agent-tz-agent-0123456789"
	stale := api.AgentInfo{Name: "tz-agent", ContainerID: id, RunID: "run-1", Phase: "stopped", ContainerStatus: "CONDITION_FAILED", ProjectPath: projectScionDir}
	current := api.AgentInfo{Name: "tz-agent", ContainerID: id, RunID: "run-2", Phase: "running", ContainerStatus: "CONDITION_SUCCEEDED", ProjectPath: projectScionDir}
	var deleted []runtime.RunRef
	rt := postRunListRuntime(id, []api.AgentInfo{stale, current}, &deleted)

	got, err := NewManager(rt).Start(context.Background(), api.StartOptions{
		Name: "tz-agent", ProjectPath: projectScionDir, BrokerMode: true, NoAuth: true, RunID: "run-2",
		Template: "default",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(deleted) != 0 {
		t.Errorf("deletes = %+v, want none", deleted)
	}
	if got.RunID != "run-2" {
		t.Errorf("Start reported run %q, want run-2", got.RunID)
	}
}

func TestPostRunEntryOfRun(t *testing.T) {
	cases := []struct {
		entryRun string
		want     bool
	}{
		{"", true},
		{"run-2", true},
		{"run-1", false},
	}
	for _, tc := range cases {
		if got := postRunEntryOfRun(api.AgentInfo{RunID: tc.entryRun}, "run-2"); got != tc.want {
			t.Errorf("postRunEntryOfRun(RunID=%q, run-2) = %v, want %v", tc.entryRun, got, tc.want)
		}
	}
}
