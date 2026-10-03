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
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// ptone/scion#2026: an outbound message with a free-text (non-dm:) thread_id
// and no conversation_id may only address a thread conversation that already
// exists. Previously the hub minted a participant-less group conversation for
// any unknown thread_id and answered "sent", so the user never saw the
// message.
// ---------------------------------------------------------------------------

// seedThreadConversation creates the native group conversation that a
// free-text thread_id resolves to in projectID, and returns it.
func seedThreadConversation(t *testing.T, s store.Store, projectID, threadID string) *store.Conversation {
	t.Helper()
	extRef, err := messaging.ThreadConversationExternalRef(projectID, threadID)
	require.NoError(t, err)
	pid := projectID
	conv, err := s.UpsertConversationByExternalRef(context.Background(), &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: extRef,
		DriftState:  "active",
		ProjectID:   &pid,
	})
	require.NoError(t, err)
	return conv
}

func countProjectConversations(t *testing.T, s store.Store, projectID string) int {
	t.Helper()
	res, err := s.ListConversations(context.Background(), store.ConversationFilter{ProjectID: projectID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	return len(res.Items)
}

func TestOutbound_FreeTextThreadID_Unresolved_Rejected(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()
	setupWebChannelBroker(t, srv, s, project)

	before := countProjectConversations(t, s, project.ID)

	const threadID = "c0ffee00-old-thread-uuid"
	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Recipient: "user:" + user.Email,
		Msg:       "to a thread that does not exist",
		ThreadID:  threadID,
		Channel:   "web",
	})
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, "body: %s", rr.Body.String())

	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, ErrCodeUnprocessable, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, threadID, "error must name the thread_id")
	assert.Contains(t, resp.Error.Message, "conv:<uuid>", "error must point to conv:<uuid> addressing")

	// No conversation was minted for the thread key ...
	extRef, err := messaging.ThreadConversationExternalRef(project.ID, threadID)
	require.NoError(t, err)
	_, err = s.GetConversationByExternalRef(ctx, "native", extRef)
	assert.True(t, errors.Is(err, store.ErrNotFound), "no thread conversation may be created; got err=%v", err)
	assert.Equal(t, before, countProjectConversations(t, s, project.ID), "no conversation row may be created")

	// ... and no message row was persisted.
	msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "a rejected send must not persist a message")
}

func TestOutbound_FreeTextThreadID_Existing_Succeeds_WithConversationID(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()
	setupWebChannelBroker(t, srv, s, project)

	const threadID = "existing-thread-2026"
	conv := seedThreadConversation(t, s, project.ID, threadID)
	before := countProjectConversations(t, s, project.ID)

	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Recipient: "user:" + user.Email,
		Msg:       "to an existing thread",
		ThreadID:  threadID,
		Channel:   "web",
	})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, conv.ID, body["conversation_id"], "response must name the existing thread conversation")
	assert.Equal(t, before, countProjectConversations(t, s, project.ID), "an existing thread must be reused, not duplicated")

	msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, msgs.Items, 1)
	assert.Equal(t, conv.ID, msgs.Items[0].ConversationID)
}

// TestOutbound_DMThreadID_Unchanged pins that a dm: thread_id is not subject
// to the existing-thread requirement: the direct conversation is still
// resolved or created as before, and its ID is now returned.
func TestOutbound_DMThreadID_Unchanged(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	setupWebChannelBroker(t, srv, s, project)

	dmKey := mustDMKey(t, "agent", agent.ID, "user", user.ID)
	_, err := s.GetConversationByExternalRef(context.Background(), "native", dmKey)
	require.True(t, errors.Is(err, store.ErrNotFound), "precondition: the DM conversation does not exist yet")

	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Recipient: "user:" + user.Email,
		Msg:       "dm thread",
		ThreadID:  dmKey,
		Channel:   "web",
	})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	conv, err := s.GetConversationByExternalRef(context.Background(), "native", dmKey)
	require.NoError(t, err, "the DM conversation is still created on demand")
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, conv.ID, body["conversation_id"])
}

// TestOutbound_NoThreadID_ReturnsConversationID pins the plain user send:
// no thread_id derives the agent<->user DM, and its ID is returned.
func TestOutbound_NoThreadID_ReturnsConversationID(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	setupWebChannelBroker(t, srv, s, project)

	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Recipient: "user:" + user.Email,
		Msg:       "plain",
	})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	conv, err := s.GetConversationByExternalRef(context.Background(), "native", mustDMKey(t, "agent", agent.ID, "user", user.ID))
	require.NoError(t, err)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, conv.ID, body["conversation_id"])
}

// fakeTopicLookup is a minimal messaging.TopicConversationLookup.
type fakeTopicLookup struct {
	convID string
	err    error
}

func (f fakeTopicLookup) GetTopicConversationID(context.Context, string) (string, error) {
	return f.convID, f.err
}

func (f fakeTopicLookup) GetTopicConversationIDIncludingDeleted(context.Context, string) (string, error) {
	return f.convID, f.err
}

// fakeConvReader is a minimal messaging.ConversationReader.
type fakeConvReader struct {
	conv *store.Conversation
	err  error
}

func (f fakeConvReader) GetConversationByExternalRef(context.Context, string, string) (*store.Conversation, error) {
	return f.conv, f.err
}

func TestOutboundThreadConversationExists(t *testing.T) {
	notFound := fakeConvReader{err: store.ErrNotFound}
	found := fakeConvReader{conv: &store.Conversation{ID: "c1"}}
	boom := errors.New("db down")

	cases := []struct {
		name    string
		cr      messaging.ConversationReader
		tl      messaging.TopicConversationLookup
		want    bool
		wantErr bool
	}{
		{"no topic store, no row", notFound, nil, false, false},
		{"no topic store, row exists", found, nil, true, false},
		{"topic with conversation", notFound, fakeTopicLookup{convID: "c2"}, true, false},
		{"topic not yet backfilled counts as existing", notFound, fakeTopicLookup{convID: ""}, true, false},
		{"not a topic, row exists", found, fakeTopicLookup{err: store.ErrNotFound}, true, false},
		{"not a topic, no row", notFound, fakeTopicLookup{err: store.ErrNotFound}, false, false},
		{"topic lookup failure is an error", notFound, fakeTopicLookup{err: boom}, false, true},
		{"conversation lookup failure is an error", fakeConvReader{err: boom}, nil, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := outboundThreadConversationExists(context.Background(), tc.cr, tc.tl, "thread:p:t", "t")
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
