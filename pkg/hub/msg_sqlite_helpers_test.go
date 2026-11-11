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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// containmentDispatchSpy records all dispatch calls and asserts zero effects
// on denial. This is the load-bearing assertion: spy.calls == 0 proves that
// no external effect escaped when authorization denied.
type containmentDispatchSpy struct {
	mu    sync.Mutex
	calls []containmentDispatchCall
}

func newContainmentMockStore() *containmentMockStore {
	return &containmentMockStore{
		mockScheduledEventStore: *newMockStore(),
		memberships:             make(map[string]*store.ProjectMembership),
	}
}

// containmentTestServer creates a minimal Server with authzService wired up
// for fire-time containment tests.
func containmentTestServer(ms *containmentMockStore) *Server {
	srv := &Server{
		store:             ms,
		agentLifecycleLog: slog.Default(),
	}
	srv.authzService = NewAuthzService(ms, slog.Default())
	return srv
}

// grantContainmentMessageRole binds userID to a project role carrying
// agent.message in projectID, so the user is admitted to the project for a
// scheduled message fire and may message project-mode agents in it.
func grantContainmentMessageRole(ms *containmentMockStore, userID, projectID string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if _, ok := ms.roleDefinitions["member-role"]; !ok {
		ms.roleDefinitions["member-role"] = &store.RoleDefinition{
			ID:          "member-role",
			Name:        "Member",
			Permissions: []string{"agent.message"},
			ScopeType:   "project",
		}
	}
	ms.roleBindings = append(ms.roleBindings, &store.RoleBinding{
		ID:               "binding-" + userID + "-" + projectID,
		RoleDefinitionID: "member-role",
		PrincipalType:    "user",
		PrincipalID:      userID,
		ScopeType:        "project",
		ScopeID:          projectID,
	})
}

// authorizeScheduledMessageFireFor resolves evt's authority and authorizes
// the send to agent, in the order messageEventHandler applies them.
func authorizeScheduledMessageFireFor(srv *Server, evt store.ScheduledEvent, agent *store.Agent) error {
	ctx := context.Background()
	auth, identity, err := srv.resolveScheduledAuthority(ctx, evt)
	if err != nil {
		return err
	}
	return srv.authorizeScheduledMessageFire(ctx, evt, auth, identity, agent)
}

// mockDispatchesTo returns the recorded dispatches to slug.
func mockDispatchesTo(d *brokerMockDispatcher, slug string) []brokerDispatchedMsg {
	var out []brokerDispatchedMsg
	for _, m := range d.getMessages() {
		if m.agentSlug == slug {
			out = append(out, m)
		}
	}
	return out
}

// setupGroupTest creates a project on an online broker plus one agent per
// (slug, phase) pair. Returns the project ID and agents keyed by slug.
func setupGroupTest(t *testing.T, s store.Store, prefix string, phases map[string]string) (string, map[string]*store.Agent) {
	t.Helper()
	ctx := context.Background()
	projectID := tid(prefix + "-project")
	brokerID := tid(prefix + "-broker")
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projectID, Name: prefix + "-project", Slug: prefix + "-project"}))
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: prefix + "-broker", Slug: prefix + "-broker", Status: store.BrokerStatusOnline,
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: projectID, BrokerID: brokerID, BrokerName: prefix + "-broker", Status: store.BrokerStatusOnline,
	}))
	_ = s.CreateUser(ctx, &store.User{ID: DevUserID, Email: "dev@localhost", DisplayName: "Development User"})
	agents := map[string]*store.Agent{}
	for slug, phase := range phases {
		a := &store.Agent{
			ID: api.NewUUID(), Name: slug, Slug: slug, ProjectID: projectID,
			RuntimeBrokerID: brokerID, Phase: phase,
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		agents[slug] = a
	}
	return projectID, agents
}

// ctxHonoringDispatcher behaves like a real transport: a dispatch on a done
// ctx fails with the ctx error and is not recorded. Dispatches to slugs in
// fail are handed to onFail (which decides the error, and may cancel the
// caller's ctx to simulate a client disconnect mid-dispatch); every other
// dispatch is recorded.
type ctxHonoringDispatcher struct {
	brokerMockDispatcher
	mu     sync.Mutex
	fail   map[string]bool
	onFail func(ctx context.Context) error
}

func requireSingleRowState(t *testing.T, s store.Store, agentID, wantState string) store.Message {
	t.Helper()
	rows, err := s.ListMessages(context.Background(), store.MessageFilter{AgentID: agentID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, rows.Items, 1)
	assert.Equal(t, wantState, rows.Items[0].DispatchState)
	return rows.Items[0]
}

func newNoticeTestProxy(t *testing.T, s store.Store, dispatcher AgentDispatcher) *MessageBrokerProxy {
	t.Helper()
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	b := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = b.Close() })
	return NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
}

// capturingBus records every message published through it before handing it
// to the wrapped bus.
type capturingBus struct {
	eventbus.EventBus
	mu   sync.Mutex
	msgs []*messages.StructuredMessage
}

func (d *containmentDispatchSpy) DispatchAgentMessage(_ context.Context, agent *store.Agent, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, containmentDispatchCall{
		Method:        "DispatchAgentMessage",
		Agent:         agent,
		Message:       message,
		Interrupt:     interrupt,
		StructuredMsg: structuredMsg,
	})
	return nil
}

func (d *containmentDispatchSpy) DispatchAgentCreate(_ context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, containmentDispatchCall{Method: "DispatchAgentCreate", Agent: agent})
	return nil, nil
}

func (d *containmentDispatchSpy) DispatchAgentProvision(_ context.Context, _ *store.Agent) error {
	return nil
}

func (d *containmentDispatchSpy) DispatchAgentReprovision(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *containmentDispatchSpy) DispatchAgentStart(_ context.Context, agent *store.Agent, _ string, _ bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, containmentDispatchCall{Method: "DispatchAgentStart", Agent: agent})
	return nil
}
func (d *containmentDispatchSpy) DispatchAgentStop(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *containmentDispatchSpy) DispatchAgentRestart(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *containmentDispatchSpy) DispatchAgentResetAuth(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *containmentDispatchSpy) DispatchAgentDelete(_ context.Context, _ *store.Agent, _, _, _ bool, _ time.Time) error {
	return nil
}
func (d *containmentDispatchSpy) DispatchAgentLogs(_ context.Context, _ *store.Agent, _ int) (string, error) {
	return "", nil
}
func (d *containmentDispatchSpy) DispatchAgentExec(_ context.Context, _ *store.Agent, _ []string, _ int) (string, int, error) {
	return "", 0, nil
}
func (d *containmentDispatchSpy) DispatchCheckAgentPrompt(_ context.Context, _ *store.Agent) (bool, error) {
	return false, nil
}
func (d *containmentDispatchSpy) DispatchAgentCreateWithGather(_ context.Context, _ *store.Agent) (*CreateDispatchResult, error) {
	return nil, nil
}
func (d *containmentDispatchSpy) DispatchFinalizeEnv(_ context.Context, _ *store.Agent, _ map[string]string) (*CreateDispatchResult, error) {
	return nil, nil
}

func (d *containmentDispatchSpy) getCalls() []containmentDispatchCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	cp := make([]containmentDispatchCall, len(d.calls))
	copy(cp, d.calls)
	return cp
}

type containmentDispatchCall struct {
	Method        string
	Agent         *store.Agent
	Message       string
	Interrupt     bool
	StructuredMsg *messages.StructuredMessage
}

func (d *ctxHonoringDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	failing := d.fail[agent.Slug]
	d.mu.Unlock()
	if failing {
		// Record the attempt (including the message ID carried on ctx) so
		// tests can assert on it, then fail.
		_ = d.brokerMockDispatcher.DispatchAgentMessage(ctx, agent, message, interrupt, structuredMsg)
		return d.onFail(ctx)
	}
	return d.brokerMockDispatcher.DispatchAgentMessage(ctx, agent, message, interrupt, structuredMsg)
}

// noticesTo returns DELIVERY_FAILED notices dispatched to slug.
func (d *ctxHonoringDispatcher) noticesTo(slug string) []brokerDispatchedMsg {
	var out []brokerDispatchedMsg
	for _, m := range d.getMessages() {
		if m.agentSlug == slug && m.structured != nil && m.structured.Status == "DELIVERY_FAILED" {
			out = append(out, m)
		}
	}
	return out
}

func (b *capturingBus) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	b.mu.Lock()
	b.msgs = append(b.msgs, msg)
	b.mu.Unlock()
	return b.EventBus.Publish(ctx, topic, msg)
}

func (b *capturingBus) published() []*messages.StructuredMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*messages.StructuredMessage(nil), b.msgs...)
}

// containmentMockStore extends mockScheduledEventStore with additional methods
// needed for messaging authorization (GetProjectMembership, etc).
type containmentMockStore struct {
	mockScheduledEventStore
	memberships map[string]*store.ProjectMembership // key: projectID+":"+userID
}

// userMessageSSECounter counts user.message SSE events for one user.
// ChannelEventPublisher sends synchronously (non-blocking) into the
// subscriber channel, so once the writers have quiesced every event is
// already buffered and count can drain it without waiting.
type userMessageSSECounter struct {
	ch <-chan Event
	n  int
}

func countUserMessageSSE(t *testing.T, events *ChannelEventPublisher, userID string) *userMessageSSECounter {
	t.Helper()
	ch, unsub := events.Subscribe("user." + userID + ".message")
	t.Cleanup(unsub)
	return &userMessageSSECounter{ch: ch}
}

func (m *containmentMockStore) GetProjectMembership(_ context.Context, projectID, userID string) (*store.ProjectMembership, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := projectID + ":" + userID
	if mb, ok := m.memberships[key]; ok {
		return mb, nil
	}
	return nil, store.ErrNotFound
}

// GetEffectiveGroupsForAgent returns nil for containment tests (no group-derived grants).
func (m *containmentMockStore) GetEffectiveGroupsForAgent(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (c *userMessageSSECounter) count() int {
	for {
		select {
		case _, ok := <-c.ch:
			if !ok { // publisher closed: a closed channel never blocks
				return c.n
			}
			c.n++
		default:
			return c.n
		}
	}
}
