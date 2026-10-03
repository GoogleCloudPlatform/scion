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

package entc

import (
	"context"
	"testing"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestMessageProvenanceColumns_AdditiveUpgrade pins that the
// sender_project_id / recipient_project_id columns (ptone/scion#2282) reach a
// pre-existing messages table through the normal AutoMigrate path, without
// disturbing existing rows, which read back with NULL provenance.
func TestMessageProvenanceColumns_AdditiveUpgrade(t *testing.T) {
	client := newTestClient(t)
	driver, ok := client.Driver().(*entsql.Driver)
	require.True(t, ok)
	db := driver.DB()
	ctx := context.Background()

	// Simulate a database created before the columns existed.
	for _, col := range []string{"sender_project_id", "recipient_project_id"} {
		_, err := db.ExecContext(ctx, "ALTER TABLE messages DROP COLUMN "+col)
		require.NoError(t, err)
	}
	legacyID, projectID := uuid.New(), uuid.New()
	_, err := db.ExecContext(ctx,
		`INSERT INTO messages (id, project_id, sender, sender_id, recipient, recipient_id, msg, type, urgent, broadcasted, read, agent_id, group_id, dispatch_state, created)
		 VALUES (?, ?, 'agent:a', 'a', 'agent:b', 'b', 'legacy', 'instruction', false, false, false, 'b', '', 'dispatched', CURRENT_TIMESTAMP)`,
		legacyID, projectID)
	require.NoError(t, err)

	require.NoError(t, AutoMigrate(ctx, client))

	legacy, err := client.Message.Get(ctx, legacyID)
	require.NoError(t, err)
	require.Equal(t, "legacy", legacy.Msg)
	require.Nil(t, legacy.SenderProjectID)
	require.Nil(t, legacy.RecipientProjectID)

	senderProj, recipientProj := uuid.New(), uuid.New()
	created, err := client.Message.Create().
		SetProjectID(projectID).
		SetSender("agent:a").
		SetRecipient("agent:b").
		SetMsg("stamped").
		SetSenderProjectID(senderProj).
		SetRecipientProjectID(recipientProj).
		Save(ctx)
	require.NoError(t, err)
	got, err := client.Message.Get(ctx, created.ID)
	require.NoError(t, err)
	require.NotNil(t, got.SenderProjectID)
	require.NotNil(t, got.RecipientProjectID)
	require.Equal(t, senderProj, *got.SenderProjectID)
	require.Equal(t, recipientProj, *got.RecipientProjectID)
}
