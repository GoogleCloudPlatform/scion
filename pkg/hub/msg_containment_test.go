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

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Containment test infrastructure
// =============================================================================

// containmentDispatchSpy records all dispatch calls and asserts zero effects
// on denial. This is the load-bearing assertion: spy.calls == 0 proves that
// no external effect escaped when authorization denied.
type containmentDispatchSpy struct {
	mu    sync.Mutex
	calls []containmentDispatchCall
}

type containmentDispatchCall struct {
	Method        string
	Agent         *store.Agent
	Message       string
	Interrupt     bool
	StructuredMsg *messages.StructuredMessage
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

func (d *containmentDispatchSpy) DispatchAgentCreate(_ context.Context, agent *store.Agent) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, containmentDispatchCall{Method: "DispatchAgentCreate", Agent: agent})
	return nil
}

func (d *containmentDispatchSpy) DispatchAgentProvision(_ context.Context, _ *store.Agent) error {
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
func (d *containmentDispatchSpy) DispatchAgentCreateWithGather(_ context.Context, _ *store.Agent) (*RemoteEnvRequirementsResponse, error) {
	return nil, nil
}
func (d *containmentDispatchSpy) DispatchFinalizeEnv(_ context.Context, _ *store.Agent, _ map[string]string) error {
	return nil
}

var _ AgentDispatcher = (*containmentDispatchSpy)(nil)

func (d *containmentDispatchSpy) getCalls() []containmentDispatchCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	cp := make([]containmentDispatchCall, len(d.calls))
	copy(cp, d.calls)
	return cp
}

// containmentMockStore extends mockScheduledEventStore with additional methods
// needed for messaging authorization (GetProjectMembership, etc).
type containmentMockStore struct {
	mockScheduledEventStore
	memberships map[string]*store.ProjectMembership // key: projectID+":"+userID
}

func newContainmentMockStore() *containmentMockStore {
	return &containmentMockStore{
		mockScheduledEventStore: *newMockStore(),
		memberships:             make(map[string]*store.ProjectMembership),
	}
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

// =============================================================================
// C1 tests: scheduled message bypass closure
// =============================================================================

func TestC1_ScheduledMessageCrossProjectAgentID(t *testing.T) {
	// T-B1-01: Schedule a message event targeting an agent in another project.
	// The fire-time authorization must deny; spy.calls == 0.
	ms := newContainmentMockStore()
	spy := &containmentDispatchSpy{}

	projectA := "project-a"
	projectB := "project-b"
	creatorID := "creator-user"

	ms.users[creatorID] = &store.User{
		ID:     creatorID,
		Email:  "creator@test.com",
		Status: store.UserStatusActive,
		Role:   "member",
	}
	ms.agents["agent-in-b"] = &store.Agent{
		ID:          "agent-in-b",
		Name:        "agent-b",
		Slug:        "agent-b",
		ProjectID:   projectB,
		MessageMode: store.MessageModeProject,
	}

	srv := containmentTestServer(ms)
	srv.SetDispatcher(spy)

	payload, _ := json.Marshal(MessageEventPayload{
		AgentID: "agent-in-b",
		Message: "cross-project message",
	})

	evt := store.ScheduledEvent{
		ID:        "evt-cross-project",
		ProjectID: projectA,
		EventType: "message",
		Payload:   string(payload),
		CreatedBy: creatorID,
		FireAt:    time.Now(),
		Status:    store.ScheduledEventPending,
	}
	ms.events[evt.ID] = &evt

	handler := srv.messageEventHandler()
	err := handler(context.Background(), evt)
	// Handler returns the denial error — the enclosing scheduler wrapper
	// (fireEvent / executeSchedule) owns status recording.
	if err == nil {
		t.Fatal("handler must return error on authorization denial")
	}

	if len(spy.getCalls()) != 0 {
		t.Errorf("expected zero dispatch calls for cross-project target, got %d", len(spy.getCalls()))
	}
}

func TestC1_ScheduledMessageCrossProjectRawPayload(t *testing.T) {
	// T-B1-02: Cross-project via raw payload string.
	ms := newContainmentMockStore()
	spy := &containmentDispatchSpy{}

	ms.users["creator"] = &store.User{
		ID: "creator", Email: "c@t.com", Status: store.UserStatusActive, Role: "member",
	}
	ms.agents["agent-other"] = &store.Agent{
		ID: "agent-other", Name: "other", Slug: "other",
		ProjectID: "project-other", MessageMode: store.MessageModeProject,
	}

	srv := containmentTestServer(ms)
	srv.SetDispatcher(spy)

	payload, _ := json.Marshal(MessageEventPayload{
		AgentID: "agent-other",
		Message: "test",
	})
	evt := store.ScheduledEvent{
		ID: "evt-raw", ProjectID: "project-a", EventType: "message",
		Payload: string(payload), CreatedBy: "creator",
		FireAt: time.Now(), Status: store.ScheduledEventPending,
	}
	ms.events[evt.ID] = &evt

	handler := srv.messageEventHandler()
	_ = handler(context.Background(), evt)

	if len(spy.getCalls()) != 0 {
		t.Errorf("expected zero dispatch calls for cross-project raw payload, got %d", len(spy.getCalls()))
	}
}

func TestC1_ScheduledMessageModeNoneDenied(t *testing.T) {
	// T-B1-05: Same-project agent with message_mode=none; fire denied.
	ms := newContainmentMockStore()
	spy := &containmentDispatchSpy{}

	projectID := "project-1"
	creatorID := "user-creator"

	ms.users[creatorID] = &store.User{
		ID: creatorID, Email: "u@t.com", Status: store.UserStatusActive, Role: "member",
	}
	ms.agents["agent-none"] = &store.Agent{
		ID: "agent-none", Name: "none-agent", Slug: "none-agent",
		ProjectID: projectID, MessageMode: store.MessageModeNone,
	}

	srv := containmentTestServer(ms)
	srv.SetDispatcher(spy)

	payload, _ := json.Marshal(MessageEventPayload{
		AgentID: "agent-none", Message: "test",
	})
	evt := store.ScheduledEvent{
		ID: "evt-mode-none", ProjectID: projectID, EventType: "message",
		Payload: string(payload), CreatedBy: creatorID,
		FireAt: time.Now(), Status: store.ScheduledEventPending,
	}
	ms.events[evt.ID] = &evt

	handler := srv.messageEventHandler()
	err := handler(context.Background(), evt)
	if err == nil {
		t.Fatal("handler must return error on authorization denial")
	}

	if len(spy.getCalls()) != 0 {
		t.Errorf("expected zero dispatch for mode=none target, got %d", len(spy.getCalls()))
	}
}

func TestC1_ScheduledMessageBranchDenied(t *testing.T) {
	// T-B1-06: Agent-authored scheduled message to same-project agent with
	// branch mode, no parent/child relationship.
	ms := newContainmentMockStore()
	spy := &containmentDispatchSpy{}

	projectID := "project-1"
	creatorAgentID := "creator-agent"
	targetAgentID := "target-agent"

	ms.agents[creatorAgentID] = &store.Agent{
		ID: creatorAgentID, Name: "creator", Slug: "creator",
		ProjectID: projectID, MessageMode: store.MessageModeBranch,
		AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)},
	}
	ms.agents[targetAgentID] = &store.Agent{
		ID: targetAgentID, Name: "target", Slug: "target",
		ProjectID: projectID, MessageMode: store.MessageModeBranch,
		// No ancestry relationship with creator
	}

	srv := containmentTestServer(ms)
	srv.SetDispatcher(spy)

	payload, _ := json.Marshal(MessageEventPayload{
		AgentID: targetAgentID, Message: "test",
	})
	evt := store.ScheduledEvent{
		ID: "evt-branch", ProjectID: projectID, EventType: "message",
		Payload: string(payload), CreatedBy: creatorAgentID,
		FireAt: time.Now(), Status: store.ScheduledEventPending,
	}
	ms.events[evt.ID] = &evt

	handler := srv.messageEventHandler()
	_ = handler(context.Background(), evt)

	if len(spy.getCalls()) != 0 {
		t.Errorf("expected zero dispatch for branch/branch without parent-child, got %d", len(spy.getCalls()))
	}
}

func TestC1_ScheduledMessageCreatorSuspended(t *testing.T) {
	// T-B1-08: Creator user suspended between authoring and fire.
	ms := newContainmentMockStore()
	spy := &containmentDispatchSpy{}

	projectID := "project-1"
	creatorID := "suspended-user"

	ms.users[creatorID] = &store.User{
		ID: creatorID, Email: "sus@t.com",
		Status: store.UserStatusSuspended, Role: "member",
	}
	ms.agents["target"] = &store.Agent{
		ID: "target", Name: "target", Slug: "target",
		ProjectID: projectID, MessageMode: store.MessageModeProject,
	}

	srv := containmentTestServer(ms)
	srv.SetDispatcher(spy)

	payload, _ := json.Marshal(MessageEventPayload{
		AgentID: "target", Message: "test",
	})
	evt := store.ScheduledEvent{
		ID: "evt-suspended", ProjectID: projectID, EventType: "message",
		Payload: string(payload), CreatedBy: creatorID,
		FireAt: time.Now(), Status: store.ScheduledEventPending,
	}
	ms.events[evt.ID] = &evt

	handler := srv.messageEventHandler()
	err := handler(context.Background(), evt)
	if err == nil {
		t.Fatal("handler must return error on authorization denial")
	}

	if len(spy.getCalls()) != 0 {
		t.Errorf("expected zero dispatch for suspended creator, got %d", len(spy.getCalls()))
	}
}

func TestC1_ScheduledMessageCreatorAgentDeleted(t *testing.T) {
	// T-B1-09: Creator agent deleted between authoring and fire.
	// The creator agent ID is not in the store, and is not a user either.
	ms := newContainmentMockStore()
	spy := &containmentDispatchSpy{}

	projectID := "project-1"

	ms.agents["target"] = &store.Agent{
		ID: "target", Name: "target", Slug: "target",
		ProjectID: projectID, MessageMode: store.MessageModeProject,
	}

	srv := containmentTestServer(ms)
	srv.SetDispatcher(spy)

	payload, _ := json.Marshal(MessageEventPayload{
		AgentID: "target", Message: "test",
	})
	evt := store.ScheduledEvent{
		ID: "evt-deleted-creator", ProjectID: projectID, EventType: "message",
		Payload: string(payload), CreatedBy: "deleted-creator-agent-id",
		FireAt: time.Now(), Status: store.ScheduledEventPending,
	}
	ms.events[evt.ID] = &evt

	handler := srv.messageEventHandler()
	err := handler(context.Background(), evt)
	if err == nil {
		t.Fatal("handler must return error on authorization denial")
	}

	if len(spy.getCalls()) != 0 {
		t.Errorf("expected zero dispatch for deleted creator, got %d", len(spy.getCalls()))
	}
}

func TestC1_ScheduledMessageEmptyCreatedBy(t *testing.T) {
	// T-B1-11: evt.CreatedBy == "" (legacy row) — fail closed.
	ms := newContainmentMockStore()
	spy := &containmentDispatchSpy{}

	projectID := "project-1"

	ms.agents["target"] = &store.Agent{
		ID: "target", Name: "target", Slug: "target",
		ProjectID: projectID, MessageMode: store.MessageModeProject,
	}

	srv := containmentTestServer(ms)
	srv.SetDispatcher(spy)

	payload, _ := json.Marshal(MessageEventPayload{
		AgentID: "target", Message: "test",
	})
	evt := store.ScheduledEvent{
		ID: "evt-empty-creator", ProjectID: projectID, EventType: "message",
		Payload: string(payload), CreatedBy: "", // empty — legacy
		FireAt: time.Now(), Status: store.ScheduledEventPending,
	}
	ms.events[evt.ID] = &evt

	handler := srv.messageEventHandler()
	err := handler(context.Background(), evt)
	if err == nil {
		t.Fatal("handler must return error on authorization denial")
	}

	if len(spy.getCalls()) != 0 {
		t.Errorf("expected zero dispatch for empty CreatedBy, got %d", len(spy.getCalls()))
	}
}

func TestC1_ScheduledMessageTargetModeChanged(t *testing.T) {
	// T-B1-10: Target agent's message_mode flipped project → none between
	// authoring and fire (D10 live evaluation).
	ms := newContainmentMockStore()
	spy := &containmentDispatchSpy{}

	projectID := "project-1"
	creatorID := "user-creator"

	ms.users[creatorID] = &store.User{
		ID: creatorID, Email: "u@t.com", Status: store.UserStatusActive, Role: "member",
	}
	// Agent's mode is now "none" — it was "project" when authored.
	ms.agents["target"] = &store.Agent{
		ID: "target", Name: "target", Slug: "target",
		ProjectID: projectID, MessageMode: store.MessageModeNone,
	}

	srv := containmentTestServer(ms)
	srv.SetDispatcher(spy)

	payload, _ := json.Marshal(MessageEventPayload{
		AgentID: "target", Message: "test",
	})
	evt := store.ScheduledEvent{
		ID: "evt-mode-changed", ProjectID: projectID, EventType: "message",
		Payload: string(payload), CreatedBy: creatorID,
		FireAt: time.Now(), Status: store.ScheduledEventPending,
	}
	ms.events[evt.ID] = &evt

	handler := srv.messageEventHandler()
	_ = handler(context.Background(), evt)

	if len(spy.getCalls()) != 0 {
		t.Errorf("expected zero dispatch after mode flip to none, got %d", len(spy.getCalls()))
	}
}

func TestC1_ScheduledMessageHappyPath(t *testing.T) {
	// T-B1-12: Happy path — same project, mode=project, creator holds
	// agent.message. spy.calls == 1.
	ms := newContainmentMockStore()
	spy := &containmentDispatchSpy{}

	projectID := "project-1"
	creatorID := "user-creator"

	ms.users[creatorID] = &store.User{
		ID: creatorID, Email: "u@t.com", Status: store.UserStatusActive, Role: "member",
	}
	ms.agents["target"] = &store.Agent{
		ID: "target", Name: "target", Slug: "target",
		ProjectID: projectID, MessageMode: store.MessageModeProject,
	}

	// Set up role binding so the creator has agent.message permission.
	roleDef := &store.RoleDefinition{
		ID:          "member-role",
		Name:        "Member",
		Permissions: []string{"agent.message"},
		ScopeType:   "project",
	}
	ms.roleDefinitions["member-role"] = roleDef
	ms.roleBindings = append(ms.roleBindings, &store.RoleBinding{
		ID:               "binding-1",
		RoleDefinitionID: "member-role",
		PrincipalType:    "user",
		PrincipalID:      creatorID,
		ScopeType:        "project",
		ScopeID:          projectID,
	})

	srv := containmentTestServer(ms)
	srv.SetDispatcher(spy)

	payload, _ := json.Marshal(MessageEventPayload{
		AgentID: "target", Message: "hello from schedule",
	})
	evt := store.ScheduledEvent{
		ID: "evt-happy", ProjectID: projectID, EventType: "message",
		Payload: string(payload), CreatedBy: creatorID,
		FireAt: time.Now(), Status: store.ScheduledEventPending,
	}
	ms.events[evt.ID] = &evt

	handler := srv.messageEventHandler()
	err := handler(context.Background(), evt)
	if err != nil {
		t.Fatalf("happy path should not return error: %v", err)
	}

	calls := spy.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 dispatch call for happy path, got %d", len(calls))
	}
	if calls[0].Agent.ID != "target" {
		t.Errorf("dispatched to wrong agent: got %q, want %q", calls[0].Agent.ID, "target")
	}
}

func TestC1_ScheduledMessageStoreErrorFailsClosed(t *testing.T) {
	// Store resolution failure → fail closed (no dispatch).
	ms := newContainmentMockStore()
	spy := &containmentDispatchSpy{}

	projectID := "project-1"
	// Creator is neither an agent nor a user — both lookups fail with ErrNotFound.
	// This simulates a creator whose record was hard-deleted.

	ms.agents["target"] = &store.Agent{
		ID: "target", Name: "target", Slug: "target",
		ProjectID: projectID, MessageMode: store.MessageModeProject,
	}

	srv := containmentTestServer(ms)
	srv.SetDispatcher(spy)

	payload, _ := json.Marshal(MessageEventPayload{
		AgentID: "target", Message: "test",
	})
	evt := store.ScheduledEvent{
		ID: "evt-store-error", ProjectID: projectID, EventType: "message",
		Payload: string(payload), CreatedBy: "nonexistent-id",
		FireAt: time.Now(), Status: store.ScheduledEventPending,
	}
	ms.events[evt.ID] = &evt

	handler := srv.messageEventHandler()
	_ = handler(context.Background(), evt)

	if len(spy.getCalls()) != 0 {
		t.Errorf("expected zero dispatch on store resolution failure, got %d", len(spy.getCalls()))
	}
}

func TestC1_ScheduledMessageCreatorAgentSoftDeleted(t *testing.T) {
	// Creator agent is soft-deleted (DeletedAt is set).
	ms := newContainmentMockStore()
	spy := &containmentDispatchSpy{}

	projectID := "project-1"
	deletedAt := time.Now().Add(-1 * time.Hour)

	ms.agents["deleted-creator"] = &store.Agent{
		ID: "deleted-creator", Name: "creator", Slug: "creator",
		ProjectID: projectID, DeletedAt: deletedAt,
		AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)},
	}
	ms.agents["target"] = &store.Agent{
		ID: "target", Name: "target", Slug: "target",
		ProjectID: projectID, MessageMode: store.MessageModeProject,
	}

	srv := containmentTestServer(ms)
	srv.SetDispatcher(spy)

	payload, _ := json.Marshal(MessageEventPayload{
		AgentID: "target", Message: "test",
	})
	evt := store.ScheduledEvent{
		ID: "evt-soft-deleted", ProjectID: projectID, EventType: "message",
		Payload: string(payload), CreatedBy: "deleted-creator",
		FireAt: time.Now(), Status: store.ScheduledEventPending,
	}
	ms.events[evt.ID] = &evt

	handler := srv.messageEventHandler()
	_ = handler(context.Background(), evt)

	if len(spy.getCalls()) != 0 {
		t.Errorf("expected zero dispatch for soft-deleted creator agent, got %d", len(spy.getCalls()))
	}
}

func TestC1_ScheduledMessageDenialReturnsError(t *testing.T) {
	// O-R2-1: handler returns the denial error (not nil) so the enclosing
	// scheduler wrapper (fireEvent / executeSchedule) records the error
	// message on the event. Zero dispatch calls regardless.
	ms := newContainmentMockStore()
	spy := &containmentDispatchSpy{}

	projectA := "project-a"
	projectB := "project-b"
	creatorID := "creator-user"

	ms.users[creatorID] = &store.User{
		ID: creatorID, Email: "creator@test.com",
		Status: store.UserStatusActive, Role: "member",
	}
	ms.agents["agent-in-b"] = &store.Agent{
		ID: "agent-in-b", Name: "agent-b", Slug: "agent-b",
		ProjectID: projectB, MessageMode: store.MessageModeProject,
	}

	srv := containmentTestServer(ms)
	srv.SetDispatcher(spy)

	payload, _ := json.Marshal(MessageEventPayload{
		AgentID: "agent-in-b", Message: "cross-project message",
	})
	evt := store.ScheduledEvent{
		ID: "evt-denial-error", ProjectID: projectA, EventType: "message",
		Payload: string(payload), CreatedBy: creatorID,
		FireAt: time.Now(), Status: store.ScheduledEventPending,
	}
	ms.events[evt.ID] = &evt

	handler := srv.messageEventHandler()
	err := handler(context.Background(), evt)

	// Handler must return the denial error.
	if err == nil {
		t.Fatal("handler must return error on authorization denial")
	}
	// Error must contain the denial reason for the scheduler to record.
	if !strings.Contains(err.Error(), "scheduled_message_cross_project") {
		t.Errorf("error should contain denial code, got: %v", err)
	}

	// Load-bearing: zero dispatch calls.
	if len(spy.getCalls()) != 0 {
		t.Errorf("denial must produce zero dispatch calls: got %d", len(spy.getCalls()))
	}
}

func TestC1_ScheduledMessageTargetDeletedReturnsError(t *testing.T) {
	// F-R2-2: target agent deleted path returns the error (not nil) so the
	// enclosing scheduler wrapper records it. Zero dispatch calls.
	ms := newContainmentMockStore()
	spy := &containmentDispatchSpy{}

	projectID := "project-1"
	creatorID := "creator-user"

	ms.users[creatorID] = &store.User{
		ID: creatorID, Email: "creator@test.com",
		Status: store.UserStatusActive, Role: "member",
	}
	// Target agent NOT in store — simulates deletion.

	srv := containmentTestServer(ms)
	srv.SetDispatcher(spy)

	payload, _ := json.Marshal(MessageEventPayload{
		AgentID: "deleted-agent", Message: "message to deleted agent",
	})
	evt := store.ScheduledEvent{
		ID: "evt-target-deleted", ProjectID: projectID, EventType: "message",
		Payload: string(payload), CreatedBy: creatorID,
		FireAt: time.Now(), Status: store.ScheduledEventPending,
	}
	ms.events[evt.ID] = &evt

	handler := srv.messageEventHandler()
	err := handler(context.Background(), evt)

	if err == nil {
		t.Fatal("handler must return error when target agent is deleted")
	}
	if !strings.Contains(err.Error(), "target agent deleted") {
		t.Errorf("error should reference target agent deletion, got: %v", err)
	}

	if len(spy.getCalls()) != 0 {
		t.Errorf("deleted target must produce zero dispatch calls: got %d", len(spy.getCalls()))
	}
}

// =============================================================================
// Production-wrapper tests: fireEvent and executeSchedule
//
// These tests exercise the real scheduler wrappers to verify that denial
// errors propagate through the production status-recording path, not just
// the handler in isolation (O-R2-1).
// =============================================================================

func TestC1_FireEvent_DenialRecordsErrorOnEvent(t *testing.T) {
	// O-R2-1: when the message handler returns a denial error, fireEvent
	// must record it on the event (status "fired" with error message).
	// Zero dispatch calls.
	ms := newContainmentMockStore()
	spy := &containmentDispatchSpy{}

	projectA := "project-a"
	projectB := "project-b"
	creatorID := "creator-user"

	ms.users[creatorID] = &store.User{
		ID: creatorID, Email: "creator@test.com",
		Status: store.UserStatusActive, Role: "member",
	}
	ms.agents["agent-in-b"] = &store.Agent{
		ID: "agent-in-b", Name: "agent-b", Slug: "agent-b",
		ProjectID: projectB, MessageMode: store.MessageModeProject,
	}

	srv := containmentTestServer(ms)
	srv.SetDispatcher(spy)

	// Create a Scheduler and register the message handler.
	sched := NewScheduler(ms, slog.Default())
	sched.RegisterEventHandler("message", srv.messageEventHandler())

	payload, _ := json.Marshal(MessageEventPayload{
		AgentID: "agent-in-b", Message: "cross-project message",
	})
	evt := store.ScheduledEvent{
		ID: "evt-fire-denial", ProjectID: projectA, EventType: "message",
		Payload: string(payload), CreatedBy: creatorID,
		FireAt: time.Now(), Status: store.ScheduledEventPending,
	}
	require.NoError(t, ms.CreateScheduledEvent(context.Background(), &evt))

	// Fire through the real scheduler wrapper.
	sched.fireEvent(context.Background(), evt, false)

	// Status must be "fired" (not "failed") — fireEvent owns status.
	e := ms.getEvent(evt.ID)
	require.NotNil(t, e, "event must exist after fireEvent")
	assert.Equal(t, store.ScheduledEventFired, e.Status,
		"fireEvent sets status to 'fired' — the error message field carries the denial reason")
	assert.Contains(t, e.Error, "scheduled_message_cross_project",
		"event error field must contain the denial reason")

	// Zero dispatch calls.
	assert.Empty(t, spy.getCalls(), "denial must produce zero dispatch calls through fireEvent")
}

func TestC1_FireEvent_SuccessRecordsFiredNoError(t *testing.T) {
	// Complementary: a successful message event through fireEvent must
	// record status "fired" with no error, and dispatch exactly once.
	ms := newContainmentMockStore()
	spy := &containmentDispatchSpy{}

	projectID := "project-1"
	creatorID := "creator-user"

	ms.users[creatorID] = &store.User{
		ID: creatorID, Email: "creator@test.com",
		Status: store.UserStatusActive, Role: "member",
	}
	ms.agents["target"] = &store.Agent{
		ID: "target", Name: "target", Slug: "target",
		ProjectID: projectID, MessageMode: store.MessageModeProject,
	}
	ms.roleDefinitions["member-role"] = &store.RoleDefinition{
		ID:          "member-role",
		Name:        "Member",
		Permissions: []string{"agent.message"},
		ScopeType:   "project",
	}
	ms.roleBindings = append(ms.roleBindings, &store.RoleBinding{
		ID:               "binding-success",
		RoleDefinitionID: "member-role",
		PrincipalType:    "user",
		PrincipalID:      creatorID,
		ScopeType:        "project",
		ScopeID:          projectID,
	})

	srv := containmentTestServer(ms)
	srv.SetDispatcher(spy)

	sched := NewScheduler(ms, slog.Default())
	sched.RegisterEventHandler("message", srv.messageEventHandler())

	payload, _ := json.Marshal(MessageEventPayload{
		AgentID: "target", Message: "hello",
	})
	evt := store.ScheduledEvent{
		ID: "evt-fire-success", ProjectID: projectID, EventType: "message",
		Payload: string(payload), CreatedBy: creatorID,
		FireAt: time.Now(), Status: store.ScheduledEventPending,
	}
	require.NoError(t, ms.CreateScheduledEvent(context.Background(), &evt))

	sched.fireEvent(context.Background(), evt, false)

	e := ms.getEvent(evt.ID)
	require.NotNil(t, e, "event must exist after fireEvent")
	assert.Equal(t, store.ScheduledEventFired, e.Status, "success event should be 'fired'")
	assert.Empty(t, e.Error, "success event should have no error message")

	calls := spy.getCalls()
	assert.Len(t, calls, 1, "success event should dispatch exactly once")
}

func TestC1_ExecuteSchedule_DenialRecordsErrorOnEvent(t *testing.T) {
	// O-R2-1 (recurring path): when the message handler denies, executeSchedule
	// must record the error on the created event. Zero dispatch calls.
	srv, s := testServer(t)
	ctx := context.Background()
	spy := &containmentDispatchSpy{}
	srv.SetDispatcher(spy)

	srv.scheduler = NewScheduler(s, slog.Default())
	srv.scheduler.RegisterEventHandler("message", srv.messageEventHandler())

	projectA := &store.Project{
		ID: tid("exec-sched-a"), Name: "Project A", Slug: "exec-sched-a",
	}
	require.NoError(t, s.CreateProject(ctx, projectA))

	projectB := &store.Project{
		ID: tid("exec-sched-b"), Name: "Project B", Slug: "exec-sched-b",
	}
	require.NoError(t, s.CreateProject(ctx, projectB))

	crossAgent := &store.Agent{
		ID: tid("exec-cross-agent"), Name: "cross-agent", Slug: "cross-agent",
		ProjectID: projectB.ID, MessageMode: store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, crossAgent))

	// Create a schedule in project A that targets agent in project B.
	crossPayload, _ := json.Marshal(MessageEventPayload{
		AgentID: crossAgent.ID, Message: "cross-project recurring",
	})
	schedule := store.Schedule{
		ID:        tid("exec-cross-sched"),
		ProjectID: projectA.ID,
		Name:      "cross-project-recurring",
		CronExpr:  "0 * * * *",
		EventType: "message",
		Payload:   string(crossPayload),
		Status:    store.ScheduleStatusActive,
		CreatedBy: "dev",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	require.NoError(t, s.CreateSchedule(ctx, &schedule))

	// Execute the schedule through the real production wrapper.
	srv.executeSchedule(ctx, schedule, time.Now())

	// Zero dispatch calls.
	assert.Empty(t, spy.getCalls(), "recurring denial must produce zero dispatch calls")

	// Find the event created by executeSchedule and verify its error.
	result, err := s.ListScheduledEvents(ctx,
		store.ScheduledEventFilter{ProjectID: projectA.ID},
		store.ListOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, result.Items, "executeSchedule must create an event")
	latestEvt := result.Items[len(result.Items)-1]
	assert.Equal(t, store.ScheduledEventFired, latestEvt.Status,
		"recurring event status must be 'fired' — the error message carries the denial")
	assert.Contains(t, latestEvt.Error, "cross_project",
		"event error field must contain the denial reason")
}

// =============================================================================
// C2 tests: synthesized-user lifecycle checks
// =============================================================================

// C2 tests use the full testServer because they exercise HTTP handler paths
// with real broker auth. See msg_containment_broker_test.go for those.
// Here we unit-test the authorization function directly.

func TestC1_AuthorizeScheduledMessageAuthoring_CrossProjectByAgentID(t *testing.T) {
	// T-B1-01 authoring path: when the target agent exists but is in a different
	// project, authoring-time validation must deny with 403.
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID: tid("authoring-proj"), Name: "Test Project", Slug: "authoring-proj",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	otherProject := &store.Project{
		ID: tid("other-proj"), Name: "Other Project", Slug: "other-proj",
	}
	require.NoError(t, s.CreateProject(ctx, otherProject))

	// Create an agent in the OTHER project.
	agent := &store.Agent{
		ID: tid("cross-agent"), Name: "cross-agent", Slug: "cross-agent",
		ProjectID: otherProject.ID, MessageMode: store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Try to create a scheduled message event in project A targeting agent in project B.
	reqBody := CreateScheduledEventRequest{
		EventType: "message",
		FireIn:    "30m",
		AgentID:   agent.ID, // cross-project agent ID
		Message:   "cross-project message",
	}
	rec := doRequest(t, srv, "POST", "/api/v1/projects/"+project.ID+"/scheduled-events", reqBody)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for cross-project agent at authoring, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestC1_AuthorizeScheduledMessageAuthoring_UATDenied(t *testing.T) {
	// T-B1-07: UAT-scoped token cannot author scheduled messages because
	// the credential caveats cannot be preserved at fire time.
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID: tid("uat-proj"), Name: "UAT Project", Slug: "uat-proj",
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroup(ctx, project)

	user := &store.User{
		ID: tid("uat-user"), Email: "uat@test.com", DisplayName: "UAT User",
		Status: store.UserStatusActive, Role: "member",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	agent := &store.Agent{
		ID: tid("uat-target"), Name: "uat-target", Slug: "uat-target",
		ProjectID: project.ID, MessageMode: store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Create a scoped user identity (UAT).
	baseUser := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "token")
	scopedIdent := NewScopedUserIdentity(baseUser, project.ID, []string{"scheduled_event:create", "agent:message"})

	// Make the request with the scoped identity.
	reqBody := CreateScheduledEventRequest{
		EventType: "message",
		FireIn:    "30m",
		AgentID:   agent.ID,
		Message:   "hello",
	}
	bodyBytes, _ := json.Marshal(reqBody)
	req := httptest.NewRequest("POST", "/api/v1/projects/"+project.ID+"/scheduled-events", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), scopedIdent))

	rec := httptest.NewRecorder()
	srv.handleScheduledEvents(rec, req, project.ID, "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for UAT-scoped authoring, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestC1_AuthorizeScheduledMessageAuthoring_ConflictingTarget(t *testing.T) {
	// O1: When both rawPayload and convenience fields specify targets that
	// disagree, authoring must reject with 400 to prevent validating one
	// target while storing another.
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID: tid("conflict-proj"), Name: "Conflict Project", Slug: "conflict-proj",
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroup(ctx, project)

	user := &store.User{
		ID: tid("conflict-user"), Email: "conflict@test.com", DisplayName: "Conflict User",
		Status: store.UserStatusActive, Role: "member",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)
	msgAuthzAddProjectMember(t, s, user.ID, project.ID, project.Slug, store.GroupMemberRoleMember)

	sameProjectAgent := &store.Agent{
		ID: tid("same-agent"), Name: "same-agent", Slug: "same-agent",
		ProjectID: project.ID, MessageMode: store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, sameProjectAgent))

	// Construct payload with a different agentId than the convenience field.
	payloadJSON, _ := json.Marshal(MessageEventPayload{
		AgentID: "different-agent-id",
		Message: "message in payload",
	})

	reqBody := CreateScheduledEventRequest{
		EventType: "message",
		FireIn:    "30m",
		AgentID:   sameProjectAgent.ID, // convenience field: same-project agent
		Payload:   string(payloadJSON), // payload: different agent
	}
	rec := doRequest(t, srv, "POST", "/api/v1/projects/"+project.ID+"/scheduled-events", reqBody)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for conflicting targets, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestC1_RecurringScheduleCreate_CrossProjectDenied(t *testing.T) {
	// O2: recurring schedule create with a cross-project target must be denied.
	srv, s := testServer(t)
	ctx := context.Background()

	srv.scheduler = NewScheduler(s, slog.Default())
	srv.scheduler.RegisterEventHandler("message", srv.messageEventHandler())

	projectA := &store.Project{
		ID: tid("sched-create-a"), Name: "Project A", Slug: "sched-create-a",
	}
	require.NoError(t, s.CreateProject(ctx, projectA))

	projectB := &store.Project{
		ID: tid("sched-create-b"), Name: "Project B", Slug: "sched-create-b",
	}
	require.NoError(t, s.CreateProject(ctx, projectB))

	agent := &store.Agent{
		ID: tid("cross-sched-agent"), Name: "cross-agent", Slug: "cross-agent",
		ProjectID: projectB.ID, MessageMode: store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Create a recurring schedule in project A targeting agent in project B via payload.
	payloadJSON, _ := json.Marshal(MessageEventPayload{
		AgentID: agent.ID, Message: "cross-project msg",
	})
	reqBody := CreateScheduleRequest{
		Name:      "cross-project-sched",
		CronExpr:  "0 * * * *",
		EventType: "message",
		Payload:   string(payloadJSON),
	}
	rec := doRequest(t, srv, "POST", "/api/v1/projects/"+projectA.ID+"/schedules", reqBody)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"recurring create must deny cross-project target: %s", rec.Body.String())
}

func TestC1_RecurringScheduleUpdate_CrossProjectDenied(t *testing.T) {
	// O2/O3: recurring schedule update that changes the payload to target a
	// cross-project agent must be denied.
	srv, s := testServer(t)
	ctx := context.Background()

	srv.scheduler = NewScheduler(s, slog.Default())
	srv.scheduler.RegisterEventHandler("message", srv.messageEventHandler())

	projectA := &store.Project{
		ID: tid("sched-update-a"), Name: "Project A", Slug: "sched-update-a",
	}
	require.NoError(t, s.CreateProject(ctx, projectA))

	projectB := &store.Project{
		ID: tid("sched-update-b"), Name: "Project B", Slug: "sched-update-b",
	}
	require.NoError(t, s.CreateProject(ctx, projectB))

	crossAgent := &store.Agent{
		ID: tid("cross-update-agent"), Name: "cross-update-agent", Slug: "cross-update-agent",
		ProjectID: projectB.ID, MessageMode: store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, crossAgent))

	// First create a valid schedule in project A (target unresolvable at authoring
	// time, which is allowed — fire-time will catch it).
	createRec := doRequest(t, srv, "POST", "/api/v1/projects/"+projectA.ID+"/schedules",
		CreateScheduleRequest{
			Name:      "update-me",
			CronExpr:  "0 * * * *",
			EventType: "message",
			AgentName: "some-agent",
			Message:   "hello",
		})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())

	var sched store.Schedule
	require.NoError(t, json.NewDecoder(createRec.Body).Decode(&sched))

	// Now update the payload to target the cross-project agent.
	crossPayload, _ := json.Marshal(MessageEventPayload{
		AgentID: crossAgent.ID, Message: "cross-project update",
	})
	updateRec := doRequest(t, srv, "PATCH", "/api/v1/projects/"+projectA.ID+"/schedules/"+sched.ID,
		UpdateScheduleRequest{
			Payload: string(crossPayload),
		})
	assert.Equal(t, http.StatusForbidden, updateRec.Code,
		"recurring update must deny cross-project target: %s", updateRec.Body.String())
}

func TestC1_RecurringScheduleUpdate_ExistingPayloadDenied(t *testing.T) {
	// O3: When the existing schedule's payload targets a cross-project agent
	// (stored before containment) and the update changes only non-payload fields,
	// the authoring check must re-validate the existing payload and deny.
	srv, s := testServer(t)
	ctx := context.Background()

	srv.scheduler = NewScheduler(s, slog.Default())
	srv.scheduler.RegisterEventHandler("message", srv.messageEventHandler())

	projectA := &store.Project{
		ID: tid("sched-existing-a"), Name: "Project A", Slug: "sched-existing-a",
	}
	require.NoError(t, s.CreateProject(ctx, projectA))

	projectB := &store.Project{
		ID: tid("sched-existing-b"), Name: "Project B", Slug: "sched-existing-b",
	}
	require.NoError(t, s.CreateProject(ctx, projectB))

	crossAgent := &store.Agent{
		ID: tid("cross-existing-agent"), Name: "cross-existing", Slug: "cross-existing",
		ProjectID: projectB.ID, MessageMode: store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, crossAgent))

	// Directly insert a schedule with a cross-project payload (simulating
	// a record stored before containment was deployed).
	crossPayload, _ := json.Marshal(MessageEventPayload{
		AgentID: crossAgent.ID, Message: "pre-containment payload",
	})
	schedule := &store.Schedule{
		ID:        tid("legacy-cross-sched"),
		ProjectID: projectA.ID,
		Name:      "legacy-schedule",
		CronExpr:  "0 * * * *",
		EventType: "message",
		Payload:   string(crossPayload),
		Status:    store.ScheduleStatusActive,
		CreatedBy: "dev",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	require.NoError(t, s.CreateSchedule(ctx, schedule))

	// Update only the name — the existing payload should be re-validated.
	updateRec := doRequest(t, srv, "PATCH", "/api/v1/projects/"+projectA.ID+"/schedules/"+schedule.ID,
		UpdateScheduleRequest{Name: "updated-name"})
	assert.Equal(t, http.StatusForbidden, updateRec.Code,
		"update must re-validate existing cross-project payload: %s", updateRec.Body.String())
}

func TestC1_AuthorizeScheduledMessageFire_DirectUnit(t *testing.T) {
	// Direct unit tests of authorizeScheduledMessageFire covering edge cases.
	tests := []struct {
		name      string
		setup     func(ms *containmentMockStore)
		evt       store.ScheduledEvent
		agentID   string
		wantAllow bool
		wantErr   string // substring match
	}{
		{
			name: "empty_created_by",
			setup: func(ms *containmentMockStore) {
				ms.agents["a1"] = &store.Agent{
					ID: "a1", ProjectID: "p1", MessageMode: store.MessageModeProject,
				}
			},
			evt:       store.ScheduledEvent{ID: "e1", ProjectID: "p1", CreatedBy: ""},
			agentID:   "a1",
			wantAllow: false,
			wantErr:   "scheduled_message_no_creator",
		},
		{
			name: "cross_project_target",
			setup: func(ms *containmentMockStore) {
				ms.users["u1"] = &store.User{
					ID: "u1", Email: "u@t.com", Status: store.UserStatusActive, Role: "member",
				}
				ms.agents["a1"] = &store.Agent{
					ID: "a1", ProjectID: "p2", MessageMode: store.MessageModeProject,
				}
			},
			evt:       store.ScheduledEvent{ID: "e1", ProjectID: "p1", CreatedBy: "u1"},
			agentID:   "a1",
			wantAllow: false,
			wantErr:   "scheduled_message_cross_project",
		},
		{
			name: "creator_user_suspended",
			setup: func(ms *containmentMockStore) {
				ms.users["u1"] = &store.User{
					ID: "u1", Email: "u@t.com", Status: store.UserStatusSuspended, Role: "member",
				}
				ms.agents["a1"] = &store.Agent{
					ID: "a1", ProjectID: "p1", MessageMode: store.MessageModeProject,
				}
			},
			evt:       store.ScheduledEvent{ID: "e1", ProjectID: "p1", CreatedBy: "u1"},
			agentID:   "a1",
			wantAllow: false,
			wantErr:   "scheduled_message_creator_inactive",
		},
		{
			name: "creator_not_found",
			setup: func(ms *containmentMockStore) {
				ms.agents["a1"] = &store.Agent{
					ID: "a1", ProjectID: "p1", MessageMode: store.MessageModeProject,
				}
			},
			evt:       store.ScheduledEvent{ID: "e1", ProjectID: "p1", CreatedBy: "gone"},
			agentID:   "a1",
			wantAllow: false,
			wantErr:   "scheduled_message_creator_not_found",
		},
		{
			name: "creator_agent_soft_deleted",
			setup: func(ms *containmentMockStore) {
				deletedAt := time.Now()
				ms.agents["ca"] = &store.Agent{
					ID: "ca", ProjectID: "p1", DeletedAt: deletedAt,
					AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)},
				}
				ms.agents["a1"] = &store.Agent{
					ID: "a1", ProjectID: "p1", MessageMode: store.MessageModeProject,
				}
			},
			evt:       store.ScheduledEvent{ID: "e1", ProjectID: "p1", CreatedBy: "ca"},
			agentID:   "a1",
			wantAllow: false,
			wantErr:   "scheduled_message_creator_deleted",
		},
		{
			name: "creator_agent_cross_project",
			setup: func(ms *containmentMockStore) {
				ms.agents["ca"] = &store.Agent{
					ID: "ca", ProjectID: "p2",
					AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)},
				}
				ms.agents["a1"] = &store.Agent{
					ID: "a1", ProjectID: "p1", MessageMode: store.MessageModeProject,
				}
			},
			evt:       store.ScheduledEvent{ID: "e1", ProjectID: "p1", CreatedBy: "ca"},
			agentID:   "a1",
			wantAllow: false,
			wantErr:   "scheduled_message_creator_cross_project",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := newContainmentMockStore()
			tt.setup(ms)

			srv := containmentTestServer(ms)
			agent := ms.agents[tt.agentID]
			if agent == nil {
				t.Fatal("test setup error: target agent not found")
			}

			_, err := srv.authorizeScheduledMessageFire(context.Background(), tt.evt, agent)
			if tt.wantAllow {
				if err != nil {
					t.Errorf("expected allow, got error: %v", err)
				}
			} else {
				if err == nil {
					t.Error("expected denial, got allow")
				} else if tt.wantErr != "" {
					if got := err.Error(); !strings.Contains(got, tt.wantErr) {
						t.Errorf("error %q does not contain %q", got, tt.wantErr)
					}
				}
			}
		})
	}
}
