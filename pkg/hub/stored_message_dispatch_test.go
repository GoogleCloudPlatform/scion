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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestStoredMessageDispatch(t *testing.T) {
	ctx := context.Background()
	created := time.Date(2026, 11, 15, 1, 2, 3, 0, time.UTC)

	validRow := func() *store.Message {
		return &store.Message{
			ID:             uuid.NewString(),
			ProjectID:      uuid.NewString(),
			Sender:         "user:alice@example.com",
			SenderID:       uuid.NewString(),
			Recipient:      "agent:worker",
			RecipientID:    uuid.NewString(),
			Msg:            "run diagnostics",
			Type:           messages.TypeInstruction,
			Urgent:         true,
			Broadcasted:    true,
			Channel:        "web",
			ThreadID:       "agent:worker",
			ConversationID: uuid.NewString(),
			CreatedAt:      created,
		}
	}

	t.Run("rebuilds structured envelope and attaches dispatch message id", func(t *testing.T) {
		row := validRow()
		dctx, sm, err := storedMessageDispatch(ctx, row)
		require.NoError(t, err)
		require.NotNil(t, sm)

		assert.Equal(t, row.ID, dispatchMessageIDFromContext(dctx))
		assert.Equal(t, messages.Version, sm.Version)
		assert.Equal(t, "2026-11-15T01:02:03Z", sm.Timestamp)
		assert.Equal(t, row.Sender, sm.Sender)
		assert.Equal(t, row.SenderID, sm.SenderID)
		assert.Equal(t, row.Recipient, sm.Recipient)
		assert.Equal(t, row.RecipientID, sm.RecipientID)
		assert.Equal(t, row.Msg, sm.Msg)
		assert.Equal(t, row.Type, sm.Type)
		assert.True(t, sm.Urgent)
		assert.True(t, sm.Broadcasted)
		assert.False(t, sm.Plain, "re-driven message must include standard sender envelope")
		assert.Equal(t, row.Channel, sm.Channel)
		assert.Equal(t, row.ThreadID, sm.ThreadID)
		assert.Equal(t, row.ConversationID, sm.ConversationID)
		assert.False(t, sm.ConversationAsserted)
		assert.Equal(t, row.ID, sm.StoredMessageID)
		assert.Empty(t, sm.Attachments)
		assert.Empty(t, sm.AttachmentDeliveries)
		assert.Empty(t, sm.AttachmentPlan)
		assert.Empty(t, sm.Metadata)

		formatted := messages.FormatForDelivery(sm)
		assert.Contains(t, formatted, "---BEGIN SCION MESSAGE---")
		assert.Contains(t, formatted, `"sender": "user:alice@example.com"`)
		assert.Contains(t, formatted, `"msg": "run diagnostics"`)
	})

	t.Run("fails closed with content-free error on unrecoverable row", func(t *testing.T) {
		cases := []struct {
			name   string
			mutate func(*store.Message)
		}{
			{name: "nil row", mutate: nil},
			{name: "missing id", mutate: func(m *store.Message) { m.ID = "" }},
			{name: "missing sender", mutate: func(m *store.Message) { m.Sender = "" }},
			{name: "missing recipient", mutate: func(m *store.Message) { m.Recipient = "" }},
			{name: "empty body", mutate: func(m *store.Message) { m.Msg = "" }},
			{name: "zero CreatedAt", mutate: func(m *store.Message) { m.CreatedAt = time.Time{} }},
			{name: "unknown type", mutate: func(m *store.Message) { m.Type = "bogus-secret-type" }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var row *store.Message
				if tc.mutate != nil {
					row = validRow()
					row.Msg = "TOP-SECRET-BODY-SENTINEL"
					tc.mutate(row)
				}
				dctx, sm, err := storedMessageDispatch(ctx, row)
				require.ErrorIs(t, err, errStoredMessageUnrecoverable)
				assert.Nil(t, dctx)
				assert.Nil(t, sm)
				assert.NotContains(t, err.Error(), "TOP-SECRET-BODY-SENTINEL")
				assert.NotContains(t, err.Error(), "bogus-secret-type")
			})
		}
	})
}

func TestShouldTranslateHeldWebMentions(t *testing.T) {
	base := func() *store.Message {
		return &store.Message{
			ID:        uuid.NewString(),
			ProjectID: uuid.NewString(),
			Sender:    "user:owner@example.com",
			SenderID:  uuid.NewString(),
			Recipient: "agent:worker",
			Msg:       "Please ask @alice-smith",
			Type:      messages.TypeInstruction,
			Channel:   "web",
		}
	}

	assert.True(t, shouldTranslateHeldWebMentions(base()))

	replyRow := base()
	replyRow.Type = messages.TypeReply
	assert.True(t, shouldTranslateHeldWebMentions(replyRow))

	mentionRow := base()
	mentionRow.Type = messages.TypeMention
	assert.True(t, shouldTranslateHeldWebMentions(mentionRow))

	cases := []struct {
		name   string
		mutate func(*store.Message)
	}{
		{name: "nil row", mutate: nil},
		{name: "no @ in body", mutate: func(m *store.Message) { m.Msg = "plain prose without mentions" }},
		{name: "decision response", mutate: func(m *store.Message) { m.Type = messages.TypeDecisionResponse }},
		{name: "non-web channel", mutate: func(m *store.Message) { m.Channel = "discord" }},
		{name: "empty channel", mutate: func(m *store.Message) { m.Channel = "" }},
		{name: "agent sender", mutate: func(m *store.Message) { m.Sender = "agent:peer" }},
		{name: "empty sender id", mutate: func(m *store.Message) { m.SenderID = "" }},
		{name: "scheduled event", mutate: func(m *store.Message) { m.ScheduledEventID = uuid.NewString() }},
		{name: "delegated grant", mutate: func(m *store.Message) { m.DelegationGrantID = uuid.NewString() }},
		{name: "authorizing user", mutate: func(m *store.Message) { m.AuthorizingUserID = uuid.NewString() }},
		{name: "empty project id", mutate: func(m *store.Message) { m.ProjectID = "" }},
		{name: "nil uuid project id", mutate: func(m *store.Message) { m.ProjectID = uuid.Nil.String() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var row *store.Message
			if tc.mutate != nil {
				row = base()
				tc.mutate(row)
			}
			assert.False(t, shouldTranslateHeldWebMentions(row))
		})
	}
}
