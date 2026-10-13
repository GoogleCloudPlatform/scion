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
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lifecycleTestDispatcher captures which lifecycle op was called and with
// what args, so we can verify executeDispatch routes correctly.
type lifecycleTestDispatcher struct {
	startCalled       atomic.Int32
	stopCalled        atomic.Int32
	restartCalled     atomic.Int32
	deleteCalled      atomic.Int32
	checkPromptCalled atomic.Int32
	finalizeEnvCalled atomic.Int32
	createCalled      atomic.Int32
	lastTask          string
	checkPromptResult bool
	lastDeleteFiles   bool
	lastFinalizeEnv   map[string]string
}

func seedAgentWithBrokerID(t *testing.T, cs store.Store, brokerID string) *store.Agent {
	t.Helper()
	ctx := context.Background()
	proj := &store.Project{
		ID:      uuid.NewString(),
		Name:    "test-proj",
		Slug:    "tp-" + uuid.NewString()[:8],
		OwnerID: uuid.NewString(),
	}
	require.NoError(t, cs.CreateProject(ctx, proj))
	broker := &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "test-broker",
		Slug:   "tb-" + uuid.NewString()[:8],
		Status: "online",
	}
	require.NoError(t, cs.CreateRuntimeBroker(ctx, broker))
	agent := &store.Agent{
		ID:              uuid.NewString(),
		Name:            "test-agent",
		Slug:            "ta-" + uuid.NewString()[:8],
		ProjectID:       proj.ID,
		RuntimeBrokerID: brokerID,
	}
	require.NoError(t, cs.CreateAgent(ctx, agent))
	return agent
}

// deferredTestClient is a RuntimeBrokerClient that returns ErrLifecycleDeferred
// for Start/Stop/Restart when the broker is "remote", and succeeds for "local".
type deferredTestClient struct {
	fakeHTTPClient
	localBroker string
	startCalled atomic.Int32
}

// deferredDataOpTestClient returns ErrLifecycleDeferred for data ops when the
// broker is not "local", simulating a cross-node dispatch.
type deferredDataOpTestClient struct {
	fakeHTTPClient
	localBroker string
}

// ownerErrDispatcher is the executing node's dispatcher. Each op returns its
// configured error, as the local broker client would on an HTTP error answer.
type ownerErrDispatcher struct {
	lifecycleTestDispatcher
	err         error
	beforeStart func()
	// createResult, when set, is what create returns (with a nil error).
	createResult *CreateDispatchResult
	// finalizeErr, when set, is what finalize_env returns instead of err.
	finalizeErr error
}

// ownerSignalBus delivers the requesting node's signal to the owner node,
// which drains the broker's dispatch rows with its real reconcileBroker.
type ownerSignalBus struct {
	NoopCommandBus
	owner *Server
}

// newCrossNodeFixture builds the fixture. With signalOwner false the
// requester's signal goes nowhere and the test plays the owner itself.
func newCrossNodeFixture(t *testing.T, ownerErr error, signalOwner bool) *crossNodeFixture {
	t.Helper()
	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)

	disp := &ownerErrDispatcher{err: ownerErr}
	owner := &Server{
		store:             cs,
		instanceID:        "hub-owner-" + uuid.NewString()[:8],
		agentLifecycleLog: slog.Default(),
		events:            events,
	}
	owner.SetDispatcher(disp)
	owner.execDispatch = owner.executeDispatch
	owner.deliverMsg = owner.deliverMessage

	requester := NewHTTPAgentDispatcherWithClient(cs, &deferredTestClient{localBroker: "local-broker"}, false, slog.Default())
	var bus CommandBus = NoopCommandBus{}
	if signalOwner {
		bus = ownerSignalBus{owner: owner}
	}
	requester.SetCrossNodeDeps(events, bus)

	agent := seedAgentWithBrokerID(t, cs, uuid.NewString())
	return &crossNodeFixture{store: cs, events: events, owner: owner, ownerDisp: disp, requester: requester, agent: agent}
}

// crossNodeCtx bounds a cross-node call. Without the result envelope the
// requester waits for a status event that never comes, so a call that ends
// well inside this bound proves the row's failure ended the wait.
func crossNodeCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func (d *lifecycleTestDispatcher) DispatchAgentCreate(context.Context, *store.Agent) (*CreateDispatchResult, error) {
	return nil, nil
}
func (d *lifecycleTestDispatcher) DispatchAgentProvision(context.Context, *store.Agent) error {
	return nil
}

func (d *lifecycleTestDispatcher) DispatchAgentReprovision(context.Context, *store.Agent) error {
	return nil
}
func (d *lifecycleTestDispatcher) DispatchAgentStart(_ context.Context, _ *store.Agent, task string, _ bool) error {
	d.startCalled.Add(1)
	d.lastTask = task
	return nil
}
func (d *lifecycleTestDispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error {
	d.stopCalled.Add(1)
	return nil
}
func (d *lifecycleTestDispatcher) DispatchAgentRestart(_ context.Context, _ *store.Agent) error {
	d.restartCalled.Add(1)
	return nil
}
func (d *lifecycleTestDispatcher) DispatchAgentResetAuth(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *lifecycleTestDispatcher) DispatchAgentDelete(_ context.Context, _ *store.Agent, deleteFiles, _, _ bool, _ time.Time) error {
	d.deleteCalled.Add(1)
	d.lastDeleteFiles = deleteFiles
	return nil
}
func (d *lifecycleTestDispatcher) DispatchAgentMessage(_ context.Context, _ *store.Agent, _ string, _ bool, _ *messages.StructuredMessage) error {
	return nil
}
func (d *lifecycleTestDispatcher) DispatchAgentLogs(context.Context, *store.Agent, int) (string, error) {
	return "", nil
}
func (d *lifecycleTestDispatcher) DispatchAgentExec(context.Context, *store.Agent, []string, int) (string, int, error) {
	return "", 0, nil
}
func (d *lifecycleTestDispatcher) DispatchCheckAgentPrompt(context.Context, *store.Agent) (bool, error) {
	d.checkPromptCalled.Add(1)
	return d.checkPromptResult, nil
}
func (d *lifecycleTestDispatcher) DispatchAgentCreateWithGather(context.Context, *store.Agent) (*CreateDispatchResult, error) {
	d.createCalled.Add(1)
	return nil, nil
}
func (d *lifecycleTestDispatcher) DispatchFinalizeEnv(_ context.Context, _ *store.Agent, env map[string]string) (*CreateDispatchResult, error) {
	d.finalizeEnvCalled.Add(1)
	d.lastFinalizeEnv = env
	return nil, nil
}

func (c *deferredTestClient) StartAgent(_ context.Context, brokerID, _, _, _, _, _, _, _, _, _ string, _ map[string]string, _ []ResolvedSecret, _ *api.ScionConfig, _ []api.SharedDir, _, _ bool, _ StartExtras) (*RemoteAgentResponse, error) {
	c.startCalled.Add(1)
	if brokerID != c.localBroker {
		return nil, ErrLifecycleDeferred
	}
	return &RemoteAgentResponse{}, nil
}

func (c *deferredTestClient) StopAgent(_ context.Context, brokerID, _, _, _, _ string) error {
	if brokerID != c.localBroker {
		return ErrLifecycleDeferred
	}
	return nil
}

func (c *deferredTestClient) RestartAgent(_ context.Context, brokerID, _, _, _ string, _ map[string]string, _ StartExtras) (*RemoteAgentResponse, error) {
	if brokerID != c.localBroker {
		return nil, ErrLifecycleDeferred
	}
	return nil, nil
}

func (c *deferredDataOpTestClient) DeleteAgent(_ context.Context, brokerID, _, _, _ string, _ DeleteAgentOptions) error {
	if brokerID != c.localBroker {
		return ErrLifecycleDeferred
	}
	return nil
}

func (c *deferredDataOpTestClient) CheckAgentPrompt(_ context.Context, brokerID, _, _, _ string) (bool, error) {
	if brokerID != c.localBroker {
		return false, ErrLifecycleDeferred
	}
	return false, nil
}

func (c *deferredDataOpTestClient) CreateAgentWithGather(_ context.Context, brokerID, _ string, _ *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	if brokerID != c.localBroker {
		return nil, nil, ErrLifecycleDeferred
	}
	return nil, nil, nil
}

func (d *ownerErrDispatcher) DispatchAgentStart(ctx context.Context, a *store.Agent, task string, resume bool) error {
	if d.beforeStart != nil {
		d.beforeStart()
	}
	_ = d.lifecycleTestDispatcher.DispatchAgentStart(ctx, a, task, resume)
	return d.err
}
func (d *ownerErrDispatcher) DispatchAgentStop(ctx context.Context, a *store.Agent) error {
	_ = d.lifecycleTestDispatcher.DispatchAgentStop(ctx, a)
	return d.err
}
func (d *ownerErrDispatcher) DispatchAgentRestart(ctx context.Context, a *store.Agent) error {
	_ = d.lifecycleTestDispatcher.DispatchAgentRestart(ctx, a)
	return d.err
}
func (d *ownerErrDispatcher) DispatchCheckAgentPrompt(ctx context.Context, a *store.Agent) (bool, error) {
	_, _ = d.lifecycleTestDispatcher.DispatchCheckAgentPrompt(ctx, a)
	return false, d.err
}
func (d *ownerErrDispatcher) DispatchAgentCreateWithGather(ctx context.Context, a *store.Agent) (*CreateDispatchResult, error) {
	_, _ = d.lifecycleTestDispatcher.DispatchAgentCreateWithGather(ctx, a)
	if d.createResult != nil {
		return d.createResult, nil
	}
	return nil, d.err
}
func (d *ownerErrDispatcher) DispatchFinalizeEnv(ctx context.Context, a *store.Agent, env map[string]string) (*CreateDispatchResult, error) {
	_, _ = d.lifecycleTestDispatcher.DispatchFinalizeEnv(ctx, a, env)
	if d.finalizeErr != nil {
		return nil, d.finalizeErr
	}
	return nil, d.err
}

func (b ownerSignalBus) SignalBrokerCmd(_ context.Context, brokerID string) error {
	go b.owner.reconcileBroker(context.Background(), brokerID)
	return nil
}

// crossNodeFixture is two hub nodes over one store and one event bus: the
// requester's broker client always defers, and the owner executes the
// dispatch rows.
type crossNodeFixture struct {
	store     store.Store
	events    *ChannelEventPublisher
	owner     *Server
	ownerDisp *ownerErrDispatcher
	requester *HTTPAgentDispatcher
	agent     *store.Agent
}

// claimPending waits for the agent's single pending dispatch row and claims
// it as an owner node would. It runs on a helper goroutine, so it reports
// failures with assert and returns ok=false instead of stopping the test.
func (f *crossNodeFixture) claimPending(t *testing.T) (store.BrokerDispatch, bool) {
	t.Helper()
	var row store.BrokerDispatch
	ok := assert.Eventually(t, func() bool {
		pending, err := f.store.ListPendingDispatch(context.Background(), f.agent.RuntimeBrokerID)
		if err != nil || len(pending) == 0 {
			return false
		}
		row = pending[0]
		return true
	}, 5*time.Second, 5*time.Millisecond, "no dispatch row was written")
	if !ok {
		return row, false
	}
	claimed, err := f.store.ClaimBrokerDispatch(context.Background(), row.ID, "test-owner")
	return row, assert.NoError(t, err) && assert.True(t, claimed)
}

// failRow fails a claimed row with execErr exactly as reconcileBroker does.
// Like claimPending it runs on a helper goroutine and reports with assert.
func (f *crossNodeFixture) failRow(t *testing.T, id string, execErr error) bool {
	t.Helper()
	return assert.NoError(t, f.store.FailBrokerDispatch(context.Background(), id, execErr.Error(), dispatchFailureResult(execErr)))
}
