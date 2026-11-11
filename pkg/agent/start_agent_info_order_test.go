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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Some runtimes do not let the host write files in the container, so Start
// writes the running phase, runtime and profile to agent-info.json before
// the container starts, not after it (ptone/scion#4766). This covers the
// path where the pre-start runtime List succeeds; when it fails, Start still
// records the run ID after the create (recordAfterCreate), in the host
// state file, not agent-info.json (ptone/scion#4771). The fake runtime
// snapshots the file when the container starts; any later host write
// replaces it (writeAgentInfoFile renames a temp file over it), which the
// comparison after Start detects.
func TestStart_ListOK_NoAgentInfoWriteWhileContainerRuns(t *testing.T) {
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
				writeRun1AgentInfo(t, projectScionDir, agentDir)
			}
			infoPath := filepath.Join(agentDir, "home", "agent-info.json")

			var atStart os.FileInfo
			var atStartData []byte
			started := false
			rt := &runtime.MockRuntime{
				NameFunc: func() string { return "fake" },
				ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
					if !started {
						return nil, nil
					}
					return []api.AgentInfo{{
						Name: "tz-agent", ContainerID: "cid-run-2", RunID: "run-2", Phase: "running",
						Labels: map[string]string{"scion.name": "tz-agent", api.LabelRunID: "run-2"},
					}}, nil
				},
				RunFunc: func(context.Context, runtime.RunConfig) (string, error) {
					fi, err := os.Stat(infoPath)
					if err != nil {
						t.Fatalf("agent-info.json missing when the container starts: %v", err)
					}
					data, err := os.ReadFile(infoPath)
					if err != nil {
						t.Fatal(err)
					}
					atStart, atStartData = fi, data
					started = true
					return "cid-run-2", nil
				},
			}
			if _, err := NewManager(rt).Start(context.Background(), api.StartOptions{
				Name: "tz-agent", ProjectPath: projectScionDir, BrokerMode: true, NoAuth: true, RunID: "run-2",
				Template: "default",
			}); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if !started {
				t.Fatal("Start never started the container")
			}

			// Written before the container started.
			var info api.AgentInfo
			if err := json.Unmarshal(atStartData, &info); err != nil {
				t.Fatalf("agent-info.json at container start: %v", err)
			}
			if info.Phase != "running" || info.Runtime != "fake" {
				t.Errorf("agent-info.json at container start: phase %q runtime %q, want running/fake written before the start", info.Phase, info.Runtime)
			}

			// Not written while the container runs.
			after, err := os.Stat(infoPath)
			if err != nil {
				t.Fatal(err)
			}
			afterData, err := os.ReadFile(infoPath)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(atStart, after) || !after.ModTime().Equal(atStart.ModTime()) || !bytes.Equal(afterData, atStartData) {
				t.Errorf("the host wrote agent-info.json while the container was running:\nat start: %s\nafter:    %s", atStartData, afterData)
			}
		})
	}
}

// clearLeakedScionEnv clears SCION_* variables an agent container exports
// that a broker-mode Start reads, so these tests do not depend on where
// they run.
func clearLeakedScionEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SCION_HOST_UID", "")
	t.Setenv("SCION_PROJECT", "")
}

// copiedHomeRuntime is a MockRuntime that declares the
// runtime.CopiedAgentHomeRuntime capability, as the Kubernetes runtime does.
type copiedHomeRuntime struct {
	*runtime.MockRuntime
}

func (copiedHomeRuntime) CopiesAgentHome() bool { return true }

// On a runtime whose agent home is a one-way copy made during Run
// (runtime.CopiesAgentHome), Start records "starting" before Run and
// "running" only once Run has returned, so no reader sees the agent running
// while the runtime is still preparing it (ptone/scion#5368). The fake lists
// the container with no phase, the shape of a Running Kubernetes pod whose
// startup gate is still closed.
func TestStart_CopiedHome_StartingBeforeRunRunningAfter(t *testing.T) {
	clearLeakedScionEnv(t)
	projectScionDir, agentDir := startTZFixture(t, "", `""`)
	writeRun1AgentInfo(t, projectScionDir, agentDir)
	infoPath := filepath.Join(agentDir, "home", "agent-info.json")

	started := false
	var atRun api.AgentInfo
	rt := copiedHomeRuntime{&runtime.MockRuntime{
		NameFunc: func() string { return "fake" },
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			if !started {
				return nil, nil
			}
			return []api.AgentInfo{{
				Name: "tz-agent", ContainerID: "cid-run-2", RunID: "run-2",
				Labels: map[string]string{"scion.name": "tz-agent", api.LabelRunID: "run-2"},
			}}, nil
		},
		RunFunc: func(context.Context, runtime.RunConfig) (string, error) {
			data, err := os.ReadFile(infoPath)
			if err != nil {
				t.Fatalf("agent-info.json missing when the container starts: %v", err)
			}
			if err := json.Unmarshal(data, &atRun); err != nil {
				t.Fatalf("agent-info.json at container start: %v", err)
			}
			started = true
			return "cid-run-2", nil
		},
	}}
	got, err := NewManager(rt).Start(context.Background(), api.StartOptions{
		Name: "tz-agent", ProjectPath: projectScionDir, BrokerMode: true, NoAuth: true, RunID: "run-2",
		Template: "default",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !started {
		t.Fatal("Start never started the container")
	}
	if atRun.Phase != "starting" || atRun.Runtime != "fake" {
		t.Errorf("agent-info.json when Run was entered: phase %q runtime %q, want starting/fake", atRun.Phase, atRun.Runtime)
	}
	data, err := os.ReadFile(infoPath)
	if err != nil {
		t.Fatal(err)
	}
	var after api.AgentInfo
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatal(err)
	}
	if after.Phase != "running" {
		t.Errorf("agent-info.json after Start: phase %q, want running", after.Phase)
	}
	if got.Phase != "running" {
		t.Errorf("Start returned phase %q, want running", got.Phase)
	}
}

// copiedHomeContainer is a fake runtime that declares
// runtime.CopiedAgentHomeRuntime and, once Run has been entered, lists the
// agent's container with no phase (a Running Kubernetes pod whose startup
// gate is still closed). run runs inside Run; a nil run starts the
// container.
func copiedHomeContainer(projectScionDir string, run func() error) copiedHomeRuntime {
	started := false
	return copiedHomeRuntime{&runtime.MockRuntime{
		NameFunc: func() string { return "fake" },
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			if !started {
				return nil, nil
			}
			return []api.AgentInfo{{
				Name: "tz-agent", ContainerID: "cid-run-2", RunID: "run-2", ProjectPath: projectScionDir,
				Labels: map[string]string{"scion.name": "tz-agent", api.LabelRunID: "run-2"},
			}}, nil
		},
		RunFunc: func(context.Context, runtime.RunConfig) (string, error) {
			started = true
			if run != nil {
				if err := run(); err != nil {
					return "", err
				}
			}
			return "cid-run-2", nil
		},
	}}
}

// Guard: on a copied-home runtime a failed Run still leaves error in
// agent-info.json, the failed-start record and List, now starting from
// starting rather than running. The fake lists the container with no phase
// (a Running pod), so the result does not depend on how List treats a
// runtime that reports starting for a failed start.
func TestStart_CopiedHome_RunFailsLeavesError(t *testing.T) {
	clearLeakedScionEnv(t)
	projectScionDir, agentDir := startTZFixture(t, "", `""`)
	writeRun1AgentInfo(t, projectScionDir, agentDir)
	rt := copiedHomeContainer(projectScionDir, func() error { return errLaunchFailed })

	if err := startTZ(t, rt, projectScionDir, "run-2"); err == nil {
		t.Fatal("Start succeeded although the launch failed")
	}
	if got := agentInfoPhase(t, agentDir); got != "error" {
		t.Errorf("agent-info.json phase = %q, want error", got)
	}
	if got := rawHostState(t, projectScionDir, "tz-agent")["startFailedRunId"]; got != "run-2" {
		t.Errorf("host state startFailedRunId = %q, want run-2", got)
	}
	if got := listedPhase(t, rt, projectScionDir, "tz-agent"); got != "error" {
		t.Errorf("List phase = %q, want error", got)
	}
}

// Guard: a stop or soft-delete mark written while Run is in progress is
// kept; Start's post-Run write moves starting to running only.
func TestStart_CopiedHome_ConcurrentStopMarkWins(t *testing.T) {
	for _, tc := range []struct {
		name string
		mark func(projectScionDir string) error
		want string
	}{
		{"stopped", func(p string) error { return UpdateAgentConfig("tz-agent", p, "stopped", "", "") }, "stopped"},
		{"soft-deleted", func(p string) error { return MarkAgentSoftDeleted("tz-agent", p, time.Now()) }, softDeletedPhase},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearLeakedScionEnv(t)
			projectScionDir, agentDir := startTZFixture(t, "", `""`)
			writeRun1AgentInfo(t, projectScionDir, agentDir)
			rt := copiedHomeContainer(projectScionDir, func() error { return tc.mark(projectScionDir) })

			if err := startTZ(t, rt, projectScionDir, "run-2"); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if got := agentInfoPhase(t, agentDir); got != tc.want {
				t.Errorf("agent-info.json phase = %q, want %q kept", got, tc.want)
			}
		})
	}
}

// advanceAgentPhase writes only while the phase is still the expected one,
// and never creates a missing agent-info.json.
func TestAdvanceAgentPhase_OnlyFromExpected(t *testing.T) {
	clearLeakedScionEnv(t)
	projectScionDir, agentDir := startTZFixture(t, "", `""`)
	infoPath := filepath.Join(agentDir, "home", "agent-info.json")

	t.Run("from matches", func(t *testing.T) {
		if err := os.WriteFile(infoPath, []byte(`{"name":"tz-agent","phase":"starting","runtime":"fake"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		advanceAgentPhase("tz-agent", projectScionDir, "starting", "running")
		if got := agentInfoPhase(t, agentDir); got != "running" {
			t.Errorf("phase = %q, want running", got)
		}
	})
	t.Run("from differs", func(t *testing.T) {
		before := []byte(`{"name":"tz-agent","phase":"stopped","runtime":"fake"}`)
		if err := os.WriteFile(infoPath, before, 0o644); err != nil {
			t.Fatal(err)
		}
		advanceAgentPhase("tz-agent", projectScionDir, "starting", "running")
		after, err := os.ReadFile(infoPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, before) {
			t.Errorf("agent-info.json rewritten although the phase was not starting:\nbefore: %s\nafter:  %s", before, after)
		}
	})
	t.Run("file missing", func(t *testing.T) {
		if err := os.Remove(infoPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		advanceAgentPhase("tz-agent", projectScionDir, "starting", "running")
		if _, err := os.Stat(infoPath); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("agent-info.json after advancing a missing file: %v, want it still missing", err)
		}
	})
}

// A phase written after advanceAgentPhase's first read, but before its
// locked update, is kept: the update checks the expected phase again under
// the agent-info lock.
func TestAdvanceAgentPhase_PhaseChangedAfterReadIsKept(t *testing.T) {
	clearLeakedScionEnv(t)
	projectScionDir, agentDir := startTZFixture(t, "", `""`)
	infoPath := filepath.Join(agentDir, "home", "agent-info.json")
	if err := os.WriteFile(infoPath, []byte(`{"name":"tz-agent","phase":"starting","runtime":"fake"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	hookRan := false
	old := afterAdvanceRead
	t.Cleanup(func() { afterAdvanceRead = old })
	afterAdvanceRead = func() {
		hookRan = true
		if err := UpdateAgentConfig("tz-agent", projectScionDir, "stopped", "", ""); err != nil {
			t.Fatal(err)
		}
	}

	advanceAgentPhase("tz-agent", projectScionDir, "starting", "running")
	if !hookRan {
		t.Fatal("advanceAgentPhase returned before its locked update")
	}
	if got := agentInfoPhase(t, agentDir); got != "stopped" {
		t.Errorf("phase = %q, want the stopped written after the first read kept", got)
	}
}

// On every runtime, a container that exits immediately after Run is
// deleted and the start fails; the failed start is now recorded as on the
// Run error path, so agent-info.json and List show error rather than the
// phase written before Run. MockRuntime declares no capability.
func TestStart_ExitedImmediately_RecordsFailedStart(t *testing.T) {
	clearLeakedScionEnv(t)
	projectScionDir, agentDir := startTZFixture(t, "", `""`)
	writeRun1AgentInfo(t, projectScionDir, agentDir)

	started := false
	deleted := false
	rt := noContainers()
	rt.ListFunc = func(context.Context, map[string]string) ([]api.AgentInfo, error) {
		if !started {
			return nil, nil
		}
		return []api.AgentInfo{{
			Name: "tz-agent", ContainerID: "cid-run-2", RunID: "run-2", Phase: "stopped", ContainerStatus: "Exited (1)", ProjectPath: projectScionDir,
			Labels: map[string]string{"scion.name": "tz-agent", api.LabelRunID: "run-2"},
		}}, nil
	}
	rt.RunFunc = func(context.Context, runtime.RunConfig) (string, error) {
		started = true
		return "cid-run-2", nil
	}
	rt.DeleteFunc = func(context.Context, runtime.RunRef) error {
		deleted = true
		return nil
	}

	err := startTZ(t, rt, projectScionDir, "run-2")
	if err == nil || !strings.Contains(err.Error(), "exited immediately") {
		t.Fatalf("Start error = %v, want the exited-immediately error", err)
	}
	if !deleted {
		t.Error("Start did not delete the exited container")
	}
	if got := rawHostState(t, projectScionDir, "tz-agent")["startFailedRunId"]; got != "run-2" {
		t.Errorf("host state startFailedRunId = %q, want run-2", got)
	}
	if got := agentInfoPhase(t, agentDir); got != "error" {
		t.Errorf("agent-info.json phase = %q, want error", got)
	}
	if got := listedPhase(t, rt, projectScionDir, "tz-agent"); got != "error" {
		t.Errorf("List phase = %q, want error", got)
	}
}

// Guard: a runtime without the capability (Docker, Podman, Apple) still
// records running before Run, as ptone/scion#4766 requires, and the host
// does not write agent-info.json after Run.
func TestStart_SharedHome_StillWritesRunningBeforeRun(t *testing.T) {
	clearLeakedScionEnv(t)
	projectScionDir, agentDir := startTZFixture(t, "", `""`)
	writeRun1AgentInfo(t, projectScionDir, agentDir)

	started := false
	atRun := ""
	rt := noContainers()
	rt.ListFunc = func(context.Context, map[string]string) ([]api.AgentInfo, error) {
		if !started {
			return nil, nil
		}
		return []api.AgentInfo{{
			Name: "tz-agent", ContainerID: "cid-run-2", RunID: "run-2", Phase: "running", ProjectPath: projectScionDir,
			Labels: map[string]string{"scion.name": "tz-agent", api.LabelRunID: "run-2"},
		}}, nil
	}
	rt.RunFunc = func(context.Context, runtime.RunConfig) (string, error) {
		atRun = agentInfoPhase(t, agentDir)
		// What the container itself would write; the host must keep it.
		if err := os.WriteFile(filepath.Join(agentDir, "home", "agent-info.json"),
			[]byte(`{"name":"tz-agent","phase":"starting"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		started = true
		return "cid-run-2", nil
	}
	if err := startTZ(t, rt, projectScionDir, "run-2"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if atRun != "running" {
		t.Errorf("agent-info.json phase when Run was entered = %q, want running", atRun)
	}
	if got := agentInfoPhase(t, agentDir); got != "starting" {
		t.Errorf("agent-info.json phase after Start = %q, want the container's starting kept", got)
	}
}

// A failed write of the pre-Run phase is logged at warn level, because the
// file then keeps an earlier phase that the post-Run write will not
// advance. An unparsable agent-info.json makes the write fail.
func TestStart_PreRunWriteFailure_LogsWarn(t *testing.T) {
	clearLeakedScionEnv(t)
	projectScionDir, agentDir := startTZFixture(t, "", `""`)
	writeRun1AgentInfo(t, projectScionDir, agentDir)
	if err := os.WriteFile(filepath.Join(agentDir, "home", "agent-info.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	var records []slog.Record
	old := slog.Default()
	slog.SetDefault(slog.New(&collectHandler{records: &records}))
	t.Cleanup(func() { slog.SetDefault(old) })

	if err := startTZ(t, copiedHomeContainer(projectScionDir, nil), projectScionDir, "run-2"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	slog.SetDefault(old)
	const want = "Start: failed to record the agent phase before the runtime launch"
	for _, r := range records {
		if r.Level == slog.LevelWarn && strings.HasPrefix(r.Message, want) {
			return
		}
	}
	t.Errorf("no warn record starting %q", want)
}
