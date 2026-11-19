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

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestMessageProvenanceColumns_AdditiveUpgrade pins that the
// sender_project_id / recipient_project_id columns (ptone/scion#2282) reach a
// pre-existing messages table through the normal AutoMigrate path, without
// disturbing existing rows, which read back with NULL provenance.
func TestMessageProvenanceColumns_AdditiveUpgrade(t *testing.T) {
	client := newTestClient(t)
	driver, ok := SQLDriver(client.Driver())
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

// TestMessageDelegationAttributionColumns_AdditiveUpgrade: on a messages
// table that predates the authorizing_user_id / delegation_grant_id columns
// (.design/agent-delegation.md §14.5), AutoMigrate adds them as nullable
// columns, a legacy row reads back with neither, and new values round-trip.
func TestMessageDelegationAttributionColumns_AdditiveUpgrade(t *testing.T) {
	client := newTestClient(t)
	driver, ok := SQLDriver(client.Driver())
	require.True(t, ok)
	db := driver.DB()
	ctx := context.Background()

	for _, col := range []string{"authorizing_user_id", "delegation_grant_id"} {
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
	require.Nil(t, legacy.AuthorizingUserID)
	require.Nil(t, legacy.DelegationGrantID)

	issuer, grant := uuid.NewString(), uuid.NewString()
	created, err := client.Message.Create().
		SetProjectID(projectID).
		SetSender("agent:a").
		SetRecipient("agent:b").
		SetMsg("delegated").
		SetAuthorizingUserID(issuer).
		SetDelegationGrantID(grant).
		Save(ctx)
	require.NoError(t, err)
	got, err := client.Message.Get(ctx, created.ID)
	require.NoError(t, err)
	require.NotNil(t, got.AuthorizingUserID)
	require.NotNil(t, got.DelegationGrantID)
	require.Equal(t, issuer, *got.AuthorizingUserID)
	require.Equal(t, grant, *got.DelegationGrantID)
}

// TestDecisionOtherColumns_AdditiveUpgrade pins that AutoMigrate adds the five
// Other free-form answer columns (design-8146 §4.2, acceptance 3) to existing
// decisions and decision_answers tables without disturbing legacy rows, which
// read back with allow_other=false and NULL other_* / other_text.
func TestDecisionOtherColumns_AdditiveUpgrade(t *testing.T) {
	client := newTestClient(t)
	driver, ok := SQLDriver(client.Driver())
	require.True(t, ok)
	db := driver.DB()
	ctx := context.Background()

	for _, col := range []string{"allow_other", "other_label", "other_description", "other_placeholder"} {
		_, err := db.ExecContext(ctx, "ALTER TABLE decisions DROP COLUMN "+col)
		require.NoError(t, err)
	}
	_, err := db.ExecContext(ctx, "ALTER TABLE decision_answers DROP COLUMN other_text")
	require.NoError(t, err)

	legacyDecID := uuid.New()
	legacyAnsID := uuid.New()
	projectID := uuid.New()
	msgID := uuid.New()
	agentID := uuid.New()
	respMsgID := uuid.New()

	_, err = db.ExecContext(ctx,
		`INSERT INTO decisions (id, project_id, message_id, position, asker_agent_id, asker_agent_name, key, key_norm, key_source, version, question, choices, intro_length, status, created_at)
		 VALUES (?, ?, ?, 0, ?, 'asker', 'legacy', 'legacy', 'agent', 1, 'Ship now?', '[{"id":"1","label":"yes"},{"id":"2","label":"no"}]', 0, 'decided', CURRENT_TIMESTAMP)`,
		legacyDecID, projectID, msgID, agentID)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx,
		`INSERT INTO decision_answers (id, decision_row_id, project_id, asker_agent_id, key, version, question_text, choice_id, choice_label, answered_by_name, answered_by_email, provenance, outcome, surface, response_message_id, matched_default, created_at)
		 VALUES (?, ?, ?, ?, 'legacy', 1, 'Ship now?', '1', 'yes', 'Alice', 'alice@example.com', 'direct', 'answer', 'native', ?, false, CURRENT_TIMESTAMP)`,
		legacyAnsID, legacyDecID, projectID, agentID, respMsgID)
	require.NoError(t, err)

	require.NoError(t, AutoMigrate(ctx, client))

	legacyDec, err := client.Decision.Get(ctx, legacyDecID)
	require.NoError(t, err)
	require.False(t, legacyDec.AllowOther)
	require.Nil(t, legacyDec.OtherLabel)
	require.Nil(t, legacyDec.OtherDescription)
	require.Nil(t, legacyDec.OtherPlaceholder)

	legacyAns, err := client.DecisionAnswer.Get(ctx, legacyAnsID)
	require.NoError(t, err)
	require.Nil(t, legacyAns.OtherText)
}
