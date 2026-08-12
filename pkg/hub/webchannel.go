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
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
)

// webChannelBus is a real broker spoke for the "web" channel. It follows the
// same contract every plugin adapter follows:
//
//   - Subscribe — records the pattern and DISCARDS the handler. Inbound web
//     messages arrive via the hub's own HTTP handlers, exactly as plugin
//     inbound arrives via POST /api/v1/broker/inbound. Installing a real
//     handler here would double-persist and double-stream every message,
//     because FanOut hands the SAME handler to every spoke (F7/F8, #944).
//
//   - Publish — does real work: owns webchat_* state. It does NOT persist the
//     message and does NOT emit the core SSE frame; deliverToUser already
//     does both on the inprocess path. One persistence path remains the
//     invariant.
//
//   - Close — releases the store handle. Returns nil.
//
// Registered as Observer: true, so a webchat_* write failure degrades the
// rail rather than failing the user's message.
type webChannelBus struct {
	log   *slog.Logger
	store WebChatStore
}

// NewWebChannelBus creates a web channel spoke backed by the given store.
func NewWebChannelBus(log *slog.Logger, store WebChatStore) eventbus.EventBus {
	if log == nil {
		log = slog.Default()
	}
	if store == nil {
		panic("webchannel: store cannot be nil")
	}
	return &webChannelBus{
		log:   log,
		store: store,
	}
}

// Publish does real work — updates webchat_* state. It does NOT persist
// the canonical message row or emit the core SSE frame (deliverToUser
// handles both on the inprocess path).
func (b *webChannelBus) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	if msg == nil || msg.ObserverOnly {
		return nil
	}

	userID, projectID, agentID, ok := identityFromTopic(topic, msg)
	if !ok {
		// Not a conversation-scoped message; nothing to record.
		return nil
	}

	now := time.Now().UTC()

	// 1. Thread watermark — this is what makes the Phase 5 rail endpoint
	//    a single indexed read instead of an aggregate query.
	//
	// messageID is "" because StructuredMessage has no ID field — the
	// store-assigned ID is only available after deliverToUser persists the
	// message on the inprocess spoke, which runs concurrently. Phase 5
	// will address this gap (design §5.3).
	if err := b.store.TouchThread(ctx, userID, projectID, agentID, "", now); err != nil {
		b.log.Error("Failed to update thread watermark",
			"user_id", userID, "project_id", projectID, "agent_id", agentID, "error", err)
		return err
	}

	// 2. Reply affinity as a row, not a config flag.
	if err := b.store.RecordChannel(ctx, userID, projectID, agentID, "web", now); err != nil {
		b.log.Error("Failed to record conversation context",
			"user_id", userID, "project_id", projectID, "agent_id", agentID, "error", err)
		return err
	}

	return nil
}

// Subscribe DISCARDS the handler. This is not a shortcut — it is the same
// contract every plugin adapter follows (F7). A real handler causes
// double-persist plus double-SSE (F8, issue #944). Unlike plugin adapters
// that track activeSubs for reconnect replay, the web spoke has no
// reconnect path, so patterns are not retained.
func (b *webChannelBus) Subscribe(_ string, _ eventbus.EventHandler) (eventbus.Subscription, error) {
	return webNoopSubscription{}, nil
}

// Close releases resources. The store handle is owned by the caller.
func (b *webChannelBus) Close() error {
	return nil
}

// webNoopSubscription is a subscription that does nothing on unsubscribe.
type webNoopSubscription struct{}

func (webNoopSubscription) Unsubscribe() error { return nil }

// identityFromTopic extracts the user, project, and agent IDs from the
// topic and message. Returns false if the message is not a conversation-
// scoped user message (e.g., broadcasts, global messages).
func identityFromTopic(topic string, msg *messages.StructuredMessage) (userID, projectID, agentID string, ok bool) {
	if msg == nil {
		return "", "", "", false
	}
	parsed, err := projectcompat.ParseTopic(topic)
	if err != nil {
		return "", "", "", false
	}

	projectID = parsed.ProjectID

	switch parsed.Kind {
	case projectcompat.TopicKindUser:
		// Agent → user message: topic has the user ID, sender is the agent.
		userID = parsed.Actor
		if strings.HasPrefix(msg.Sender, "agent:") {
			agentID = msg.SenderID
			if agentID == "" {
				agentID = strings.TrimPrefix(msg.Sender, "agent:")
			}
		}
	case projectcompat.TopicKindAgent:
		// User → agent message: topic has the agent slug, recipient is the agent.
		// NOTE(O1): agentID here is the slug from the topic, whereas for
		// TopicKindUser above it is msg.SenderID (a UUID). This inconsistency
		// must be normalized to a single identifier form before Phase 6 to
		// avoid duplicate webchat_thread rows for the same conversation. See
		// review nc-p1-rev-2 O1.
		agentID = parsed.Actor
		if strings.HasPrefix(msg.Sender, "user:") {
			userID = msg.SenderID
		}
	default:
		// Broadcast or other — not conversation-scoped.
		return "", "", "", false
	}

	if userID == "" || projectID == "" || agentID == "" {
		return "", "", "", false
	}

	return userID, projectID, agentID, true
}
