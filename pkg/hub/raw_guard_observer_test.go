//go:build !no_sqlite

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

package hub

// ---------------------------------------------------------------------------
// Do not mirror terminal input to message observers.
//
// For the still-supported raw shape (an unadorned direct single-agent DM),
// agent_dm_operation.go's observer publish (the agent-sender DM fork,
// ExecuteAgentDM) — which otherwise sends an observer-only copy of the
// message to plugin observers (Telegram, broker-log) and chat relays via
// MessageBrokerProxy — skips that publication entirely when the message is
// raw, and the tests below exercise that skip directly.
//
// handlers_agent_messaging.go's general-fallthrough observer publish
// carries the same `!structuredMsg.Raw` skip as a second check, but it is
// untested here (and untestable through this file's ExecuteAgentDM-only
// setup) because it is unreachable for raw in practice: the sender is only
// stamped "agent:" when an agent identity is in the request context, and
// that same condition always takes the ExecuteAgentDM fork in
// handleAgentMessage before that fallthrough code runs. Both call sites
// carry the guard; only the reachable one has a test.
// ---------------------------------------------------------------------------

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rawObserverTestEnv wires a same-project agent pair (so the still-supported
// raw shape is reachable) with a spy broker bus behind a real
// MessageBrokerProxy, so observer publications are directly observable.
type rawObserverTestEnv struct {
	srv        *Server
	store      store.Store
	sender     *store.Agent
	target     *store.Agent
	dispatcher *recordingDispatcher
	spyBus     *spyBrokerBus
}

func setupRawObserverTest(t *testing.T) rawObserverTestEnv {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	owner := &store.User{
		ID:      tid("obs-owner"),
		Email:   "obs-owner@test.example",
		Role:    store.UserRoleMember,
		Status:  "active",
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	project := &store.Project{
		ID:        tid("obs-project"),
		Name:      "obs-project",
		Slug:      "obs-project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	brokerID := tid("obs-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "obs-broker",
		Slug:   "obs-broker",
		Status: store.BrokerStatusOnline,
	}))

	sender := &store.Agent{
		ID:              tid("obs-sender"),
		Name:            "obs-sender",
		Slug:            "obs-sender",
		ProjectID:       project.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeProject,
		Ancestry:        []string{owner.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, sender))

	target := &store.Agent{
		ID:              tid("obs-target"),
		Name:            "obs-target",
		Slug:            "obs-target",
		ProjectID:       project.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeProject,
		Ancestry:        []string{owner.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, target))

	dmKey, err := messages.DMConversationKey("agent", sender.ID, "agent", target.ID)
	require.NoError(t, err)
	_, err = s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	spyBus := &spyBrokerBus{}
	inproc := eventbus.NewInProcessEventBus(slog.Default())
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: inproc},
		{Name: "spy", Bus: spyBus},
	}, slog.Default())
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	proxy := NewMessageBrokerProxy(fanout, s, events,
		func() AgentDispatcher { return dispatcher }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)

	return rawObserverTestEnv{srv: srv, store: s, sender: sender, target: target, dispatcher: dispatcher, spyBus: spyBus}
}

// TestExecuteAgentDM_RawSkipsObserverPublish proves the agent-sender DM fork
// (agent_dm_operation.go) does not publish an observer copy for an accepted
// raw DM.
func TestExecuteAgentDM_RawSkipsObserverPublish(t *testing.T) {
	env := setupRawObserverTest(t)
	ctx := context.Background()

	input := deliveryDMInput(env.sender, env.target, "RAWOBSERVER-PROBE")
	input.Raw = true

	result, dmErr := env.srv.ExecuteAgentDM(ctx, input)
	require.Nil(t, dmErr, "raw DM must be accepted (still-supported shape)")
	require.Equal(t, AgentDMAccepted, result.Outcome)

	require.Len(t, env.dispatcher.getCalls(), 1, "the raw DM must still dispatch to its target")
	assert.Empty(t, env.spyBus.getEvents(), "an accepted raw DM must not mirror its body to message observers")
}

// TestExecuteAgentDM_NormalStillPublishesObserverCopy is the positive
// control: a normal (non-raw, non-plain) agent DM must still publish
// exactly one observer copy, proving the raw skip above is a raw-specific
// change and not a break of observer publication generally.
func TestExecuteAgentDM_NormalStillPublishesObserverCopy(t *testing.T) {
	env := setupRawObserverTest(t)
	ctx := context.Background()

	input := deliveryDMInput(env.sender, env.target, "NORMALOBSERVER-PROBE")

	result, dmErr := env.srv.ExecuteAgentDM(ctx, input)
	require.Nil(t, dmErr)
	require.Equal(t, AgentDMAccepted, result.Outcome)

	events := env.spyBus.getEvents()
	require.Len(t, events, 1, "a normal agent DM must publish exactly one observer copy")
	assert.True(t, events[0].msg.ObserverOnly)
	assert.Equal(t, "NORMALOBSERVER-PROBE", events[0].msg.Msg)
}

// TestExecuteAgentDM_PlainStillPublishesObserverCopy proves Plain is
// unaffected by the raw-specific observer skip — only Raw carries literal
// keystrokes; Plain is still an ordinary message body.
func TestExecuteAgentDM_PlainStillPublishesObserverCopy(t *testing.T) {
	env := setupRawObserverTest(t)
	ctx := context.Background()

	input := deliveryDMInput(env.sender, env.target, "PLAINOBSERVER-PROBE")
	input.Plain = true

	result, dmErr := env.srv.ExecuteAgentDM(ctx, input)
	require.Nil(t, dmErr)
	require.Equal(t, AgentDMAccepted, result.Outcome)

	events := env.spyBus.getEvents()
	require.Len(t, events, 1, "a plain agent DM must publish exactly one observer copy")
	assert.True(t, events[0].msg.ObserverOnly)
	assert.True(t, events[0].msg.Plain)
}
