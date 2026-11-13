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

package runtimebroker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

const (
	flatRelinkAgentID      = "0d2c8a6e-5f1b-4c3a-9e7d-6b2f1a4c8e01"
	flatRelinkAgentID2     = "1e3d9b7f-6a2c-4d4b-8f8e-7c3a2b5d9f12"
	flatRelinkNewAgentID   = "2f4e0c8a-7b3d-4e5c-9a9f-8d4b3c6e0a23"
	flatRelinkForeignAgent = "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"
)

type flatRelinkNode struct {
	srv      *Server
	mgr      *mockManager
	identity *brokeridentity.Identity
	stateDir string
}

type flatRelinkHost struct {
	globalDir          string
	locks              *WorkspaceLocks
	a, b               *flatRelinkNode
	root, dotScion     string
	oldRoot, newRoot   string
	gitProject         bool
	previousProjectIDs []string
}

func newFlatRelinkHost(t *testing.T, gitProject bool) *flatRelinkHost {
	t.Helper()
	clearSCIONEnv(t)
	t.Setenv("HOME", t.TempDir())
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settings := `schema_version: "1"
server:
  broker:
    enabled: true
    instances:
      - key: docker-a
        name: docker-a
        runtime_target:
          type: docker
      - key: docker-b
        name: docker-b
        runtime_target:
          type: docker
`
	writeFlatFixtureFile(t, filepath.Join(globalDir, "settings.yaml"), settings)
	for _, name := range []string{"default", "claude"} {
		writeFlatFixtureFile(t, filepath.Join(globalDir, "templates", name, "scion-agent.yaml"), "harness_config: "+name+"\n")
		writeFlatFixtureFile(t, filepath.Join(globalDir, "harness-configs", name, "config.yaml"), "harness: "+name+"\nimage: test-image:"+name+"\n")
	}
	instances, err := config.LoadRuntimeBrokerInstances("")
	if err != nil {
		t.Fatal(err)
	}
	locks := NewWorkspaceLocks()
	buildNode := func(inst config.V1RuntimeBrokerInstanceConfig, daemonID string) *flatRelinkNode {
		id, err := brokeridentity.LoadOrCreate(
			brokeridentity.InstanceDir(globalDir, inst.Key),
			inst.Key,
			brokeridentity.TargetTypeDocker,
			brokeridentity.ExecutionScope{Type: brokeridentity.TargetTypeDocker, Docker: &brokeridentity.DockerScope{DaemonID: daemonID}},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		stateDir := t.TempDir()
		mgr := &mockManager{}
		rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
		cfg := DefaultServerConfig()
		cfg.Host = "127.0.0.1"
		cfg.Port = 0
		cfg.ForceRuntime = ""
		cfg.BrokerID = id.RuntimeBrokerID
		cfg.BrokerName = inst.Name
		cfg.StateDir = stateDir
		cfg.HubEnabled = true
		cfg.HubEndpoint = "http://127.0.0.1:1"
		cfg.BrokerAuthStrictMode = false
		cfg.WorkspaceLocks = locks
		cfg.FlatInstance = &FlatInstanceConfig{Identity: id, Instance: inst, HubInProcess: true}
		cfg.InMemoryCredentials = &brokercredentials.BrokerCredentials{
			BrokerID:    id.RuntimeBrokerID,
			SecretKey:   "c2VjcmV0",
			HubEndpoint: cfg.HubEndpoint,
		}
		srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})
		endAsyncLaunchesAtCleanup(t, srv)
		return &flatRelinkNode{srv: srv, mgr: mgr, identity: id, stateDir: stateDir}
	}

	h := &flatRelinkHost{
		globalDir:  globalDir,
		locks:      locks,
		a:          buildNode(instances[0], "daemon-a"),
		b:          buildNode(instances[1], "daemon-b"),
		gitProject: gitProject,
	}

	// Place the workspace under ~/.scion/projects/proj so deleteProject
	// (/api/v1/projects/proj) exercises the real sibling-instance check too.
	h.root = filepath.Join(globalDir, "projects", "proj")
	h.dotScion = filepath.Join(h.root, config.DotScion)
	if gitProject {
		if err := os.MkdirAll(h.dotScion, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := config.WriteProjectID(h.dotScion, relinkOldID); err != nil {
			t.Fatal(err)
		}
		h.previousProjectIDs = []string{relinkOldID}
	} else {
		if err := os.MkdirAll(h.root, 0o755); err != nil {
			t.Fatal(err)
		}
		writeRelinkMarker(t, h.dotScion, relinkOldID, "proj")
	}

	oldRoot, ok := config.ConfinedProjectConfigRoot("proj", relinkOldID)
	newRoot, ok2 := config.ConfinedProjectConfigRoot("proj", relinkNewID)
	if !ok || !ok2 {
		t.Fatal("project config roots")
	}
	h.oldRoot = oldRoot
	h.newRoot = newRoot
	writeRelinkSettings(t, filepath.Join(oldRoot, config.DotScion), h.root)
	h.writeAgentFiles(t, oldRoot, relinkAgent)
	return h
}

func (h *flatRelinkHost) writeAgentFiles(t *testing.T, configRoot, slug string) {
	t.Helper()
	agentDir := filepath.Join(configRoot, config.DotScion, "agents", slug)
	writeRelinkFile(t, filepath.Join(agentDir, "home", "state.txt"), relinkState)
	writeRelinkFile(t, filepath.Join(agentDir, "scion-agent.json"), `{}`)
	writeRelinkFile(t, filepath.Join(agentDir, "image-provenance.json"), `{"version": 1}`)
	writeRelinkFile(t, filepath.Join(agentDir, "home", "agent-info.json"), `{"name": "`+slug+`"}`)
}

func (n *flatRelinkNode) seedStoppedOwned(t *testing.T, projectID, agentID, slug, runID, containerID string) {
	t.Helper()
	if err := n.srv.ownership.BeginRun(projectID, agentID, slug, runID); err != nil {
		t.Fatal(err)
	}
	if containerID != "" {
		if err := n.srv.ownership.AddResource(projectID, agentID, runID, api.ResourceHandle{
			Kind: api.ResourceKindContainer, Name: slug, UID: containerID,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.srv.ownership.SetRunState(projectID, agentID, runID, OwnershipStateCreated); err != nil {
		t.Fatal(err)
	}
}

func (h *flatRelinkHost) recordedWorkspaceID(t *testing.T) string {
	t.Helper()
	ident, err := config.ReadWorkspaceIdentity(h.root)
	if err != nil || ident == nil {
		t.Fatalf("ReadWorkspaceIdentity: ident=%v err=%v", ident, err)
	}
	return ident.ID
}

func (h *flatRelinkHost) startOrRestartBody(targetID, agentID, runID string, prevIDs []string) string {
	prev := ""
	if len(prevIDs) > 0 {
		quoted := make([]string, len(prevIDs))
		for i, p := range prevIDs {
			quoted[i] = strconvQuote(p)
		}
		prev = `, "previousProjectIds": [` + strings.Join(quoted, ", ") + `]`
	}
	return `{"runId": ` + strconvQuote(runID) +
		`, "expectedRuntimeTargetId": ` + strconvQuote(targetID) +
		`, "sharedWorkspace": true` +
		`, "projectPath": ` + strconvQuote(h.root) +
		`, "projectSlug": "proj"` +
		prev +
		`, "resolvedEnv": {"SCION_AGENT_ID": ` + strconvQuote(agentID) + `, "SCION_PROJECT_ID": ` + strconvQuote(relinkNewID) + `}}`
}

// TestFlatRelink_StartAndRestartResumeAndRetireOldOwnership covers
// ptone/scion#4764 and ptone/scion#4765 across start and restart, for both a
// .scion marker workspace and a git project workspace with previousProjectIds:
//   - !create runs the shared-workspace re-link and migrates ownership from
//     (relinkOldID, agentID) to (relinkNewID, agentID) before checking it;
//   - the agent resumes with its kept state at newRoot;
//   - (relinkOldID, agentID) is no longer reported active, so a sibling
//     instance's project cleanup on relinkOldID answers 204 instead of 409.
func TestFlatRelink_StartAndRestartResumeAndRetireOldOwnership(t *testing.T) {
	for _, gitProject := range []bool{false, true} {
		kind := "marker"
		if gitProject {
			kind = "git"
		}
		for _, op := range []string{"start", "restart"} {
			t.Run(kind+"/"+op, func(t *testing.T) {
				h := newFlatRelinkHost(t, gitProject)
				h.a.seedStoppedOwned(t, relinkOldID, flatRelinkAgentID, relinkAgent, "seed-run-1", "cid-old-1")

				// Before re-link, sibling B's project cleanup on relinkOldID is
				// refused because instance A has an active record in relinkOldID.
				pre := serveFlat(h.b.srv, http.MethodDelete, "/api/v1/projects/proj?project_id="+relinkOldID, "")
				if pre.Code != http.StatusConflict {
					t.Fatalf("pre-relink sibling project delete status = %d, want 409: %s", pre.Code, pre.Body.String())
				}

				path := "/api/v1/agents/" + relinkAgent + "/" + op + "?projectId=" + relinkNewID
				w := serveFlat(h.a.srv, http.MethodPost, path,
					h.startOrRestartBody(h.a.identity.RuntimeTarget.ID, flatRelinkAgentID, "run-after-relink", h.previousProjectIDs))
				if w.Code != http.StatusAccepted {
					t.Fatalf("%s status = %d, want 202: %s", op, w.Code, w.Body.String())
				}

				if got := h.recordedWorkspaceID(t); got != relinkNewID {
					t.Fatalf("workspace project ID = %q, want %q", got, relinkNewID)
				}
				if got, err := os.ReadFile(filepath.Join(h.newRoot, config.DotScion, "agents", relinkAgent, "home", "state.txt")); err != nil || string(got) != relinkState {
					t.Fatalf("agent state at newRoot = %q, err = %v", got, err)
				}
				assertAbsent(t, h.oldRoot)

				// New ownership record is active and retains the earlier run's
				// recorded container handle alongside the new run.
				rec, ok, err := h.a.srv.ownership.Get(relinkNewID, flatRelinkAgentID)
				if err != nil || !ok || rec.State != OwnershipStateActive {
					t.Fatalf("new ownership record: rec=%+v ok=%v err=%v", rec, ok, err)
				}
				if rec.Run("seed-run-1") == nil || rec.Run("run-after-relink") == nil {
					t.Fatalf("runs = %+v, want seed-run-1 and run-after-relink", rec.Runs)
				}
				if owns, err := h.a.srv.ownership.RecordsResource("cid-old-1"); err != nil || !owns {
					t.Fatalf("RecordsResource(cid-old-1) = %v, %v; want true", owns, err)
				}

				// Earlier project ID no longer has any active record or slug reservation.
				if oldRec, ok, err := h.a.srv.ownership.Get(relinkOldID, flatRelinkAgentID); err != nil || ok {
					t.Fatalf("stale record under earlier project ID: rec=%+v ok=%v err=%v", oldRec, ok, err)
				}
				if live, err := h.a.srv.ownership.HasLiveAgents(relinkOldID); err != nil || live {
					t.Fatalf("HasLiveAgents(relinkOldID) = %v, %v; want false", live, err)
				}
				if holder, pending, err := h.a.srv.ownership.SlugReservation(relinkOldID, relinkAgent); err != nil || holder != "" || pending {
					t.Fatalf("SlugReservation(relinkOldID) = (%q, %v, %v), want empty", holder, pending, err)
				}
				if problems, err := h.a.srv.ownership.RepairSlugIndex(); err != nil || len(problems) != 0 {
					t.Fatalf("RepairSlugIndex = %v, %v; want none", problems, err)
				}

				// Sibling B's project cleanup on relinkOldID is no longer blocked with 409.
				post := serveFlat(h.b.srv, http.MethodDelete, "/api/v1/projects/proj?project_id="+relinkOldID, "")
				if post.Code != http.StatusNoContent {
					t.Fatalf("post-relink sibling project delete status = %d, want 204: %s", post.Code, post.Body.String())
				}

				// A subsequent start/restart on relinkNewID continues to pass.
				w2 := serveFlat(h.a.srv, http.MethodPost, path,
					h.startOrRestartBody(h.a.identity.RuntimeTarget.ID, flatRelinkAgentID, "run-second", h.previousProjectIDs))
				if w2.Code != http.StatusAccepted {
					t.Fatalf("second %s status = %d, want 202: %s", op, w2.Code, w2.Body.String())
				}
			})
		}
	}
}

// TestFlatRelink_CreateRetiresEarlierOwnershipRecord covers ptone/scion#4764
// when a re-linked resume reaches the flat Runtime Broker as a create (both
// when the agent ID is unchanged and when the Hub minted a new agent ID row
// for the re-linked project).
func TestFlatRelink_CreateRetiresEarlierOwnershipRecord(t *testing.T) {
	for _, tc := range []struct {
		name       string
		newAgentID string
	}{
		{name: "same agent ID", newAgentID: flatRelinkAgentID},
		{name: "new Hub agent ID for same slug", newAgentID: flatRelinkNewAgentID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFlatRelinkHost(t, true)
			h.a.seedStoppedOwned(t, relinkOldID, flatRelinkAgentID, relinkAgent, "seed-run", "cid-old")

			body := `{"requestId": "req-relink-create", "name": "` + relinkAgent + `", "slug": "` + relinkAgent + `",` +
				`"id": ` + strconvQuote(tc.newAgentID) + `, "provisionOnly": true,` +
				`"expectedRuntimeTargetId": ` + strconvQuote(h.a.identity.RuntimeTarget.ID) + `,` +
				`"projectId": "` + relinkNewID + `", "projectSlug": "proj",` +
				`"previousProjectIds": ["` + relinkOldID + `"],` +
				`"config": {"template": "claude", "sharedWorkspace": true}}`
			w := serveFlat(h.a.srv, http.MethodPost, "/api/v1/agents", body)
			if w.Code != http.StatusCreated {
				t.Fatalf("create status = %d, want 201: %s", w.Code, w.Body.String())
			}

			if got := h.recordedWorkspaceID(t); got != relinkNewID {
				t.Fatalf("workspace project ID = %q, want %q", got, relinkNewID)
			}
			assertAbsent(t, h.oldRoot)

			rec, ok, err := h.a.srv.ownership.Get(relinkNewID, tc.newAgentID)
			if err != nil || !ok || rec.State != OwnershipStateActive {
				t.Fatalf("new ownership record: rec=%+v ok=%v err=%v", rec, ok, err)
			}
			if live, err := h.a.srv.ownership.HasLiveAgents(relinkOldID); err != nil || live {
				t.Fatalf("HasLiveAgents(relinkOldID) = %v, %v; want false", live, err)
			}
			post := serveFlat(h.b.srv, http.MethodDelete, "/api/v1/projects/proj?project_id="+relinkOldID, "")
			if post.Code != http.StatusNoContent {
				t.Fatalf("post-relink sibling project delete status = %d, want 204: %s", post.Code, post.Body.String())
			}
		})
	}
}

// TestFlatRelink_MultiAgentSharedWorkspaceCarriesAllInstanceRecords verifies
// that when one flat instance owned multiple stopped shared-workspace agents
// in the earlier project config directory, carrying that directory over to the
// new project ID moves all of those agents' ownership records so neither
// CleanupProject(relinkOldID) nor starting the second agent is stranded.
func TestFlatRelink_MultiAgentSharedWorkspaceCarriesAllInstanceRecords(t *testing.T) {
	h := newFlatRelinkHost(t, false)
	const secondSlug = "relink-agent-2"
	h.writeAgentFiles(t, h.oldRoot, secondSlug)
	h.a.seedStoppedOwned(t, relinkOldID, flatRelinkAgentID, relinkAgent, "seed-1", "cid-1")
	h.a.seedStoppedOwned(t, relinkOldID, flatRelinkAgentID2, secondSlug, "seed-2", "cid-2")

	w1 := serveFlat(h.a.srv, http.MethodPost, "/api/v1/agents/"+relinkAgent+"/start?projectId="+relinkNewID,
		h.startOrRestartBody(h.a.identity.RuntimeTarget.ID, flatRelinkAgentID, "run-1-new", nil))
	if w1.Code != http.StatusAccepted {
		t.Fatalf("first agent start status = %d, want 202: %s", w1.Code, w1.Body.String())
	}
	if live, err := h.a.srv.ownership.HasLiveAgents(relinkOldID); err != nil || live {
		t.Fatalf("HasLiveAgents(relinkOldID) after first start = %v, %v; want false", live, err)
	}
	post := serveFlat(h.b.srv, http.MethodDelete, "/api/v1/projects/proj?project_id="+relinkOldID, "")
	if post.Code != http.StatusNoContent {
		t.Fatalf("sibling project delete status = %d, want 204: %s", post.Code, post.Body.String())
	}

	// Second agent's marker already names relinkNewID; its !create start must succeed too.
	w2 := serveFlat(h.a.srv, http.MethodPost, "/api/v1/agents/"+secondSlug+"/start?projectId="+relinkNewID,
		h.startOrRestartBody(h.a.identity.RuntimeTarget.ID, flatRelinkAgentID2, "run-2-new", nil))
	if w2.Code != http.StatusAccepted {
		t.Fatalf("second agent start status = %d, want 202: %s", w2.Code, w2.Body.String())
	}
}

// TestFlatRelink_UnownedAndDifferentInstanceStartRefusedWithoutSideEffects
// verifies that !create never adopts an unowned agent or mutates a workspace
// belonging to another instance or lacking an active record under the
// workspace's earlier project ID.
func TestFlatRelink_UnownedAndDifferentInstanceStartRefusedWithoutSideEffects(t *testing.T) {
	t.Run("unrecorded on this instance", func(t *testing.T) {
		for _, op := range []string{"start", "restart"} {
			t.Run(op, func(t *testing.T) {
				h := newFlatRelinkHost(t, false)
				path := "/api/v1/agents/" + relinkAgent + "/" + op + "?projectId=" + relinkNewID
				w := serveFlat(h.a.srv, http.MethodPost, path,
					h.startOrRestartBody(h.a.identity.RuntimeTarget.ID, flatRelinkForeignAgent, "run-unowned", nil))
				if w.Code != http.StatusConflict {
					t.Fatalf("%s status = %d, want 409: %s", op, w.Code, w.Body.String())
				}
				if !strings.Contains(w.Body.String(), "The agent's ownership cannot be recorded by this Runtime Broker instance") {
					t.Fatalf("expected ErrOwnershipNotRecorded message, got: %s", w.Body.String())
				}
				_, bErr := h.a.srv.buildStartContext(context.Background(), startContextInputs{
					Name:             relinkAgent,
					Slug:             relinkAgent,
					AgentID:          flatRelinkForeignAgent,
					ProjectID:        relinkNewID,
					ProjectSlug:      "proj",
					ProjectPath:      h.root,
					SharedWorkspace:  true,
					HTTPRequest:      httptest.NewRequest(http.MethodPost, path, nil),
					Operation:        opHTTPStart,
					OwnershipAgentID: flatRelinkForeignAgent,
				})
				var sce *startContextError
				if !errors.As(bErr, &sce) || !errors.Is(sce.OriginalErr, ErrOwnershipNotRecorded) {
					t.Fatalf("buildStartContext err = %v, want ErrOwnershipNotRecorded", bErr)
				}
				if got := h.recordedWorkspaceID(t); got != relinkOldID {
					t.Fatalf("marker mutated to %q", got)
				}
				assertAbsent(t, h.newRoot)
				if _, err := os.Stat(h.oldRoot); err != nil {
					t.Fatalf("oldRoot missing: %v", err)
				}
				if keys, err := h.a.srv.ownership.LiveKeys(); err != nil || len(keys) != 0 {
					t.Fatalf("instance A LiveKeys = %v, %v; want empty", keys, err)
				}
			})
		}
	})

	t.Run("owned by sibling instance B", func(t *testing.T) {
		for _, op := range []string{"start", "restart"} {
			t.Run(op, func(t *testing.T) {
				h := newFlatRelinkHost(t, false)
				h.b.seedStoppedOwned(t, relinkOldID, flatRelinkAgentID, relinkAgent, "seed-b", "cid-b")

				path := "/api/v1/agents/" + relinkAgent + "/" + op + "?projectId=" + relinkNewID
				w := serveFlat(h.a.srv, http.MethodPost, path,
					h.startOrRestartBody(h.a.identity.RuntimeTarget.ID, flatRelinkAgentID, "run-steal", nil))
				if w.Code != http.StatusConflict {
					t.Fatalf("%s status = %d, want 409: %s", op, w.Code, w.Body.String())
				}
				if !strings.Contains(w.Body.String(), "The agent's ownership cannot be recorded by this Runtime Broker instance") {
					t.Fatalf("expected ErrOwnershipNotRecorded message, got: %s", w.Body.String())
				}
				_, bErr := h.a.srv.buildStartContext(context.Background(), startContextInputs{
					Name:             relinkAgent,
					Slug:             relinkAgent,
					AgentID:          flatRelinkAgentID,
					ProjectID:        relinkNewID,
					ProjectSlug:      "proj",
					ProjectPath:      h.root,
					SharedWorkspace:  true,
					HTTPRequest:      httptest.NewRequest(http.MethodPost, path, nil),
					Operation:        opHTTPStart,
					OwnershipAgentID: flatRelinkAgentID,
				})
				var sce *startContextError
				if !errors.As(bErr, &sce) || !errors.Is(sce.OriginalErr, ErrOwnershipNotRecorded) {
					t.Fatalf("buildStartContext err = %v, want ErrOwnershipNotRecorded", bErr)
				}
				if got := h.recordedWorkspaceID(t); got != relinkOldID {
					t.Fatalf("marker mutated to %q", got)
				}
				assertAbsent(t, h.newRoot)
				if keys, err := h.a.srv.ownership.LiveKeys(); err != nil || len(keys) != 0 {
					t.Fatalf("instance A LiveKeys = %v, %v; want empty", keys, err)
				}
				if rec, ok, err := h.b.srv.ownership.Get(relinkOldID, flatRelinkAgentID); err != nil || !ok || rec.State != OwnershipStateActive {
					t.Fatalf("instance B ownership disturbed: rec=%+v ok=%v err=%v", rec, ok, err)
				}
			})
		}
	})

	t.Run("same agent ID owned in unrelated project on instance A", func(t *testing.T) {
		h := newFlatRelinkHost(t, false)
		h.a.seedStoppedOwned(t, relinkOtherID, flatRelinkAgentID, relinkAgent, "seed-other", "cid-other")

		w := serveFlat(h.a.srv, http.MethodPost, "/api/v1/agents/"+relinkAgent+"/start?projectId="+relinkNewID,
			h.startOrRestartBody(h.a.identity.RuntimeTarget.ID, flatRelinkAgentID, "run-wrong-proj", nil))
		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "The agent's ownership cannot be recorded by this Runtime Broker instance") {
			t.Fatalf("expected ErrOwnershipNotRecorded message, got: %s", w.Body.String())
		}
		_, bErr := h.a.srv.buildStartContext(context.Background(), startContextInputs{
			Name:             relinkAgent,
			Slug:             relinkAgent,
			AgentID:          flatRelinkAgentID,
			ProjectID:        relinkNewID,
			ProjectSlug:      "proj",
			ProjectPath:      h.root,
			SharedWorkspace:  true,
			HTTPRequest:      httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+relinkAgent+"/start", nil),
			Operation:        opHTTPStart,
			OwnershipAgentID: flatRelinkAgentID,
		})
		var sce *startContextError
		if !errors.As(bErr, &sce) || !errors.Is(sce.OriginalErr, ErrOwnershipNotRecorded) {
			t.Fatalf("buildStartContext err = %v, want ErrOwnershipNotRecorded", bErr)
		}
		if got := h.recordedWorkspaceID(t); got != relinkOldID {
			t.Fatalf("marker mutated to %q", got)
		}
		assertAbsent(t, h.newRoot)
		if rec, ok, err := h.a.srv.ownership.Get(relinkOtherID, flatRelinkAgentID); err != nil || !ok || rec.State != OwnershipStateActive {
			t.Fatalf("unrelated project ownership disturbed: rec=%+v ok=%v err=%v", rec, ok, err)
		}
	})

	t.Run("git project without previousProjectIds not carried", func(t *testing.T) {
		h := newFlatRelinkHost(t, true)
		h.a.seedStoppedOwned(t, relinkOldID, flatRelinkAgentID, relinkAgent, "seed-git", "cid-git")

		w := serveFlat(h.a.srv, http.MethodPost, "/api/v1/agents/"+relinkAgent+"/start?projectId="+relinkNewID,
			h.startOrRestartBody(h.a.identity.RuntimeTarget.ID, flatRelinkAgentID, "run-no-prev", nil))
		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
		}
		if got := h.recordedWorkspaceID(t); got != relinkOldID {
			t.Fatalf("marker mutated to %q", got)
		}
		assertAbsent(t, h.newRoot)
		if rec, ok, err := h.a.srv.ownership.Get(relinkOldID, flatRelinkAgentID); err != nil || !ok || rec.State != OwnershipStateActive {
			t.Fatalf("ownership record under relinkOldID disturbed: rec=%+v ok=%v err=%v", rec, ok, err)
		}
	})
}

// TestFlatRelink_CrashAndRollbackIdempotency covers interrupted re-links and
// marker-write failures on flat Runtime Broker instances.
func TestFlatRelink_CrashAndRollbackIdempotency(t *testing.T) {
	t.Run("directory moved before crash, ownership still at old ID", func(t *testing.T) {
		h := newFlatRelinkHost(t, false)
		h.a.seedStoppedOwned(t, relinkOldID, flatRelinkAgentID, relinkAgent, "seed-run", "cid-1")
		if err := os.Rename(h.oldRoot, h.newRoot); err != nil {
			t.Fatal(err)
		}

		w := serveFlat(h.a.srv, http.MethodPost, "/api/v1/agents/"+relinkAgent+"/start?projectId="+relinkNewID,
			h.startOrRestartBody(h.a.identity.RuntimeTarget.ID, flatRelinkAgentID, "run-retry", nil))
		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202: %s", w.Code, w.Body.String())
		}
		if got := h.recordedWorkspaceID(t); got != relinkNewID {
			t.Fatalf("marker = %q, want %q", got, relinkNewID)
		}
		if live, err := h.a.srv.ownership.HasLiveAgents(relinkOldID); err != nil || live {
			t.Fatalf("HasLiveAgents(relinkOldID) = %v, %v; want false", live, err)
		}
		if rec, ok, err := h.a.srv.ownership.Get(relinkNewID, flatRelinkAgentID); err != nil || !ok || rec.State != OwnershipStateActive {
			t.Fatalf("new ownership record: rec=%+v ok=%v err=%v", rec, ok, err)
		}
	})

	t.Run("directory moved and ownership already at new ID before crash", func(t *testing.T) {
		h := newFlatRelinkHost(t, false)
		h.a.seedStoppedOwned(t, relinkNewID, flatRelinkAgentID, relinkAgent, "seed-run", "cid-1")
		if err := os.Rename(h.oldRoot, h.newRoot); err != nil {
			t.Fatal(err)
		}

		w := serveFlat(h.a.srv, http.MethodPost, "/api/v1/agents/"+relinkAgent+"/start?projectId="+relinkNewID,
			h.startOrRestartBody(h.a.identity.RuntimeTarget.ID, flatRelinkAgentID, "run-retry-2", nil))
		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202: %s", w.Code, w.Body.String())
		}
		if got := h.recordedWorkspaceID(t); got != relinkNewID {
			t.Fatalf("marker = %q, want %q", got, relinkNewID)
		}
		if live, err := h.a.srv.ownership.HasLiveAgents(relinkOldID); err != nil || live {
			t.Fatalf("HasLiveAgents(relinkOldID) = %v, %v; want false", live, err)
		}
	})

	t.Run("marker write failure rolls back directory and ownership", func(t *testing.T) {
		h := newFlatRelinkHost(t, false)
		h.a.seedStoppedOwned(t, relinkOldID, flatRelinkAgentID, relinkAgent, "seed-run", "cid-1")

		prevWrite := relinkWriteMarker
		relinkWriteMarker = func(*config.WorkspaceIdentity, string, string) error {
			return errors.New("injected marker write error")
		}
		defer func() { relinkWriteMarker = prevWrite }()

		w := serveFlat(h.a.srv, http.MethodPost, "/api/v1/agents/"+relinkAgent+"/start?projectId="+relinkNewID,
			h.startOrRestartBody(h.a.identity.RuntimeTarget.ID, flatRelinkAgentID, "run-fail", nil))
		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
		}
		assertAbsent(t, h.newRoot)
		if _, err := os.Stat(h.oldRoot); err != nil {
			t.Fatalf("oldRoot not restored: %v", err)
		}
		if got := h.recordedWorkspaceID(t); got != relinkOldID {
			t.Fatalf("marker = %q, want %q", got, relinkOldID)
		}
		if live, err := h.a.srv.ownership.HasLiveAgents(relinkNewID); err != nil || live {
			t.Fatalf("HasLiveAgents(relinkNewID) after rollback = %v, %v; want false", live, err)
		}
		if rec, ok, err := h.a.srv.ownership.Get(relinkOldID, flatRelinkAgentID); err != nil || !ok || rec.State != OwnershipStateActive {
			t.Fatalf("ownership under relinkOldID not restored: rec=%+v ok=%v err=%v", rec, ok, err)
		}

		// Clearing the fault lets the next start complete cleanly.
		relinkWriteMarker = prevWrite
		w2 := serveFlat(h.a.srv, http.MethodPost, "/api/v1/agents/"+relinkAgent+"/start?projectId="+relinkNewID,
			h.startOrRestartBody(h.a.identity.RuntimeTarget.ID, flatRelinkAgentID, "run-recovered", nil))
		if w2.Code != http.StatusAccepted {
			t.Fatalf("retry status = %d, want 202: %s", w2.Code, w2.Body.String())
		}
		if live, err := h.a.srv.ownership.HasLiveAgents(relinkOldID); err != nil || live {
			t.Fatalf("HasLiveAgents(relinkOldID) after recovery = %v, %v; want false", live, err)
		}
	})
}

// TestFlatRelink_LegacyBrokerUnchanged verifies that non-flat Runtime Brokers
// (s.ownership == nil) resume across a project re-link unchanged.
func TestFlatRelink_LegacyBrokerUnchanged(t *testing.T) {
	for _, op := range []string{"start", "restart"} {
		t.Run(op, func(t *testing.T) {
			h := newHubNativeRelink(t)
			if h.srv.ownership != nil {
				t.Fatal("expected legacy Runtime Broker with nil ownership store")
			}
			assertNotEntryRefusal(t, op, h.startOrRestart(t, op))
			h.assertCarried(t)
			h.assertRecordNamesNewID(t)
		})
	}
}

// TestOwnershipStore_MoveProjectRecord exercises MoveProjectRecord directly:
// carry of runs/resources and PreviousProjectIDs, idempotency, undo rollback,
// RepairSlugIndex convergence of a mid-move crash, and startup Reconstruct of
// stopped pre-relink runtime objects still labelled with the earlier project ID.
func TestOwnershipStore_MoveProjectRecord(t *testing.T) {
	store := NewOwnershipStore(t.TempDir(), "flat-broker-unit")
	if err := store.BeginRun(relinkOldID, flatRelinkAgentID, relinkAgent, "run-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.AddResource(relinkOldID, flatRelinkAgentID, "run-1", api.ResourceHandle{
		Kind: api.ResourceKindContainer,
		UID:  "cid-unit-1",
		Name: "scion-" + relinkAgent,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRunState(relinkOldID, flatRelinkAgentID, "run-1", OwnershipStateCreated); err != nil {
		t.Fatal(err)
	}

	undo, moved, err := store.MoveProjectRecord(relinkOldID, flatRelinkAgentID, relinkNewID, flatRelinkAgentID, relinkAgent)
	if err != nil || !moved {
		t.Fatalf("MoveProjectRecord = (%v, %v), want moved=true, err=nil", moved, err)
	}
	if live, err := store.HasLiveAgents(relinkOldID); err != nil || live {
		t.Fatalf("HasLiveAgents(relinkOldID) = %v, %v; want false", live, err)
	}
	rec, ok, err := store.Get(relinkNewID, flatRelinkAgentID)
	if err != nil || !ok || rec.State != OwnershipStateActive {
		t.Fatalf("Get(relinkNewID) = (%+v, %v, %v)", rec, ok, err)
	}
	if len(rec.PreviousProjectIDs) != 1 || rec.PreviousProjectIDs[0] != relinkOldID {
		t.Fatalf("PreviousProjectIDs = %v, want [%s]", rec.PreviousProjectIDs, relinkOldID)
	}
	if !rec.OwnsUID("cid-unit-1") {
		t.Fatalf("moved record lost resource cid-unit-1: %+v", rec.Runs)
	}

	// Idempotent retry when already moved.
	if _, moved2, err := store.MoveProjectRecord(relinkOldID, flatRelinkAgentID, relinkNewID, flatRelinkAgentID, relinkAgent); err != nil || moved2 {
		t.Fatalf("idempotent MoveProjectRecord = (%v, %v), want moved=false, err=nil", moved2, err)
	}

	// Undo restores old record & slug and removes new record & slug.
	if err := undo(); err != nil {
		t.Fatalf("undo() = %v", err)
	}
	if live, err := store.HasLiveAgents(relinkNewID); err != nil || live {
		t.Fatalf("HasLiveAgents(relinkNewID) after undo = %v, %v; want false", live, err)
	}
	if live, err := store.HasLiveAgents(relinkOldID); err != nil || !live {
		t.Fatalf("HasLiveAgents(relinkOldID) after undo = %v, %v; want true", live, err)
	}

	// Move again with a new Hub agent ID (create-time re-link), then verify
	// startup Reconstruct of the pre-relink stopped container neither fails nor
	// resurrects relinkOldID.
	if _, moved3, err := store.MoveProjectRecord(relinkOldID, flatRelinkAgentID, relinkNewID, flatRelinkNewAgentID, relinkAgent); err != nil || !moved3 {
		t.Fatalf("MoveProjectRecord(newAgentID) = (%v, %v), want true, nil", moved3, err)
	}
	rt := &runtime.MockRuntime{}
	if err := store.Reconstruct(api.AgentInfo{
		Name:        relinkAgent,
		ContainerID: "cid-unit-1",
		Runtime:     "docker",
		Labels: map[string]string{
			api.LabelRuntimeBrokerID: "flat-broker-unit",
			"scion.project_id":       relinkOldID,
			"agent_id":               flatRelinkAgentID,
			"scion.name":             relinkAgent,
			api.LabelRunID:           "run-1",
		},
	}, rt); err != nil {
		t.Fatalf("Reconstruct pre-relink container = %v", err)
	}
	if live, err := store.HasLiveAgents(relinkOldID); err != nil || live {
		t.Fatalf("HasLiveAgents(relinkOldID) after Reconstruct = %v, %v; want false", live, err)
	}

	// Simulate an interrupted move that left the old record & slug behind after
	// writing the new record with PreviousProjectIDs: RepairSlugIndex finishes it.
	oldRec := *rec
	oldRec.ProjectID = relinkOldID
	oldRec.AgentID = flatRelinkAgentID
	oldRec.PreviousProjectIDs = nil
	oldRecPath, _ := store.recordPath(relinkOldID, flatRelinkAgentID)
	oldSlugPath, _ := store.slugPath(relinkOldID, relinkAgent)
	if err := writeOwnershipFile(oldRecPath, mustJSON(&oldRec)); err != nil {
		t.Fatal(err)
	}
	if err := writeOwnershipFile(oldSlugPath, []byte(flatRelinkAgentID+"\n")); err != nil {
		t.Fatal(err)
	}
	if problems, err := store.RepairSlugIndex(); err != nil || len(problems) != 0 {
		t.Fatalf("RepairSlugIndex = %v, %v; want none", problems, err)
	}
	if live, err := store.HasLiveAgents(relinkOldID); err != nil || live {
		t.Fatalf("HasLiveAgents(relinkOldID) after RepairSlugIndex = %v, %v; want false", live, err)
	}
}
