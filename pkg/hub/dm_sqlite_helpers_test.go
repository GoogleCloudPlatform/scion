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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// setupDMConversation creates a direct conversation between two agents with
// proper canonical DM key and participant rows.
func setupDMConversation(t *testing.T, s store.Store, agentAID, agentBID string) *store.Conversation {
	t.Helper()
	ctx := context.Background()

	extRef, err := messages.DMConversationKey("agent", agentAID, "agent", agentBID)
	require.NoError(t, err)

	now := time.Now().UTC()
	conv := &store.Conversation{
		ID:             api.NewUUID(),
		Kind:           "direct",
		Surface:        "native",
		ExternalRef:    extRef,
		DriftState:     "active",
		LastActivityAt: now,
		CreatedAt:      now,
		// ProjectID intentionally nil — DMs are global.
	}
	require.NoError(t, s.CreateConversation(ctx, conv))

	// Add both agents as participants (listing concern).
	addConvParticipant(t, s, conv.ID, "agent", agentAID)
	addConvParticipant(t, s, conv.ID, "agent", agentBID)

	return conv
}

// nullSpokeEventBus is a no-op EventBus used as a non-inprocess spoke in the
// FanOutEventBus so that ListChannels() returns "web" without triggering
// handler panics. It accepts nil handlers and discards published messages.
type nullSpokeEventBus struct{}

type nullSub struct{}

func (nullSpokeEventBus) Publish(context.Context, string, *messages.StructuredMessage) error {
	return nil
}
func (nullSpokeEventBus) Subscribe(string, eventbus.EventHandler) (eventbus.Subscription, error) {
	return nullSub{}, nil
}
func (nullSpokeEventBus) Close() error { return nil }

func (nullSub) Unsubscribe() error { return nil }
