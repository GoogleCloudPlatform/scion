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

// handleExistingAgent's env-gather re-provisioning branch removes the
// existing provisioning row the way a create rollback does
// (ptone/scion#4075): conditionally, leaving a row that a delete holds to
// that delete. A delete holds the row with a live deleting claim or while
// finalizing, and a row already removed or soft-deleted is the delete's as
// well; the answer is then 409 delete_in_progress, the row and its quotas
// are left alone and nothing is created. A failed delete, or a deleting row
// whose lease lapsed, does not hold the row, and the recreate goes ahead as
// before.
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// envGatherDeleteDispatcher runs onDelete inside DispatchAgentDelete, the
// broker call that runs after the start gate and before the row removal,
// and counts env-gather create dispatches.
type envGatherDeleteDispatcher struct {
	*createAgentDispatcher
	onDelete func(agentID string)
	creates  int
}

func (d *envGatherDeleteDispatcher) DispatchAgentDelete(ctx context.Context, a *store.Agent, deleteFiles, removeBranch, softDelete bool, deletedAt time.Time) error {
	if d.onDelete != nil {
		d.onDelete(a.ID)
	}
	return d.createAgentDispatcher.DispatchAgentDelete(ctx, a, deleteFiles, removeBranch, softDelete, deletedAt)
}

func (d *envGatherDeleteDispatcher) DispatchAgentCreateWithGather(ctx context.Context, a *store.Agent) (*CreateDispatchResult, error) {
	d.creates++
	return d.createAgentDispatcher.DispatchAgentCreateWithGather(ctx, a)
}

// envGatherRecreate is an existing provisioning agent ready for a second
// env-gather create of the same name.
type envGatherRecreate struct {
	srv     *Server
	s       store.Store
	disp    *envGatherDeleteDispatcher
	cs      *rollbackClaimStore
	fault   *storeFaultSwitch
	body    CreateAgentRequest
	oldID   string
	project *store.Project
}

// newEnvGatherRecreate creates the provisioning agent (202, both quota
// reservations and one delegation edge held). The store wrapper counts the
// row removals once armed and returns finalizeErr from every
// FinalizeAgentDeletion when set.
func newEnvGatherRecreate(t *testing.T, name string, finalizeErr error) *envGatherRecreate {
	t.Helper()
	disp := &envGatherDeleteDispatcher{createAgentDispatcher: &createAgentDispatcher{
		envReqs: &RemoteEnvRequirementsResponse{Needs: []string{"SOME_REQUIRED_KEY"}},
	}}
	srv, s, project := setupCreateAgentServer(t, disp)
	setAgentQuotaLimits(t, s)
	cs, fault := installRollbackStore(t, srv, s, rollbackFaults{finalizeErr: finalizeErr})

	body := CreateAgentRequest{Name: name, ProjectID: project.ID, Task: "t", GatherEnv: true}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.NotNil(t, disp.capturedAgent)
	oldID := disp.capturedAgent.ID
	assertReservationsHeldBeforeCleanup(t, observeReservations(t, s, oldID))
	require.Len(t, activeEdgesFor(t, s, oldID), 1, "precondition: the create recorded an edge")
	require.Equal(t, 1, disp.creates)
	return &envGatherRecreate{srv: srv, s: s, disp: disp, cs: cs, fault: fault, body: body, oldID: oldID, project: project}
}

// recreate arms the store wrapper and sends the second create.
func (f *envGatherRecreate) recreate(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	f.fault.Arm()
	return doRequest(t, f.srv, http.MethodPost, "/api/v1/agents", f.body)
}

// projectAgentIDs lists the project's agent IDs, soft-deleted ones included.
func (f *envGatherRecreate) projectAgentIDs(t *testing.T) []string {
	t.Helper()
	res, err := f.s.ListAgents(context.Background(), store.AgentFilter{ProjectID: f.project.ID, IncludeDeleted: true}, store.ListOptions{})
	require.NoError(t, err)
	var ids []string
	for _, a := range res.Items {
		ids = append(ids, a.ID)
	}
	return ids
}

// A delete that holds the row by the time the recreate removes it (claimed
// while the broker delete runs) keeps the row: 409 delete_in_progress with
// details.agentId, the row, its edge and its quotas left to the delete, no
// agent_hard_delete record from this path, and no new agent.
func TestHandleExistingAgent_EnvGatherRecreate_DeleteHeld(t *testing.T) {
	for _, del := range rollbackDeletes() {
		if !del.held {
			continue
		}
		t.Run(del.name, func(t *testing.T) {
			f := newEnvGatherRecreate(t, "eg-held-"+tidSlugSafe(del.name), nil)
			f.disp.onDelete = func(id string) { del.apply(t, f.s, id) }

			rec := f.recreate(t)
			require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			var resp ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, ErrCodeDeleteInProgress, resp.Error.Code)
			assert.Equal(t, deleteInProgressRefusal(f.oldID).Message, resp.Error.Message)
			assert.Equal(t, f.oldID, resp.Error.Details["agentId"])

			assertRowLeftToDelete(t, f.s, f.oldID, del, true)
			assert.Empty(t, agentAudits(t, f.s, mutationTypeAgentHardDelete, f.oldID), "this path wrote no hard-delete record")
			_, finalizeCalls, deleteCalls := f.cs.snapshot()
			assert.Equal(t, 1, finalizeCalls, "one conditional row removal")
			assert.Zero(t, deleteCalls, "no unconditional row delete")

			assert.Equal(t, 1, f.disp.creates, "no create was dispatched")
			if del.removed {
				assert.Empty(t, f.projectAgentIDs(t), "no new agent row")
			} else {
				assert.Equal(t, []string{f.oldID}, f.projectAgentIDs(t), "no new agent row")
			}
		})
	}
}

// With no delete, a failed delete, or a deleting row whose lease lapsed,
// nothing holds the row and the recreate is unchanged: the old row is
// hard-deleted (edge deactivated, one agent_hard_delete record), its quotas
// are released and a new agent is created (202).
func TestHandleExistingAgent_EnvGatherRecreate_DeleteNotHeld(t *testing.T) {
	cases := []rollbackDelete{{name: "no delete"}}
	for _, del := range rollbackDeletes() {
		if !del.held {
			cases = append(cases, del)
		}
	}
	for _, del := range cases {
		t.Run(del.name, func(t *testing.T) {
			f := newEnvGatherRecreate(t, "eg-free-"+tidSlugSafe(del.name), nil)
			if del.apply != nil {
				f.disp.onDelete = func(id string) { del.apply(t, f.s, id) }
			}

			rec := f.recreate(t)
			require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
			var resp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			require.NotNil(t, resp.Agent)
			assert.NotEqual(t, f.oldID, resp.Agent.ID)

			assert.True(t, agentGone(t, f.s, f.oldID), "the old row is removed")
			assert.Empty(t, activeEdgesFor(t, f.s, f.oldID), "no active edge outlives the removed row")
			assert.Len(t, agentAudits(t, f.s, mutationTypeAgentHardDelete, f.oldID), 1)
			assertQuotasReleased(t, f.s, f.oldID)
			_, finalizeCalls, deleteCalls := f.cs.snapshot()
			assert.Equal(t, 1, finalizeCalls, "one conditional row removal")
			assert.Zero(t, deleteCalls, "no unconditional row delete")
			assert.Equal(t, 2, f.disp.creates, "the fresh agent was created")
			assert.Equal(t, []string{resp.Agent.ID}, f.projectAgentIDs(t))
		})
	}
}

// A row removal that fails for another reason keeps today's answer
// (writeErrorFromErr), keeps the row and its quotas, and creates nothing.
func TestHandleExistingAgent_EnvGatherRecreate_RowRemoveError(t *testing.T) {
	injected := errors.New("injected row removal failure")
	f := newEnvGatherRecreate(t, "eg-remove-error", injected)

	rec := f.recreate(t)
	want := httptest.NewRecorder()
	writeErrorFromErr(want, injected, "")
	assert.Equal(t, want.Code, rec.Code, rec.Body.String())
	assert.JSONEq(t, want.Body.String(), rec.Body.String())

	row, err := f.s.GetAgent(context.Background(), f.oldID)
	require.NoError(t, err, "the row is kept")
	assert.Empty(t, row.DeletionState)
	held := observeReservations(t, f.s, f.oldID)
	assert.True(t, held.broker && held.project, "quotas are kept")
	assert.Equal(t, 1, f.disp.creates, "no create was dispatched")
}

// claimingBrokerClient runs onDelete while the broker delete runs. The
// broker call names the agent by slug, so onDelete takes no ID.
type claimingBrokerClient struct {
	*envGatherMockBrokerClient
	onDelete func()
}

func (c *claimingBrokerClient) DeleteAgent(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID string, opts DeleteAgentOptions) error {
	if c.onDelete != nil {
		c.onDelete()
	}
	return c.envGatherMockBrokerClient.DeleteAgent(ctx, brokerID, brokerEndpoint, agentSlug, projectID, opts)
}

// Through the HTTP dispatcher, a recreate that finds the row held by a
// delete mints no credential and creates no agent row for the name.
func TestHandleExistingAgent_EnvGatherRecreate_DeleteHeld_NoNewCredential(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	project := &store.Project{ID: tid("project-eg-held-cred"), Name: "eg-held-cred-project", Slug: "eg-held-cred-project"}
	require.NoError(t, st.CreateProject(ctx, project))
	broker := &store.RuntimeBroker{
		ID: tid("broker-eg-held-cred"), Name: "eg-held-cred-broker", Slug: "eg-held-cred-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	require.NoError(t, st.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, st.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: broker.ID, BrokerName: "test-broker", LocalPath: "/tmp/test-project",
	}))

	client := &claimingBrokerClient{envGatherMockBrokerClient: &envGatherMockBrokerClient{
		gatherReturnEnvReqs: &RemoteEnvRequirementsResponse{
			AgentID: "will-be-set", Required: []string{"GEMINI_API_KEY"}, Needs: []string{"GEMINI_API_KEY"},
		},
	}}
	dispatcher := NewHTTPAgentDispatcherWithClient(st, client, true, slog.Default())
	gen := &fakeMintingTokenGenerator{store: st}
	dispatcher.SetTokenGenerator(gen)
	srv.SetDispatcher(dispatcher)

	reqBody := map[string]interface{}{"name": "eg-held-cred-agent", "projectId": project.ID, "template": "claude", "gatherEnv": true}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", reqBody)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var first CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &first))
	oldID := first.Agent.ID
	require.Len(t, gen.jtis, 1)

	client.onDelete = func() { claimForTest(t, st, oldID, store.DeletionStateDeleting, time.Minute) }
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents", reqBody)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, ErrCodeDeleteInProgress, resp.Error.Code)
	assert.Equal(t, oldID, resp.Error.Details["agentId"])

	assert.Len(t, gen.jtis, 1, "no credential was minted for a new agent")
	res, err := st.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID, IncludeDeleted: true}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, res.Items, 1, "no new agent row")
	assert.Equal(t, oldID, res.Items[0].ID)
	assert.Equal(t, store.DeletionStateDeleting, res.Items[0].DeletionState, "the row is left to the delete")
}
