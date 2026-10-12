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

// Observer publication for agent-to-agent DMs: ExecuteAgentDM publishes one
// observer-only copy of every accepted DM (normal or plain) to plugin
// observers through MessageBrokerProxy.

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

// dmObserverTestEnv wires a same-project agent pair with a spy broker bus behind a real
// MessageBrokerProxy, so observer publications are directly observable.
type dmObserverTestEnv struct {
	srv        *Server
	store      store.Store
	sender     *store.Agent
	target     *store.Agent
	dispatcher *recordingDispatcher
	spyBus     *spyBrokerBus
}

func setupDMObserverTest(t *testing.T) dmObserverTestEnv {
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
	// The agents' ancestry root is a member of the project, so they are
	// in good standing (ptone/scion#3433).
	ensureStandingRoot(t, s, project.ID, owner.ID)

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

	return dmObserverTestEnv{srv: srv, store: s, sender: sender, target: target, dispatcher: dispatcher, spyBus: spyBus}
}

// TestExecuteAgentDM_NormalStillPublishesObserverCopy pins that a normal
// agent DM publishes exactly one observer copy.
func TestExecuteAgentDM_NormalStillPublishesObserverCopy(t *testing.T) {
	env := setupDMObserverTest(t)
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

// TestExecuteAgentDM_PlainStillPublishesObserverCopy pins that a plain
// agent DM publishes exactly one observer copy, with Plain preserved.
func TestExecuteAgentDM_PlainStillPublishesObserverCopy(t *testing.T) {
	env := setupDMObserverTest(t)
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
