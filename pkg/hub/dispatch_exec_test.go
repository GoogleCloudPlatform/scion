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
	startCalled   atomic.Int32
	stopCalled    atomic.Int32
	restartCalled atomic.Int32
	lastTask      string
}

func (d *lifecycleTestDispatcher) DispatchAgentCreate(context.Context, *store.Agent) error { return nil }
func (d *lifecycleTestDispatcher) DispatchAgentProvision(context.Context, *store.Agent) error {
	return nil
}
func (d *lifecycleTestDispatcher) DispatchAgentStart(_ context.Context, _ *store.Agent, task string) error {
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
func (d *lifecycleTestDispatcher) DispatchAgentDelete(_ context.Context, _ *store.Agent, _, _, _ bool, _ time.Time) error {
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
	return false, nil
}
func (d *lifecycleTestDispatcher) DispatchAgentCreateWithGather(context.Context, *store.Agent) (*RemoteEnvRequirementsResponse, error) {
	return nil, nil
}
func (d *lifecycleTestDispatcher) DispatchFinalizeEnv(context.Context, *store.Agent, map[string]string) error {
	return nil
}

func newLifecycleTestServer(t *testing.T) (*Server, *lifecycleTestDispatcher, store.Store) {
	t.Helper()
	client := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(client)
	disp := &lifecycleTestDispatcher{}
	srv := &Server{
		store:             cs,
		instanceID:        "hub-test-" + uuid.NewString()[:8],
		agentLifecycleLog: slog.Default(),
	}
	srv.SetDispatcher(disp)
	srv.execDispatch = srv.executeDispatch
	srv.deliverMsg = srv.deliverMessage
	return srv, disp, cs
}

// seedAgent creates a project + runtime broker + agent and returns the agent.
// The broker has no endpoint (simulates a NAT'd control-channel-only broker).
func seedAgent(t *testing.T, cs store.Store) *store.Agent {
	return seedAgentWithBrokerID(t, cs, uuid.NewString())
}

func seedAgentWithBrokerID(t *testing.T, cs store.Store, brokerID string) *store.Agent {
	t.Helper()
	ctx := context.Background()
	proj := &store.Project{
		ID:         uuid.NewString(),
		Name:       "test-proj",
		Slug:       "tp-" + uuid.NewString()[:8],
		Visibility: store.VisibilityPrivate,
		OwnerID:    uuid.NewString(),
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

func TestExecuteDispatch_Start(t *testing.T) {
	ctx := context.Background()
	srv, disp, cs := newLifecycleTestServer(t)
	agent := seedAgent(t, cs)

	args, err := MarshalDispatchArgs(&StartDispatchArgs{
		Task: "run tests",
	})
	require.NoError(t, err)

	d := store.BrokerDispatch{
		ID:       uuid.NewString(),
		BrokerID: agent.RuntimeBrokerID,
		AgentID:  agent.ID,
		Op:       "start",
		Args:     args,
	}

	result, execErr := srv.executeDispatch(ctx, d)
	require.NoError(t, execErr)
	assert.Empty(t, result)
	assert.Equal(t, int32(1), disp.startCalled.Load())
	assert.Equal(t, "run tests", disp.lastTask)
}

func TestExecuteDispatch_Stop(t *testing.T) {
	ctx := context.Background()
	srv, disp, cs := newLifecycleTestServer(t)
	agent := seedAgent(t, cs)

	d := store.BrokerDispatch{
		ID:       uuid.NewString(),
		BrokerID: agent.RuntimeBrokerID,
		AgentID:  agent.ID,
		Op:       "stop",
	}

	_, execErr := srv.executeDispatch(ctx, d)
	require.NoError(t, execErr)
	assert.Equal(t, int32(1), disp.stopCalled.Load())
}

func TestExecuteDispatch_Restart(t *testing.T) {
	ctx := context.Background()
	srv, disp, cs := newLifecycleTestServer(t)
	agent := seedAgent(t, cs)

	d := store.BrokerDispatch{
		ID:       uuid.NewString(),
		BrokerID: agent.RuntimeBrokerID,
		AgentID:  agent.ID,
		Op:       "restart",
	}

	_, execErr := srv.executeDispatch(ctx, d)
	require.NoError(t, execErr)
	assert.Equal(t, int32(1), disp.restartCalled.Load())
}

func TestExecuteDispatch_UnknownOp(t *testing.T) {
	ctx := context.Background()
	srv, _, cs := newLifecycleTestServer(t)
	agent := seedAgent(t, cs)

	d := store.BrokerDispatch{
		ID:       uuid.NewString(),
		BrokerID: agent.RuntimeBrokerID,
		AgentID:  agent.ID,
		Op:       "finalize_env",
	}

	_, err := srv.executeDispatch(ctx, d)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not yet wired")
}

func TestExecuteDispatch_MissingAgent(t *testing.T) {
	ctx := context.Background()
	srv, _, _ := newLifecycleTestServer(t)

	d := store.BrokerDispatch{
		ID:       uuid.NewString(),
		BrokerID: uuid.NewString(),
		AgentID:  uuid.NewString(),
		Op:       "start",
	}

	_, err := srv.executeDispatch(ctx, d)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "resolve agent")
}

// =========================================================================
// Deferred lifecycle integration test (originator side)
// =========================================================================

// deferredTestClient is a RuntimeBrokerClient that returns ErrLifecycleDeferred
// for Start/Stop/Restart when the broker is "remote", and succeeds for "local".
type deferredTestClient struct {
	fakeHTTPClient
	localBroker string
	startCalled atomic.Int32
}

func (c *deferredTestClient) StartAgent(_ context.Context, brokerID, _, _, _, _, _, _, _ string, _ map[string]string, _ []ResolvedSecret, _ *api.ScionConfig, _ []api.SharedDir, _ bool) (*RemoteAgentResponse, error) {
	c.startCalled.Add(1)
	if brokerID != c.localBroker {
		return nil, ErrLifecycleDeferred
	}
	return &RemoteAgentResponse{}, nil
}

func (c *deferredTestClient) StopAgent(_ context.Context, brokerID, _, _, _ string) error {
	if brokerID != c.localBroker {
		return ErrLifecycleDeferred
	}
	return nil
}

func (c *deferredTestClient) RestartAgent(_ context.Context, brokerID, _, _, _ string, _ map[string]string) error {
	if brokerID != c.localBroker {
		return ErrLifecycleDeferred
	}
	return nil
}

func TestDeferredStart_WritesIntentAndWaits(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(client)

	remoteBroker := uuid.NewString()
	fakeClient := &deferredTestClient{localBroker: "local-broker"}

	events := NewChannelEventPublisher()
	defer events.Close()

	dispatcher := NewHTTPAgentDispatcherWithClient(cs, fakeClient, false, slog.Default())
	dispatcher.SetCrossNodeDeps(events, NoopCommandBus{})

	agent := seedAgentWithBrokerID(t, cs, remoteBroker)

	// Simulate the owner publishing "running" shortly after intent is written.
	go func() {
		time.Sleep(50 * time.Millisecond)
		updatedAgent := *agent
		updatedAgent.Phase = "running"
		events.PublishAgentStatus(ctx, &updatedAgent)
	}()

	err := dispatcher.DispatchAgentStart(ctx, agent, "my-task")
	require.NoError(t, err, "deferred start should succeed when 'running' event arrives")

	// Verify a broker_dispatch row was written (intent is durable). No owner
	// claimed it in this test, so it stays pending.
	pending, err := cs.ListPendingDispatch(ctx, remoteBroker)
	require.NoError(t, err)
	assert.Len(t, pending, 1, "durable intent row should exist")
	assert.Equal(t, "start", pending[0].Op)
	assert.Equal(t, agent.ID, pending[0].AgentID)
}

func TestDeferredStart_ReturnsErrorOnErrorPhase(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(client)

	remoteBroker := uuid.NewString()
	fakeClient := &deferredTestClient{localBroker: "local-broker"}

	events := NewChannelEventPublisher()
	defer events.Close()

	dispatcher := NewHTTPAgentDispatcherWithClient(cs, fakeClient, false, slog.Default())
	dispatcher.SetCrossNodeDeps(events, NoopCommandBus{})

	agent := seedAgentWithBrokerID(t, cs, remoteBroker)

	go func() {
		time.Sleep(50 * time.Millisecond)
		updatedAgent := *agent
		updatedAgent.Phase = "error"
		updatedAgent.Message = "container crash"
		events.PublishAgentStatus(ctx, &updatedAgent)
	}()

	err := dispatcher.DispatchAgentStart(ctx, agent, "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "error phase")
}

func TestLocalStart_SkipsIntentRow(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(client)

	localBroker := uuid.NewString()
	fakeClient := &deferredTestClient{localBroker: localBroker}

	events := NewChannelEventPublisher()
	defer events.Close()

	dispatcher := NewHTTPAgentDispatcherWithClient(cs, fakeClient, false, slog.Default())
	dispatcher.SetCrossNodeDeps(events, NoopCommandBus{})

	agent := seedAgentWithBrokerID(t, cs, localBroker)

	err := dispatcher.DispatchAgentStart(ctx, agent, "local-task")
	require.NoError(t, err, "local start should succeed directly")

	// Verify no broker_dispatch row was written (local path skips intent).
	pending, err := cs.ListPendingDispatch(ctx, localBroker)
	require.NoError(t, err)
	assert.Empty(t, pending, "local path should not write intent rows")

	assert.Equal(t, int32(1), fakeClient.startCalled.Load(), "client.StartAgent called once")
}

// TestReconcileBroker_LifecycleEndToEnd verifies the full reconcile path:
// insert a start dispatch, reconcile, verify the dispatcher was called and
// the dispatch row is marked done.
func TestReconcileBroker_LifecycleEndToEnd(t *testing.T) {
	ctx := context.Background()
	srv, disp, cs := newLifecycleTestServer(t)
	agent := seedAgent(t, cs)

	args, err := MarshalDispatchArgs(&StartDispatchArgs{Task: "deploy"})
	require.NoError(t, err)

	d := &store.BrokerDispatch{
		ID:       uuid.NewString(),
		BrokerID: agent.RuntimeBrokerID,
		AgentID:  agent.ID,
		Op:       "start",
		Args:     args,
	}
	require.NoError(t, cs.InsertBrokerDispatch(ctx, d))

	srv.reconcileBroker(ctx, agent.RuntimeBrokerID)

	assert.Equal(t, int32(1), disp.startCalled.Load())
	assert.Equal(t, "deploy", disp.lastTask)

	pending, err := cs.ListPendingDispatch(ctx, agent.RuntimeBrokerID)
	require.NoError(t, err)
	assert.Empty(t, pending, "dispatch should be completed")
}
