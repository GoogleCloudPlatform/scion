// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Some runtimes do not let the host write files in the container, so a
// failed start is recorded in the host-owned host state file and List shows
// it (ptone/scion#4815). These tests read the file as raw JSON, so they
// build without the change. Tests and table rows that reproduce the bug
// fail on an assertion without the change; the ones marked "Guard" pass
// without it and pin behaviour the change must keep.

var errLaunchFailed = errors.New("launch failed")

// rawHostState returns the top-level keys of the agent's host state file
// as strings ("" for a missing file).
func rawHostState(t *testing.T, projectScionDir, name string) map[string]string {
	t.Helper()
	path, err := HostStatePath(filepath.Dir(projectScionDir), name)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}
	}
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("host state file: %v: %s", err, data)
	}
	return m
}

// writeRawHostState replaces the agent's host state file with content.
func writeRawHostState(t *testing.T, projectScionDir, name, content string) {
	t.Helper()
	path, err := HostStatePath(filepath.Dir(projectScionDir), name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func agentInfoPhase(t *testing.T, agentDir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(agentDir, "home", "agent-info.json"))
	if err != nil {
		t.Fatal(err)
	}
	var info api.AgentInfo
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatal(err)
	}
	return info.Phase
}

// listedPhase returns the phase List shows for name in the project.
func listedPhase(t *testing.T, rt runtime.Runtime, projectScionDir, name string) string {
	t.Helper()
	agents, err := NewManager(rt).List(context.Background(), map[string]string{"scion.project_path": projectScionDir})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, a := range agents {
		if a.Name == name {
			return a.Phase
		}
	}
	t.Fatalf("List did not report %s: %+v", name, agents)
	return ""
}

func noContainers() *runtime.MockRuntime {
	return &runtime.MockRuntime{
		NameFunc: func() string { return "fake" },
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
	}
}

func failingLaunch(onRun func()) *runtime.MockRuntime {
	rt := noContainers()
	rt.RunFunc = func(context.Context, runtime.RunConfig) (string, error) {
		if onRun != nil {
			onRun()
		}
		return "", errLaunchFailed
	}
	return rt
}

func startTZ(t *testing.T, rt runtime.Runtime, projectScionDir, runID string) error {
	t.Helper()
	_, err := NewManager(rt).Start(context.Background(), api.StartOptions{
		Name: "tz-agent", ProjectPath: projectScionDir, BrokerMode: true, NoAuth: true, RunID: runID,
		Template: "default",
	})
	return err
}

// The agent home cannot be written after the start attempt (as on a
// runtime that does not let the host write files in the container): the
// failed start is still recorded, and List shows error. The fake launch
// puts a file where the home directory was, so no host write into the
// home can land whatever the test's user (root included); the home is
// put back after Start, for List to read.
func TestStart_FailedLaunch_HomeNotWritable_RecordsError(t *testing.T) {
	projectScionDir, agentDir := startTZFixture(t, "", `""`)
	writeRun1AgentInfo(t, projectScionDir, agentDir)
	home := filepath.Join(agentDir, "home")
	aside := filepath.Join(agentDir, "home.aside")

	launched := false
	rt := failingLaunch(func() {
		launched = true
		if err := os.Rename(home, aside); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(home, []byte("not a directory"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if err := startTZ(t, rt, projectScionDir, "run-2"); err == nil {
		t.Fatal("Start succeeded although the launch failed")
	}
	if !launched {
		t.Fatal("Start failed before the launch")
	}
	if err := os.Remove(home); err != nil {
		t.Fatalf("the fake launch's file at the home path: %v", err)
	}
	if err := os.Rename(aside, home); err != nil {
		t.Fatal(err)
	}

	// The scenario: the host could not write the error phase into the home.
	if got := agentInfoPhase(t, agentDir); got != "running" {
		t.Fatalf("agent-info.json phase = %q, want running (the post-launch write must not have landed)", got)
	}
	if got := rawHostState(t, projectScionDir, "tz-agent")["startFailedRunId"]; got != "run-2" {
		t.Errorf("host state startFailedRunId = %q, want run-2", got)
	}
	if got := listedPhase(t, noContainers(), projectScionDir, "tz-agent"); got != "error" {
		t.Errorf("List phase = %q, want error", got)
	}
}

// Where the host can write the agent home, the agent-info.json error
// write still lands, and the host state record agrees. Only the
// startFailedRunId assertion fails without the change; the agent-info.json
// and List assertions are guards.
func TestStart_FailedLaunch_HomeWritable_RecordsError(t *testing.T) {
	projectScionDir, agentDir := startTZFixture(t, "", `""`)
	writeRun1AgentInfo(t, projectScionDir, agentDir)

	if err := startTZ(t, failingLaunch(nil), projectScionDir, "run-2"); err == nil {
		t.Fatal("Start succeeded although the launch failed")
	}
	if got := agentInfoPhase(t, agentDir); got != "error" {
		t.Errorf("agent-info.json phase = %q, want error", got)
	}
	if got := rawHostState(t, projectScionDir, "tz-agent")["startFailedRunId"]; got != "run-2" {
		t.Errorf("host state startFailedRunId = %q, want run-2", got)
	}
	if got := listedPhase(t, noContainers(), projectScionDir, "tz-agent"); got != "error" {
		t.Errorf("List phase = %q, want error", got)
	}
}

// A later start clears the record before the launch, so a successful
// start is not shown as the earlier failure.
func TestStart_ClearsFailedStartRecordBeforeLaunch(t *testing.T) {
	projectScionDir, agentDir := startTZFixture(t, "", `""`)
	writeRun1AgentInfo(t, projectScionDir, agentDir)
	writeRawHostState(t, projectScionDir, "tz-agent", `{"runId":"run-1","startFailedRunId":"run-1"}`)

	started := false
	rt := noContainers()
	rt.ListFunc = func(context.Context, map[string]string) ([]api.AgentInfo, error) {
		if !started {
			return nil, nil
		}
		return []api.AgentInfo{{
			// Running, so Start's post-launch check accepts the container
			// (a stopped one would be "exited immediately").
			Name: "tz-agent", ContainerID: "cid-run-2", RunID: "run-2", Phase: "running", ContainerStatus: "Up", ProjectPath: projectScionDir,
			Labels: map[string]string{"scion.name": "tz-agent", api.LabelRunID: "run-2"},
		}}, nil
	}
	rt.RunFunc = func(context.Context, runtime.RunConfig) (string, error) {
		if got, ok := rawHostState(t, projectScionDir, "tz-agent")["startFailedRunId"]; ok {
			t.Errorf("startFailedRunId = %q at launch, want it cleared before the launch", got)
		}
		started = true
		return "cid-run-2", nil
	}
	if err := startTZ(t, rt, projectScionDir, "run-2"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	hs := rawHostState(t, projectScionDir, "tz-agent")
	if got, ok := hs["startFailedRunId"]; ok {
		t.Errorf("startFailedRunId = %q after a successful start, want none", got)
	}
	if hs["runId"] != "run-2" {
		t.Errorf("runId = %q, want run-2 kept", hs["runId"])
	}
}

// Container path: the record shows as error only for the run of the
// container List reports, and not while the runtime reports it running.
func TestList_FailedStartRecord_ContainerRunMatch(t *testing.T) {
	for _, tc := range []struct {
		name, recorded, containerPhase, want string
	}{
		{"same run", "run-2", "stopped", "error"},
		// Guard rows: no overlay.
		{"other run", "run-1", "stopped", "stopped"},
		{"running", "run-2", "running", "running"},
		{"invalid value", "../run-2", "stopped", "stopped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projectScionDir, agentDir := startTZFixture(t, "", `""`)
			writeRun1AgentInfo(t, projectScionDir, agentDir)
			writeRawHostState(t, projectScionDir, "tz-agent", `{"runId":"run-2","startFailedRunId":"`+tc.recorded+`"}`)
			rt := noContainers()
			rt.ListFunc = func(context.Context, map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{{
					Name: "tz-agent", ContainerID: "cid-run-2", RunID: "run-2", Phase: tc.containerPhase, ProjectPath: projectScionDir,
					Labels: map[string]string{"scion.name": "tz-agent", api.LabelRunID: "run-2"},
				}}, nil
			}
			if got := listedPhase(t, rt, projectScionDir, "tz-agent"); got != tc.want {
				t.Errorf("List phase = %q, want %q", got, tc.want)
			}
		})
	}
}

// Guard: a host state file from before the record existed changes nothing.
func TestList_HostStateWithoutFailedStartRecord_Unchanged(t *testing.T) {
	projectScionDir, agentDir := startTZFixture(t, "", `""`)
	writeRun1AgentInfo(t, projectScionDir, agentDir)
	writeRawHostState(t, projectScionDir, "tz-agent", `{"runId":"run-1"}`)
	if got := listedPhase(t, noContainers(), projectScionDir, "tz-agent"); got != "stopped" {
		t.Errorf("List phase = %q, want stopped from agent-info.json", got)
	}
}

// Guard: on the file-only path where the host state contract gives no
// trusted root (a symlinked .scion), the record is not read and the phase
// is unchanged. Only the sanity step fails without the change.
func TestList_FailedStartRecord_RefusedRoot_NoOverlay(t *testing.T) {
	projectScionDir, agentDir := startTZFixture(t, "", `""`)
	writeRun1AgentInfo(t, projectScionDir, agentDir)
	writeRawHostState(t, projectScionDir, "tz-agent", `{"runId":"run-1","startFailedRunId":"run-1"}`)
	// Sanity: with the real .scion dir the record shows.
	if got := listedPhase(t, noContainers(), projectScionDir, "tz-agent"); got != "error" {
		t.Fatalf("List phase = %q with a trusted root, want error", got)
	}

	linkedProject := filepath.Join(t.TempDir(), "linked")
	if err := os.MkdirAll(linkedProject, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedScion := filepath.Join(linkedProject, ".scion")
	if err := os.Symlink(projectScionDir, linkedScion); err != nil {
		t.Fatal(err)
	}
	if got := listedPhase(t, noContainers(), linkedScion, "tz-agent"); got != "stopped" {
		t.Errorf("List phase = %q through a symlinked .scion, want stopped (no overlay)", got)
	}
}

// Guard: on the file-only path, a host state file that cannot be read as a
// record (corrupt JSON, or a directory at its path) gives no overlay and no
// error from List.
func TestList_FailedStartRecord_UnreadableFile_NoOverlay(t *testing.T) {
	for _, tc := range []struct {
		name  string
		place func(t *testing.T, path string)
	}{
		{"corrupt", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(`{"startFailedRunId":"run-1"`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"directory", func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projectScionDir, agentDir := startTZFixture(t, "", `""`)
			writeRun1AgentInfo(t, projectScionDir, agentDir)
			path, err := HostStatePath(filepath.Dir(projectScionDir), "tz-agent")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			tc.place(t, path)
			if got := listedPhase(t, noContainers(), projectScionDir, "tz-agent"); got != "stopped" {
				t.Errorf("List phase = %q, want stopped (no overlay)", got)
			}
		})
	}
}
