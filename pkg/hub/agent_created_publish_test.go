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
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/managedagent"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createAgent × delete interleaving (ptone/scion#2972): a create whose
// dispatch returns after a DELETE claimed (or finished with) the row must
// not publish agent.created, on the synchronous dispatch path and on the
// asynchronous launch path (ptone/scion#2153).

// createdRecordingPublisher is deleteRecordingPublisher plus agent.created.
type createdRecordingPublisher struct {
	*deleteRecordingPublisher
}

func (p *createdRecordingPublisher) PublishAgentCreated(ctx context.Context, a *store.Agent) {
	p.mu.Lock()
	p.events = append(p.events, recordedAgentEvent{
		kind: "created", phase: a.Phase, activity: a.Activity,
		deletion: store.ComputeAgentDeletion(a, time.Now()),
	})
	p.mu.Unlock()
	p.deleteRecordingPublisher.PublishAgentCreated(ctx, a)
}

func recordCreatedEvents(t *testing.T, srv *Server) *createdRecordingPublisher {
	t.Helper()
	bus := NewChannelEventPublisher()
	t.Cleanup(bus.Close)
	pub := &createdRecordingPublisher{newDeleteRecordingPublisher(bus)}
	srv.events = pub
	return pub
}

// kinds lists the recorded event kinds in order.
func (p *createdRecordingPublisher) kinds() []string {
	var out []string
	for _, e := range p.snapshot() {
		out = append(out, e.kind)
	}
	return out
}

// createRaceDispatcher runs hook inside DispatchAgentCreateWithGather, as a
// DELETE that lands while the broker create is in flight would.
type createRaceDispatcher struct {
	engineStubDispatcher
	hook func(a *store.Agent)
}

func (d *createRaceDispatcher) DispatchAgentCreateWithGather(ctx context.Context, a *store.Agent) (*CreateDispatchResult, error) {
	if d.hook != nil {
		d.hook(a)
	}
	return d.engineStubDispatcher.DispatchAgentCreateWithGather(ctx, a)
}

// raceAsyncClient is asyncLaunchClient whose broker delete can block.
type raceAsyncClient struct {
	*asyncLaunchClient
	mu       sync.Mutex
	deleteFn func(ctx context.Context) error
}

func (c *raceAsyncClient) DeleteAgent(ctx context.Context, _, _, _, _ string, _ DeleteAgentOptions) error {
	c.mu.Lock()
	fn := c.deleteFn
	c.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(ctx)
}

func (c *raceAsyncClient) setDeleteFn(fn func(ctx context.Context) error) {
	c.mu.Lock()
	c.deleteFn = fn
	c.mu.Unlock()
}

// newRaceAsyncCreateServer is newAsyncCreateServer (flag on) with a
// raceAsyncClient, so a test can interleave a DELETE with the accepted
// launch.
func newRaceAsyncCreateServer(t *testing.T) (*Server, store.Store, *store.Project, *raceAsyncClient) {
	t.Helper()
	ctx := context.Background()
	srv, s, project := setupCreateAgentServer(t, &createAgentDispatcher{})
	broker, err := s.GetRuntimeBroker(ctx, tid("broker-create"))
	require.NoError(t, err)
	broker.Endpoint = "http://localhost:9800"
	broker.Capabilities = &store.BrokerCapabilities{AsyncLaunch: true}
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

	client := &raceAsyncClient{asyncLaunchClient: &asyncLaunchClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}}}
	d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	d.SetAsyncLaunchSettingsProvider(func() AsyncLaunchSettings {
		return AsyncLaunchSettings{Enabled: true, Timeout: 5 * time.Minute, KeepaliveSeconds: 15}
	})
	srv.SetDispatcher(d)
	return srv, s, project, client
}

// deleteMode is how far the racing DELETE gets before the create's dispatch
// returns.
type deleteMode string

const (
	// deleteClaimed: the DELETE has claimed the row and is blocked in its
	// broker dispatch; it completes after the create answers.
	deleteClaimed deleteMode = "claimed"
	// deleteDone: the DELETE has finished (row gone or soft-deleted).
	deleteDone deleteMode = "done"
)

var createDeleteRaceCases = []struct {
	name      string
	mode      deleteMode
	retention time.Duration
}{
	{"hard/claimed", deleteClaimed, 0},
	{"hard/done", deleteDone, 0},
	{"soft/claimed", deleteClaimed, time.Hour},
	{"soft/done", deleteDone, time.Hour},
}

// assertDeleteLanded checks the row is hard-deleted, or soft-deleted when
// retention is on.
func assertDeleteLanded(t *testing.T, s store.Store, agentID string, retention time.Duration) {
	t.Helper()
	if retention == 0 {
		assert.True(t, agentGone(t, s, agentID), "hard delete removes the row")
		return
	}
	got := mustGetAgent(t, s, agentID)
	assert.False(t, got.DeletedAt.IsZero(), "soft delete keeps a tombstoned row")
}

// assertNoStoppedStatus checks the delete published no status with phase
// stopped (design ptone/scion#2483 R1: claim status, then deleted).
func assertNoStoppedStatus(t *testing.T, pub *createdRecordingPublisher) {
	t.Helper()
	for _, e := range pub.snapshot() {
		assert.False(t, e.kind == "status" && e.phase == string(state.PhaseStopped),
			"no stopped status: %+v", pub.snapshot())
	}
}

// assertAsyncDeleteLanded checks the row is hard-deleted: a create whose
// launch is still in flight is an incomplete create, which the delete
// hard-deletes even with retention on (agent_delete_engine.go claim, design
// ptone/scion#2483 acceptance (bb)).
func assertAsyncDeleteLanded(t *testing.T, s store.Store, agentID string) {
	t.Helper()
	assert.True(t, agentGone(t, s, agentID), "an in-flight create is hard-deleted")
}

// raceDelete is a DELETE racing a create: start runs it from inside the
// create (a dispatcher, backend or storage hook) and returns once it has
// reached mode; finish lets a claimed DELETE complete and checks it did.
type raceDelete struct {
	t       *testing.T
	srv     *Server
	disp    *engineStubDispatcher
	mode    deleteMode
	release chan struct{}
	ch      <-chan deleteResult
	agentID string
}

func (d *raceDelete) start(agentID string) {
	t := d.t
	d.agentID = agentID
	switch d.mode {
	case deleteClaimed:
		entered := make(chan struct{})
		d.disp.setFn(blockingDelete(entered, d.release, nil))
		d.ch = deleteAsync(t, d.srv, "/api/v1/agents/"+agentID, nil)
		waitClosed(t, entered, 5*time.Second, "broker delete dispatch")
		assert.Equal(t, store.DeletionStateDeleting, mustGetAgent(t, d.srv.store, agentID).DeletionState)
	case deleteDone:
		rec := doRequest(t, d.srv, http.MethodDelete, "/api/v1/agents/"+agentID, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	}
}

func (d *raceDelete) finish() {
	if d.mode != deleteClaimed {
		return
	}
	close(d.release)
	r := waitDelete(d.t, d.ch, 10*time.Second)
	require.Equal(d.t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
}

// hookStorage is mockStorage whose first GenerateSignedURL runs hook: the
// workspace-bootstrap create signs its upload URLs after the row is written
// and before it publishes created.
type hookStorage struct {
	*mockStorage
	once sync.Once
	hook func()
}

func (h *hookStorage) GenerateSignedURL(ctx context.Context, objectPath string, opts storage.SignedURLOptions) (*storage.SignedURL, error) {
	h.once.Do(h.hook)
	return h.mockStorage.GenerateSignedURL(ctx, objectPath, opts)
}

// raceManagedBackend is a managed-agent backend whose CreateInteraction runs
// hook (with the create's row ID) and then succeeds.
type raceManagedBackend struct {
	failingManagedAgentBackend
	s         store.Store
	projectID string
	hook      func(agentID string)
}

func (b *raceManagedBackend) CreateInteraction(context.Context, managedagent.InteractionRequest) (*managedagent.InteractionHandle, error) {
	// The backend is not handed the agent; the row is already written.
	result, err := b.s.ListAgents(context.Background(), store.AgentFilter{ProjectID: b.projectID}, store.ListOptions{})
	if err == nil && len(result.Items) == 1 {
		b.hook(result.Items[0].ID)
	}
	return &managedagent.InteractionHandle{InteractionID: "interaction-1"}, nil
}

// createPublishSite is one createAgent created-publish site, driven so the
// racing DELETE lands between the row write and the publish.
type createPublishSite struct {
	name string
	// arm wires hook into the create path and returns the create request.
	arm func(t *testing.T, srv *Server, s store.Store, project *store.Project, disp *createRaceDispatcher, name string, hook func(agentID string)) interface{}
}

var createPublishSites = []createPublishSite{
	{
		// Final publish after a synchronous broker create.
		name: "dispatch",
		arm: func(_ *testing.T, _ *Server, _ store.Store, project *store.Project, disp *createRaceDispatcher, name string, hook func(string)) interface{} {
			disp.hook = func(a *store.Agent) { hook(a.ID) }
			return CreateAgentRequest{Name: name, ProjectID: project.ID, Task: "do it"}
		},
	},
	{
		// Broker 202 env-gather: created is published with the env
		// requirements after the broker round trip.
		name: "env-gather",
		arm: func(_ *testing.T, _ *Server, _ store.Store, project *store.Project, disp *createRaceDispatcher, name string, hook func(string)) interface{} {
			disp.envReqs = &RemoteEnvRequirementsResponse{Required: []string{"API_KEY"}, Needs: []string{"API_KEY"}}
			disp.hook = func(a *store.Agent) { hook(a.ID) }
			return CreateAgentRequest{Name: name, ProjectID: project.ID, Task: "do it", GatherEnv: true}
		},
	},
	{
		// Workspace bootstrap: created is published (provisioning, not
		// dispatched) after the upload URLs are signed.
		name: "bootstrap",
		arm: func(_ *testing.T, srv *Server, s store.Store, project *store.Project, _ *createRaceDispatcher, name string, hook func(string)) interface{} {
			srv.SetStorage(&hookStorage{mockStorage: newMockStorage("test-bucket"), hook: func() {
				result, err := s.ListAgents(context.Background(), store.AgentFilter{ProjectID: project.ID}, store.ListOptions{})
				if err == nil && len(result.Items) == 1 {
					hook(result.Items[0].ID)
				}
			}})
			return CreateAgentRequest{
				Name: name, ProjectID: project.ID, Task: "do it",
				WorkspaceFiles: []transfer.FileInfo{{Path: "main.go", Size: 100, Hash: "sha256:abc123"}},
			}
		},
	},
	{
		// Managed agent: created is published after the backend creates
		// the first interaction.
		name: "managed",
		arm: func(t *testing.T, _ *Server, s store.Store, project *store.Project, _ *createRaceDispatcher, name string, hook func(string)) interface{} {
			backend := &raceManagedBackend{s: s, projectID: project.ID, hook: hook}
			managedBackendMu.Lock()
			prev := managedBackendInst
			managedBackendInst = backend
			managedBackendMu.Unlock()
			t.Cleanup(func() {
				managedBackendMu.Lock()
				managedBackendInst = prev
				managedBackendMu.Unlock()
			})
			return CreateAgentRequest{Name: name, ProjectID: project.ID, Task: "do it", Profile: ManagedAgentsProfile}
		},
	},
}

// Sync paths: at every createAgent publish site except the async one, the
// DELETE lands between the row write and the publish. No created is
// published, exactly one deleted is, and the delete completes. Must not run
// in parallel: the managed site swaps the package-level backend.
func TestCreateAgentPublish_SyncDispatchRacesDelete(t *testing.T) {
	for si, site := range createPublishSites {
		for i, tc := range createDeleteRaceCases {
			t.Run(site.name+"/"+tc.name, func(t *testing.T) {
				disp := &createRaceDispatcher{}
				srv, s, project := setupCreateAgentServer(t, disp)
				srv.config.SoftDeleteRetention = tc.retention
				pub := recordCreatedEvents(t, srv)

				race := &raceDelete{t: t, srv: srv, disp: &disp.engineStubDispatcher, mode: tc.mode, release: make(chan struct{})}
				body := site.arm(t, srv, s, project, disp, "race-"+string(rune('a'+si))+string(rune('a'+i)), race.start)

				rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)
				require.NotEmpty(t, race.agentID, "the hook ran: %d %s", rec.Code, rec.Body.String())
				assert.Zero(t, pub.count("created"), "no created after the delete claimed: %v", pub.kinds())

				race.finish()
				assert.Zero(t, pub.count("created"), "no created at all: %v", pub.kinds())
				assert.Equal(t, 1, pub.count("deleted"), "exactly one deleted: %v", pub.kinds())
				assertNoStoppedStatus(t, pub)
				assertDeleteLanded(t, s, race.agentID, tc.retention)
			})
		}
	}
}

// asyncDeleteRaceCases are createDeleteRaceCases for the async path. With
// retention on the delete still ends hard (an in-flight create is an
// incomplete create), so those cases are named retention-on, not soft.
var asyncDeleteRaceCases = []struct {
	name      string
	mode      deleteMode
	retention time.Duration
}{
	{"hard/claimed", deleteClaimed, 0},
	{"hard/done", deleteDone, 0},
	{"retention-on/claimed", deleteClaimed, time.Hour},
	{"retention-on/done", deleteDone, time.Hour},
}

// Async launch: the broker accepts the launch, and the DELETE lands before
// createAgent publishes. No created is published; a late launch report for
// the deleted agent publishes no status either.
func TestCreateAgentPublish_AsyncLaunchRacesDelete(t *testing.T) {
	for i, tc := range asyncDeleteRaceCases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, project, client := newRaceAsyncCreateServer(t)
			srv.config.SoftDeleteRetention = tc.retention
			pub := recordCreatedEvents(t, srv)

			var sent *RemoteCreateAgentRequest
			var delCh <-chan deleteResult
			release := make(chan struct{})
			client.answer = func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
				sent = req
				require.True(t, req.AsyncLaunch, "the create is dispatched for async launch")
				switch tc.mode {
				case deleteClaimed:
					entered := make(chan struct{})
					var once sync.Once
					client.setDeleteFn(func(ctx context.Context) error {
						once.Do(func() { close(entered) })
						select {
						case <-release:
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					})
					delCh = deleteAsync(t, srv, "/api/v1/agents/"+req.ID, nil)
					waitClosed(t, entered, 5*time.Second, "broker delete dispatch")
					assert.Equal(t, store.DeletionStateDeleting, mustGetAgent(t, s, req.ID).DeletionState)
				case deleteDone:
					rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+req.ID, nil)
					require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
				}
				return acceptedAnswer(req, req.LaunchID), nil, nil
			}

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]interface{}{
				"name": "race-async-" + string(rune('a'+i)), "projectId": project.ID, "task": "do it", "acceptAsyncLaunch": true,
			})
			require.NotNil(t, sent, "dispatch ran: %s", rec.Body.String())
			assert.Zero(t, pub.count("created"), "no created after the delete claimed: %v", pub.kinds())

			if tc.mode == deleteClaimed {
				close(release)
				r := waitDelete(t, delCh, 10*time.Second)
				require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
			}
			assert.Zero(t, pub.count("created"), "no created at all: %v", pub.kinds())
			require.Equal(t, 1, pub.count("deleted"), "exactly one deleted: %v", pub.kinds())
			assertNoStoppedStatus(t, pub)
			assertAsyncDeleteLanded(t, s, sent.ID)

			// A launch report arriving after the delete is refused and
			// publishes nothing.
			before := len(pub.snapshot())
			lr := postLaunchReport(t, srv, tid("broker-create"), sent.ID, tid("broker-create"), AgentLaunchReport{
				LaunchID: sent.LaunchID, InstanceID: "broker-instance-1", Seq: 1, State: store.LaunchReportStateSucceeded,
				Phase: string(state.PhaseRunning),
			})
			// Every async case ends hard-deleted (see assertAsyncDeleteLanded),
			// so the launch is unknown.
			require.Equal(t, http.StatusNotFound, lr.Code, lr.Body.String())
			var lrBody agentLaunchReportErrorResponse
			require.NoError(t, json.Unmarshal(lr.Body.Bytes(), &lrBody))
			assert.Equal(t, store.LaunchReportCodeUnknownLaunch, lrBody.Code)
			assert.Equal(t, before, len(pub.snapshot()), "late launch report publishes nothing: %v", pub.kinds())
		})
	}
}

// Control: a create with no racing delete publishes exactly one created, on
// both paths.
func TestCreateAgentPublish_NoDeletePublishesOnce(t *testing.T) {
	t.Run("sync", func(t *testing.T) {
		srv, _, project := setupCreateAgentServer(t, &createRaceDispatcher{})
		pub := recordCreatedEvents(t, srv)
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name: "ctl-sync", ProjectID: project.ID, Task: "do it",
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		assert.Equal(t, []string{"created"}, pub.kinds())
	})
	t.Run("async", func(t *testing.T) {
		srv, _, project, _ := newRaceAsyncCreateServer(t)
		pub := recordCreatedEvents(t, srv)
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]interface{}{
			"name": "ctl-async", "projectId": project.ID, "task": "do it", "acceptAsyncLaunch": true,
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.NotNil(t, resp.Agent.Launch, "the launch was accepted asynchronously")
		evs := pub.snapshot()
		require.Len(t, evs, 1, "%v", pub.kinds())
		assert.Equal(t, "created", evs[0].kind)
		assert.Equal(t, string(state.PhaseProvisioning), evs[0].phase)
	})
}

// The helper's predicate, per deletion state: only a gone, soft-deleted or
// claimed row suppresses the publish; a failed or lapsed delete does not.
func TestPublishAgentCreatedIfLive_DeletionStates(t *testing.T) {
	cases := []struct {
		name    string
		seed    *deleteSeed
		soft    bool
		gone    bool
		publish bool
	}{
		{name: "no marker", publish: true},
		{name: "failed", seed: &deleteSeed{state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeRuntimeError}, publish: true},
		{name: "deleting lease expired", seed: &deleteSeed{state: store.DeletionStateDeleting, leaseIn: -time.Minute}, publish: true},
		{name: "deleting live", seed: &deleteSeed{state: store.DeletionStateDeleting, leaseIn: time.Minute}},
		{name: "finalizing live", seed: &deleteSeed{state: store.DeletionStateFinalizing, leaseIn: time.Minute}},
		{name: "finalizing lease expired", seed: &deleteSeed{state: store.DeletionStateFinalizing, leaseIn: -time.Minute}},
		{name: "soft-deleted", soft: true},
		{name: "gone", gone: true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			pub := recordCreatedEvents(t, srv)
			ctx := context.Background()
			agent := setupBrokerAgentInPhase(t, s, "cpl-"+string(rune('a'+i)), state.PhaseProvisioning)
			if tc.seed != nil {
				seedAgentDeletion(t, s, agent.ID, *tc.seed)
			}
			if tc.soft {
				row := mustGetAgent(t, s, agent.ID)
				row.DeletedAt = time.Now()
				require.NoError(t, s.UpdateAgent(ctx, row))
				require.False(t, mustGetAgent(t, s, agent.ID).DeletedAt.IsZero())
			}
			if tc.gone {
				require.NoError(t, s.DeleteAgent(ctx, agent.ID))
			}

			srv.publishAgentCreatedIfLive(ctx, agent)
			if tc.publish {
				assert.Equal(t, []string{"created"}, pub.kinds())
			} else {
				assert.Empty(t, pub.kinds())
			}
		})
	}
}
