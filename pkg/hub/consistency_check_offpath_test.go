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
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// lookupHoldingStore holds the consistency check's lookups (the thread or
// sender/recipient reads that skip the total count) until release is closed.
// It ignores the context on purpose: a caller that waited for the check
// would wait until the test releases it.
type lookupHoldingStore struct {
	store.Store
	enteredOnce sync.Once
	entered     chan struct{}
	release     chan struct{}
}

func (s *lookupHoldingStore) ListMessages(ctx context.Context, f store.MessageFilter, o store.ListOptions) (*store.ListResult[store.Message], error) {
	if o.SkipTotalCount && (f.ThreadID != "" || (f.SenderID != "" && f.RecipientID != "")) {
		s.enteredOnce.Do(func() { close(s.entered) })
		<-s.release
	}
	return s.Store.ListMessages(ctx, f, o)
}

// TestConsistencyCheck_DeliveryDoesNotWaitForLookup pins that the message
// path hands the consistency check to the background runner: delivery
// completes, and the message is stored, while the check's lookup is still
// held.
func TestConsistencyCheck_DeliveryDoesNotWaitForLookup(t *testing.T) {
	inner := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, inner)
	ctx := context.Background()

	agentID := api.NewUUID()
	require.NoError(t, inner.CreateAgent(ctx, &store.Agent{
		ID:        agentID,
		Name:      "cc-offpath-agent",
		Slug:      "cc-offpath-agent",
		ProjectID: projectID,
		Phase:     "running",
	}))
	recipientID := api.NewUUID()
	require.NoError(t, inner.CreateUser(ctx, &store.User{
		ID:          recipientID,
		Email:       "cc-offpath@example.com",
		DisplayName: "CC Offpath User",
	}))
	conv, err := inner.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: "dm:agent:" + agentID + ":user:" + recipientID,
		DriftState:  "active",
	})
	require.NoError(t, err)

	held := &lookupHoldingStore{Store: inner, entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(held.release) }) }
	// Registered after the store's own cleanup, so it runs first: the held
	// check finishes before the store closes.
	t.Cleanup(func() {
		release()
		messaging.ConsistencyCheckRunner.Wait()
	})

	events := NewChannelEventPublisher()
	defer events.Close()
	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()
	proxy := NewMessageBrokerProxy(b, held, events, func() AgentDispatcher { return &brokerMockDispatcher{} }, slog.Default())

	msg := messages.NewInstruction("agent:cc-offpath-agent", "user:cc-offpath@example.com", "off the path")
	msg.SenderID = agentID
	msg.RecipientID = recipientID
	msg.ConversationID = conv.ID

	delivered := make(chan struct{})
	go func() {
		defer close(delivered)
		proxy.deliverToUser(ctx, projectID, "project."+projectID+".user.message", msg)
	}()
	select {
	case <-delivered:
	case <-time.After(10 * time.Second):
		t.Fatal("deliverToUser did not return while the consistency lookup was held")
	}
	select {
	case <-held.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the consistency check never started its lookup")
	}

	stored, err := inner.ListMessages(ctx, store.MessageFilter{ConversationID: conv.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, stored.Items, 1, "the message is stored before the held check finishes")

	release()
	messaging.ConsistencyCheckRunner.Wait()
}
