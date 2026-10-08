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
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
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

// setupThreadTestChannels registers the given channels (each as a no-op
// spoke) on srv's broker proxy, so Channel:<name> passes
// validateChannelRegistered, and subscribes the project's user messages so a
// web send is persisted (asynchronously, by the in-process subscriber).
func setupThreadTestChannels(t *testing.T, srv *Server, s store.Store, project *store.Project, channels ...string) {
	t.Helper()
	buses := []eventbus.NamedEventBus{{Name: eventbus.InProcessBusName, Bus: eventbus.NewInProcessEventBus(slog.Default())}}
	for _, ch := range channels {
		buses = append(buses, eventbus.NamedEventBus{Name: ch, Bus: nullSpokeEventBus{}})
	}
	fanout := eventbus.NewFanOutEventBus(buses, slog.Default())
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	proxy := NewMessageBrokerProxy(fanout, s, events,
		func() AgentDispatcher { return nil }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)
	proxy.subscribeProjectUserMessages(project.ID)
}

// attachWebChatStore gives srv a real sqlite WebChatStore on s's database
// and returns it.
func attachWebChatStore(t *testing.T, srv *Server, s store.Store) WebChatStore {
	t.Helper()
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok)
	wcs := NewWebChatStore(dbProvider.DB(), "sqlite3")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)
	return wcs
}

// waitForSenderMessage waits until a message with text msg from agentID has
// been persisted (the broker path persists asynchronously) and returns it.
func waitForSenderMessage(t *testing.T, s store.Store, agentID, msg string) store.Message {
	t.Helper()
	var found store.Message
	require.Eventually(t, func() bool {
		rows, err := s.ListMessages(context.Background(), store.MessageFilter{SenderID: agentID}, store.ListOptions{Limit: 50})
		if err != nil {
			return false
		}
		for _, m := range rows.Items {
			if m.Msg == msg {
				found = m
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond, "message %q was never persisted", msg)
	return found
}

// assertOnlyControlMessage sends a plain control message (no thread) after a
// rejected send, waits for it to be persisted, then asserts it is the only
// message from the agent. The in-process subscriber persists in publish
// order, so a rejected message that had been published would be visible by
// the time the control row is.
func assertOnlyControlMessage(t *testing.T, srv *Server, s store.Store, project *store.Project, agent *store.Agent, user *store.User) {
	t.Helper()
	const control = "control message after rejection"
	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Recipient: "user:" + user.Email,
		Msg:       control,
		Channel:   "web",
	})
	require.Equal(t, http.StatusOK, rr.Code, "control send: %s", rr.Body.String())
	waitForSenderMessage(t, s, agent.ID, control)

	rows, err := s.ListMessages(context.Background(), store.MessageFilter{SenderID: agent.ID}, store.ListOptions{Limit: 50})
	require.NoError(t, err)
	require.Len(t, rows.Items, 1, "a rejected send must not persist a message")
	assert.Equal(t, control, rows.Items[0].Msg)
}

func decodeErrorResponse(t *testing.T, body []byte) APIError {
	t.Helper()
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	return resp.Error
}

func TestOutbound_FreeTextThreadID_Unresolved_Rejected(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()
	setupThreadTestChannels(t, srv, s, project, "web")

	before := countProjectConversations(t, s, project.ID)

	const threadID = "c0ffee00-old-thread-uuid"
	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Recipient: "user:" + user.Email,
		Msg:       "to a thread that does not exist",
		ThreadID:  threadID,
		Channel:   "web",
	})
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, "body: %s", rr.Body.String())

	apiErr := decodeErrorResponse(t, rr.Body.Bytes())
	assert.Equal(t, ErrCodeUnprocessable, apiErr.Code)
	// Pin the exact signed-off text: it names the thread_id, points to
	// conv:<uuid> addressing, and makes no scope claim.
	assert.Equal(t, `thread_id "c0ffee00-old-thread-uuid" does not match an existing conversation; `+
		`address the conversation with conv:<uuid> (see 'scion conversation list'), `+
		`or omit thread_id to message the recipient directly`, apiErr.Message)

	// No conversation was minted for the thread key ...
	extRef, err := messaging.ThreadConversationExternalRef(project.ID, threadID)
	require.NoError(t, err)
	_, err = s.GetConversationByExternalRef(ctx, "native", extRef)
	assert.True(t, errors.Is(err, store.ErrNotFound), "no thread conversation may be created; got err=%v", err)
	assert.Equal(t, before, countProjectConversations(t, s, project.ID), "no conversation row may be created")

	// ... and no message row was persisted.
	assertOnlyControlMessage(t, srv, s, project, agent, user)
}

func TestOutbound_FreeTextThreadID_Existing_Succeeds_WithConversationID(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	setupThreadTestChannels(t, srv, s, project, "web")

	const threadID = "existing-thread-2026"
	conv := seedThreadConversation(t, s, project.ID, threadID)
	before := countProjectConversations(t, s, project.ID)

	const text = "to an existing thread"
	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Recipient: "user:" + user.Email,
		Msg:       text,
		ThreadID:  threadID,
		Channel:   "web",
	})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, conv.ID, body["conversation_id"], "response must name the existing thread conversation")
	assert.Equal(t, before, countProjectConversations(t, s, project.ID), "an existing thread must be reused, not duplicated")

	stored := waitForSenderMessage(t, s, agent.ID, text)
	assert.Equal(t, conv.ID, stored.ConversationID)
}

// TestOutbound_FreeTextThreadID_ExternalChannel_NotGated pins that the
// existing-thread check applies only to native delivery: an external
// channel's plugin delivers to the ThreadID itself, so an unknown thread on
// slack is still accepted, while the same send on web is rejected.
func TestOutbound_FreeTextThreadID_ExternalChannel_NotGated(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	setupThreadTestChannels(t, srv, s, project, "web", "slack")

	const threadID = "C123:1700000000.000100"
	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Recipient: "user:" + user.Email,
		Msg:       "to a slack thread",
		ThreadID:  threadID,
		Channel:   "slack",
	})
	require.Equal(t, http.StatusOK, rr.Code, "slack: an unknown thread must not be rejected: %s", rr.Body.String())

	rr = postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Recipient: "user:" + user.Email,
		Msg:       "to a web thread",
		ThreadID:  "another-unknown-thread",
		Channel:   "web",
	})
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, "web: an unknown thread is rejected: %s", rr.Body.String())
}

func TestOutbound_FreeTextThreadID_LiveTopic_Succeeds(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()
	wcs := attachWebChatStore(t, srv, s)
	setupThreadTestChannels(t, srv, s, project, "web")

	topicID := api.NewUUID()
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: project.ID, Name: "thread-2026-live",
		CreatedBy: user.ID, CreatedAt: time.Now(),
	}))
	topicConvID, err := wcs.GetTopicConversationID(ctx, topicID)
	require.NoError(t, err)
	require.NotEmpty(t, topicConvID, "precondition: CreateTopic links a conversation")

	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Recipient: "user:" + user.Email,
		Msg:       "to a live topic",
		ThreadID:  topicID,
		Channel:   "web",
	})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, topicConvID, body["conversation_id"])
}

func TestOutbound_FreeTextThreadID_DeletedTopic_Rejected(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()
	wcs := attachWebChatStore(t, srv, s)
	setupThreadTestChannels(t, srv, s, project, "web")

	// The store refuses to delete a project's last thread, so keep one.
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: api.NewUUID(), ProjectID: project.ID, Name: "thread-2026-keep",
		CreatedBy: user.ID, CreatedAt: time.Now(),
	}))
	topicID := api.NewUUID()
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: project.ID, Name: "thread-2026-deleted",
		CreatedBy: user.ID, CreatedAt: time.Now(),
	}))
	require.NoError(t, wcs.DeleteTopic(ctx, topicID))

	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Recipient: "user:" + user.Email,
		Msg:       "to a deleted topic",
		ThreadID:  topicID,
		Channel:   "web",
	})
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, "body: %s", rr.Body.String())
	apiErr := decodeErrorResponse(t, rr.Body.Bytes())
	assert.Equal(t, ErrCodeUnprocessable, apiErr.Code)
	// Pin the exact signed-off text (no scope claim).
	assert.Equal(t, `thread_id "`+topicID+`" refers to a deleted conversation; `+
		`address an active conversation with conv:<uuid> (see 'scion conversation list'), `+
		`or omit thread_id to message the recipient directly`, apiErr.Message)

	assertOnlyControlMessage(t, srv, s, project, agent, user)
}

// topicLookupFailingStore is a WebChatStore whose topic lookups fail with an
// infrastructure error (not store.ErrNotFound).
type topicLookupFailingStore struct {
	WebChatStore
}

func (topicLookupFailingStore) GetTopicConversationID(context.Context, string) (string, error) {
	return "", errors.New("injected topic lookup failure: secret-detail")
}

func (topicLookupFailingStore) GetTopicConversationIDIncludingDeleted(context.Context, string) (string, error) {
	return "", errors.New("injected topic lookup failure: secret-detail")
}

func TestOutbound_FreeTextThreadID_LookupFailure_500(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	wcs := attachWebChatStore(t, srv, s)
	srv.SetWebChatStore(topicLookupFailingStore{WebChatStore: wcs})
	setupThreadTestChannels(t, srv, s, project, "web")

	before := countProjectConversations(t, s, project.ID)
	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Recipient: "user:" + user.Email,
		Msg:       "lookup will fail",
		ThreadID:  "some-thread",
		Channel:   "web",
	})
	require.Equal(t, http.StatusInternalServerError, rr.Code, "body: %s", rr.Body.String())
	apiErr := decodeErrorResponse(t, rr.Body.Bytes())
	assert.Equal(t, ErrCodeInternalError, apiErr.Code)
	assert.Equal(t, "thread conversation lookup failed", apiErr.Message)
	assert.NotContains(t, rr.Body.String(), "secret-detail", "lookup error detail must not leak")
	assert.Equal(t, before, countProjectConversations(t, s, project.ID), "no conversation row may be created")
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

// fakeTopicLookup is a minimal messaging.TopicConversationLookup. live is the
// answer for non-deleted topics; all is the answer including deleted ones.
type fakeTopicLookup struct {
	liveID, allID   string
	liveErr, allErr error
}

func (f fakeTopicLookup) GetTopicConversationID(context.Context, string) (string, error) {
	return f.liveID, f.liveErr
}

func (f fakeTopicLookup) GetTopicConversationIDIncludingDeleted(context.Context, string) (string, error) {
	return f.allID, f.allErr
}

// fakeConvReader is a minimal messaging.ConversationReader.
type fakeConvReader struct {
	conv *store.Conversation
	err  error
}

func (f fakeConvReader) GetConversationByExternalRef(context.Context, string, string) (*store.Conversation, error) {
	return f.conv, f.err
}

func TestOutboundThreadConversationState(t *testing.T) {
	notFound := fakeConvReader{err: store.ErrNotFound}
	found := fakeConvReader{conv: &store.Conversation{ID: "c1"}}
	boom := errors.New("db down")
	noTopic := fakeTopicLookup{liveErr: store.ErrNotFound, allErr: store.ErrNotFound}

	cases := []struct {
		name    string
		cr      messaging.ConversationReader
		tl      messaging.TopicConversationLookup
		want    outboundThreadState
		wantErr bool
	}{
		{"no topic store, no row", notFound, nil, outboundThreadMissing, false},
		{"no topic store, row exists", found, nil, outboundThreadExists, false},
		{"live topic with conversation", notFound, fakeTopicLookup{liveID: "c2", allID: "c2"}, outboundThreadExists, false},
		{"live topic not yet backfilled counts as existing", notFound, fakeTopicLookup{}, outboundThreadExists, false},
		{"deleted topic", found, fakeTopicLookup{liveErr: store.ErrNotFound, allID: "c3"}, outboundThreadDeleted, false},
		{"not a topic, row exists", found, noTopic, outboundThreadExists, false},
		{"not a topic, no row", notFound, noTopic, outboundThreadMissing, false},
		{"live topic lookup failure is an error", notFound, fakeTopicLookup{liveErr: boom}, outboundThreadMissing, true},
		{"deleted topic lookup failure is an error", notFound, fakeTopicLookup{liveErr: store.ErrNotFound, allErr: boom}, outboundThreadMissing, true},
		{"conversation lookup failure is an error", fakeConvReader{err: boom}, nil, outboundThreadMissing, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := outboundThreadConversationState(context.Background(), tc.cr, tc.tl, "thread:p:t", "t")
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestOutboundThreadGateApplies(t *testing.T) {
	for channel, want := range map[string]bool{
		"":                 true,
		"web":              true,
		"native":           true,
		"slack":            false,
		"teams":            false,
		"gchat":            false,
		"telegram":         false,
		"discord":          false,
		"custom-plugin-ch": false, // unknown names are plugins, not native
	} {
		assert.Equal(t, want, outboundThreadGateApplies(channel), "channel %q", channel)
	}
}

// An outbound threaded send on an external channel stores its conversation
// under the channel's surface, so an inbound reply on the same thread
// resolves to the same conversation and no native one is created.
func TestOutbound_ExternalChannelThread_SharesInboundConversation(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	setupThreadTestChannels(t, srv, s, project, "web", "slack")

	const threadID = "C999:1700000000.000200"
	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Recipient: "user:" + user.Email,
		Msg:       "outbound to a slack thread",
		ThreadID:  threadID,
		Channel:   "slack",
	})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	outboundConvID, _ := body["conversation_id"].(string)
	require.NotEmpty(t, outboundConvID)

	extRef, err := messaging.ThreadConversationExternalRef(project.ID, threadID)
	require.NoError(t, err)
	_, err = s.GetConversationByExternalRef(context.Background(), "native", extRef)
	assert.ErrorIs(t, err, store.ErrNotFound, "no native conversation for an external thread")

	inbound, err := srv.resolvePhase5Conversation(context.Background(), threadID, project.ID, user.ID, agent.ID, "slack")
	require.NoError(t, err)
	require.NotNil(t, inbound)
	assert.Equal(t, outboundConvID, inbound.ConversationID, "inbound reply must reuse the outbound conversation")
	assert.Equal(t, "slack", inbound.Surface)
}

func TestOutboundThreadSurface(t *testing.T) {
	assert.Equal(t, "slack", outboundThreadSurface("group", "slack"))
	assert.Equal(t, "", outboundThreadSurface("group", "web"))
	assert.Equal(t, "", outboundThreadSurface("group", ""))
	assert.Equal(t, "", outboundThreadSurface("group", "no-such-channel"))
	assert.Equal(t, "", outboundThreadSurface("direct", "slack"))
}
