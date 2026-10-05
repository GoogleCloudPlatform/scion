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

//go:build !no_sqlite

package hub

import (
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// End-to-end check for ptone/scion#2675, in process: the hub dispatcher
// (real run ID mint, persist and delete query) talks over HTTP to a real
// runtimebroker.Server, whose agent manager removes real agent files by
// name. A late delete for a ghost run must leave the files of the agent
// recreated under the same name intact.

// fileDeletingRunManager is runLabelManager whose DeleteTarget also removes
// the agent's files by name, as pkg/agent's manager does.
type fileDeletingRunManager struct {
	runLabelManager
}

func (m *fileDeletingRunManager) DeleteTarget(ctx context.Context, name string, ref runtime.RunRef, deleteFiles bool, projectPath string, removeBranch bool) (bool, error) {
	if _, err := m.runLabelManager.DeleteTarget(ctx, name, ref, deleteFiles, projectPath, removeBranch); err != nil {
		return false, err
	}
	if deleteFiles {
		return agent.DeleteAgentFiles(name, projectPath, removeBranch)
	}
	return false, nil
}

// filesBrokerClient sends deletes over HTTP to the real broker. CreateAgent
// stands in for the broker's create handler, which provisions the agent's
// files (recording the hub's run ID in agent-info.json, as pkg/agent does)
// and then starts the container; create decides how far it gets.
type filesBrokerClient struct {
	RuntimeBrokerClient
	create func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error)
}

func (c *filesBrokerClient) CreateAgent(_ context.Context, _, _ string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	return c.create(req)
}

func TestRunID_E2E_LateGhostDeleteSparesRecreatedAgentFiles(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bStarted bool // B's container exists; otherwise B is still provisioning
	}{
		{"recreated agent still provisioning (no container)", false},
		{"recreated agent running", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Chdir(t.TempDir())

			const slug, name = "e2e-files", "dev"
			projectID := tid("project-e2e-files")
			scionDir := filepath.Join(home, ".scion", "projects", slug, ".scion")
			if err := os.MkdirAll(scionDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := config.WriteProjectID(scionDir, projectID); err != nil {
				t.Fatal(err)
			}
			agentDir := filepath.Join(scionDir, "agents", name)
			workFile := filepath.Join(agentDir, "workspace", "work.txt")

			// provision stands in for pkg/agent provisioning: the agent's
			// files, with the run recorded as their owner.
			provision := func(runID, work string) {
				t.Helper()
				agentHome := config.GetAgentHomePath(scionDir, name)
				for _, dir := range []string{agentHome, filepath.Dir(workFile)} {
					if err := os.MkdirAll(dir, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{}`), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"),
					[]byte(`{"name":"dev","phase":"provisioning","runId":"`+runID+`"}`), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(workFile, []byte(work), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			mgr := &fileDeletingRunManager{}
			cfg := runtimebroker.DefaultServerConfig()
			cfg.BrokerID = "e2e-broker"
			cfg.BrokerName = "e2e-broker"
			brokerSrv := runtimebroker.New(cfg, mgr, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})
			httpSrv := httptest.NewServer(brokerSrv.Handler())
			defer httpSrv.Close()

			s := createTestStore(t)
			if err := s.CreateProject(ctx, &store.Project{ID: projectID, Name: slug, Slug: slug}); err != nil {
				t.Fatal(err)
			}
			brokerID := tid("broker-e2e-files")
			if err := s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
				ID: brokerID, Name: "e2e-broker", Slug: "e2e-broker", Endpoint: httpSrv.URL, Status: store.BrokerStatusOnline,
			}); err != nil {
				t.Fatal(err)
			}
			client := &filesBrokerClient{RuntimeBrokerClient: NewHTTPRuntimeBrokerClient()}
			d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
			newAgent := func(id string) *store.Agent {
				a := &store.Agent{
					ID: tid(id), Name: name, Slug: name, ProjectID: projectID, RuntimeBrokerID: brokerID,
					AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude"},
				}
				if err := s.CreateAgent(ctx, a); err != nil {
					t.Fatal(err)
				}
				return a
			}

			// Ghost A: the broker provisions A's files, but the create
			// fails as far as the hub knows.
			client.create = func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
				provision(req.RunID, "A's work")
				return nil, errors.New("broker create timed out")
			}
			agentA := newAgent("agent-e2e-files-a")
			if _, err := d.DispatchAgentCreate(ctx, agentA); err == nil {
				t.Fatal("create A: expected the stand-in failure")
			}
			runA := agentA.RunID
			requireUUID(t, "run A", runA)
			// The hub drops the ghost's row; its delete is still in flight
			// (a retried or delayed request carrying A's row).
			if err := s.DeleteAgent(ctx, agentA.ID); err != nil {
				t.Fatal(err)
			}

			// B: recreate the same name; its provisioning takes over the
			// files under the name.
			client.create = func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
				provision(req.RunID, "B's work")
				if tc.bStarted {
					mgr.run(req.Slug, "cid-b", req.ProjectID, scionDir, req.RunID)
				}
				return &RemoteAgentResponse{Agent: &RemoteAgentInfo{
					ID: req.ID, Slug: req.Slug, Name: req.Name, Phase: "provisioning", RunID: req.RunID,
				}, Created: true}, nil
			}
			agentB := newAgent("agent-e2e-files-b")
			if _, err := d.DispatchAgentCreate(ctx, agentB); err != nil {
				t.Fatalf("create B: %v", err)
			}
			runB := agentB.RunID
			requireUUID(t, "run B", runB)
			if runB == runA {
				t.Fatal("recreate reused run A's ID")
			}

			// The late delete for ghost A, with files and branch: a 404 on
			// the broker, which the hub treats as an idempotent success.
			if err := d.DispatchAgentDelete(ctx, agentA, true, true, false, time.Time{}); err != nil {
				t.Fatalf("late delete for run A: %v", err)
			}
			if data, err := os.ReadFile(workFile); err != nil || string(data) != "B's work" {
				t.Fatalf("B's workspace did not survive the late delete of A: %q, %v", data, err)
			}
			if got := agent.GetSavedRunID(name, scionDir); got != runB {
				t.Fatalf("B's agent-info.json run = %q, want %q", got, runB)
			}
			entries, deletes := mgr.snapshot()
			if len(deletes) != 0 {
				t.Fatalf("the late delete of A reached DeleteTarget: %v", deletes)
			}
			if tc.bStarted && (len(entries) != 1 || entries[0].RunID != runB) {
				t.Fatalf("B's entry was touched: %v", entries)
			}

			// B's own delete removes B's files.
			if err := d.DispatchAgentDelete(ctx, agentB, true, true, false, time.Time{}); err != nil {
				t.Fatalf("delete B: %v", err)
			}
			if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
				t.Fatalf("B's delete left its files (stat err %v)", err)
			}
		})
	}
}
