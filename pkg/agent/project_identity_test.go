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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// These tests pin that message delivery, send-keys and the manager's
// stop and delete resolve runtime entries with no project identity by the
// rule every project-scoped operation uses (LegacyEntryInProject): such an
// entry stands for an agent of the requested project only if it is a
// container whose recorded project path identifies as that project.

const (
	identityProjA = "11111111-aaaa-aaaa-aaaa-111111111111"
	identityProjB = "22222222-bbbb-bbbb-bbbb-222222222222"
)

// identityTestProjectPath returns a fresh .scion directory whose
// project-id file records projectID.
func identityTestProjectPath(t *testing.T, projectID string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "project", config.DotScion)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteProjectID(dir, projectID); err != nil {
		t.Fatal(err)
	}
	return dir
}

// unlabelledRunning is a running entry named name with no project identity,
// recorded under projectPath.
func unlabelledRunning(name, containerID, projectPath string) api.AgentInfo {
	return api.AgentInfo{
		Name: name, ContainerID: containerID, ProjectPath: projectPath,
		Phase:  string(state.PhaseRunning),
		Labels: map[string]string{"scion.name": name, "agent_id": "agent-abc"},
	}
}

func TestLegacyEntryInProject(t *testing.T) {
	pathB := identityTestProjectPath(t, identityProjB)
	pathA := identityTestProjectPath(t, identityProjA)
	cases := []struct {
		name  string
		entry api.AgentInfo
		want  bool
	}{
		{"container whose path identifies as the project", api.AgentInfo{ContainerID: "c", ProjectPath: pathB}, true},
		{"path of another project", api.AgentInfo{ContainerID: "c", ProjectPath: pathA}, false},
		{"no path", api.AgentInfo{ContainerID: "c"}, false},
		{"no container", api.AgentInfo{ProjectPath: pathB}, false},
		{"labelled entry is not a legacy entry", api.AgentInfo{ContainerID: "c", ProjectPath: pathB,
			Labels: map[string]string{"scion.project_id": identityProjB}}, false},
		{"ProjectID field is an identity", api.AgentInfo{ContainerID: "c", ProjectPath: pathB, ProjectID: identityProjB}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LegacyEntryInProject(tc.entry, identityProjB); got != tc.want {
				t.Fatalf("LegacyEntryInProject = %v, want %v", got, tc.want)
			}
		})
	}
	if !EntryInProject(api.AgentInfo{ProjectID: identityProjB}, identityProjB) {
		t.Fatal("EntryInProject must accept an entry identified as the project")
	}
	if EntryInProject(api.AgentInfo{ContainerID: "c", ProjectPath: pathA}, identityProjB) {
		t.Fatal("EntryInProject must not accept another project's unlabelled entry")
	}
}

// listAll is a List that returns entries regardless of the filter, as a
// runtime or list that does not apply the project filter would.
func listAll(entries ...api.AgentInfo) func(context.Context, map[string]string) ([]api.AgentInfo, error) {
	return func(context.Context, map[string]string) ([]api.AgentInfo, error) {
		return append([]api.AgentInfo(nil), entries...), nil
	}
}

// Message delivery resolves entries with no project identity by the same
// rule as the other operations.
func TestMessageTarget_UnlabelledEntries_PicksEntryOfProject(t *testing.T) {
	mgr := &AgentManager{Runtime: &runtime.MockRuntime{ListFunc: listAll(
		unlabelledRunning("worker", "cid-a", identityTestProjectPath(t, identityProjA)),
		unlabelledRunning("worker", "cid-b", identityTestProjectPath(t, identityProjB)),
	)}}

	target, matches, err := mgr.findMessageTargetCounted(context.Background(), "worker", identityProjB)

	if err != nil {
		t.Fatalf("findMessageTargetCounted: %v, want cid-b", err)
	}
	if target.ContainerID != "cid-b" || matches != 1 {
		t.Fatalf("target=%q matches=%d, want cid-b and 1", target.ContainerID, matches)
	}
}

func TestMessageTarget_UnlabelledPathNotInProject_NotFound(t *testing.T) {
	mgr := &AgentManager{Runtime: &runtime.MockRuntime{ListFunc: listAll(
		unlabelledRunning("worker", "cid-a", identityTestProjectPath(t, identityProjA)),
		unlabelledRunning("worker", "cid-nopath", ""),
	)}}

	target, _, err := mgr.findMessageTargetCounted(context.Background(), "worker", identityProjB)

	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("findMessageTargetCounted = (%+v, %v), want a not-found error", target, err)
	}
}

// The buffered path's pre-check applies the same rule.
func TestMessage_PreCheck_UnlabelledPathNotInProject_NotFound(t *testing.T) {
	mgr, delivered, _ := newMessageTargetManager(t, listAll(
		unlabelledRunning("worker", "cid-a", identityTestProjectPath(t, identityProjA)),
	))

	err := mgr.Message(context.Background(), "worker", identityProjB, "hello", false)

	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("Message err=%v, want a not-found error", err)
	}
	if got := flushCount(mgr, delivered); got != 0 {
		t.Fatalf("delivered=%d, want 0", got)
	}
}

func TestMessage_PreCheck_UnlabelledInRequestedProject_Buffered(t *testing.T) {
	mgr, delivered, _ := newMessageTargetManager(t, listAll(
		unlabelledRunning("worker", "cid-b", identityTestProjectPath(t, identityProjB)),
	))

	if err := mgr.Message(context.Background(), "worker", identityProjB, "hello", false); err != nil {
		t.Fatalf("Message: %v, want buffered", err)
	}
	if got := flushCount(mgr, delivered); got != 1 {
		t.Fatalf("delivered=%d, want 1", got)
	}
}

// Send-keys resolves entries with no project identity by the same rule,
// independently of the agent_id check.
func TestSendKeys_UnlabelledPathNotInProject_TargetNotFound(t *testing.T) {
	var captured []execRecord
	mock := newSendKeysMock([]api.AgentInfo{
		unlabelledRunning("test-agent", "cid-a", identityTestProjectPath(t, identityProjA)),
	}, &captured, nil)
	mgr := &AgentManager{Runtime: mock}

	err := mgr.SendKeys(context.Background(), identityProjB, "test-agent", "agent-abc", "C-c")

	if !errors.Is(err, agentkeys.ErrTargetNotFound) {
		t.Fatalf("SendKeys err=%v, want agentkeys.ErrTargetNotFound", err)
	}
	if len(captured) != 0 {
		t.Fatalf("exec calls=%v, want none", captured)
	}
}

func TestSendKeys_UnlabelledEntries_UsesEntryOfProject(t *testing.T) {
	var ids []string
	mock := newSendKeysMock([]api.AgentInfo{
		unlabelledRunning("test-agent", "cid-a", identityTestProjectPath(t, identityProjA)),
		unlabelledRunning("test-agent", "cid-b", identityTestProjectPath(t, identityProjB)),
	}, &[]execRecord{}, nil)
	exec := mock.ExecFunc
	mock.ExecFunc = func(ctx context.Context, id string, cmd []string) (string, error) {
		ids = append(ids, id)
		return exec(ctx, id, cmd)
	}
	mgr := &AgentManager{Runtime: mock}

	if err := mgr.SendKeys(context.Background(), identityProjB, "test-agent", "agent-abc", "C-c"); err != nil {
		t.Fatalf("SendKeys: %v, want delivery to cid-b", err)
	}
	for _, id := range ids {
		if id != "cid-b" {
			t.Fatalf("exec targets=%v, want only cid-b", ids)
		}
	}
	if len(ids) == 0 {
		t.Fatal("no exec calls, want delivery to cid-b")
	}
}

// Manager stop and delete: the project-scoped resolution accepts only the
// requested project's unlabelled entry, and a project-scoped stop that
// finds none stops nothing.
func TestStop_UnlabelledSameNameInTwoProjects_StopsRequestedProject(t *testing.T) {
	pathB := identityTestProjectPath(t, identityProjB)
	var stopped []string
	mgr := &AgentManager{Runtime: &runtime.MockRuntime{
		ListFunc: listAll(
			unlabelledRunning("dev", "cid-a", identityTestProjectPath(t, identityProjA)),
			unlabelledRunning("dev", "cid-b", pathB),
		),
		StopFunc: func(_ context.Context, ref runtime.RunRef) error { stopped = append(stopped, ref.ID); return nil },
	}}

	if err := mgr.Stop(context.Background(), "dev", pathB, ""); err != nil {
		t.Fatalf("Stop: %v, want cid-b stopped", err)
	}
	if len(stopped) != 1 || stopped[0] != "cid-b" {
		t.Fatalf("stopped=%v, want [cid-b]", stopped)
	}
}

func TestStop_ProjectScopedNoEntry_StopsNothing(t *testing.T) {
	pathB := identityTestProjectPath(t, identityProjB)
	var stopped []string
	mgr := &AgentManager{Runtime: &runtime.MockRuntime{
		ListFunc: listAll(unlabelledRunning("dev", "cid-a", identityTestProjectPath(t, identityProjA))),
		StopFunc: func(_ context.Context, ref runtime.RunRef) error { stopped = append(stopped, ref.ID); return nil },
	}}

	err := mgr.Stop(context.Background(), "dev", pathB, "")

	if !errors.Is(err, ErrStopTargetNotFound) {
		t.Fatalf("Stop err=%v, want ErrStopTargetNotFound", err)
	}
	if len(stopped) != 0 {
		t.Fatalf("stopped=%v, want none (no bare-name stop)", stopped)
	}
}

func TestDelete_UnlabelledOnlyInOtherProject_NothingDeleted(t *testing.T) {
	pathB := identityTestProjectPath(t, identityProjB)
	var deleted []string
	mgr := &AgentManager{Runtime: &runtime.MockRuntime{
		ListFunc:   listAll(unlabelledRunning("dev", "cid-a", identityTestProjectPath(t, identityProjA))),
		StopFunc:   func(context.Context, runtime.RunRef) error { return nil },
		DeleteFunc: func(_ context.Context, ref runtime.RunRef) error { deleted = append(deleted, ref.ID); return nil },
	}}

	if _, err := mgr.Delete(context.Background(), "dev", false, pathB, false); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(deleted) != 0 {
		t.Fatalf("deleted=%v, want none", deleted)
	}
}

// The manager's stop compares the project ID of an entry that carries only
// the ProjectID field (as a Cloud Run entry does).
func TestStop_IDOnlyEntryOfOtherProject_StopsNothing(t *testing.T) {
	pathB := identityTestProjectPath(t, identityProjB)
	var stopped []string
	mgr := &AgentManager{Runtime: &runtime.MockRuntime{
		ListFunc: listAll(api.AgentInfo{Name: "dev", ContainerID: "agent-dev-abc", ProjectID: identityProjA,
			Labels: map[string]string{"scion_name": "dev"}}),
		StopFunc: func(_ context.Context, ref runtime.RunRef) error { stopped = append(stopped, ref.ID); return nil },
	}}

	err := mgr.Stop(context.Background(), "dev", pathB, "")

	if !errors.Is(err, ErrStopTargetNotFound) || len(stopped) != 0 {
		t.Fatalf("Stop err=%v stopped=%v, want ErrStopTargetNotFound and none", err, stopped)
	}
}

// A project name label alone does not make an entry one of the project.
func TestStop_NameLabelAlone_StopsNothing(t *testing.T) {
	pathB := identityTestProjectPath(t, identityProjB)
	var stopped []string
	mgr := &AgentManager{Runtime: &runtime.MockRuntime{
		ListFunc: listAll(api.AgentInfo{Name: "dev", ContainerID: "cid-name", Phase: string(state.PhaseRunning),
			Labels: map[string]string{"scion.name": "dev", "scion.project": resolveProjectName(pathB)}}),
		StopFunc: func(_ context.Context, ref runtime.RunRef) error { stopped = append(stopped, ref.ID); return nil },
	}}

	err := mgr.Stop(context.Background(), "dev", pathB, "")

	if !errors.Is(err, ErrStopTargetNotFound) || len(stopped) != 0 {
		t.Fatalf("Stop err=%v stopped=%v, want ErrStopTargetNotFound and none", err, stopped)
	}
}

// startPreCleanFixture is a machine and project for Start. It returns the
// project's .scion entry and the project ID the start is dispatched with.
func startPreCleanFixture(t *testing.T) (string, string) {
	t.Helper()
	mockRuntimeForTest(t)
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", tmpDir)
	if err := config.InitMachine(getTestHarnesses()); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(tmpDir, "project")
	scionDir := filepath.Join(root, config.DotScion)
	if err := config.InitProject(scionDir, getTestHarnesses()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	return scionDir, identityProjB
}

// Start's pre-clean deletes (or adopts) only entries of its project: an
// unlabelled stopped entry recorded under another project's path is left
// alone, one recorded under this project's path is cleaned.
func TestStart_PreCleanUnlabelledEntries_OnlyOwnProject(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ownPath   bool
		wantClean bool
	}{
		{"entry of another project", false, false},
		{"entry of this project", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scionDir, projectID := startPreCleanFixture(t)
			path := identityTestProjectPath(t, identityProjA)
			if tc.ownPath {
				path = identityTestProjectPath(t, projectID)
			}
			var deleted []string
			rt := &runtime.MockRuntime{
				ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{{ContainerID: "cid-old", Name: "worker", Phase: "stopped",
						ContainerStatus: "Exited (0)", ProjectPath: path, Labels: map[string]string{"scion.name": "worker"}}}, nil
				},
				RunFunc: func(context.Context, runtime.RunConfig) (string, error) { return "cid-new", nil },
				DeleteFunc: func(_ context.Context, ref runtime.RunRef) error {
					deleted = append(deleted, ref.ID)
					return nil
				},
			}
			_, _ = NewManager(rt).Start(context.Background(), api.StartOptions{Name: "worker", ProjectPath: scionDir, HarnessConfig: "claude", NoAuth: true,
				Env: map[string]string{"SCION_PROJECT_ID": projectID, "SCION_AGENT_ID": "agent-a1"}})
			cleaned := false
			for _, id := range deleted {
				cleaned = cleaned || id == "cid-old"
			}
			if cleaned != tc.wantClean {
				t.Fatalf("pre-clean deleted=%v, want cid-old cleaned=%v", deleted, tc.wantClean)
			}
		})
	}
}

// hasRuntimeEntry counts only entries of the start's project.
func TestHasRuntimeEntry_UnlabelledEntries_OnlyOwnProject(t *testing.T) {
	pathB := identityTestProjectPath(t, identityProjB)
	opts := api.StartOptions{Name: "dev", ProjectPath: pathB, Env: map[string]string{"SCION_PROJECT_ID": identityProjB}}
	other := &AgentManager{Runtime: &runtime.MockRuntime{ListFunc: listAll(
		unlabelledRunning("dev", "cid-a", identityTestProjectPath(t, identityProjA)))}}
	own := &AgentManager{Runtime: &runtime.MockRuntime{ListFunc: listAll(unlabelledRunning("dev", "cid-b", pathB))}}

	if other.hasRuntimeEntry(context.Background(), opts) {
		t.Fatal("hasRuntimeEntry = true for another project's unlabelled entry, want false")
	}
	if !own.hasRuntimeEntry(context.Background(), opts) {
		t.Fatal("hasRuntimeEntry = false for this project's unlabelled entry, want true")
	}
}
