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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestCreateInboxMessage_StampsConversationID verifies that when the subscriber
// has a valid UUID SubscriberID, the persisted inbox message gets a non-empty
// ConversationID stamped via DM conversation resolution.
func TestCreateInboxMessage_StampsConversationID(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   api.NewUUID(),
		Name: "conv-stamp-project",
		Slug: "conv-stamp-project",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	agent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "conv-agent",
		Slug:      "conv-agent",
		ProjectID: project.ID,
		Phase:     "running",
		Runtime:   "managed",
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	user := &store.User{
		ID:          api.NewUUID(),
		Email:       "convuser@example.com",
		DisplayName: "ConvUser",
	}
	if err := s.CreateUser(ctx, user); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	spy := &spyEventPublisher{}
	nd := NewNotificationDispatcher(s, spy, func() AgentDispatcher { return nil }, slog.Default())

	sub := &store.NotificationSubscription{
		ID:             api.NewUUID(),
		Scope:          store.SubscriptionScopeAgent,
		AgentID:        agent.ID,
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   user.ID, // valid UUID
		ProjectID:      project.ID,
	}

	notif := &store.Notification{
		ID:             api.NewUUID(),
		SubscriptionID: sub.ID,
		AgentID:        agent.ID,
		ProjectID:      project.ID,
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   user.ID,
		Status:         "WAITING_FOR_INPUT",
		Message:        "Agent needs input",
	}

	nd.createInboxMessage(ctx, sub, notif, agent)

	// Retrieve the persisted message via the spy (which records PublishUserMessage calls).
	msgs := spy.getUserMessages()
	if len(msgs) == 0 {
		t.Fatal("expected at least one published user message, got none")
	}

	last := msgs[len(msgs)-1]
	if last.ConversationID == "" {
		t.Errorf("expected non-empty ConversationID for UUID subscriber, got empty")
	}
}

// TestCreateInboxMessage_StampsDispatchStateDispatched is a regression test
// for nc-promote-busy round 2 (R2): createInboxMessage persists an
// agent-to-user inbox notification, and that persist is itself the
// delivery. Leaving DispatchState unset falls through to the Ent schema's
// "pending" default and is never transitioned — the same omission fixed for
// deliverToUser and deliveryUserDirect — which would put the row on the
// sweep-then-purge path and silently delete it after 7 days.
func TestCreateInboxMessage_StampsDispatchStateDispatched(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   api.NewUUID(),
		Name: "dispatch-stamp-project",
		Slug: "dispatch-stamp-project",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	agent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "dispatch-agent",
		Slug:      "dispatch-agent",
		ProjectID: project.ID,
		Phase:     "running",
		Runtime:   "managed",
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	user := &store.User{
		ID:          api.NewUUID(),
		Email:       "dispatchuser@example.com",
		DisplayName: "DispatchUser",
	}
	if err := s.CreateUser(ctx, user); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	spy := &spyEventPublisher{}
	nd := NewNotificationDispatcher(s, spy, func() AgentDispatcher { return nil }, slog.Default())

	sub := &store.NotificationSubscription{
		ID:             api.NewUUID(),
		Scope:          store.SubscriptionScopeAgent,
		AgentID:        agent.ID,
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   user.ID,
		ProjectID:      project.ID,
	}

	notif := &store.Notification{
		ID:             api.NewUUID(),
		SubscriptionID: sub.ID,
		AgentID:        agent.ID,
		ProjectID:      project.ID,
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   user.ID,
		Status:         "WAITING_FOR_INPUT",
		Message:        "Agent needs input",
	}

	nd.createInboxMessage(ctx, sub, notif, agent)

	msgs := spy.getUserMessages()
	if len(msgs) == 0 {
		t.Fatal("expected at least one published user message, got none")
	}
	last := msgs[len(msgs)-1]
	if last.DispatchState != store.MessageDispatchDispatched {
		t.Fatalf("expected DispatchState %q, got %q — an unset value defaults "+
			"to \"pending\" and is never transitioned, so the sweep would "+
			"eventually delete this notification", store.MessageDispatchDispatched, last.DispatchState)
	}
}

// TestCreateInboxMessage_NonUUIDSubscriber_NoStampNoPanic verifies that when
// the subscriber has a non-UUID SubscriberID (e.g. a slug or federated identity),
// createInboxMessage does not panic and the persisted message has an empty
// ConversationID.
//
// DEF-32: when federated-identity → user UUID resolution is added to the store,
// this expectation inverts — a non-UUID subscriber SHOULD resolve to a canonical
// UUID and receive a ConversationID. Update this assertion when DEF-32 lands.
func TestCreateInboxMessage_NonUUIDSubscriber_NoStampNoPanic(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   api.NewUUID(),
		Name: "conv-noid-project",
		Slug: "conv-noid-project",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	agent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "conv-noid-agent",
		Slug:      "conv-noid-agent",
		ProjectID: project.ID,
		Phase:     "running",
		Runtime:   "managed",
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	spy := &spyEventPublisher{}
	nd := NewNotificationDispatcher(s, spy, func() AgentDispatcher { return nil }, slog.Default())

	sub := &store.NotificationSubscription{
		ID:             api.NewUUID(),
		Scope:          store.SubscriptionScopeAgent,
		AgentID:        agent.ID,
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   "some-user-slug", // NOT a UUID
		ProjectID:      project.ID,
	}

	notif := &store.Notification{
		ID:             api.NewUUID(),
		SubscriptionID: sub.ID,
		AgentID:        agent.ID,
		ProjectID:      project.ID,
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   "some-user-slug",
		Status:         "WAITING_FOR_INPUT",
		Message:        "Agent needs input",
	}

	// Must not panic.
	nd.createInboxMessage(ctx, sub, notif, agent)

	msgs := spy.getUserMessages()
	if len(msgs) == 0 {
		t.Fatal("expected at least one published user message, got none")
	}

	last := msgs[len(msgs)-1]
	// DEF-32: this assertion inverts when federated-identity resolution lands.
	if last.ConversationID != "" {
		t.Errorf("expected empty ConversationID for non-UUID subscriber, got %q", last.ConversationID)
	}
}
