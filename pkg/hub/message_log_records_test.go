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

//go:build !no_sqlite && (!hubshard || hubshard_4)

package hub

// ---------------------------------------------------------------------------
// ptone/scion#3852: the message log holds one message record per message.
// Lifecycle lines are tagged log_kind=event and name sender and recipient,
// so the Messages view (which leaves event entries out) shows no
// "unknown -> unknown" rows and no duplicates.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newJSONMessageLogger captures the dedicated message log as JSON lines.
func newJSONMessageLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// messageLogEntries parses every captured message log line.
func messageLogEntries(t *testing.T, buf *syncBuffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "line: %s", line)
		out = append(out, rec)
	}
	return out
}

// messageRecords returns the entries the Messages view lists: no
// log_kind=event tag and a sender or recipient. It fails the test on an
// untagged entry without sender or recipient (an "unknown" row).
func messageRecords(t *testing.T, entries []map[string]any) []map[string]any {
	t.Helper()
	var recs []map[string]any
	for _, e := range entries {
		if e[logging.AttrLogKind] == logging.LogKindEvent {
			continue
		}
		sender, _ := e[logging.AttrSender].(string)
		recipient, _ := e[logging.AttrRecipient].(string)
		if sender == "" && recipient == "" {
			t.Errorf("untagged message log entry without sender or recipient: %v", e)
			continue
		}
		recs = append(recs, e)
	}
	return recs
}

func findEntry(t *testing.T, entries []map[string]any, msg string) map[string]any {
	t.Helper()
	for _, e := range entries {
		if e["msg"] == msg {
			return e
		}
	}
	t.Fatalf("no %q entry in message log: %v", msg, entries)
	return nil
}

// Through handleAgentMessage: "message received for delivery" is the
// record; the agent DM dispatch line is a lifecycle entry with sender and
// recipient.
func TestMessageLog_AgentDM_HandleAgentMessage_OneRecord(t *testing.T) {
	srv, _, _, sender, target, _, _, _ := paritySetup(t)
	logger, buf := newJSONMessageLogger()
	srv.SetMessageLogger(logger)

	rr := sendViaStructured(t, srv, sender, target, "records-structured")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	entries := messageLogEntries(t, buf)
	recs := messageRecords(t, entries)
	require.Len(t, recs, 1, "exactly one message record: %v", entries)
	assert.Equal(t, "message received for delivery", recs[0]["msg"])
	assert.Equal(t, "records-structured", recs[0]["message_content"])

	dm := findEntry(t, entries, "agent DM: message dispatched")
	assert.Equal(t, logging.LogKindEvent, dm[logging.AttrLogKind])
	assert.Equal(t, "agent:"+sender.Slug, dm[logging.AttrSender])
	assert.Equal(t, sender.ID, dm[logging.AttrSenderID])
	assert.Equal(t, "agent:"+target.Slug, dm[logging.AttrRecipient])
	assert.Equal(t, target.ID, dm[logging.AttrRecipientID])
	assert.NotContains(t, dm, "message_content", "lifecycle entry carries no body")
}

// Through the outbound adapter (ExecuteAgentDM without RecordLogged): the
// agent DM dispatch line is the single record.
func TestMessageLog_AgentDM_Outbound_OneRecord(t *testing.T) {
	srv, _, _, sender, target, convID, _, _ := paritySetup(t)
	logger, buf := newJSONMessageLogger()
	srv.SetMessageLogger(logger)

	rr := sendViaOutbound(t, srv, sender, convID, "records-outbound")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	recs := messageRecords(t, messageLogEntries(t, buf))
	require.Len(t, recs, 1, "exactly one message record")
	rec := recs[0]
	assert.Equal(t, "agent DM: message dispatched", rec["msg"])
	assert.Equal(t, "agent:"+sender.Slug, rec[logging.AttrSender])
	assert.Equal(t, "agent:"+target.Slug, rec[logging.AttrRecipient])
	assert.Equal(t, target.ID, rec[logging.AttrRecipientID])
	assert.Equal(t, "records-outbound", rec["message_content"])
	assert.Equal(t, convID, rec["conversation_id"])
}

// Direct ExecuteAgentDM calls: the record is written unless the caller
// already logged it.
func TestMessageLog_ExecuteAgentDM_RecordLogged(t *testing.T) {
	for _, tc := range []struct {
		name         string
		recordLogged bool
		wantRecords  int
	}{
		{"not yet logged", false, 1},
		{"already logged", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _, sender, target, _, _, _ := paritySetup(t)
			logger, buf := newJSONMessageLogger()
			srv.SetMessageLogger(logger)

			_, dmErr := srv.ExecuteAgentDM(context.Background(), &AgentDMInput{
				SenderAgent: sender,
				SenderIdentity: &agentIdentityWrapper{&AgentTokenClaims{
					Claims:    jwt.Claims{Subject: sender.ID},
					ProjectID: sender.ProjectID,
					Ancestry:  sender.Ancestry,
				}},
				TargetAgent:  target,
				Msg:          "records-direct",
				Type:         "instruction",
				ProjectID:    sender.ProjectID,
				RecordLogged: tc.recordLogged,
			})
			require.Nil(t, dmErr)

			entries := messageLogEntries(t, buf)
			assert.Len(t, messageRecords(t, entries), tc.wantRecords)
			dm := findEntry(t, entries, "agent DM: message dispatched")
			assert.Equal(t, "agent:"+sender.Slug, dm[logging.AttrSender])
			assert.Equal(t, "agent:"+target.Slug, dm[logging.AttrRecipient])
		})
	}
}

// The agent reply to a user line names sender and recipient.
func TestMessageLog_OutboundMessageSent_HasSenderAndRecipient(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	threadRef := "thread:" + project.ID + ":records-outbound-user"
	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "group",
		Surface:     "discord",
		ExternalRef: threadRef,
		ProjectID:   &project.ID,
		DriftState:  "active",
	})
	require.NoError(t, err)

	logger, buf := newJSONMessageLogger()
	srv.SetMessageLogger(logger)

	rr := postOutboundWithConv(t, srv, project.ID, agent.ID, user.Email, "records reply", conv.ID)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	entries := messageLogEntries(t, buf)
	messageRecords(t, entries) // fails on an "unknown" row
	line := findEntry(t, entries, "outbound message sent")
	assert.Equal(t, "agent:"+agent.Slug, line[logging.AttrSender])
	assert.NotEmpty(t, line[logging.AttrRecipient])
	assert.NotEmpty(t, line[logging.AttrRecipientID])
}

// group[] agent members: each delivered member gets one record from the
// hub; the summary line is a lifecycle entry with sender and recipient.
func TestMessageLog_GroupMessage_RecordPerAgentMember(t *testing.T) {
	srv, s := testServer(t)
	projectID, _ := setupGroupTest(t, s, "3852-grp", map[string]string{
		"rec-grp-a": string(state.PhaseRunning),
		"rec-grp-b": string(state.PhaseRunning),
	})
	srv.SetDispatcher(&brokerMockDispatcher{})
	logger, buf := newJSONMessageLogger()
	srv.SetMessageLogger(logger)

	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+projectID+"/agents/rec-grp-a/message",
		MessageRequest{StructuredMessage: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Sender:    "user:3852",
			SenderID:  DevUserID,
			Recipient: "group[agent:rec-grp-a,agent:rec-grp-b]",
			Msg:       "group records",
			Type:      messages.TypeInstruction,
		}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	entries := messageLogEntries(t, buf)
	recs := messageRecords(t, entries)
	var recipients []string
	for _, r := range recs {
		assert.Equal(t, "set message delivered to member", r["msg"])
		assert.Equal(t, "user:3852", r[logging.AttrSender])
		recipients = append(recipients, r[logging.AttrRecipient].(string))
	}
	assert.ElementsMatch(t, []string{"agent:rec-grp-a", "agent:rec-grp-b"}, recipients)

	summary := findEntry(t, entries, "set message dispatched")
	assert.Equal(t, logging.LogKindEvent, summary[logging.AttrLogKind])
	assert.Equal(t, "user:3852", summary[logging.AttrSender])
	assert.NotEmpty(t, summary[logging.AttrRecipient])
}
