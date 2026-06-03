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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newReconcileServer builds a minimal Server wired only with what reconcileBroker
// needs, plus overridable executor seams.
func newReconcileServer(st store.Store, exec func(context.Context, store.BrokerDispatch) (string, error), deliver func(context.Context, *store.Message) error) *Server {
	return &Server{
		store:             st,
		instanceID:        "hub-" + uuid.NewString()[:8],
		agentLifecycleLog: slog.Default(),
		execDispatch:      exec,
		deliverMsg:        deliver,
	}
}

func TestReconcileBroker_DrainsDispatchOnce(t *testing.T) {
	ctx := context.Background()
	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	var execN int32
	s := newReconcileServer(cs,
		func(context.Context, store.BrokerDispatch) (string, error) { atomic.AddInt32(&execN, 1); return `{"ok":true}`, nil },
		func(context.Context, *store.Message) error { return nil })

	broker := uuid.NewString()
	d := &store.BrokerDispatch{ID: uuid.NewString(), BrokerID: broker, Op: "start"}
	require.NoError(t, cs.InsertBrokerDispatch(ctx, d))

	s.reconcileBroker(ctx, broker)

	assert.Equal(t, int32(1), atomic.LoadInt32(&execN), "executor runs once")
	pending, err := cs.ListPendingDispatch(ctx, broker)
	require.NoError(t, err)
	assert.Empty(t, pending, "drained dispatch is no longer pending")
}

func TestReconcileBroker_ConcurrentDrainsExecuteOnce(t *testing.T) {
	ctx := context.Background()
	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	var execN int32
	s := newReconcileServer(cs,
		func(context.Context, store.BrokerDispatch) (string, error) { atomic.AddInt32(&execN, 1); return "", nil },
		func(context.Context, *store.Message) error { return nil })

	broker := uuid.NewString()
	require.NoError(t, cs.InsertBrokerDispatch(ctx, &store.BrokerDispatch{ID: uuid.NewString(), BrokerID: broker, Op: "start"}))

	const racers = 6
	var wg sync.WaitGroup
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() { defer wg.Done(); s.reconcileBroker(ctx, broker) }()
	}
	wg.Wait()

	assert.Equal(t, int32(1), atomic.LoadInt32(&execN), "concurrent drains execute the intent exactly once")
}

func TestReconcileBroker_FailedOpMarkedFailed(t *testing.T) {
	ctx := context.Background()
	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	s := newReconcileServer(cs,
		func(context.Context, store.BrokerDispatch) (string, error) { return "", assertErr{} },
		func(context.Context, *store.Message) error { return nil })

	broker := uuid.NewString()
	d := &store.BrokerDispatch{ID: uuid.NewString(), BrokerID: broker, Op: "start"}
	require.NoError(t, cs.InsertBrokerDispatch(ctx, d))

	s.reconcileBroker(ctx, broker)

	pending, err := cs.ListPendingDispatch(ctx, broker)
	require.NoError(t, err)
	assert.Empty(t, pending, "a failed op leaves no pending row (it is marked failed, not retried in-loop)")
}

func TestReconcileBroker_DrainsPendingMessageOnce(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(client)
	var deliverN int32
	s := newReconcileServer(cs,
		func(context.Context, store.BrokerDispatch) (string, error) { return "", nil },
		func(context.Context, *store.Message) error { atomic.AddInt32(&deliverN, 1); return nil })

	broker := uuid.NewString()
	proj := &store.Project{ID: uuid.NewString(), Name: "p", Slug: "p-" + uuid.NewString()[:8], Visibility: store.VisibilityPrivate, OwnerID: uuid.NewString()}
	require.NoError(t, cs.CreateProject(ctx, proj))
	agent, err := client.Agent.Create().
		SetSlug("a-" + uuid.NewString()[:8]).SetName("a").
		SetProjectID(uuid.MustParse(proj.ID)).SetRuntimeBrokerID(broker).
		Save(ctx)
	require.NoError(t, err)

	msg := &store.Message{ID: uuid.NewString(), ProjectID: proj.ID, Sender: "user:x", Recipient: "agent:a", Msg: "hi", AgentID: agent.ID.String()}
	require.NoError(t, cs.CreateMessage(ctx, msg))

	s.reconcileBroker(ctx, broker)

	assert.Equal(t, int32(1), atomic.LoadInt32(&deliverN), "pending message delivered once")
	got, err := cs.GetMessage(ctx, msg.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDispatched, got.DispatchState)
}

// TestDeliverMessage_TunnelsViaDispatcher verifies that deliverMessage resolves
// the agent from the store and dispatches via the local AgentDispatcher.
func TestDeliverMessage_TunnelsViaDispatcher(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(client)

	proj := &store.Project{ID: uuid.NewString(), Name: "p", Slug: "p-" + uuid.NewString()[:8], Visibility: store.VisibilityPrivate, OwnerID: uuid.NewString()}
	require.NoError(t, cs.CreateProject(ctx, proj))

	brokerID := uuid.NewString()
	agent, err := client.Agent.Create().
		SetSlug("a-" + uuid.NewString()[:8]).SetName("deliver-test").
		SetProjectID(uuid.MustParse(proj.ID)).SetRuntimeBrokerID(brokerID).
		Save(ctx)
	require.NoError(t, err)

	var dispatched atomic.Int32
	var lastMsg string
	fakeDispatcher := &reconcileTestDispatcher{
		onMessage: func(a *store.Agent, msg string) error {
			dispatched.Add(1)
			lastMsg = msg
			return nil
		},
	}

	srv := &Server{
		store:             cs,
		instanceID:        "hub-test",
		agentLifecycleLog: slog.Default(),
	}
	srv.SetDispatcher(fakeDispatcher)
	srv.deliverMsg = srv.deliverMessage

	m := &store.Message{
		ID:      uuid.NewString(),
		AgentID: agent.ID.String(),
		Msg:     "hello from reconcile",
		Urgent:  true,
	}

	err = srv.deliverMsg(ctx, m)
	require.NoError(t, err)
	assert.Equal(t, int32(1), dispatched.Load(), "message dispatched once")
	assert.Equal(t, "hello from reconcile", lastMsg)
}

// TestDeliverMessage_MissingAgent returns an error when the agent doesn't exist.
func TestDeliverMessage_MissingAgent(t *testing.T) {
	ctx := context.Background()
	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	srv := &Server{
		store:             cs,
		instanceID:        "hub-test",
		agentLifecycleLog: slog.Default(),
	}
	srv.deliverMsg = srv.deliverMessage

	m := &store.Message{ID: uuid.NewString(), AgentID: uuid.NewString(), Msg: "test"}
	err := srv.deliverMsg(ctx, m)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "resolve agent")
}

// reconcileTestDispatcher is a minimal AgentDispatcher for deliverMessage tests.
type reconcileTestDispatcher struct {
	onMessage func(agent *store.Agent, msg string) error
}

func (d *reconcileTestDispatcher) DispatchAgentCreate(context.Context, *store.Agent) error { return nil }
func (d *reconcileTestDispatcher) DispatchAgentProvision(context.Context, *store.Agent) error {
	return nil
}
func (d *reconcileTestDispatcher) DispatchAgentStart(context.Context, *store.Agent, string) error {
	return nil
}
func (d *reconcileTestDispatcher) DispatchAgentStop(context.Context, *store.Agent) error { return nil }
func (d *reconcileTestDispatcher) DispatchAgentRestart(context.Context, *store.Agent) error {
	return nil
}
func (d *reconcileTestDispatcher) DispatchAgentDelete(_ context.Context, _ *store.Agent, _, _, _ bool, _ time.Time) error {
	return nil
}
func (d *reconcileTestDispatcher) DispatchAgentMessage(_ context.Context, agent *store.Agent, msg string, _ bool, _ *messages.StructuredMessage) error {
	if d.onMessage != nil {
		return d.onMessage(agent, msg)
	}
	return nil
}
func (d *reconcileTestDispatcher) DispatchAgentLogs(context.Context, *store.Agent, int) (string, error) {
	return "", nil
}
func (d *reconcileTestDispatcher) DispatchAgentExec(context.Context, *store.Agent, []string, int) (string, int, error) {
	return "", 0, nil
}
func (d *reconcileTestDispatcher) DispatchCheckAgentPrompt(context.Context, *store.Agent) (bool, error) {
	return false, nil
}
func (d *reconcileTestDispatcher) DispatchAgentCreateWithGather(context.Context, *store.Agent) (*RemoteEnvRequirementsResponse, error) {
	return nil, nil
}
func (d *reconcileTestDispatcher) DispatchFinalizeEnv(context.Context, *store.Agent, map[string]string) error {
	return nil
}

type assertErr struct{}

func (assertErr) Error() string { return "boom" }
