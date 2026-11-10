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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Tests for the run that owns an agent's files (ptone/scion#2675): it is
// recorded in the host-owned host state file (ptone/scion#4771), never in
// agent-info.json, so a delete or failure cleanup for a different run can
// leave the files of an agent recreated under the same name alone.

// provisionRunIDFixture sets up a machine and a project and returns the
// project's .scion dir. The machine and project are startTZFixture's: a
// global "default" template and test-harness config, and a project whose
// .scion is a directory. (config.InitProject on a non-git dir would write a
// .scion marker instead, and a marker project without a hub project ID has
// no host state root, so no run would be recorded.)
func provisionRunIDFixture(t *testing.T) string {
	t.Helper()
	mockRuntimeForTest(t)
	projectScionDir, _ := startTZFixture(t, "", `""`)
	t.Chdir(filepath.Dir(projectScionDir))
	return projectScionDir
}

// Provisioning records the run carried by the context; a provision without
// one (provision-only, reprovision) keeps the run already recorded, since
// the files still belong to that run's runtime entry.
func TestProvisionAgent_RecordsRunID(t *testing.T) {
	projectScionDir := provisionRunIDFixture(t)

	ctx := api.ContextWithRunID(context.Background(), "run-1")
	if _, _, _, err := ProvisionAgent(ctx, "dev", "default", "", "", projectScionDir, "", "", "", ""); err != nil {
		t.Fatalf("ProvisionAgent: %v", err)
	}
	if got := hostStateRun(t, projectScionDir, "dev"); got != "run-1" {
		t.Fatalf("recorded run = %q, want run-1", got)
	}
	info := filepath.Join(config.GetAgentHomePath(projectScionDir, "dev"), "agent-info.json")
	if _, err := os.Stat(info); err != nil {
		t.Fatalf("ProvisionAgent wrote no agent-info.json: %v", err)
	}
	if got := agentInfoRunID(t, info); got != "" {
		t.Errorf("agent-info.json runId = %q, want none: the run is recorded in the host state file", got)
	}

	if _, _, _, err := ProvisionAgent(context.Background(), "dev", "default", "", "", projectScionDir, "", "", "", ""); err != nil {
		t.Fatalf("ProvisionAgent without a run: %v", err)
	}
	if got := hostStateRun(t, projectScionDir, "dev"); got != "run-1" {
		t.Errorf("a provision without a run changed the recorded run to %q, want run-1 kept", got)
	}

	if _, _, _, err := ProvisionAgent(context.Background(), "fresh", "default", "", "", projectScionDir, "", "", "", ""); err != nil {
		t.Fatalf("ProvisionAgent fresh: %v", err)
	}
	if got := hostStateRun(t, projectScionDir, "fresh"); got != "" {
		t.Errorf("a provision-only agent records run %q, want none", got)
	}
}

// legacyProvisionedAgent provisions "dev" and then makes it a legacy
// agent: no host state record (or hostRun, when set), and legacyRun as the
// runId of its agent-info.json. It returns the project's .scion dir and
// the agent-info.json path.
func legacyProvisionedAgent(t *testing.T, legacyRun, hostRun string) (string, string) {
	t.Helper()
	projectScionDir := provisionRunIDFixture(t)
	ctx := api.ContextWithRunID(context.Background(), "run-first")
	if _, _, _, err := ProvisionAgent(ctx, "dev", "default", "", "", projectScionDir, "", "", "", ""); err != nil {
		t.Fatalf("ProvisionAgent: %v", err)
	}
	root, err := HostStateRootFor(RunOwnerTarget{ProjectPath: projectScionDir, AgentName: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, config.DotScion, "agents", "dev", HostStateFile)); err != nil {
		t.Fatal(err)
	}
	if hostRun != "" {
		writeHostStateRun(t, projectScionDir, "dev", hostRun)
	}
	info := filepath.Join(config.GetAgentHomePath(projectScionDir, "dev"), "agent-info.json")
	if err := os.WriteFile(info, []byte(`{"name":"dev","phase":"stopped","runId":"`+legacyRun+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return projectScionDir, info
}

// A provision without a run (reincarnate, provision-only) keeps the owner
// of a legacy agent, whose run is still only in agent-info.json: it is
// copied into the host state file before the agent-info.json rewrite drops
// it, so a delete for another run keeps the files (ptone/scion#4771).
func TestProvisionAgent_CopiesLegacyRunOwner(t *testing.T) {
	projectScionDir, info := legacyProvisionedAgent(t, "run-legacy", "")
	if _, _, _, err := ProvisionAgent(context.Background(), "dev", "default", "", "", projectScionDir, "", "", "", ""); err != nil {
		t.Fatalf("ProvisionAgent without a run: %v", err)
	}
	if got := hostStateRun(t, projectScionDir, "dev"); got != "run-legacy" {
		t.Fatalf("host state run = %q, want the legacy run-legacy carried over", got)
	}
	if got := agentInfoRunID(t, info); got != "" {
		t.Errorf("agent-info.json runId = %q, want none", got)
	}
	if filesRemovedByDeleteOf("dev", projectScionDir, "run-other") {
		t.Error("a delete for another run would remove the legacy owner's files")
	}
	if !filesRemovedByDeleteOf("dev", projectScionDir, "run-legacy") {
		t.Error("a delete for the legacy owner's run would leave its files")
	}
}

// The copy never replaces a run the host state file already records: the
// recorded run stays the owner, and the legacy value is dropped.
func TestProvisionAgent_LegacyRunOwnerDoesNotReplaceRecordedRun(t *testing.T) {
	projectScionDir, info := legacyProvisionedAgent(t, "run-old", "run-new")
	if _, _, _, err := ProvisionAgent(context.Background(), "dev", "default", "", "", projectScionDir, "", "", "", ""); err != nil {
		t.Fatalf("ProvisionAgent without a run: %v", err)
	}
	if got := hostStateRun(t, projectScionDir, "dev"); got != "run-new" {
		t.Fatalf("host state run = %q, want run-new kept", got)
	}
	if got := agentInfoRunID(t, info); got != "" {
		t.Errorf("agent-info.json runId = %q, want none", got)
	}
	if filesRemovedByDeleteOf("dev", projectScionDir, "run-old") {
		t.Error("a delete for the legacy run would remove run-new's files")
	}
}

// When the copy fails (here the host state file's name is taken by a
// directory), the agent-info.json rewrite keeps the legacy value as it was
// read, and the owner reads as unknown, so a delete for another run keeps
// the files.
func TestProvisionAgent_KeepsLegacyRunOwnerWhenCopyFails(t *testing.T) {
	projectScionDir, info := legacyProvisionedAgent(t, "run-legacy", "")
	root, err := HostStateRootFor(RunOwnerTarget{ProjectPath: projectScionDir, AgentName: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, config.DotScion, "agents", "dev", HostStateFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ProvisionAgent(context.Background(), "dev", "default", "", "", projectScionDir, "", "", "", ""); err != nil {
		t.Fatalf("ProvisionAgent without a run: %v", err)
	}
	if got := agentInfoRunID(t, info); got != "run-legacy" {
		t.Errorf("agent-info.json runId = %q, want run-legacy kept after the failed copy", got)
	}
	if filesRemovedByDeleteOf("dev", projectScionDir, "run-other") {
		t.Error("a delete for another run would remove the legacy owner's files")
	}
}

// Start records its run (the hub's, or the one it mints) as the owner of an
// already provisioned agent's files before the container is created.
func TestStart_RecordsRunIDBeforeContainer(t *testing.T) {
	for _, tc := range []struct{ name, runID string }{
		{"hub run ID", "run-2"},
		{"minted when absent", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projectScionDir, agentDir := startTZFixture(t, "", `""`)
			// An agent provisioned earlier by run-1.
			info := filepath.Join(agentDir, "home", "agent-info.json")
			if err := os.WriteFile(info, []byte(`{"name":"tz-agent","phase":"stopped"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			writeHostStateRun(t, projectScionDir, "tz-agent", "run-1")
			var atRun, labelled string
			rt := &runtime.MockRuntime{
				ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
				RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
					atRun = hostStateRun(t, projectScionDir, "tz-agent")
					labelled = cfg.Labels[api.LabelRunID]
					return "mock-id", nil
				},
			}
			if _, err := NewManager(rt).Start(context.Background(), api.StartOptions{
				Name:        "tz-agent",
				ProjectPath: projectScionDir,
				BrokerMode:  true,
				NoAuth:      true,
				RunID:       tc.runID,
			}); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if labelled == "" || labelled == "run-1" {
				t.Fatalf("label %s = %q, want this start's run", api.LabelRunID, labelled)
			}
			if tc.runID != "" && labelled != tc.runID {
				t.Fatalf("label %s = %q, want %q", api.LabelRunID, labelled, tc.runID)
			}
			if atRun != labelled {
				t.Errorf("recorded run when the container was created = %q, want the labelled %q", atRun, labelled)
			}
			if got := hostStateRun(t, projectScionDir, "tz-agent"); got != labelled {
				t.Errorf("recorded run after Start = %q, want %q", got, labelled)
			}
			if got := agentInfoRunID(t, info); got != "" {
				t.Errorf("agent-info.json runId = %q after Start, want none", got)
			}
		})
	}
}

// A start that fails after removing the previous run's container still
// records its run (ptone/scion#2675 review B1): the hub keeps that run once
// the broker has acted, so its delete must find files that name it, not the
// previous run (which would leave them behind with a 404). The failure here
// is an unreadable scion-agent.json, after the pre-clean.
func TestStart_FailureAfterPreCleanRecordsNewRun(t *testing.T) {
	projectScionDir, agentDir := startTZFixture(t, "", `""`)
	writeHostStateRun(t, projectScionDir, "tz-agent", "run-1")
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	var deleted []string
	rt := &runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{{
				Name: "tz-agent", ContainerID: "cid-run-1", RunID: "run-1", Phase: "stopped", ProjectID: identityProjB,
				Labels: map[string]string{"scion.name": "tz-agent", api.LabelRunID: "run-1"},
			}}, nil
		},
		DeleteFunc: func(_ context.Context, ref runtime.RunRef) error {
			deleted = append(deleted, ref.ID)
			return nil
		},
		RunFunc: func(context.Context, runtime.RunConfig) (string, error) {
			t.Error("Start reached runtime.Run despite the unreadable config")
			return "", nil
		},
	}
	_, err := NewManager(rt).Start(context.Background(), api.StartOptions{
		Name: "tz-agent", ProjectPath: projectScionDir, BrokerMode: true, NoAuth: true, RunID: "run-2",
		Env: map[string]string{"SCION_PROJECT_ID": identityProjB},
	})
	if err == nil {
		t.Fatal("Start succeeded with an unreadable scion-agent.json")
	}
	if len(deleted) != 1 || deleted[0] != "cid-run-1" {
		t.Fatalf("pre-clean deletes = %v, want the previous run's cid-run-1", deleted)
	}
	if got := hostStateRun(t, projectScionDir, "tz-agent"); got != "run-2" {
		t.Errorf("recorded run after the failed start = %q, want run-2 (the run the hub keeps)", got)
	}
}

// The pre-clean failing leaves the previous run recorded: the start returns
// before acting, and the hub swaps back to the run that still exists.
func TestStart_PreCleanFailureKeepsPreviousRun(t *testing.T) {
	projectScionDir, _ := startTZFixture(t, "", `""`)
	writeHostStateRun(t, projectScionDir, "tz-agent", "run-1")
	rt := &runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{{Name: "tz-agent", ContainerID: "cid-run-1", RunID: "run-1", Phase: "stopped", ProjectID: identityProjB}}, nil
		},
		DeleteFunc: func(context.Context, runtime.RunRef) error { return errors.New("runtime unavailable") },
	}
	if _, err := NewManager(rt).Start(context.Background(), api.StartOptions{
		Name: "tz-agent", ProjectPath: projectScionDir, BrokerMode: true, NoAuth: true, RunID: "run-2",
		Env: map[string]string{"SCION_PROJECT_ID": identityProjB},
	}); err == nil {
		t.Fatal("Start succeeded although the pre-clean failed")
	}
	if got := hostStateRun(t, projectScionDir, "tz-agent"); got != "run-1" {
		t.Errorf("recorded run = %q, want run-1 kept", got)
	}
}

// A provision-only create (review N3) reusing a same-named predecessor's
// files records a provision owner in place of the predecessor's run, so
// the predecessor's late delete (naming its run) leaves them alone. A
// provision that is not a create (no FreshProvision) keeps the recorded run.
func TestProvision_CreateRecordsProvisionOwner(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fresh   bool
		entries []api.AgentInfo
		listErr bool
	}{
		{"provision-only create", true, nil, false},
		{"provision of an existing agent", false, nil, false},
		{"create while a runtime entry holds the name", true, []api.AgentInfo{{Name: "tz-agent", ContainerID: "cid-ghost", RunID: "run-ghost",
			Labels: map[string]string{"scion.name": "tz-agent", "scion.project_id": "this-project"}}}, false},
		{"create when the runtime cannot be listed", true, nil, true},
		{"create while only another project's entry holds the name", true, []api.AgentInfo{{
			Name: "tz-agent", ContainerID: "cid-other", RunID: "run-other",
			Labels: map[string]string{"scion.name": "tz-agent", "scion.project_id": "other-project"},
		}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projectScionDir, _ := startTZFixture(t, "", `""`)
			writeHostStateRun(t, projectScionDir, "tz-agent", "run-ghost")
			rt := &runtime.MockRuntime{ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
				if tc.listErr {
					return nil, errors.New("runtime unavailable")
				}
				return tc.entries, nil
			}}
			if _, err := NewManager(rt).Provision(context.Background(), api.StartOptions{
				Name: "tz-agent", ProjectPath: projectScionDir, BrokerMode: true, NoAuth: true, FreshProvision: tc.fresh,
				Env: map[string]string{"SCION_PROJECT_ID": "this-project"},
			}); err != nil {
				t.Fatalf("Provision: %v", err)
			}
			got := hostStateRun(t, projectScionDir, "tz-agent")
			blocked := tc.listErr
			for _, e := range tc.entries {
				if e.Labels["scion.project_id"] == "this-project" {
					blocked = true
				}
			}
			if !tc.fresh || blocked {
				if got != "run-ghost" {
					t.Errorf("recorded = %q, want run-ghost kept", got)
				}
				return
			}
			if got == "run-ghost" || !strings.HasPrefix(got, ProvisionOwnerPrefix) {
				t.Errorf("recorded = %q, want a %q owner in place of the predecessor's run", got, ProvisionOwnerPrefix)
			}
		})
	}
}

// A start whose project has no trusted root for the host state file (here
// an agents directory reached through a symlink) records no run and carries on, as a failed
// agent-info.json record did: the container is still created, and nothing
// is written through the refused root or into agent-info.json.
func TestStart_RefusedRootStillStarts(t *testing.T) {
	projectScionDir, agentDir := startTZFixture(t, "", `""`)
	info := filepath.Join(agentDir, "home", "agent-info.json")
	if err := os.WriteFile(info, []byte(`{"name":"tz-agent","phase":"stopped"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(filepath.Dir(filepath.Dir(projectScionDir)), "linked-project")
	if err := os.MkdirAll(linked, 0o755); err != nil {
		t.Fatal(err)
	}
	// linked's .scion is a real directory (a .scion symlinked elsewhere is
	// refused before the start, ptone/scion#6647); its entries, agents
	// included, are symlinks into the fixture's .scion, so the agent
	// directory sits below a symlink.
	linkedScion := filepath.Join(linked, config.DotScion)
	if err := os.Mkdir(linkedScion, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(projectScionDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.Symlink(filepath.Join(projectScionDir, e.Name()), filepath.Join(linkedScion, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
	ran := false
	rt := &runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
		RunFunc: func(context.Context, runtime.RunConfig) (string, error) {
			ran = true
			return "mock-id", nil
		},
	}
	if _, err := NewManager(rt).Start(context.Background(), api.StartOptions{
		Name: "tz-agent", ProjectPath: linked, BrokerMode: true, NoAuth: true, RunID: "run-2",
	}); err != nil {
		t.Fatalf("Start with a refused host state root: %v", err)
	}
	if !ran {
		t.Fatal("Start did not create the container")
	}
	if _, err := os.Lstat(filepath.Join(agentDir, HostStateFile)); !os.IsNotExist(err) {
		t.Errorf("a host state file was written through the refused root (lstat err %v)", err)
	}
	if got := agentInfoRunID(t, info); got != "" {
		t.Errorf("agent-info.json runId = %q, want none", got)
	}
}
