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

// DEF-157 tests: PromoteDM must write a derived external_ref via
// messaging.ThreadConversationExternalRef, not ''.

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// AC-157-2 — PromoteDM writes derived external_ref, asserted against both
// the function result AND a literal expected string (golden vector).
// ---------------------------------------------------------------------------

func TestPromoteDM_ExternalRef_DerivedKey(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, db := newPromoteTestStoreWithConversations(t)
	defer db.Close() //nolint:errcheck

	dmKey := "dm:agent:agent-ext:user:user-ext"

	// Seed a DM message.
	_, err := db.Exec(`
INSERT INTO messages (id, project_id, sender, sender_id, recipient, recipient_id, channel, thread_id, msg, created)
VALUES ('msg-ext-1', 'proj-ext', 'user:eve', 'user-ext', 'agent:bot', 'agent-ext', 'web', ?, 'hey', '2026-08-22T10:00:00Z')
`, dmKey)
	require.NoError(t, err)

	require.NoError(t, s.UpsertDM(ctx, WebChatDM{
		ConversationKey: dmKey,
		ParticipantID:   "user-ext",
		PeerID:          "agent-ext",
		PeerKind:        "agent",
	}))

	topicID := "promoted-extref-topic"
	projectID := "proj-ext"

	convID := uuid.New().String()
	now := time.Now().UTC().Truncate(time.Second)
	topic := WebChatTopic{
		ID:             topicID,
		ProjectID:      projectID,
		Name:           "ExtRef Thread",
		ConversationID: convID,
		CreatedBy:      "user-ext",
		CreatedAt:      now,
		LastActivityAt: now,
	}

	result, err := s.PromoteDM(ctx, topic, PromoteKeys{DMKey: dmKey})
	require.NoError(t, err)
	require.NotNil(t, result)

	// Read external_ref from the conversations row.
	var extRef string
	err = db.QueryRow("SELECT external_ref FROM conversations WHERE id = ?", convID).Scan(&extRef)
	require.NoError(t, err)

	// Assert against the function.
	expectedFromFunc, funcErr := messaging.ThreadConversationExternalRef(projectID, topicID)
	require.NoError(t, funcErr)
	require.Equal(t, expectedFromFunc, extRef,
		"external_ref must equal ThreadConversationExternalRef(projectID, topicID)")

	// Assert against a literal expected string (golden vector).
	const expectedLiteral = "thread:proj-ext:promoted-extref-topic"
	require.Equal(t, expectedLiteral, extRef,
		"external_ref must match the literal golden vector")
}

// ---------------------------------------------------------------------------
// AC-157-3 — Round trip: after promotion, resolving
// thread:<projectID>:<topicID> returns the SAME conversation ID the
// promoted topic points at.
// ---------------------------------------------------------------------------

func TestPromoteDM_ExternalRef_RoundTrip_NoShadow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, db := newPromoteTestStoreWithConversations(t)
	defer db.Close() //nolint:errcheck

	dmKey := "dm:agent:agent-rt:user:user-rt"

	// Seed a DM message.
	_, err := db.Exec(`
INSERT INTO messages (id, project_id, sender, sender_id, recipient, recipient_id, channel, thread_id, msg, created)
VALUES ('msg-rt-1', 'proj-rt', 'user:frank', 'user-rt', 'agent:helper', 'agent-rt', 'web', ?, 'round trip', '2026-08-22T10:00:00Z')
`, dmKey)
	require.NoError(t, err)

	require.NoError(t, s.UpsertDM(ctx, WebChatDM{
		ConversationKey: dmKey,
		ParticipantID:   "user-rt",
		PeerID:          "agent-rt",
		PeerKind:        "agent",
	}))

	topicID := "promoted-roundtrip-topic"
	projectID := "proj-rt"

	convID := uuid.New().String()
	now := time.Now().UTC().Truncate(time.Second)
	topic := WebChatTopic{
		ID:             topicID,
		ProjectID:      projectID,
		Name:           "RoundTrip Thread",
		ConversationID: convID,
		CreatedBy:      "user-rt",
		CreatedAt:      now,
		LastActivityAt: now,
	}

	result, err := s.PromoteDM(ctx, topic, PromoteKeys{DMKey: dmKey})
	require.NoError(t, err)
	require.NotNil(t, result)

	// Derive the key the same way the message backfill would.
	derivedKey, keyErr := messaging.ThreadConversationExternalRef(projectID, topicID)
	require.NoError(t, keyErr)

	// Resolve: look up the conversation by (surface, external_ref) — this is
	// exactly what the read path and the message backfill do.
	var resolvedConvID string
	err = db.QueryRow(
		`SELECT id FROM conversations WHERE surface = 'native' AND external_ref = ? AND deleted_at IS NULL`,
		derivedKey).Scan(&resolvedConvID)
	require.NoError(t, err, "resolving thread:%s:%s must find a conversation row", projectID, topicID)

	// The resolved conversation must be the SAME one the promoted topic points at.
	require.Equal(t, convID, resolvedConvID,
		"round trip failed: resolving thread:%s:%s returned %q, but the promoted topic points at %q — a shadow conversation exists",
		projectID, topicID, resolvedConvID, convID)

	// Confirm the topic also has the right conversation_id.
	topicConvID := getTopicConvID(t, db, topicID)
	require.Equal(t, convID, topicConvID,
		"promoted topic should carry the same conversation_id")
}

// ---------------------------------------------------------------------------
// AC-157-5 — The derivation error path: empty projectID or topicID must
// refuse, never fall back to ''.
// ---------------------------------------------------------------------------

func TestPromoteDM_ExternalRef_DerivationError_Refuses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, db := newPromoteTestStoreWithConversations(t)
	defer db.Close() //nolint:errcheck

	dmKey := "dm:agent:agent-de:user:user-de"

	// Seed DM data.
	_, err := db.Exec(`
INSERT INTO messages (id, project_id, sender, sender_id, recipient, recipient_id, channel, thread_id, msg, created)
VALUES ('msg-de-1', 'proj-de', 'user:grace', 'user-de', 'agent:bot', 'agent-de', 'web', ?, 'oops', '2026-08-22T10:00:00Z')
`, dmKey)
	require.NoError(t, err)

	require.NoError(t, s.UpsertDM(ctx, WebChatDM{
		ConversationKey: dmKey,
		ParticipantID:   "user-de",
		PeerID:          "agent-de",
		PeerKind:        "agent",
	}))

	now := time.Now().UTC().Truncate(time.Second)

	// Empty projectID — derivation must fail, PromoteDM must refuse.
	_, err = s.PromoteDM(ctx, WebChatTopic{
		ID:             "promoted-empty-proj",
		ProjectID:      "", // empty
		Name:           "Bad Topic",
		ConversationID: uuid.New().String(),
		CreatedBy:      "user-de",
		CreatedAt:      now,
		LastActivityAt: now,
	}, PromoteKeys{DMKey: dmKey})
	require.Error(t, err, "PromoteDM must refuse when projectID is empty")
	require.Contains(t, err.Error(), "derive conversation key",
		"error should mention derivation failure")

	// No conversation row should have been created with external_ref = ''.
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM conversations WHERE external_ref = ''").Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 0, count,
		"no conversation row with external_ref='' should exist after a derivation refusal")
}
