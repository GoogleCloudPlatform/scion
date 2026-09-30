//go:build !no_sqlite

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

// ---------------------------------------------------------------------------
// Phase 0.2 (ptone/scion#2192): broker/plugin ingress must reject raw before
// sender identity synthesis, conversation resolution, mention work or
// dispatch.
//
// Both handleBrokerInbound and handleBrokerInboundRouted accept a claimed
// sender inside the request body rather than an authenticated one; these
// routes reject raw before the claimed sender is used (AC-1 of
// ptone/scion#2192). These tests prove the blanket rejection fires before
// ANY of that work. The ordering cases (RawRejectedBeforeSenderResolution,
// RawRejectedBeforeTopicParsing, Routed_RawRejectedBeforeSenderPrefixCheck)
// use a sender/topic/routing shape that independently produces a different
// downstream error (403 unresolvable sender, 400 malformed topic, 400 bad
// sender prefix); observing the raw-specific 422 here rather than one of
// those confirms the guard runs before those checks — a different error
// code there would mean the guard is not running at this point. The
// remaining cases assert the rejection itself, each checking a different
// property: RawRejectedWithResolvableSender uses a real, resolvable sender
// and asserts the full zero-side-effect set; LogCapture_NoRawContentExposed
// (itself using an unresolvable sender, the same shape as
// RawRejectedBeforeSenderResolution) asserts that the content appears in
// neither the error body nor the logs; and Routed_RawRejected asserts zero
// dispatch.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHandleBrokerInbound_RawRejectedBeforeSenderResolution proves raw is
// rejected before the sender-identity lookup: the sender email does not
// exist in the store, so if the guard did not fire first, resolution would
// fail with 403 "sender identity could not be resolved" instead of the raw
// guard's 422.
func TestHandleBrokerInbound_RawRejectedBeforeSenderResolution(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:      tid("proj-broker-raw"),
		Slug:    "broker-raw-proj",
		Name:    "Broker Raw Test",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:           tid("agent-broker-raw"),
		Slug:         "broker-raw-agent",
		Name:         "Broker Raw Agent",
		ProjectID:    project.ID,
		Phase:        "running",
		MessageMode:  store.MessageModeProject,
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	topic := "scion.project." + project.ID + ".agent." + agent.Slug + ".messages"
	payload := inboundMessageRequest{
		Topic: topic,
		Message: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Sender:    "user:does-not-exist@example.com", // unresolvable — proves ordering
			Recipient: "agent:" + agent.Slug,
			Msg:       "SECRET-RAW-KEYSTROKES",
			Type:      messages.TypeInstruction,
			Raw:       true,
		},
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code,
		"raw must be rejected before sender resolution is attempted; body: %s", rec.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Equal(t, ErrCodeUnsupportedCapability, errResp.Error.Code)
	assert.Equal(t, string(MessageDenialRawBrokerIngressUnsupported), errResp.Error.Details["reason"])

	assert.Empty(t, dispatcher.getCalls(), "rejected raw inbound message must produce zero dispatch calls")

	msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "rejected raw inbound message must produce zero persisted rows")
}

// TestHandleBrokerInbound_RawRejectedWithResolvableSender complements the
// test above: that one uses an unresolvable claimed sender, which pins
// ordering but does not exercise AC1 — a *resolvable* claimed user sender
// using a raw message to obtain terminal authority. This test uses a real,
// active user whose email matches the claimed sender, a valid topic, and a
// running target, and asserts the full zero-side-effect set (via
// assertZeroMessagingSideEffects), not just dispatch/messages.
func TestHandleBrokerInbound_RawRejectedWithResolvableSender(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid("user-broker-raw-resolvable"),
		Email:       "broker-raw-resolvable@example.com",
		DisplayName: "Broker Raw Resolvable User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	project := &store.Project{
		ID:        tid("proj-broker-raw-resolvable"),
		Slug:      "broker-raw-resolvable-proj",
		Name:      "Broker Raw Resolvable Test",
		OwnerID:   user.ID,
		CreatedBy: user.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroup(ctx, project)
	msgAuthzAddProjectMember(t, s, user.ID, project.ID, project.Slug, store.GroupMemberRoleMember)

	agent := &store.Agent{
		ID:           tid("agent-broker-raw-resolvable"),
		Slug:         "broker-raw-resolvable-agent",
		Name:         "Broker Raw Resolvable Agent",
		ProjectID:    project.ID,
		Phase:        "running",
		MessageMode:  store.MessageModeProject,
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)
	spy := &spyEventPublisher{}
	srv.SetEventPublisher(spy)

	convCountBefore := countStoreConversations(t, s, ctx)
	subsBefore, err := s.GetNotificationSubscriptions(ctx, agent.ID)
	require.NoError(t, err)

	topic := "scion.project." + project.ID + ".agent." + agent.Slug + ".messages"
	payload := inboundMessageRequest{
		Topic: topic,
		Message: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Sender:    "user:" + user.Email, // resolvable — this is the actual AC1 threat
			Recipient: "agent:" + agent.Slug,
			Msg:       "SECRET-RESOLVABLE-RAW",
			Type:      messages.TypeInstruction,
			Raw:       true,
		},
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code,
		"raw must be rejected even with a resolvable claimed sender; body: %s", rec.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Equal(t, string(MessageDenialRawBrokerIngressUnsupported), errResp.Error.Details["reason"])

	assertZeroMessagingSideEffects(t, s, ctx, dispatcher, spy, agent, convCountBefore, len(subsBefore))

	// In-test control confirming the fixture delivers with raw:false —
	// without it, a drifted fixture (membership, broker, topic) could turn
	// this into just another rejected-before-resolution test rather than
	// one that exercises a genuinely resolvable sender. Run after the raw
	// assertions above so it cannot affect them.
	rawFalsePayload := payload
	rawFalseMsg := *payload.Message
	rawFalseMsg.Raw = false
	rawFalseMsg.Msg = "CONTROL-NON-RAW-DELIVERS"
	rawFalsePayload.Message = &rawFalseMsg
	controlBody, err := json.Marshal(rawFalsePayload)
	require.NoError(t, err)

	controlReq := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(controlBody))
	controlReq.Header.Set("Content-Type", "application/json")
	controlReq = controlReq.WithContext(contextWithBrokerIdentity(controlReq.Context(), NewBrokerIdentity("test-broker")))

	controlRec := httptest.NewRecorder()
	srv.mux.ServeHTTP(controlRec, controlReq)

	require.Equal(t, http.StatusOK, controlRec.Code,
		"control: the identical fixture with raw:false must deliver, proving the raw case above exercised a real resolvable sender, not a fixture that would fail anyway; body: %s", controlRec.Body.String())
	assert.Len(t, dispatcher.getCalls(), 1, "control: exactly one dispatch for the non-raw sibling request")
}

// TestHandleBrokerInbound_RawRejectedBeforeTopicParsing proves the guard
// runs even earlier than topic parsing: an unparseable topic would normally
// yield 400 "invalid topic", but raw must be rejected first with its own
// 422 and reason.
func TestHandleBrokerInbound_RawRejectedBeforeTopicParsing(t *testing.T) {
	srv, _ := testServer(t)

	payload := inboundMessageRequest{
		Topic: "not-a-valid-topic",
		Message: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Sender:    "user:whoever@example.com",
			Recipient: "agent:whoever",
			Msg:       "SECRET-RAW-KEYSTROKES",
			Type:      messages.TypeInstruction,
			Raw:       true,
		},
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code,
		"raw must be rejected before topic parsing; body: %s", rec.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Equal(t, string(MessageDenialRawBrokerIngressUnsupported), errResp.Error.Details["reason"])
}

// TestHandleBrokerInbound_LogCapture_NoRawContentExposed is a regression
// guard using a distinctive secret string: even though this request is
// rejected before any log line is emitted, this pins the invariant that no
// code path between decode and rejection writes the raw body to any log.
func TestHandleBrokerInbound_LogCapture_NoRawContentExposed(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	buf := captureSlog(t)

	const secret = "BROKER-INBOUND-RAW-SECRET-7Q3ZK9"

	project := &store.Project{
		ID:      tid("proj-broker-raw-log"),
		Slug:    "broker-raw-log-proj",
		Name:    "Broker Raw Log Test",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:           tid("agent-broker-raw-log"),
		Slug:         "broker-raw-log-agent",
		Name:         "Broker Raw Log Agent",
		ProjectID:    project.ID,
		Phase:        "running",
		MessageMode:  store.MessageModeProject,
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	topic := "scion.project." + project.ID + ".agent." + agent.Slug + ".messages"
	payload := inboundMessageRequest{
		Topic: topic,
		Message: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Sender:    "user:log-test@example.com",
			Recipient: "agent:" + agent.Slug,
			Msg:       secret,
			Type:      messages.TypeInstruction,
			Raw:       true,
		},
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)

	assert.NotContains(t, rec.Body.String(), secret, "raw content must not appear in the error response")
	assert.NotContains(t, buf.String(), secret, "raw content must not appear in captured logs")
}

// TestHandleBrokerInboundRouted_RawRejectedBeforeSenderPrefixCheck proves
// the guard on the routed endpoint runs before even the "sender must use
// user: prefix" validation — a raw message with a malformed sender still
// gets the raw-specific 422, not the generic 400 prefix error.
func TestHandleBrokerInboundRouted_RawRejectedBeforeSenderPrefixCheck(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:      tid("proj-routed-raw"),
		Slug:    "routed-raw-proj",
		Name:    "Routed Raw Test",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)
	buf := captureSlog(t)

	// Assert unchanged message/conversation counts (before/after) instead
	// of looping over persisted rows checking body text — a stronger proof
	// that the rejected send produced no row at all, not just a row with
	// different content.
	msgCountBefore, err := s.ListMessages(ctx, store.MessageFilter{}, store.ListOptions{Limit: 100, SkipTotalCount: true})
	require.NoError(t, err)
	convCountBefore := countStoreConversations(t, s, ctx)

	const secret = "SECRET-ROUTED-RAW"
	payload := routedInboundRequest{
		ProjectID: project.ID,
		Message: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Sender:    "not-a-user-prefix", // would 400 downstream if reached
			Msg:       secret,
			Type:      messages.TypeInstruction,
			Raw:       true,
		},
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound/routed", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code,
		"raw must be rejected before the sender-prefix check; body: %s", rec.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Equal(t, string(MessageDenialRawBrokerIngressUnsupported), errResp.Error.Details["reason"])

	assert.Empty(t, dispatcher.getCalls(), "rejected routed raw message must produce zero dispatch calls")
	msgsAfter, err := s.ListMessages(ctx, store.MessageFilter{}, store.ListOptions{Limit: 100, SkipTotalCount: true})
	require.NoError(t, err)
	assert.Len(t, msgsAfter.Items, len(msgCountBefore.Items), "rejected routed raw message must not persist any row")
	assert.Equal(t, convCountBefore, countStoreConversations(t, s, ctx),
		"rejected routed raw message must not create a conversation")

	// Rejected-case log/error-body secret capture for the routed inbound
	// route.
	assert.NotContains(t, rec.Body.String(), secret, "rejected routed raw content must not appear in the error response")
	assert.NotContains(t, buf.String(), secret, "rejected routed raw content must not appear in captured logs")
}

// TestHandleBrokerInboundRouted_RawRejected proves a raw message on this
// route is rejected outright, so a caller gets an explicit error.
func TestHandleBrokerInboundRouted_RawRejected(t *testing.T) {
	env := setupRoutedTestEnv(t)

	rec := env.doRoutedRequest(t, routedInboundRequest{
		ProjectID:    env.project.ID,
		DefaultAgent: "alpha",
		Message: &messages.StructuredMessage{
			Version: messages.Version,
			Channel: "slack",
			Sender:  "user:" + env.user.Email,
			Msg:     "hello",
			Type:    messages.TypeInstruction,
			Raw:     true,
		},
	})

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "body: %s", rec.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Equal(t, string(MessageDenialRawBrokerIngressUnsupported), errResp.Error.Details["reason"])
	assert.Empty(t, env.dispatcher.getCalls())
}
