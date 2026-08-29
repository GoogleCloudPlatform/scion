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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// spyEventPublisher embeds noopEventPublisher and records PublishUserMessage
// and PublishChatNotification calls.
type spyEventPublisher struct {
	noopEventPublisher
	mu          sync.Mutex
	userMsgs    []*store.Message
	chatNotifCh chan struct{} // optional; signalled on PublishChatNotification
}

func (s *spyEventPublisher) PublishUserMessage(_ context.Context, msg *store.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.userMsgs = append(s.userMsgs, msg)
}

func (s *spyEventPublisher) getUserMessages() []*store.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*store.Message, len(s.userMsgs))
	copy(out, s.userMsgs)
	return out
}

func (s *spyEventPublisher) PublishChatNotification(_ context.Context, _ *store.Notification, _ ChatMessageContext) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.chatNotifCh != nil {
		select {
		case s.chatNotifCh <- struct{}{}:
		default:
		}
	}
}

// chatNotifFired reports whether PublishChatNotification was invoked.
// Safe to call only when no concurrent goroutine can still call
// PublishChatNotification (e.g. after the goroutine has been joined
// or was never spawned).
func (s *spyEventPublisher) chatNotifFired() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.chatNotifCh == nil {
		return false
	}
	select {
	case <-s.chatNotifCh:
		return true
	default:
		return false
	}
}

// stubWebChatStore embeds the WebChatStore interface so that only the
// methods actually exercised need a real implementation. Unimplemented
// methods panic with a nil-receiver dereference, which is the desired
// signal in a test.
type stubWebChatStore struct {
	WebChatStore
}

func (s *stubWebChatStore) IsConversationMuted(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}

// createMessageFailStore wraps a real store and makes CreateMessage return an error.
type createMessageFailStore struct {
	store.Store
}

func (s *createMessageFailStore) CreateMessage(_ context.Context, _ *store.Message) error {
	return errors.New("injected CreateMessage failure")
}

// ---------------------------------------------------------------------------
// Site 1: messagebroker.go  deliverToUser
// ---------------------------------------------------------------------------

func TestDeliverToUser_SkipsPublishOnPersistFailure(t *testing.T) {
	realStore := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, realStore)
	setupBrokerTestAgent(t, realStore, projectID, "agent-a", "running")

	spy := &spyEventPublisher{}
	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()

	failStore := &createMessageFailStore{Store: realStore}
	proxy := NewMessageBrokerProxy(b, failStore, spy, func() AgentDispatcher { return nil }, slog.Default())

	msg := messages.NewInstruction("agent:agent-a", "user:bob", "hello")
	msg.SenderID = "agent-uuid"
	msg.RecipientID = "user-bob-id"

	proxy.deliverToUser(context.Background(), projectID, "user.user-bob-id.message", msg)

	if msgs := spy.getUserMessages(); len(msgs) != 0 {
		t.Errorf("expected no PublishUserMessage calls when persistence fails, got %d", len(msgs))
	}
}

func TestDeliverToUser_PublishesOnPersistSuccess(t *testing.T) {
	realStore := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, realStore)
	setupBrokerTestAgent(t, realStore, projectID, "agent-a", "running")

	spy := &spyEventPublisher{}
	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()

	proxy := NewMessageBrokerProxy(b, realStore, spy, func() AgentDispatcher { return nil }, slog.Default())

	msg := messages.NewInstruction("agent:agent-a", "user:bob", "hello")
	msg.SenderID = "agent-uuid"
	msg.RecipientID = "user-bob-id"

	proxy.deliverToUser(context.Background(), projectID, "user.user-bob-id.message", msg)

	if msgs := spy.getUserMessages(); len(msgs) != 1 {
		t.Errorf("expected 1 PublishUserMessage call when persistence succeeds, got %d", len(msgs))
	}
}

// ---------------------------------------------------------------------------
// Site 2: handleAgentMessage
// ---------------------------------------------------------------------------

func TestHandleAgentMessage_SkipsPublishOnPersistFailure(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "guard-project",
		Slug:       "guard-project",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	agent := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "guard-agent",
		Slug:       "guard-agent",
		ProjectID:  project.ID,
		Phase:      "running",
		Runtime:    "managed",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	spy := &spyEventPublisher{}
	srv.events = spy
	srv.store = &createMessageFailStore{Store: s}

	structuredMsg := &messages.StructuredMessage{
		Sender:    "user:tester",
		SenderID:  "user-id-1",
		Recipient: "agent:" + agent.Slug,
		Msg:       "test message",
		Type:      messages.TypeInstruction,
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	body, _ := json.Marshal(MessageRequest{StructuredMessage: structuredMsg})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(),
		NewAuthenticatedUser("user-id-1", "tester@example.com", "Tester", "user", "web")))

	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, req, agent.ID)

	// The handler should still return 200 (B10: non-fatal) but publish must not fire.
	if msgs := spy.getUserMessages(); len(msgs) != 0 {
		t.Errorf("expected no PublishUserMessage calls when persistence fails, got %d", len(msgs))
	}
}

func TestHandleAgentMessage_ResponseStatusNotDeliveredOnPersistFailure(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "status-project",
		Slug:       "status-project",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	agent := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "status-agent",
		Slug:       "status-agent",
		ProjectID:  project.ID,
		Phase:      "running",
		Runtime:    "managed",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	spy := &spyEventPublisher{}
	srv.events = spy
	srv.store = &createMessageFailStore{Store: s}

	structuredMsg := &messages.StructuredMessage{
		Sender:    "user:tester",
		SenderID:  "user-id-1",
		Recipient: "agent:" + agent.Slug,
		Msg:       "test message",
		Type:      messages.TypeInstruction,
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	body, _ := json.Marshal(MessageRequest{StructuredMessage: structuredMsg})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(),
		NewAuthenticatedUser("user-id-1", "tester@example.com", "Tester", "user", "web")))

	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, req, agent.ID)

	// The handler should return 200 but the status must NOT be "delivered".
	if rr.Code != http.StatusOK {
		t.Logf("response: %d %s", rr.Code, rr.Body.String())
		// 503 (no dispatcher) or other infrastructure errors are acceptable,
		// but if we got here with a managed runtime, it should be 200.
	}
	if rr.Code == http.StatusOK {
		var resp MessageDeliveryResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp.Status == "delivered" {
			t.Errorf("response Status should not be 'delivered' when persistence fails, got %q", resp.Status)
		}
		if resp.MessageID != "" {
			t.Errorf("response MessageID should be empty when persistence fails, got %q", resp.MessageID)
		}
	}
}

func TestHandleAgentMessage_PublishesOnPersistSuccess(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "ok-project",
		Slug:       "ok-project",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	agent := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "ok-agent",
		Slug:       "ok-agent",
		ProjectID:  project.ID,
		Phase:      "running",
		Runtime:    "managed",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	spy := &spyEventPublisher{}
	srv.events = spy
	// Keep using real store so persistence succeeds.

	structuredMsg := &messages.StructuredMessage{
		Sender:    "user:tester",
		SenderID:  "user-id-1",
		Recipient: "agent:" + agent.Slug,
		Msg:       "test message",
		Type:      messages.TypeInstruction,
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	body, _ := json.Marshal(MessageRequest{StructuredMessage: structuredMsg})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(),
		NewAuthenticatedUser("user-id-1", "tester@example.com", "Tester", "user", "web")))

	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, req, agent.ID)

	if msgs := spy.getUserMessages(); len(msgs) != 1 {
		t.Errorf("expected 1 PublishUserMessage call when persistence succeeds, got %d", len(msgs))
	}
}

// ---------------------------------------------------------------------------
// Sites 3 & 4: handleGroupMessage (agent + user recipients)
// ---------------------------------------------------------------------------

func TestHandleGroupMessage_SkipsPublishOnPersistFailure(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "group-project",
		Slug:       "group-project",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	// Create a user recipient.
	user := &store.User{
		ID:          api.NewUUID(),
		Email:       "groupuser@example.com",
		DisplayName: "GroupUser",
	}
	if err := s.CreateUser(ctx, user); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Create an anchor agent and a target agent.
	anchor := &store.Agent{
		ID:              api.NewUUID(),
		Name:            "anchor",
		Slug:            "anchor",
		ProjectID:       project.ID,
		Phase:           "running",
		RuntimeBrokerID: "broker-1",
		Visibility:      store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, anchor); err != nil {
		t.Fatalf("CreateAgent (anchor): %v", err)
	}
	target := &store.Agent{
		ID:              api.NewUUID(),
		Name:            "target",
		Slug:            "target",
		ProjectID:       project.ID,
		Phase:           "running",
		RuntimeBrokerID: "broker-1",
		Visibility:      store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, target); err != nil {
		t.Fatalf("CreateAgent (target): %v", err)
	}

	spy := &spyEventPublisher{}
	srv.events = spy
	srv.store = &createMessageFailStore{Store: s}

	// Message to group[agent:target,user:groupuser@example.com]
	structuredMsg := &messages.StructuredMessage{
		Sender:    "user:tester",
		SenderID:  "user-id-1",
		Recipient: "group[agent:target,user:groupuser@example.com]",
		Msg:       "group message",
		Type:      messages.TypeInstruction,
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	body, _ := json.Marshal(MessageRequest{StructuredMessage: structuredMsg})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+anchor.ID+"/message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(),
		NewAuthenticatedUser("user-id-1", "tester@example.com", "Tester", "user", "web")))

	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, req, anchor.ID)

	t.Logf("group response: %d %s", rr.Code, rr.Body.String())

	// Neither agent nor user recipient should have had PublishUserMessage called.
	if msgs := spy.getUserMessages(); len(msgs) != 0 {
		t.Errorf("expected no PublishUserMessage calls when persistence fails for group message, got %d", len(msgs))
	}
}

func TestHandleGroupMessage_PublishesOnPersistSuccess(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "group-ok-project",
		Slug:       "group-ok-project",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	// Create a user recipient.
	user := &store.User{
		ID:          api.NewUUID(),
		Email:       "groupuser2@example.com",
		DisplayName: "GroupUser2",
	}
	if err := s.CreateUser(ctx, user); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	anchor := &store.Agent{
		ID:              api.NewUUID(),
		Name:            "anchor2",
		Slug:            "anchor2",
		ProjectID:       project.ID,
		Phase:           "running",
		RuntimeBrokerID: "broker-1",
		Visibility:      store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, anchor); err != nil {
		t.Fatalf("CreateAgent (anchor2): %v", err)
	}
	target := &store.Agent{
		ID:              api.NewUUID(),
		Name:            "target2",
		Slug:            "target2",
		ProjectID:       project.ID,
		Phase:           "running",
		RuntimeBrokerID: "broker-1",
		Visibility:      store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, target); err != nil {
		t.Fatalf("CreateAgent (target2): %v", err)
	}

	spy := &spyEventPublisher{}
	srv.events = spy
	// Keep real store for success path.

	structuredMsg := &messages.StructuredMessage{
		Sender:    "user:tester",
		SenderID:  "user-id-1",
		Recipient: "group[agent:target2,user:groupuser2@example.com]",
		Msg:       "group message ok",
		Type:      messages.TypeInstruction,
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	body, _ := json.Marshal(MessageRequest{StructuredMessage: structuredMsg})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+anchor.ID+"/message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Phase 3 msg-authz: use admin role so the super-admin bypass in
	// authorizeAgentMessage passes — this test validates publish-on-persist,
	// not message authorization.
	req = req.WithContext(contextWithIdentity(req.Context(),
		NewAuthenticatedUser("user-id-1", "tester@example.com", "Tester", "admin", "web")))

	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, req, anchor.ID)

	t.Logf("group ok response: %d %s", rr.Code, rr.Body.String())

	// Both agent and user recipient should have PublishUserMessage called (2 total).
	if msgs := spy.getUserMessages(); len(msgs) != 2 {
		t.Errorf("expected 2 PublishUserMessage calls when persistence succeeds for group message, got %d", len(msgs))
	}
}

// ---------------------------------------------------------------------------
// Site 5: processMentions
// ---------------------------------------------------------------------------

func TestProcessMentions_SkipsPublishOnPersistFailure(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "mention-project",
		Slug:       "mention-project",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	primary := &store.Agent{
		ID:              api.NewUUID(),
		Name:            "primary",
		Slug:            "primary",
		ProjectID:       project.ID,
		Phase:           "running",
		RuntimeBrokerID: "broker-1",
		Visibility:      store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, primary); err != nil {
		t.Fatalf("CreateAgent (primary): %v", err)
	}

	mentioned := &store.Agent{
		ID:              api.NewUUID(),
		Name:            "mentioned",
		Slug:            "mentioned",
		ProjectID:       project.ID,
		Phase:           "running",
		RuntimeBrokerID: "broker-1",
		Visibility:      store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, mentioned); err != nil {
		t.Fatalf("CreateAgent (mentioned): %v", err)
	}

	spy := &spyEventPublisher{}
	srv.events = spy
	srv.store = &createMessageFailStore{Store: s}

	originalMsg := &messages.StructuredMessage{
		Sender:    "user:tester",
		SenderID:  "user-id-1",
		Recipient: "agent:" + primary.Slug,
		Msg:       "hello @mentioned",
		Type:      messages.TypeInstruction,
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	// Phase 3 msg-authz: inject admin identity so authorizeAgentMessage
	// passes — this test validates publish-on-persist, not authorization.
	ctx = contextWithIdentity(ctx,
		NewAuthenticatedUser("user-id-1", "tester@example.com", "Tester", "admin", "web"))

	results := srv.processMentions(ctx, []string{"mentioned"}, primary, originalMsg)

	// The mention should still produce a result (dispatch may fail, but that's OK).
	t.Logf("mention results: %+v", results)

	// After C3 dual-write changes, SSE publish fires regardless of
	// The publish must NOT have fired because CreateMessage failed.
	if msgs := spy.getUserMessages(); len(msgs) != 0 {
		t.Errorf("expected no PublishUserMessage calls when persistence fails for mention, got %d", len(msgs))
	}
}

func TestProcessMentions_PublishesOnPersistSuccess(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "mention-ok-project",
		Slug:       "mention-ok-project",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	primary := &store.Agent{
		ID:              api.NewUUID(),
		Name:            "primary2",
		Slug:            "primary2",
		ProjectID:       project.ID,
		Phase:           "running",
		RuntimeBrokerID: "broker-1",
		Visibility:      store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, primary); err != nil {
		t.Fatalf("CreateAgent (primary2): %v", err)
	}

	mentioned := &store.Agent{
		ID:              api.NewUUID(),
		Name:            "mentioned2",
		Slug:            "mentioned2",
		ProjectID:       project.ID,
		Phase:           "running",
		RuntimeBrokerID: "broker-1",
		Visibility:      store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, mentioned); err != nil {
		t.Fatalf("CreateAgent (mentioned2): %v", err)
	}

	spy := &spyEventPublisher{}
	srv.events = spy
	// Real store => persistence succeeds.

	originalMsg := &messages.StructuredMessage{
		Sender:    "user:tester",
		SenderID:  "user-id-1",
		Recipient: "agent:" + primary.Slug,
		Msg:       "hello @mentioned2",
		Type:      messages.TypeInstruction,
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	// Phase 3 msg-authz: inject admin identity so authorizeAgentMessage
	// passes — this test validates publish-on-persist, not authorization.
	ctx = contextWithIdentity(ctx,
		NewAuthenticatedUser("user-id-1", "tester@example.com", "Tester", "admin", "web"))

	results := srv.processMentions(ctx, []string{"mentioned2"}, primary, originalMsg)
	t.Logf("mention ok results: %+v", results)

	// The publish MUST have fired because CreateMessage succeeded.
	if msgs := spy.getUserMessages(); len(msgs) != 1 {
		t.Errorf("expected 1 PublishUserMessage call when persistence succeeds for mention, got %d", len(msgs))
	}
}

// ---------------------------------------------------------------------------
// Site 6: deliverToUser — W6 DM notification (NotifyDMReceived)
// ---------------------------------------------------------------------------

// TestDeliverToUser_SkipsNotifyOnPersistFailure verifies that when
// CreateMessage fails, the W6 NotifyDMReceived goroutine is never
// spawned — and therefore PublishChatNotification is never called.
//
// Determinism: with the early-return fix, deliverToUser returns before
// reaching the "go p.chatNotifier.NotifyDMReceived(...)" line. No
// goroutine is launched, so there is no race to synchronise on; the
// observation (zero PublishChatNotification calls) is checked after
// deliverToUser returns synchronously.
func TestDeliverToUser_SkipsNotifyOnPersistFailure(t *testing.T) {
	realStore := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, realStore)
	setupBrokerTestAgent(t, realStore, projectID, "agent-a", "running")

	spy := &spyEventPublisher{chatNotifCh: make(chan struct{}, 1)}
	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()

	failStore := &createMessageFailStore{Store: realStore}
	proxy := NewMessageBrokerProxy(b, failStore, spy, func() AgentDispatcher { return nil }, slog.Default())

	// Wire a ChatNotifier so the guard (p.chatNotifier != nil) would pass
	// if execution ever reached it. The notifier uses the real store and a
	// stub WebChatStore — but neither is exercised because the function
	// returns before the notification path.
	cn := NewChatNotifier(realStore, spy, &stubWebChatStore{}, nil, slog.Default())
	proxy.chatNotifier = cn

	// Craft a message that satisfies all four W6 guard conditions:
	//   ThreadID starts with "dm:", RecipientID non-empty,
	//   Sender starts with "agent:".
	msg := messages.NewInstruction("agent:agent-a", "user:bob", "hello")
	msg.SenderID = "agent-uuid"
	msg.RecipientID = "user-bob-id"
	msg.ThreadID = "dm:user:user-bob-id:agent:agent-uuid"

	proxy.deliverToUser(context.Background(), projectID, "user.user-bob-id.message", msg)

	// The goroutine was never spawned, so the channel must be empty.
	if spy.chatNotifFired() {
		t.Error("PublishChatNotification must not be called when CreateMessage fails")
	}
}

// TestDeliverToUser_NotifiesOnPersistSuccess is a positive control:
// when CreateMessage succeeds and the W6 conditions are met,
// PublishChatNotification IS called (via the NotifyDMReceived goroutine).
//
// Determinism: NotifyDMReceived runs in a goroutine. We wait on a
// buffered channel that the spy signals from PublishChatNotification,
// with a generous timeout so CI is not flaky.
func TestDeliverToUser_NotifiesOnPersistSuccess(t *testing.T) {
	realStore := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, realStore)
	setupBrokerTestAgent(t, realStore, projectID, "agent-a", "running")

	spy := &spyEventPublisher{chatNotifCh: make(chan struct{}, 1)}
	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()

	proxy := NewMessageBrokerProxy(b, realStore, spy, func() AgentDispatcher { return nil }, slog.Default())

	cn := NewChatNotifier(realStore, spy, &stubWebChatStore{}, nil, slog.Default())
	proxy.chatNotifier = cn

	msg := messages.NewInstruction("agent:agent-a", "user:bob", "hello")
	msg.SenderID = "agent-uuid"
	msg.RecipientID = "user-bob-id"
	msg.ThreadID = "dm:user:user-bob-id:agent:agent-uuid"

	proxy.deliverToUser(context.Background(), projectID, "user.user-bob-id.message", msg)

	// Wait for the goroutine to signal PublishChatNotification.
	select {
	case <-spy.chatNotifCh:
		// success — notification was delivered
	case <-time.After(5 * time.Second):
		t.Error("PublishChatNotification was not called within timeout; expected W6 DM notification on persist success")
	}
}
