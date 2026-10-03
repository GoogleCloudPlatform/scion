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

package entadapter

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// TestMessageProvenanceColumns_AdditiveUpgrade_Postgres is the Postgres
// counterpart of entc's TestMessageProvenanceColumns_AdditiveUpgrade
// (ptone/scion#2282): on a messages table that predates the
// sender_project_id / recipient_project_id columns, AutoMigrate adds them as
// nullable uuid columns, legacy rows read back with NULL provenance, and new
// stamps round-trip. Skips unless built with -tags integration and
// SCION_TEST_POSTGRES_URL is set:
//
//	SCION_TEST_POSTGRES_URL='postgres://user:pass@host:5432/postgres?sslmode=disable' \
//	  go test -tags integration -run TestMessageProvenanceColumns_AdditiveUpgrade_Postgres ./pkg/store/entadapter/
func TestMessageProvenanceColumns_AdditiveUpgrade_Postgres(t *testing.T) {
	url := enttest.NewSchemaURL(t) // fresh, fully migrated schema
	ctx := context.Background()

	raw, err := sql.Open("pgx", url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })

	// Simulate a database created before the columns existed.
	for _, col := range []string{"sender_project_id", "recipient_project_id"} {
		_, err := raw.ExecContext(ctx, "ALTER TABLE messages DROP COLUMN "+col)
		require.NoError(t, err)
	}
	legacyID, projectID := uuid.New(), uuid.New()
	_, err = raw.ExecContext(ctx,
		`INSERT INTO messages (id, project_id, sender, sender_id, recipient, recipient_id, msg, type, urgent, broadcasted, read, agent_id, group_id, dispatch_state, created)
		 VALUES ($1, $2, 'agent:a', 'a', 'agent:b', 'b', 'legacy', 'instruction', false, false, false, 'b', '', 'dispatched', CURRENT_TIMESTAMP)`,
		legacyID, projectID)
	require.NoError(t, err)

	client, err := entc.OpenPostgres(url, entc.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, entc.AutoMigrate(ctx, client))

	for _, col := range []string{"sender_project_id", "recipient_project_id"} {
		var dataType, nullable string
		require.NoError(t, raw.QueryRowContext(ctx,
			`SELECT data_type, is_nullable FROM information_schema.columns
			 WHERE table_schema = current_schema() AND table_name = 'messages' AND column_name = $1`,
			col).Scan(&dataType, &nullable), "column %s must be re-added by AutoMigrate", col)
		require.Equal(t, "uuid", dataType, col)
		require.Equal(t, "YES", nullable, col)
	}

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
