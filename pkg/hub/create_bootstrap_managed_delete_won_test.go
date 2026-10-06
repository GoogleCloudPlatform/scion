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
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/managedagent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The workspace-bootstrap and managed (hub-direct) creates dispatch nothing
// to a broker, so they never reached the ptone/scion#3099 check. A delete
// that wins the race now answers 409 delete_in_progress there too, with no
// agent body and no upload URLs; a failed or expired delete leaves the
// agent live, so the create still answers 201 (ptone/scion#3454).

// requireNoCreateBody checks a 409 carries neither upload URLs nor an
// expiry, on top of requireDeletedDuringCreate's no-agent check.
func requireNoCreateBody(t *testing.T, body []byte) {
	t.Helper()
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &raw))
	assert.NotContains(t, raw, "uploadUrls", "the 409 carries no upload URLs")
	assert.NotContains(t, raw, "expires")
}

// bootstrapRequest is a workspace-bootstrap create: workspace files and a
// task, against a broker with no local path for the project.
func bootstrapRequest(projectID, name string) CreateAgentRequest {
	return CreateAgentRequest{
		Name: name, ProjectID: projectID, Task: "do it",
		WorkspaceFiles: []transfer.FileInfo{{Path: "main.go", Size: 100, Hash: "sha256:abc123"}},
	}
}

// onlyAgentID returns the ID of the one agent in project.
func onlyAgentID(t *testing.T, s store.Store, projectID string) string {
	t.Helper()
	result, err := s.ListAgents(context.Background(), store.AgentFilter{ProjectID: projectID, IncludeDeleted: true}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	return result.Items[0].ID
}

func TestBootstrapCreate_DeleteWon_Answers409(t *testing.T) {
	for i, del := range landingDeletes {
		t.Run(del.name, func(t *testing.T) {
			srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
			pub := recordCreatedEvents(t, srv)
			var agentID string
			// The upload URLs are signed after the row is written and
			// before the created publish.
			srv.SetStorage(&hookStorage{mockStorage: newMockStorage("test-bucket"), hook: func() {
				agentID = onlyAgentID(t, s, project.ID)
				del.apply(t, s, agentID)
			}})

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents",
				bootstrapRequest(project.ID, "boot-"+string(rune('a'+i))))
			require.NotEmpty(t, agentID, "the hook ran: %d %s", rec.Code, rec.Body.String())

			if !del.compensate {
				require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
				var resp CreateAgentResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
				require.NotNil(t, resp.Agent)
				assert.Equal(t, agentID, resp.Agent.ID, "the agent body is returned")
				assert.NotEmpty(t, resp.UploadURLs, "the upload URLs are returned")
				assert.Equal(t, 1, pub.count("created"), "created is published: %v", pub.kinds())
				return
			}

			warnings := requireDeletedDuringCreate(t, rec, agentID)
			assert.Empty(t, warnings, "nothing was dispatched, so nothing to report")
			requireNoCreateBody(t, rec.Body.Bytes())
			assert.Zero(t, pub.count("created"), "no created: %v", pub.kinds())
		})
	}
}

// recordingManagedBackend is a managed-agent backend whose CreateInteraction
// runs hook (with the create's row ID) and succeeds, and which records the
// interactions it is asked to cancel. Every interaction reads in progress.
type recordingManagedBackend struct {
	failingManagedAgentBackend
	s         store.Store
	projectID string
	hook      func(agentID string)

	mu        sync.Mutex
	cancelled []string
	cancelErr error
}

func (b *recordingManagedBackend) CreateInteraction(context.Context, managedagent.InteractionRequest) (*managedagent.InteractionHandle, error) {
	if b.hook != nil {
		result, err := b.s.ListAgents(context.Background(), store.AgentFilter{ProjectID: b.projectID}, store.ListOptions{})
		if err == nil && len(result.Items) == 1 {
			b.hook(result.Items[0].ID)
		}
	}
	return &managedagent.InteractionHandle{InteractionID: "interaction-1"}, nil
}

func (b *recordingManagedBackend) GetInteraction(_ context.Context, id string) (*managedagent.InteractionState, error) {
	return &managedagent.InteractionState{InteractionID: id, Status: managedagent.StatusInProgress}, nil
}

func (b *recordingManagedBackend) CancelInteraction(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cancelled = append(b.cancelled, id)
	return b.cancelErr
}

func (b *recordingManagedBackend) cancels() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.cancelled...)
}

// useManagedBackend installs backend for the test. Tests that call it must
// not run in parallel: the backend is package-level.
func useManagedBackend(t *testing.T, backend managedagent.ManagedAgentBackend) {
	t.Helper()
	managedBackendMu.Lock()
	prev := managedBackendInst
	managedBackendInst = backend
	managedBackendMu.Unlock()
	t.Cleanup(func() {
		managedBackendMu.Lock()
		managedBackendInst = prev
		managedBackendMu.Unlock()
	})
}

// afterRunningWriteStore runs hook once, right after the managed create's
// post-create write (phase running) succeeds: a delete that claims the row
// after that write and before the created publish.
type afterRunningWriteStore struct {
	store.Store
	once sync.Once
	hook func(agentID string)
}

func (s *afterRunningWriteStore) UpdateAgent(ctx context.Context, a *store.Agent) error {
	if err := s.Store.UpdateAgent(ctx, a); err != nil {
		return err
	}
	if a.Phase == string(state.PhaseRunning) {
		s.once.Do(func() { s.hook(a.ID) })
	}
	return nil
}

// The delete lands while the backend creates the first interaction, before
// the create's post-create write. A delete claim bumps state_version, so
// that write conflicts and the delete's row has no interaction ID: the
// create stops the interaction itself and reports it. A hard delete or soft
// delete written straight to the store (no engine) behaves the same.
func TestManagedCreate_DeleteWon_BeforeWrite_Answers409AndStopsInteraction(t *testing.T) {
	for i, del := range landingDeletes {
		t.Run(del.name, func(t *testing.T) {
			srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
			pub := recordCreatedEvents(t, srv)
			var agentID string
			backend := &recordingManagedBackend{s: s, projectID: project.ID, hook: func(id string) {
				agentID = id
				del.apply(t, s, id)
			}}
			useManagedBackend(t, backend)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name: "mgd-" + string(rune('a'+i)), ProjectID: project.ID, Task: "do it", Profile: ManagedAgentsProfile,
			})
			require.NotEmpty(t, agentID, "the hook ran: %d %s", rec.Code, rec.Body.String())

			if !del.compensate {
				require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
				var resp CreateAgentResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
				require.NotNil(t, resp.Agent)
				assert.Equal(t, agentID, resp.Agent.ID, "the agent body is returned")
				assert.Empty(t, backend.cancels(), "a live agent's interaction is left running")
				assert.Equal(t, 1, pub.count("created"), "created is published: %v", pub.kinds())
				return
			}

			warnings := requireDeletedDuringCreate(t, rec, agentID)
			assert.Equal(t, []string{managedCreateCompensatedWarning}, warnings)
			assert.Equal(t, []string{"interaction-1"}, backend.cancels(),
				"the create stops the interaction the delete cannot see")
			assert.Zero(t, pub.count("created"), "no created: %v", pub.kinds())
		})
	}
}

// The delete claims the row after the create's post-create write landed.
// The delete's row then carries the interaction ID and the delete stops it,
// so the create does not stop it a second time.
func TestManagedCreate_DeleteWon_AfterWrite_Answers409WithoutSecondStop(t *testing.T) {
	for i, del := range landingDeletes {
		if !del.compensate {
			continue // covered by the before-write test
		}
		t.Run(del.name, func(t *testing.T) {
			srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
			pub := recordCreatedEvents(t, srv)
			var agentID string
			srv.store = &afterRunningWriteStore{Store: s, hook: func(id string) {
				agentID = id
				del.apply(t, s, id)
			}}
			backend := &recordingManagedBackend{s: s, projectID: project.ID}
			useManagedBackend(t, backend)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name: "mgd-after-" + string(rune('a'+i)), ProjectID: project.ID, Task: "do it", Profile: ManagedAgentsProfile,
			})
			require.NotEmpty(t, agentID, "the hook ran: %d %s", rec.Code, rec.Body.String())
			warnings := requireDeletedDuringCreate(t, rec, agentID)
			assert.Empty(t, warnings, "the delete owns the stop")
			assert.Empty(t, backend.cancels(), "no second stop from the create")
			assert.Zero(t, pub.count("created"), "no created: %v", pub.kinds())
		})
	}
}

// A managed create with no task starts no interaction: a delete that wins
// still answers 409, and there is nothing to stop.
func TestManagedCreate_NoTask_DeleteWon_Answers409(t *testing.T) {
	srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
	backend := &recordingManagedBackend{s: s, projectID: project.ID}
	useManagedBackend(t, backend)
	var agentID string
	srv.store = &afterRunningWriteStore{Store: s, hook: func(id string) {
		agentID = id
		require.NoError(t, s.DeleteAgent(context.Background(), id))
	}}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "mgd-notask", ProjectID: project.ID, Profile: ManagedAgentsProfile,
	})
	require.NotEmpty(t, agentID, "the hook ran: %d %s", rec.Code, rec.Body.String())
	assert.Empty(t, requireDeletedDuringCreate(t, rec, agentID))
	assert.Empty(t, backend.cancels())
}

// The rule itself, per case.
func TestCompensateManagedCreate(t *testing.T) {
	srv, s := testServer(t)
	backend := &recordingManagedBackend{s: s}
	useManagedBackend(t, backend)
	ctx := context.Background()
	withID := &store.Agent{ID: "a1", Annotations: map[string]string{annotationInteractionID: "i-1"}}

	assert.Nil(t, srv.compensateManagedCreate(ctx, withID, true), "recorded: the delete owns the stop")
	assert.Nil(t, srv.compensateManagedCreate(ctx, &store.Agent{ID: "a2"}, false), "no interaction: nothing to stop")
	assert.Empty(t, backend.cancels())

	assert.Equal(t, []string{managedCreateCompensatedWarning}, srv.compensateManagedCreate(ctx, withID, false))
	assert.Equal(t, []string{"i-1"}, backend.cancels())
}
