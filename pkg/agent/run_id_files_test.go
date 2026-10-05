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
// recorded as runId in agent-info.json, so a delete or failure cleanup for
// a different run can leave the files of an agent recreated under the same
// name alone.

// provisionRunIDFixture sets up a machine and a non-git project and returns
// the project's .scion dir.
func provisionRunIDFixture(t *testing.T) string {
	t.Helper()
	mockRuntimeForTest(t)
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)
	if err := config.InitMachine(getTestHarnesses()); err != nil {
		t.Fatalf("InitMachine failed: %v", err)
	}
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	if err := config.InitProject(projectScionDir, getTestHarnesses()); err != nil {
		t.Fatalf("InitProject failed: %v", err)
	}
	t.Chdir(projectDir)
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
	if got := GetSavedRunID("dev", projectScionDir); got != "run-1" {
		t.Fatalf("recorded run = %q, want run-1", got)
	}

	if _, _, _, err := ProvisionAgent(context.Background(), "dev", "default", "", "", projectScionDir, "", "", "", ""); err != nil {
		t.Fatalf("ProvisionAgent without a run: %v", err)
	}
	if got := GetSavedRunID("dev", projectScionDir); got != "run-1" {
		t.Errorf("a provision without a run changed the recorded run to %q, want run-1 kept", got)
	}

	if _, _, _, err := ProvisionAgent(context.Background(), "fresh", "default", "", "", projectScionDir, "", "", "", ""); err != nil {
		t.Fatalf("ProvisionAgent fresh: %v", err)
	}
	if got := GetSavedRunID("fresh", projectScionDir); got != "" {
		t.Errorf("a provision-only agent records run %q, want none", got)
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
			if err := os.WriteFile(filepath.Join(agentDir, "home", "agent-info.json"),
				[]byte(`{"name":"tz-agent","phase":"stopped","runId":"run-1"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			var atRun, labelled string
			rt := &runtime.MockRuntime{
				ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
				RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
					atRun = GetSavedRunID("tz-agent", projectScionDir)
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
			if got := GetSavedRunID("tz-agent", projectScionDir); got != labelled {
				t.Errorf("recorded run after Start = %q, want %q", got, labelled)
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
	if err := os.WriteFile(filepath.Join(agentDir, "home", "agent-info.json"),
		[]byte(`{"name":"tz-agent","phase":"stopped","runId":"run-1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	var deleted []string
	rt := &runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{{
				Name: "tz-agent", ContainerID: "cid-run-1", RunID: "run-1", Phase: "stopped",
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
	})
	if err == nil {
		t.Fatal("Start succeeded with an unreadable scion-agent.json")
	}
	if len(deleted) != 1 || deleted[0] != "cid-run-1" {
		t.Fatalf("pre-clean deletes = %v, want the previous run's cid-run-1", deleted)
	}
	if got := GetSavedRunID("tz-agent", projectScionDir); got != "run-2" {
		t.Errorf("recorded run after the failed start = %q, want run-2 (the run the hub keeps)", got)
	}
}

// The pre-clean failing leaves the previous run recorded: the start returns
// before acting, and the hub swaps back to the run that still exists.
func TestStart_PreCleanFailureKeepsPreviousRun(t *testing.T) {
	projectScionDir, agentDir := startTZFixture(t, "", `""`)
	if err := os.WriteFile(filepath.Join(agentDir, "home", "agent-info.json"),
		[]byte(`{"name":"tz-agent","phase":"stopped","runId":"run-1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := &runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{{Name: "tz-agent", ContainerID: "cid-run-1", RunID: "run-1", Phase: "stopped"}}, nil
		},
		DeleteFunc: func(context.Context, runtime.RunRef) error { return errors.New("runtime unavailable") },
	}
	if _, err := NewManager(rt).Start(context.Background(), api.StartOptions{
		Name: "tz-agent", ProjectPath: projectScionDir, BrokerMode: true, NoAuth: true, RunID: "run-2",
	}); err == nil {
		t.Fatal("Start succeeded although the pre-clean failed")
	}
	if got := GetSavedRunID("tz-agent", projectScionDir); got != "run-1" {
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
		{"create while a runtime entry holds the name", true, []api.AgentInfo{{Name: "tz-agent", ContainerID: "cid-ghost", RunID: "run-ghost"}}, false},
		{"create when the runtime cannot be listed", true, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projectScionDir, agentDir := startTZFixture(t, "", `""`)
			if err := os.WriteFile(filepath.Join(agentDir, "home", "agent-info.json"),
				[]byte(`{"name":"tz-agent","phase":"stopped","runId":"run-ghost"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			rt := &runtime.MockRuntime{ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
				if tc.listErr {
					return nil, errors.New("runtime unavailable")
				}
				return tc.entries, nil
			}}
			if _, err := NewManager(rt).Provision(context.Background(), api.StartOptions{
				Name: "tz-agent", ProjectPath: projectScionDir, BrokerMode: true, NoAuth: true, FreshProvision: tc.fresh,
			}); err != nil {
				t.Fatalf("Provision: %v", err)
			}
			got := GetSavedRunID("tz-agent", projectScionDir)
			if !tc.fresh || tc.entries != nil || tc.listErr {
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

func TestSetSavedRunID_NoAgentInfoIsNoop(t *testing.T) {
	projectScionDir := filepath.Join(t.TempDir(), ".scion")
	if err := os.MkdirAll(filepath.Join(projectScionDir, "agents", "dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SetSavedRunID("dev", projectScionDir, "run-1"); err != nil {
		t.Fatalf("SetSavedRunID: %v", err)
	}
	if _, err := os.Stat(config.GetAgentHomePath(projectScionDir, "dev")); !os.IsNotExist(err) {
		t.Errorf("SetSavedRunID created agent files for an agent with no agent-info.json (stat err %v)", err)
	}
	if got := GetSavedRunID("dev", projectScionDir); got != "" {
		t.Errorf("GetSavedRunID = %q, want none", got)
	}
}
