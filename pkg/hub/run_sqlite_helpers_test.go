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
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runLabelManager is a broker agent.Manager whose runtime entries carry
// labels, as Docker containers do. Only the methods the broker's delete
// path uses are implemented; the embedded nil Manager makes any other call
// panic, so the test notices if the delete path starts using more.
type runLabelManager struct {
	agent.Manager
	mu      sync.Mutex
	entries []api.AgentInfo
	deletes []runtime.RunRef
	stops   []runtime.RunRef
}

// e2eBrokerClient sends deletes over HTTP to the real broker. Create stands
// in for the broker's create handler, which ends in pkg/agent.Start
// labelling the new container with req.RunID: it runs the entry on the
// fake runtime with the run ID the hub sent.
type e2eBrokerClient struct {
	RuntimeBrokerClient
	mgr         *runLabelManager
	projectPath string
	cids        int
}

// runIDFixture is a store with a broker, a project and an agent row, plus a
// dispatcher over a mock broker client.
type runIDFixture struct {
	store      store.Store
	client     *mockRuntimeBrokerClient
	dispatcher *HTTPAgentDispatcher
	agent      *store.Agent
}

func newRunIDFixture(t *testing.T, name string) *runIDFixture {
	t.Helper()
	ctx := context.Background()
	s := createTestStore(t)
	if err := s.CreateProject(ctx, &store.Project{ID: tid("project-" + name), Name: name, Slug: name}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:       tid("broker-" + name),
		Name:     "broker-" + name,
		Slug:     "broker-" + name,
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}); err != nil {
		t.Fatalf("CreateRuntimeBroker: %v", err)
	}
	agent := &store.Agent{
		ID:              tid("agent-" + name),
		Name:            name,
		Slug:            name,
		ProjectID:       tid("project-" + name),
		RuntimeBrokerID: tid("broker-" + name),
		AppliedConfig:   &store.AgentAppliedConfig{HarnessConfig: "claude"},
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	client := &mockRuntimeBrokerClient{}
	return &runIDFixture{
		store:      s,
		client:     client,
		dispatcher: NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()),
		agent:      agent,
	}
}

// brokerEnvelope is a broker JSON error response, as runtimebroker's
// writeError produces.
func brokerEnvelope(t *testing.T, status int, code string, details map[string]interface{}) *brokerStatusError {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{"code": code, "message": code, "details": details},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &brokerStatusError{StatusCode: status, Body: string(body)}
}

// startAttempted is the marker a broker sets on a failure from inside
// Manager.Start.
func startAttempted(runID string) map[string]interface{} {
	return map[string]interface{}{api.BrokerErrorDetailStartAttempted: true, api.BrokerErrorDetailRunID: runID}
}

// startAttemptedAt is the marker plus the run the broker's runtime holds
// after the failure (api.BrokerErrorDetailCurrentRunID).
func startAttemptedAt(runID, current string) map[string]interface{} {
	d := startAttempted(runID)
	d[api.BrokerErrorDetailCurrentRunID] = current
	return d
}

// restartQuotaAgent is a running agent on run-x holding one broker
// reservation.
func restartQuotaAgent(t *testing.T, name string) (*Server, store.Store, *store.Agent) {
	t.Helper()
	ctx := context.Background()
	srv, s := testServer(t)
	setBrokerAgentCeiling(t, s, 5)
	agent := setupBrokerAgentInPhase(t, s, name, state.PhaseRunning)
	broker, err := s.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	if err != nil {
		t.Fatal(err)
	}
	reserveBrokerSlot(t, s, broker, agent.ID)
	if _, err := s.SetAgentRunID(ctx, agent.ID, "run-x", nil); err != nil {
		t.Fatal(err)
	}
	return srv, s, agent
}

// runSwapStopClient answers a stop with success after a newer run has been
// minted and reported running, as when the row moves on while the stop for
// the older run is in flight.
type runSwapStopClient struct {
	mockRuntimeBrokerClient
	s       store.Store
	agentID string
}

// requireIntentDeleteInProgress asserts the delete_in_progress answer of a
// refused running-intent write, in the shape every delete_in_progress
// answer has: details.agentId names the agent (round 6 n1).
func requireIntentDeleteInProgress(t *testing.T, rec *httptest.ResponseRecorder, agentID string) {
	t.Helper()
	requireDeleteInProgress(t, rec)
	var body ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, ErrCodeDeleteInProgress, body.Error.Code)
	assert.Equal(t, agentID, body.Error.Details["agentId"], "details.agentId")
}

func newSiteIntentDispatcher(s store.Store) *siteIntentDispatcher {
	return &siteIntentDispatcher{s: s, seen: map[string][]store.RunIntent{}}
}

func createSiteAgent(t *testing.T, s store.Store, project *store.Project, name string, phase state.Phase, intent store.RunIntent) *store.Agent {
	t.Helper()
	ctx := context.Background()
	agent := &store.Agent{
		ID:              tid("agent-site-" + name),
		Slug:            name,
		Name:            name,
		ProjectID:       project.ID,
		RuntimeBrokerID: project.DefaultRuntimeBrokerID,
		Phase:           string(phase),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	if intent != "" {
		_, err := s.SetRunIntent(ctx, agent.ID, intent)
		require.NoError(t, err)
	}
	return agent
}

// runIntentDispatcher counts start and stop dispatches and can fail stops.
type runIntentDispatcher struct {
	createAgentDispatcher
	starts  atomic.Int32
	stops   atomic.Int32
	stopErr error
}

func requireRunIntent(t *testing.T, s store.Store, agentID string, want store.RunIntent) *store.Agent {
	t.Helper()
	a, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	require.Equal(t, want, a.RunIntent)
	require.NotNil(t, a.RunIntentAt)
	return a
}

// recordingCommandBus records the brokers SignalBrokerCmd was called for.
type recordingCommandBus struct {
	NoopCommandBus
	mu      sync.Mutex
	signals []string
}

func (m *runLabelManager) List(_ context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []api.AgentInfo
	for _, e := range m.entries {
		match := true
		for k, v := range filter {
			if e.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			out = append(out, e)
		}
	}
	return out, nil
}

func (m *runLabelManager) DeleteTarget(_ context.Context, _ string, ref runtime.RunRef, _ bool, _ string, _ bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletes = append(m.deletes, ref)
	kept := m.entries[:0]
	for _, e := range m.entries {
		if e.ContainerID != ref.ID {
			kept = append(kept, e)
		}
	}
	m.entries = kept
	return true, nil
}

// run adds a running entry labelled with runID, as pkg/agent.Start does
// with the run ID the broker passes in StartOptions.
func (m *runLabelManager) run(name, cid, projectID, projectPath, runID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, api.AgentInfo{
		Name:        name,
		ContainerID: cid,
		ProjectID:   projectID,
		ProjectPath: projectPath,
		RunID:       runID,
		Phase:       "running",
		Labels: map[string]string{
			"scion.agent":      "true",
			"scion.name":       name,
			"scion.project_id": projectID,
			api.LabelRunID:     runID,
		},
	})
}

func (m *runLabelManager) snapshot() ([]api.AgentInfo, []runtime.RunRef) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]api.AgentInfo(nil), m.entries...), append([]runtime.RunRef(nil), m.deletes...)
}

// runLabelManager.StopTarget is the broker's stop of a resolved entry on
// the fake labelled runtime: it records the stop and marks the entry
// stopped.
func (m *runLabelManager) StopTarget(_ context.Context, ref runtime.RunRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stops = append(m.stops, ref)
	for i := range m.entries {
		if m.entries[i].ContainerID == ref.ID {
			m.entries[i].Phase = "stopped"
		}
	}
	return nil
}

func (m *runLabelManager) stopSnapshot() ([]api.AgentInfo, []runtime.RunRef) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]api.AgentInfo(nil), m.entries...), append([]runtime.RunRef(nil), m.stops...)
}

func (c *e2eBrokerClient) CreateAgent(_ context.Context, _, _ string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	c.cids++
	cid := "cid-" + string(rune('0'+c.cids))
	c.mgr.run(req.Slug, cid, req.ProjectID, c.projectPath, req.RunID)
	return &RemoteAgentResponse{Agent: &RemoteAgentInfo{
		ID: req.ID, Slug: req.Slug, Name: req.Name, ContainerID: cid, Phase: "running", RunID: req.RunID,
	}, Created: true}, nil
}

func (f *runIDFixture) storedRunID(t *testing.T) string {
	t.Helper()
	got, err := f.store.GetAgent(context.Background(), f.agent.ID)
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	return got.RunID
}

func (c *runSwapStopClient) StopAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, runID string) error {
	c.lastStopRunID = runID
	if _, err := c.s.SetAgentRunID(ctx, c.agentID, "run-new", nil); err != nil {
		return err
	}
	return c.s.UpdateAgentStatus(ctx, c.agentID, store.AgentStatusUpdate{Phase: string(state.PhaseRunning), ContainerStatus: "running"})
}

// DispatchAgentStart applies a running phase to agent, as a broker's start
// response does.
func (d *runIntentDispatcher) DispatchAgentStart(_ context.Context, agent *store.Agent, _ string, _ bool) error {
	d.starts.Add(1)
	agent.Phase = string(state.PhaseRunning)
	return nil
}

func (d *runIntentDispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error {
	d.stops.Add(1)
	return d.stopErr
}

func (b *recordingCommandBus) SignalBrokerCmd(_ context.Context, brokerID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.signals = append(b.signals, brokerID)
	return nil
}

func (b *recordingCommandBus) signaled() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.signals...)
}

// siteIntentDispatcher records, at each dispatch, the run intent the store
// holds for the agent, so a test can tell that the intent was written before
// the dispatch rather than after it. A deleted row records "".
type siteIntentDispatcher struct {
	createAgentDispatcher
	s         store.Store
	createErr error
	startErr  error

	mu   sync.Mutex
	seen map[string][]store.RunIntent
}

func (d *siteIntentDispatcher) record(op, agentID string) {
	var intent store.RunIntent
	if a, err := d.s.GetAgent(context.Background(), agentID); err == nil {
		intent = a.RunIntent
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen[op] = append(d.seen[op], intent)
}

func (d *siteIntentDispatcher) intents(op string) []store.RunIntent {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]store.RunIntent(nil), d.seen[op]...)
}

func (d *siteIntentDispatcher) DispatchAgentCreate(_ context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	d.record("create", agent.ID)
	if d.createErr != nil {
		return nil, d.createErr
	}
	agent.Phase = string(state.PhaseRunning)
	return nil, nil
}

func (d *siteIntentDispatcher) DispatchAgentCreateWithGather(ctx context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	return d.DispatchAgentCreate(ctx, agent)
}

func (d *siteIntentDispatcher) DispatchAgentProvision(_ context.Context, agent *store.Agent) error {
	d.record("provision", agent.ID)
	agent.Phase = string(state.PhaseCreated)
	return nil
}

func (d *siteIntentDispatcher) DispatchAgentStart(_ context.Context, agent *store.Agent, _ string, _ bool) error {
	d.record("start", agent.ID)
	if d.startErr != nil {
		return d.startErr
	}
	agent.Phase = string(state.PhaseRunning)
	return nil
}

func (d *siteIntentDispatcher) DispatchAgentDelete(_ context.Context, agent *store.Agent, _, _, _ bool, _ time.Time) error {
	d.record("delete", agent.ID)
	return nil
}
