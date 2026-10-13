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
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rawNFSHealthCheck is an nfs_mounts value in the form /healthz shows it:
// share ID, NFS server and export, mount path and mount command output.
const rawNFSHealthCheck = "unhealthy: ws1: mount failed: mount 10.0.0.2:/export on /mnt/nfs/ws1 failed: exit status 32 (output: mount.nfs: access denied by server)"

// reconcileFixture is a hub with one online broker, one project and helpers
// to drive heartbeats that carry an inventory.
type reconcileFixture struct {
	t         *testing.T
	srv       *Server
	s         store.Store
	brokerID  string
	projectID string
}

func newReconcileFixture(t *testing.T) *reconcileFixture {
	t.Helper()
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:      tid("rc-broker"),
		Name:    "RC Broker",
		Slug:    "rc-broker",
		Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.UpdateRuntimeBrokerHeartbeat(ctx, broker.ID, store.BrokerStatusOnline))

	project := &store.Project{
		ID:      tid("rc-project"),
		Slug:    "rc-project",
		Name:    "RC Project",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     broker.Status,
	}))

	return &reconcileFixture{t: t, srv: srv, s: s, brokerID: broker.ID, projectID: project.ID}
}

// completeInventory reports the given targets (default: "docker") as
// completely listed.
func completeInventory(targets ...string) *brokerInventory {
	if len(targets) == 0 {
		targets = []string{"docker"}
	}
	inv := &brokerInventory{}
	for _, id := range targets {
		inv.Targets = append(inv.Targets, brokerInventoryTarget{ID: id, Complete: true})
	}
	return inv
}

const (
	k8sTargetA = "kubernetes|context=hybval|namespace=default"
	k8sTargetB = "kubernetes|context=hybval|namespace=scion-agents"
)

// mintJoinToken sends the request 'hub brokers join-token create' sends.
func mintJoinToken(t *testing.T, srv *Server, user *store.User, name string, ttlSeconds int) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:                name,
		JoinTokenTTLSeconds: ttlSeconds,
		PreserveSettings:    true,
		Labels:              map[string]string{"scion.io/broker-role": "remote"},
	})
}

func decodeRegistration(t *testing.T, rec *httptest.ResponseRecorder) CreateBrokerRegistrationResponse {
	t.Helper()
	var resp CreateBrokerRegistrationResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	return resp
}

// setProjectAgentCeiling overrides the seeded max_agents_per_project limit
// (default 0 = unlimited, see seed.go) to a small value so tests can hit it.
func setProjectAgentCeiling(t *testing.T, s store.Store, value int64) {
	t.Helper()
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerProject)
	require.NoError(t, err, "max_agents_per_project must be seeded by New()/seedLimitDefinitions")
	def.DefaultValue = value
	_, err = s.UpdateLimitDefinition(context.Background(), def)
	require.NoError(t, err)
}

// failingStartDispatcher is a quotaLifecycleDispatcher whose start dispatch
// always fails without touching the agent, as a broker that cannot launch
// the container would.
type failingStartDispatcher struct {
	quotaLifecycleDispatcher
}

// failingStopStartDispatcher fails both legs of a restart, as a broker that
// cannot reach a still-running container would.
type failingStopStartDispatcher struct {
	failingStartDispatcher
}

func hasReservation(t *testing.T, s store.Store, limitName, resourceID string) bool {
	t.Helper()
	def, err := s.GetLimitDefinitionByName(context.Background(), limitName)
	require.NoError(t, err)
	held, err := s.HasActiveReservation(context.Background(), def.ID, resourceID)
	require.NoError(t, err)
	return held
}

// brokerReservationIDs returns the IDs of broker's active
// max_agents_per_broker reservations.
func brokerReservationIDs(t *testing.T, s store.Store, brokerID string) []string {
	t.Helper()
	ctx := context.Background()
	def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	rows, err := s.ListActiveReservations(ctx, def.ID, store.QuotaScopeBroker, brokerID)
	require.NoError(t, err)
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

// quotaLifecycleDispatcher extends createAgentDispatcher with atomic
// start/stop call counters and phase mutation that mimics a real broker's
// start/stop acknowledgment (handleAgentLifecycle reuses the broker-reported
// phase off the agent pointer it passed in). Used to test the
// max_agents_per_broker reservation lifecycle across stop/start/resume/crash
// (ptone/scion#1963).
type quotaLifecycleDispatcher struct {
	createAgentDispatcher
	startCount atomic.Int32
	stopCount  atomic.Int32
}

// brokerReservationCount returns the number of active (non-released)
// max_agents_per_broker reservations for broker.
func brokerReservationCount(t *testing.T, s store.Store, brokerID string) int64 {
	t.Helper()
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	n, err := s.CountActiveReservations(context.Background(), def.ID, brokerID, store.QuotaScopeBroker, brokerID)
	require.NoError(t, err)
	return n
}

// newQuotaTestBrokerAndProject creates an online runtime broker and a project
// wired to it (project provider + DefaultRuntimeBrokerID), the same wiring
// setupCreateAgentServer uses, so both the full create-agent HTTP flow and
// directly store-created agents can share one broker.
func newQuotaTestBrokerAndProject(t *testing.T, s store.Store, suffix string) (*store.RuntimeBroker, *store.Project) {
	t.Helper()
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:     tid("broker-quota-" + suffix),
		Name:   "Quota Broker " + suffix,
		Slug:   "quota-broker-" + suffix,
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	project := &store.Project{
		ID:   tid("proj-quota-" + suffix),
		Name: "Quota Project " + suffix,
		Slug: "quota-project-" + suffix,
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     broker.Status,
	}))
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))

	return broker, project
}

// newQuotaTestAgent creates an agent directly via the store (bypassing the
// create-agent HTTP flow and its automatic reservation) in the given phase,
// assigned to broker/project.
func newQuotaTestAgent(t *testing.T, s store.Store, broker *store.RuntimeBroker, project *store.Project, name string, phase state.Phase) *store.Agent {
	t.Helper()
	agent := &store.Agent{
		ID:              tid("agent-" + name),
		Slug:            name,
		Name:            name,
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(phase),
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))
	return agent
}

// reserveBrokerSlot manually creates a max_agents_per_broker reservation for
// agentID against broker, mirroring what createAgentInProject would have done
// had the agent been created through the normal HTTP flow.
func reserveBrokerSlot(t *testing.T, s store.Store, broker *store.RuntimeBroker, agentID string) {
	t.Helper()
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	_, err = s.CreateUsageReservation(context.Background(), &store.UsageReservation{
		LimitDefinitionID: def.ID,
		SubjectID:         broker.ID,
		ScopeType:         store.QuotaScopeBroker,
		ScopeID:           broker.ID,
		ResourceID:        agentID,
		Reserved:          1,
	})
	require.NoError(t, err)
}

// reserveStaleBrokerSlot is reserveBrokerSlot but backdates the reservation's
// CreatedAt past reconcileMinReservationAge, simulating a reservation left
// over from a genuinely old dispatch (as opposed to one reconcile might
// observe mid-dispatch) so that phase-based reconcile release still applies
// to it in tests (ptone/scion#2011).
func reserveStaleBrokerSlot(t *testing.T, s store.Store, broker *store.RuntimeBroker, agentID string) {
	t.Helper()
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	_, err = s.CreateUsageReservation(context.Background(), &store.UsageReservation{
		LimitDefinitionID: def.ID,
		SubjectID:         broker.ID,
		ScopeType:         store.QuotaScopeBroker,
		ScopeID:           broker.ID,
		ResourceID:        agentID,
		Reserved:          1,
		CreatedAt:         time.Now().Add(-2 * reconcileMinReservationAge),
	})
	require.NoError(t, err)
}

// assertBrokerQuotaExceeded checks rec is the broker-cap rejection, with the
// exact code and message every path uses.
func assertBrokerQuotaExceeded(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	assert.Equal(t, ErrCodeQuotaExceeded, resp.Error.Code)
	assert.Equal(t, "quota exceeded: max_agents_per_broker", resp.Error.Message)
}

// fakeHTTPClient records calls to MessageAgent so we can verify the HTTP
// fallback path. Other methods are stubs.
type fakeHTTPClient struct {
	messageAgentCalled bool
	startAgentCalled   bool
	stopAgentCalled    bool
	deleteAgentCalled  bool
	lastStartExtras    StartExtras
	lastRestartExtras  StartExtras
}

const brokerValidationMessage = `GCP identity mode "block" is not supported on the Kubernetes runtime`

// addAgent creates an agent of the fixture's broker, last seen long ago.
func (f *reconcileFixture) addAgent(slug, phase, activity string, mutate ...func(a *store.Agent)) *store.Agent {
	f.t.Helper()
	a := &store.Agent{
		ID:              tid("rc-" + slug),
		Slug:            slug,
		Name:            slug,
		Template:        "default",
		ProjectID:       f.projectID,
		RuntimeBrokerID: f.brokerID,
		Runtime:         "docker",
		AppliedConfig:   &store.AgentAppliedConfig{RuntimeTarget: "docker"},
		Phase:           phase,
		Activity:        activity,
		LastSeen:        time.Now().Add(-time.Hour),
		Labels:          map[string]string{},
	}
	for _, m := range mutate {
		m(a)
	}
	require.NoError(f.t, f.s.CreateAgent(context.Background(), a))
	return a
}

// heartbeat sends an online heartbeat that reports the given slugs as
// running agents of the fixture's project.
func (f *reconcileFixture) heartbeat(inv *brokerInventory, slugs ...string) {
	f.t.Helper()
	agents := make([]brokerAgentHeartbeat, 0, len(slugs))
	for _, slug := range slugs {
		agents = append(agents, brokerAgentHeartbeat{Slug: slug, Phase: "running", Activity: "working", RuntimeTarget: "docker"})
	}
	f.send(brokerHeartbeatRequest{
		Status:    store.BrokerStatusOnline,
		Inventory: inv,
		Projects:  []brokerProjectHeartbeat{{ProjectID: f.projectID, Agents: agents}},
	})
}

func (f *reconcileFixture) send(hb brokerHeartbeatRequest) {
	f.t.Helper()
	rec := doRequest(f.t, f.srv, http.MethodPost, "/api/v1/runtime-brokers/"+f.brokerID+"/heartbeat", hb)
	require.Equal(f.t, http.StatusOK, rec.Code, rec.Body.String())
}

// expireClock moves an agent's first-missing time past the grace period, as
// if it had been absent from complete inventories for that long.
func (f *reconcileFixture) expireClock(agentID string) {
	f.t.Helper()
	tr := &f.srv.missingAgents
	tr.mu.Lock()
	defer tr.mu.Unlock()
	m := tr.since[f.brokerID]
	require.Contains(f.t, m, agentID, "agent has no missing clock")
	m[agentID] = time.Now().Add(-2 * f.srv.missingAgentGrace())
}

func (f *reconcileFixture) hasClock(agentID string) bool {
	tr := &f.srv.missingAgents
	tr.mu.Lock()
	defer tr.mu.Unlock()
	_, ok := tr.since[f.brokerID][agentID]
	return ok
}

func (f *reconcileFixture) get(id string) *store.Agent {
	f.t.Helper()
	a, err := f.s.GetAgent(context.Background(), id)
	require.NoError(f.t, err)
	return a
}

func (f *reconcileFixture) assertReconciled(id string) {
	f.t.Helper()
	a := f.get(id)
	assert.Equal(f.t, string(state.PhaseError), a.Phase)
	assert.Equal(f.t, string(state.ExitReasonContainerMissing), a.ExitReason)
	assert.Equal(f.t, "", a.Activity)
	assert.NotEmpty(f.t, a.Message)
}

func (f *reconcileFixture) assertUntouched(id, phase string) {
	f.t.Helper()
	a := f.get(id)
	assert.Equal(f.t, phase, a.Phase)
	assert.Empty(f.t, a.ExitReason)
}

// snapshotAgents returns the current rows of the given agents.
func (f *reconcileFixture) snapshotAgents(agents ...*store.Agent) map[string]*store.Agent {
	f.t.Helper()
	out := make(map[string]*store.Agent, len(agents))
	for _, a := range agents {
		out[a.ID] = f.get(a.ID)
	}
	return out
}

// assertAgentsUnchanged checks that the heartbeat-driven fields of each
// agent still match its snapshot.
func (f *reconcileFixture) assertAgentsUnchanged(before map[string]*store.Agent) {
	f.t.Helper()
	for id, prev := range before {
		got := f.get(id)
		assert.Equal(f.t, prev.Phase, got.Phase, "phase of %s", prev.Slug)
		assert.Equal(f.t, prev.Activity, got.Activity, "activity of %s", prev.Slug)
		assert.Equal(f.t, prev.ExitReason, got.ExitReason, "exit reason of %s", prev.Slug)
		assert.Equal(f.t, prev.ContainerStatus, got.ContainerStatus, "container status of %s", prev.Slug)
		assert.True(f.t, prev.LastSeen.Equal(got.LastSeen), "last seen of %s", prev.Slug)
		assert.Equal(f.t, agentRuntimeTarget(prev), agentRuntimeTarget(got), "runtime target of %s", prev.Slug)
	}
}

func (d *failingStartDispatcher) DispatchAgentStart(_ context.Context, _ *store.Agent, _ string, _ bool) error {
	d.startCount.Add(1)
	return errors.New("simulated broker start failure")
}

func (d *failingStopStartDispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error {
	return errors.New("simulated broker stop failure")
}

func (d *quotaLifecycleDispatcher) DispatchAgentStart(_ context.Context, agent *store.Agent, _ string, _ bool) error {
	d.startCount.Add(1)
	agent.Phase = string(state.PhaseRunning)
	agent.ContainerStatus = "running"
	return nil
}

func (d *quotaLifecycleDispatcher) DispatchAgentStop(_ context.Context, agent *store.Agent) error {
	d.stopCount.Add(1)
	agent.Phase = string(state.PhaseStopped)
	agent.ContainerStatus = "stopped"
	return nil
}

func (f *fakeHTTPClient) MessageAgent(context.Context, string, string, string, string, string, bool, *messages.StructuredMessage) error {
	f.messageAgentCalled = true
	return nil
}

// Stub implementations for the RuntimeBrokerClient interface — only MessageAgent matters.
func (f *fakeHTTPClient) CreateAgent(context.Context, string, string, *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	return nil, nil
}
func (f *fakeHTTPClient) StartAgent(_ context.Context, _, _, _, _, _, _, _, _, _, _ string, _ map[string]string, _ []ResolvedSecret, _ *api.ScionConfig, _ []api.SharedDir, _, _ bool, extras StartExtras) (*RemoteAgentResponse, error) {
	f.startAgentCalled = true
	f.lastStartExtras = extras
	return nil, nil
}
func (f *fakeHTTPClient) StopAgent(context.Context, string, string, string, string, string) error {
	f.stopAgentCalled = true
	return nil
}
func (f *fakeHTTPClient) RestartAgent(_ context.Context, _, _, _, _ string, _ map[string]string, extras StartExtras) (*RemoteAgentResponse, error) {
	f.lastRestartExtras = extras
	return nil, nil
}
func (f *fakeHTTPClient) ResetAuthAgent(context.Context, string, string, string, string, string, string) error {
	return nil
}
func (f *fakeHTTPClient) DeleteAgent(context.Context, string, string, string, string, DeleteAgentOptions) error {
	f.deleteAgentCalled = true
	return nil
}
func (f *fakeHTTPClient) CheckAgentPrompt(context.Context, string, string, string, string) (bool, error) {
	return false, nil
}
func (f *fakeHTTPClient) CreateAgentWithGather(context.Context, string, string, *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	return nil, nil, nil
}
func (f *fakeHTTPClient) GetAgentLogs(context.Context, string, string, string, string, int) (string, error) {
	return "", nil
}
func (f *fakeHTTPClient) ExecAgent(context.Context, string, string, string, string, []string, int) (string, int, error) {
	return "", 0, nil
}
func (f *fakeHTTPClient) CleanupProject(context.Context, string, string, string, string) error {
	return nil
}
