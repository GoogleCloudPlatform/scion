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
	"fmt"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/internal/hubtestutil"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// TestPromoteDM_ReallocatesConversationSeqWithoutCollision_Postgres verifies
// pgWebChatStore.PromoteDM on a real Ent Postgres schema: promoting a DM into a
// new topic conversation re-sequences conversation_seq in (created, id) order
// and advances conversations.last_message_seq so subsequent CreateMessage calls
// allocate seq N+1 cleanly. Self-skips unless SCION_TEST_POSTGRES_DSN is set.
func TestPromoteDM_ReallocatesConversationSeqWithoutCollision_Postgres(t *testing.T) {
	dsn := hubtestutil.RequirePostgresDSN(t)
	ctx := context.Background()
	schema := fmt.Sprintf("promote_seq_test_%d", time.Now().UnixNano())

	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.ExecContext(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec("DROP SCHEMA " + schema + " CASCADE") })

	drv := entsql.OpenDB(dialect.Postgres, db)
	client := ent.NewClient(ent.Driver(drv))
	require.NoError(t, entc.AutoMigrate(ctx, client))

	cs := entadapter.NewCompositeStore(client)
	wcs := NewWebChatStore(db, "postgres")
	require.NoError(t, wcs.Init())

	projID := uuid.NewString()
	userID := uuid.NewString()
	agentID := uuid.NewString()
	extRef, err := messages.DMConversationKey("user", userID, "agent", agentID)
	require.NoError(t, err)

	now := time.Now().UTC().Truncate(time.Microsecond)
	dm := &store.Conversation{
		ID:             uuid.NewString(),
		Kind:           "direct",
		Surface:        "native",
		ExternalRef:    extRef,
		DriftState:     "active",
		LastActivityAt: now,
		CreatedAt:      now,
	}
	require.NoError(t, cs.CreateConversation(ctx, dm))

	m1 := &store.Message{
		ID:             uuid.NewString(),
		ProjectID:      projID,
		ConversationID: dm.ID,
		ThreadID:       dm.ExternalRef,
		Channel:        "web",
		Sender:         "user:alice",
		SenderID:       userID,
		Recipient:      "agent:a1",
		RecipientID:    agentID,
		Msg:            "first pg DM msg",
		Type:           "chat",
		DispatchState:  store.MessageDispatchDispatched,
	}
	require.NoError(t, cs.CreateMessage(ctx, m1))
	m2 := &store.Message{
		ID:             uuid.NewString(),
		ProjectID:      projID,
		ConversationID: dm.ID,
		ThreadID:       dm.ExternalRef,
		Channel:        "web",
		Sender:         "agent:a1",
		SenderID:       agentID,
		Recipient:      "user:alice",
		RecipientID:    userID,
		Msg:            "second pg DM msg",
		Type:           "chat",
		DispatchState:  store.MessageDispatchDispatched,
	}
	require.NoError(t, cs.CreateMessage(ctx, m2))

	topic, err := wcs.PromoteDM(ctx, WebChatTopic{
		ID:             uuid.NewString(),
		ProjectID:      projID,
		Name:           "promoted-pg-topic",
		CreatedBy:      userID,
		CreatedAt:      now,
		LastActivityAt: now,
	}, PromoteKeys{
		DMKey:                dm.ExternalRef,
		DirectConversationID: dm.ID,
	})
	require.NoError(t, err)
	require.NotEmpty(t, topic.ConversationID)

	head, err := cs.ConversationHeadSeq(ctx, topic.ConversationID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), head)

	m3 := &store.Message{
		ID:             uuid.NewString(),
		ProjectID:      projID,
		ConversationID: topic.ConversationID,
		ThreadID:       topic.ID,
		Channel:        "web",
		Sender:         "user:alice",
		SenderID:       userID,
		Recipient:      "agent:a1",
		RecipientID:    agentID,
		Msg:            "after pg promote",
		Type:           "chat",
		DispatchState:  store.MessageDispatchDispatched,
	}
	require.NoError(t, cs.CreateMessage(ctx, m3))
	assert.Equal(t, int64(3), m3.ConversationSeq)

	page, err := cs.ListConversationFeed(ctx, store.ConversationFeedQuery{
		ConversationID: topic.ConversationID,
		AfterSeq:       0,
		Filter:         store.ConversationFeedFilterAll,
	})
	require.NoError(t, err)
	require.Len(t, page.Messages, 3)
	assert.Equal(t, []string{m1.ID, m2.ID, m3.ID}, []string{
		page.Messages[0].ID, page.Messages[1].ID, page.Messages[2].ID,
	})
}
