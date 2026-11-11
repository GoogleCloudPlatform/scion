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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// Old CLI + new hub: the primary send already fanned the mention out
// server-side. The old CLI's own client-side follow-up POST (Type=mention,
// same sender/recipient/body) must be deduplicated: the bystander receives
// exactly one dispatch overall, and the second POST reports status
// "deduplicated" without charging the budget.
func TestMentionFanout_OldCLISkewDedup(t *testing.T) {
	srv, _, _, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)

	body := "hey @" + mentionBystanderSlug + " can you take a look?"
	rr := sendViaStructured(t, srv, sender, target, body)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	require.Len(t, dispatchesTo(dispatcher, bystander.ID), 1, "server-side fan-out must have delivered the mention once")

	tokensBefore := srv.chatSendLimiter.buckets["agent:"+sender.ID].tokens

	// Old CLI's own client-side mention POST for the same body mention
	// (cmd/message.go sendMentionMessages, used by CLIs that predate
	// server-side fan-out).
	mentionMsg := messages.NewMention("agent:"+sender.Slug, "agent:"+bystander.Slug, body, "agent:"+target.Slug)
	reqBody, err := json.Marshal(MessageRequest{StructuredMessage: mentionMsg})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+bystander.ProjectID+"/agents/"+bystander.ID+"/message",
		bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentCtx(req.Context(), sender))
	skewRR := httptest.NewRecorder()
	srv.handleAgentMessage(skewRR, req, bystander.ID)
	require.Equal(t, http.StatusOK, skewRR.Code, "body: %s", skewRR.Body.String())

	var resp MessageDeliveryResponse
	require.NoError(t, json.Unmarshal(skewRR.Body.Bytes(), &resp))
	require.Equal(t, "deduplicated", resp.Status)

	// Exactly one dispatch overall: the skew POST must not have dispatched again.
	require.Len(t, dispatchesTo(dispatcher, bystander.ID), 1, "the old-CLI skew POST must not double-deliver")

	// No budget charged for the deduplicated POST.
	tokensAfter := srv.chatSendLimiter.buckets["agent:"+sender.ID].tokens
	require.Equal(t, tokensBefore, tokensAfter, "a deduplicated skew POST must not charge the sender's send budget")
}

// When the server-side fan-out's own delivery attempt failed (the row is
// persisted but marked failed, e.g. a broker timeout), an old CLI's
// follow-up POST for the same body mention must not be treated as a
// duplicate — the mention was never actually delivered, so this POST is the
// user's only real chance to have it go out.
func TestMentionFanout_SkewDedup_FailedFanoutRowDoesNotSuppressRetry(t *testing.T) {
	srv, s, _, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)
	ctx := context.Background()

	body := "hey @" + mentionBystanderSlug + " can you take a look?"
	rr := sendViaStructured(t, srv, sender, target, body)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	require.Len(t, dispatchesTo(dispatcher, bystander.ID), 1, "server-side fan-out must have delivered the mention once")

	rows, err := s.ListMessages(ctx, store.MessageFilter{
		SenderID: sender.ID, RecipientID: bystander.ID, Type: messages.TypeMention,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows.Items, 1)
	require.NoError(t, s.MarkMessageFailed(ctx, rows.Items[0].ID, "simulated broker timeout"))

	mentionMsg := messages.NewMention("agent:"+sender.Slug, "agent:"+bystander.Slug, body, "agent:"+target.Slug)
	reqBody, err := json.Marshal(MessageRequest{StructuredMessage: mentionMsg})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+bystander.ProjectID+"/agents/"+bystander.ID+"/message",
		bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentCtx(req.Context(), sender))
	skewRR := httptest.NewRecorder()
	srv.handleAgentMessage(skewRR, req, bystander.ID)
	require.Equal(t, http.StatusOK, skewRR.Code, "body: %s", skewRR.Body.String())

	var resp MessageDeliveryResponse
	require.NoError(t, json.Unmarshal(skewRR.Body.Bytes(), &resp))
	require.NotEqual(t, "deduplicated", resp.Status, "a failed prior delivery must not suppress the retry")

	require.Len(t, dispatchesTo(dispatcher, bystander.ID), 2, "the retry after a failed delivery must actually dispatch")
}

// The dedup gate only ever applies to type=mention POSTs. An ordinary
// instruction whose body happens to exactly match a mention body fanned out
// moments earlier between the same two agents must still be delivered, not
// silently dropped behind a "deduplicated" success status.
func TestMentionFanout_OrdinaryMessageWithMatchingBodyIsNotDeduplicated(t *testing.T) {
	srv, s, _, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)
	ctx := context.Background()

	body := "hey @" + mentionBystanderSlug + " can you take a look?"
	rr := sendViaStructured(t, srv, sender, target, body)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	require.Len(t, dispatchesTo(dispatcher, bystander.ID), 1, "server-side fan-out must have delivered the mention once")

	// An ordinary instruction, sender -> bystander directly, whose body
	// exactly matches the mention body the dedup gate would otherwise match
	// on. This is Type=instruction, not Type=mention, so the gate must never
	// even look at it.
	rr2 := sendViaStructured(t, srv, sender, bystander, body)
	require.Equal(t, http.StatusOK, rr2.Code, "body: %s", rr2.Body.String())

	var resp MessageDeliveryResponse
	require.NoError(t, json.Unmarshal(rr2.Body.Bytes(), &resp))
	require.NotEqual(t, "deduplicated", resp.Status,
		"an ordinary instruction must never be deduplicated, regardless of body content")

	require.Len(t, dispatchesTo(dispatcher, bystander.ID), 2, "the ordinary instruction must actually be dispatched, not dropped")

	rows, err := s.ListMessages(ctx, store.MessageFilter{SenderID: sender.ID, RecipientID: bystander.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows.Items, 2, "both the mention row and the ordinary instruction row must be persisted")
}

// A non-duplicate Type=mention POST (different body, or outside the window)
// is NOT deduplicated — it is a distinct, deliberate mention.
func TestMentionFanout_SkewDedup_DistinctBodyNotDeduped(t *testing.T) {
	srv, _, _, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)

	rr := sendViaStructured(t, srv, sender, target, "hey @"+mentionBystanderSlug+" look at this")
	require.Equal(t, http.StatusOK, rr.Code)
	require.Len(t, dispatchesTo(dispatcher, bystander.ID), 1)

	mentionMsg := messages.NewMention("agent:"+sender.Slug, "agent:"+bystander.Slug, "a completely different message", "agent:"+target.Slug)
	reqBody, err := json.Marshal(MessageRequest{StructuredMessage: mentionMsg})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+bystander.ProjectID+"/agents/"+bystander.ID+"/message",
		bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentCtx(req.Context(), sender))
	rr2 := httptest.NewRecorder()
	srv.handleAgentMessage(rr2, req, bystander.ID)
	require.Equal(t, http.StatusOK, rr2.Code, "body: %s", rr2.Body.String())

	var resp MessageDeliveryResponse
	require.NoError(t, json.Unmarshal(rr2.Body.Bytes(), &resp))
	require.NotEqual(t, "deduplicated", resp.Status)
	require.Len(t, dispatchesTo(dispatcher, bystander.ID), 2, "a distinct mention body must be delivered, not deduplicated")
}

// mentionRowFor builds a persistable Type=mention store.Message row from
// sender to recipient with the given body and creation time.
func mentionRowFor(sender, recipient *store.Agent, body string, createdAt time.Time) *store.Message {
	return &store.Message{
		ID:          api.NewUUID(),
		ProjectID:   sender.ProjectID,
		Sender:      "agent:" + sender.Slug,
		SenderID:    sender.ID,
		Recipient:   "agent:" + recipient.Slug,
		RecipientID: recipient.ID,
		Msg:         body,
		Type:        messages.TypeMention,
		AgentID:     sender.ID,
		CreatedAt:   createdAt,
	}
}

// recentDuplicateMention itself: unit-level coverage of the window boundary.
func TestRecentDuplicateMention_WindowBoundary(t *testing.T) {
	srv, s, _, sender, _, bystander, _, _ := mentionFanoutSetup(t)
	ctx := context.Background()

	_, found := srv.recentDuplicateMention(ctx, sender.ID, bystander.ID, "hello")
	require.False(t, found, "no prior mention row: not a duplicate")

	old := mentionRowFor(sender, bystander, "hello", time.Now().Add(-3*time.Minute))
	require.NoError(t, s.CreateMessage(ctx, old))
	_, found = srv.recentDuplicateMention(ctx, sender.ID, bystander.ID, "hello")
	require.False(t, found, "a matching row older than the window is not a duplicate")

	fresh := mentionRowFor(sender, bystander, "hello", time.Now())
	require.NoError(t, s.CreateMessage(ctx, fresh))
	existing, found := srv.recentDuplicateMention(ctx, sender.ID, bystander.ID, "hello")
	require.True(t, found)
	require.Equal(t, fresh.ID, existing.ID)

	_, found = srv.recentDuplicateMention(ctx, sender.ID, bystander.ID, "a different body")
	require.False(t, found, "an exact body mismatch is not a duplicate")
}

// The shipped production window — not a test-shrunk stand-in — decides the
// boundary: a row just inside mentionDedupWindow is a duplicate, a row just
// outside it is not. This pins the actual constant; changing it would fail
// this test.
func TestRecentDuplicateMention_ProductionWindowBoundary(t *testing.T) {
	// A literal expectation, not derived from the constant under test: if
	// mentionDedupWindow ever changes, this test must fail rather than
	// silently re-deriving new offsets from the very thing it exists to pin.
	require.Equal(t, 2*time.Minute, mentionDedupWindow, "this test's literal offsets assume the shipped window is 2m; update both together")

	srv, s, _, sender, _, bystander, _, _ := mentionFanoutSetup(t)
	ctx := context.Background()

	justOutside := mentionRowFor(sender, bystander, "boundary body", time.Now().Add(-125*time.Second))
	require.NoError(t, s.CreateMessage(ctx, justOutside))
	_, found := srv.recentDuplicateMention(ctx, sender.ID, bystander.ID, "boundary body")
	require.False(t, found, "a row just outside the production window must not be a duplicate")

	justInside := mentionRowFor(sender, bystander, "boundary body", time.Now().Add(-115*time.Second))
	require.NoError(t, s.CreateMessage(ctx, justInside))
	existing, found := srv.recentDuplicateMention(ctx, sender.ID, bystander.ID, "boundary body")
	require.True(t, found, "a row just inside the production window must be a duplicate")
	require.Equal(t, justInside.ID, existing.ID)
}
