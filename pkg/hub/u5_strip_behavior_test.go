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

// ---------------------------------------------------------------------------
// Impl review r1, finding 2 (U5(a)-(c), design auto-offload-large-dm §12):
// behavioral strip tests, one per §4.2 item 1 site. TestReservedKeyStripCoverage
// (reserved_metadata_strip_test.go) is U5(d): it only checks that every site
// is CLASSIFIED. These tests check that a listed site actually STRIPS.
//
// For most sites below, deleting the strip line at that site fails the
// matching test here. Two sites are the exception (impl review r2, finding
// 1): TestU5a_ChatV2SendAgentRouted_StripsReservedMetadata and
// TestU5a_ProcessMentions_StripsReservedMetadata pin the AC9 *outcome* —
// no reserved key reaches the dispatched output — but a mutation that
// deletes their strip call does NOT fail them, because a different,
// pre-existing mechanism at that same site independently removes the same
// keys: chat v2's allowedClientMetadataKeys allowlist (only "RE-to" passes
// client metadata through at all) and messages.NewMention's fresh-metadata
// construction (mention_source/mention_position only, never copied from
// originalMsg). Both are called out inline at their own test. The strip
// calls at those two sites remain defence in depth, not the only barrier.
//
// (b) (hub values override client values on an offload) and (c) (other
// client metadata passes through) are properties of the shared
// messaging.StripReservedMetadata/OffloadForDelivery mechanism, not of any
// one site's wiring — they are exhaustively unit-tested in
// pkg/messaging/offload_test.go (TestStripReservedMetadata_NewMapWhenReservedPresent,
// TestOffloadForDelivery_StubContentAndBounds) and exercised end-to-end once
// in TestOffload_ClientSuppliedReservedMetadataAlwaysStripped (offload_integration_test.go).
// This file's job is (a): per-site coverage, so a deleted strip line is caught
// (except the two documented exceptions above).
// ---------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// spoofedReservedMetadata is the client/plugin-supplied metadata every test
// below tries to sneak through: the 3 reserved keys, plus one harmless key
// to confirm ordinary metadata is unaffected.
func spoofedReservedMetadata() map[string]string {
	return map[string]string{
		messaging.MetaBodyOffloaded: "true",
		messaging.MetaBodyChars:     "999999",
		messaging.MetaBodySHA256:    "deadbeef",
		"harmless":                  "kept",
	}
}

// assertReservedStripped checks both that the reserved keys are absent from
// msg's Metadata map and that their spoofed values never reached either
// rendering of the message (r2 finding 2): checking only the metadata map
// would miss a bug where a render call was moved ahead of the strip call,
// since the map check would still pass while the rendered text leaked the
// spoofed values.
func assertReservedStripped(t *testing.T, msg *messages.StructuredMessage) {
	t.Helper()
	md := msg.Metadata
	_, hasOffloaded := md[messaging.MetaBodyOffloaded]
	_, hasChars := md[messaging.MetaBodyChars]
	_, hasSHA := md[messaging.MetaBodySHA256]
	assert.False(t, hasOffloaded, "client-supplied body_offloaded must be stripped")
	assert.False(t, hasChars, "client-supplied body_chars must be stripped")
	assert.False(t, hasSHA, "client-supplied body_sha256 must be stripped")
	assert.Equal(t, "kept", md["harmless"], "non-reserved client metadata is unaffected")

	// r3 finding 1: require.NotEmpty here so this check cannot silently go
	// vacuous again — assert.NotContains("", x) always passes, which is
	// exactly how r2's version of this check went undetected until r3.
	require.NotEmpty(t, msg.DeliveryText, "test setup must enable the envelope switch so this check exercises real rendered content")
	assert.NotContains(t, msg.DeliveryText, "deadbeef", "spoofed body_sha256 value must not reach the rendered (switch-ON) envelope")
	assert.NotContains(t, msg.DeliveryText, "999999", "spoofed body_chars value must not reach the rendered (switch-ON) envelope")

	// deliveryMetadataAllowlist (pkg/messages/format.go) deliberately lets
	// body_* through the switch-OFF (legacy) renderer so it can show a real
	// offload marker — which means a failed strip would leak the spoofed
	// values here too. Render the already-captured message through the
	// legacy path directly to cover that renderer as well.
	legacy := messages.FormatForDelivery(msg)
	assert.NotContains(t, legacy, "deadbeef", "spoofed body_sha256 value must not reach the switch-OFF (legacy) rendering")
	assert.NotContains(t, legacy, "999999", "spoofed body_chars value must not reach the switch-OFF (legacy) rendering")
}

// U5(a): handleGroupMessage. The recipient's dispatched copy aliases msg's
// (stripped) Metadata map — deleting the strip line in handleGroupMessage
// would let this leak through.
func TestU5a_HandleGroupMessage_StripsReservedMetadata(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	// r3 finding 1: without the envelope switch on, DeliveryText is never
	// rendered, and the NotContains checks in assertReservedStripped run
	// against an empty string (vacuously true). Turn it on so those checks
	// actually exercise the rendered envelope.
	enableOffload(t, srv, 0, true)

	project := &store.Project{ID: tid("u5a-group-project"), Name: "u5a-group", Slug: "u5a-group"}
	require.NoError(t, s.CreateProject(ctx, project))
	broker := &store.RuntimeBroker{ID: tid("u5a-group-broker"), Name: "b", Slug: "b", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	agentA := &store.Agent{ID: tid("u5a-group-a"), Name: "a", Slug: "a", ProjectID: project.ID, RuntimeBrokerID: broker.ID, Phase: "running"}
	require.NoError(t, s.CreateAgent(ctx, agentA))
	agentB := &store.Agent{ID: tid("u5a-group-b"), Name: "b", Slug: "b", ProjectID: project.ID, RuntimeBrokerID: broker.ID, Phase: "running"}
	require.NoError(t, s.CreateAgent(ctx, agentB))
	user := &store.User{ID: tid("u5a-group-user"), Email: "u5a-group@test.example"}
	require.NoError(t, s.CreateUser(ctx, user))

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	rr := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/agents/"+agentA.Slug+"/message",
		MessageRequest{
			StructuredMessage: &messages.StructuredMessage{
				Version:   messages.Version,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Sender:    "user:" + user.Email,
				SenderID:  user.ID,
				Recipient: "group[agent:" + agentA.Slug + ",agent:" + agentB.Slug + "]",
				Msg:       "group message with spoofed metadata",
				Type:      messages.TypeInstruction,
				Metadata:  spoofedReservedMetadata(),
			},
		})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	callsA := dispatchesTo(dispatcher, agentA.ID)
	require.Len(t, callsA, 1)
	assertReservedStripped(t, callsA[0].StructuredMessage)

	callsB := dispatchesTo(dispatcher, agentB.ID)
	require.Len(t, callsB, 1)
	assertReservedStripped(t, callsB[0].StructuredMessage)
}

// U5(a): broadcastDirect. Same aliasing pattern as handleGroupMessage.
func TestU5a_BroadcastDirect_StripsReservedMetadata(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	// r3 finding 1: see the identical comment in TestU5a_HandleGroupMessage.
	enableOffload(t, srv, 0, true)

	project := &store.Project{ID: tid("u5a-bcast-project"), Name: "u5a-bcast", Slug: "u5a-bcast"}
	require.NoError(t, s.CreateProject(ctx, project))
	user := &store.User{ID: tid("u5a-bcast-user"), Email: "u5a-bcast@test.example", Role: store.UserRoleMember, Status: "active"}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)
	project.CreatedBy = user.ID
	require.NoError(t, s.UpdateProject(ctx, project))
	srv.createProjectMembersGroup(ctx, project)

	agent := &store.Agent{ID: tid("u5a-bcast-agent"), Name: "agent", Slug: "agent", ProjectID: project.ID, Phase: "running"}
	require.NoError(t, s.CreateAgent(ctx, agent))

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher) // no message broker proxy set -> broadcastDirect branch

	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Type:      messages.TypeInstruction,
		Msg:       "broadcast with spoofed metadata",
		Metadata:  spoofedReservedMetadata(),
	}
	body, err := json.Marshal(BroadcastMessageRequest{StructuredMessage: msg})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+project.ID+"/broadcast", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(),
		NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, "user", "web")))

	rr := httptest.NewRecorder()
	srv.handleProjectBroadcast(rr, req, project.ID)
	require.True(t, rr.Code >= 200 && rr.Code < 300, "expected 2xx, got %d: %s", rr.Code, rr.Body.String())

	calls := dispatchesTo(dispatcher, agent.ID)
	require.Len(t, calls, 1)
	assertReservedStripped(t, calls[0].StructuredMessage)
}

// U5(a): handleBrokerInbound. Plugin-relayed messages carry plugin-supplied
// metadata that must never reach the agent as a genuine offload marker.
func TestU5a_HandleBrokerInbound_StripsReservedMetadata(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	// r3 finding 1: see the identical comment in TestU5a_HandleGroupMessage.
	enableOffload(t, srv, 0, true)

	broker := &store.RuntimeBroker{ID: tid("u5a-inbound-broker"), Name: "b", Slug: "b", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{ID: tid("u5a-inbound-project"), Name: "u5a-inbound", Slug: "u5a-inbound"}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroup(ctx, project)
	target := &store.Agent{
		ID: tid("u5a-inbound-target"), Name: "target", Slug: "target",
		ProjectID: project.ID, Phase: "running", RuntimeBrokerID: broker.ID,
		MessageMode: store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, target))
	senderUser := &store.User{
		ID: tid("u5a-inbound-user"), Email: "u5a-inbound@test.example",
		DisplayName: "Sender", Role: "member", Status: store.UserStatusActive,
	}
	require.NoError(t, s.CreateUser(ctx, senderUser))
	ensureHubMembership(ctx, s, senderUser.ID)
	msgAuthzAddProjectMember(t, s, senderUser.ID, project.ID, project.Slug, store.GroupMemberRoleMember)
	msgAuthzGrantAgentMessage(t, s, senderUser.ID, project.ID)

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	reqBody := inboundMessageRequest{
		Topic: "scion.project." + project.ID + ".agent." + target.Slug + ".messages",
		Message: &messages.StructuredMessage{
			Sender: "user:" + senderUser.Email, Recipient: "agent:" + target.Slug,
			Msg: "plugin-relayed with spoofed metadata", Type: messages.TypeInstruction,
			Metadata: spoofedReservedMetadata(),
		},
	}
	bodyBytes, err := json.Marshal(reqBody)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity(broker.ID)))
	rec := httptest.NewRecorder()
	srv.handleBrokerInbound(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	calls := dispatchesTo(dispatcher, target.ID)
	require.Len(t, calls, 1)
	assertReservedStripped(t, calls[0].StructuredMessage)
}

// U5(a): dispatchRoutedRecipient. The existing hub-owned-key exclusion
// switch does not exclude body_*, so the dedicated strip call is the only
// thing stopping a plugin from spoofing it here.
func TestU5a_DispatchRoutedRecipient_StripsReservedMetadata(t *testing.T) {
	env := setupRoutedTestEnv(t)

	rec := env.doRoutedRequest(t, routedInboundRequest{
		ProjectID:    env.project.ID,
		DefaultAgent: "alpha",
		Message: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Channel:   "slack",
			Sender:    "user:" + env.user.Email,
			Msg:       "routed with spoofed metadata",
			Type:      messages.TypeInstruction,
			Metadata:  spoofedReservedMetadata(),
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// r2 finding 4: require at least one call to the expected agent with a
	// non-nil StructuredMessage, rather than only requiring the overall call
	// list be non-empty — the previous form could pass vacuously if every
	// call had a nil StructuredMessage.
	calls := dispatchesTo(env.dispatcher, env.agent1.ID)
	require.NotEmpty(t, calls, "expected at least one dispatch to the routed default agent (alpha)")
	for _, c := range calls {
		require.NotNil(t, c.StructuredMessage)
		assertReservedStripped(t, c.StructuredMessage)
	}
}

// U5(a): MessageBrokerProxy.deliverToAgent. Called directly (as a plugin
// publish would reach it through the event bus) with plugin-supplied
// metadata.
func TestU5a_DeliverToAgent_StripsReservedMetadata(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("u5a-deliver-project"), Name: "u5a-deliver", Slug: "u5a-deliver"}
	require.NoError(t, s.CreateProject(ctx, project))
	broker := &store.RuntimeBroker{ID: tid("u5a-deliver-broker"), Name: "b", Slug: "b", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	agent := &store.Agent{ID: tid("u5a-deliver-agent"), Name: "agent", Slug: "agent", ProjectID: project.ID, RuntimeBrokerID: broker.ID, Phase: "running"}
	require.NoError(t, s.CreateAgent(ctx, agent))

	dispatcher := &recordingDispatcher{}
	inproc := eventbus.NewInProcessEventBus(slog.Default())
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{{Name: eventbus.InProcessBusName, Bus: inproc}}, slog.Default())
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	proxy := NewMessageBrokerProxy(fanout, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
	// r3 finding 1: this proxy is built by hand and never receives the
	// writeDenyEnabled hook server.go:3084 wires up in production, so
	// DeliveryText would never render here even with enableOffload. Set it
	// directly, matching delivery_text_system_test.go's pattern for the
	// notification dispatcher.
	proxy.writeDenyEnabled = func() bool { return true }
	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)

	msg := &messages.StructuredMessage{
		Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender: "agent:someone", Recipient: "agent:" + agent.Slug,
		Msg: "plugin-published with spoofed metadata", Type: messages.TypeInstruction,
		Metadata: spoofedReservedMetadata(),
	}
	proxy.deliverToAgent(ctx, project.ID, agent.Slug, msg)

	calls := dispatchesTo(dispatcher, agent.ID)
	require.Len(t, calls, 1)
	assertReservedStripped(t, calls[0].StructuredMessage)

	// The original event-bus msg pointer must not have been mutated in
	// place (r1 FYI: messagebroker.go always copies before touching it).
	assert.Equal(t, "true", msg.Metadata[messaging.MetaBodyOffloaded],
		"the caller's own msg object must be untouched")
}

// U5(a): chat v2 sendAgentRouted primary. allowedClientMetadataKeys already
// excludes body_* from client-supplied metadata (only "RE-to" passes), so
// this is defence in depth rather than the only barrier — but AC9 asks for
// the dispatched-output invariant regardless of which mechanism enforces it.
func TestU5a_ChatV2SendAgentRouted_StripsReservedMetadata(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	ctx := context.Background()
	// r3 finding 1: see the identical comment in TestU5a_HandleGroupMessage.
	enableOffload(t, srv, 0, true)

	agent := &store.Agent{
		ID: tid("u5a-chatv2-agent"), ProjectID: proj.ID, Name: "Helper", Slug: "helper",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	topicID := tid("u5a-chatv2-topic")
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: proj.ID, Name: "agent-thread", CreatedBy: "dev",
		CreatedAt: time.Now().UTC(), DefaultAgent: agent.ID,
	}))
	setTopicConversationID(t, db, s, topicID, proj.ID)

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	body := map[string]interface{}{
		"content":  "please help, with spoofed metadata",
		"metadata": spoofedReservedMetadata(),
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages", body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	calls := dispatchesTo(dispatcher, agent.ID)
	require.Len(t, calls, 1)
	// Not assertReservedStripped: chat v2's allowedClientMetadataKeys allows
	// only "RE-to", so "harmless" is dropped here too — that is this site's
	// own pre-existing, unrelated behavior, not something this PR changes.
	// The property this test actually pins is AC9's outcome: no reserved key
	// reaches the dispatched output.
	msg := calls[0].StructuredMessage
	md := msg.Metadata
	_, hasOffloaded := md[messaging.MetaBodyOffloaded]
	_, hasChars := md[messaging.MetaBodyChars]
	_, hasSHA := md[messaging.MetaBodySHA256]
	assert.False(t, hasOffloaded, "client-supplied body_offloaded must be stripped")
	assert.False(t, hasChars, "client-supplied body_chars must be stripped")
	assert.False(t, hasSHA, "client-supplied body_sha256 must be stripped")

	// r2 finding 2: also check the rendered output, not just the metadata
	// map (see assertReservedStripped's doc comment for why).
	// r3 finding 1: require.NotEmpty first — see assertReservedStripped.
	require.NotEmpty(t, msg.DeliveryText, "test setup must enable the envelope switch so this check exercises real rendered content")
	assert.NotContains(t, msg.DeliveryText, "deadbeef", "spoofed body_sha256 value must not reach the rendered (switch-ON) envelope")
	assert.NotContains(t, msg.DeliveryText, "999999", "spoofed body_chars value must not reach the rendered (switch-ON) envelope")
	legacy := messages.FormatForDelivery(msg)
	assert.NotContains(t, legacy, "deadbeef", "spoofed body_sha256 value must not reach the switch-OFF (legacy) rendering")
	assert.NotContains(t, legacy, "999999", "spoofed body_chars value must not reach the switch-OFF (legacy) rendering")
}

// U5(a): processMentions. messages.NewMention builds each mention's own
// fresh metadata (mention_source, mention_position only — never copied from
// originalMsg), so this is inherently a no-leak path; the strip calls are
// defence in depth. This test pins the actual guarantee AC9 cares about:
// no reserved key ever reaches a human/broker-sender mention recipient.
func TestU5a_ProcessMentions_StripsReservedMetadata(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	// r3 finding 1: see the identical comment in TestU5a_HandleGroupMessage.
	enableOffload(t, srv, 0, true)

	project := &store.Project{ID: tid("u5a-mentions-project"), Name: "u5a-mentions", Slug: "u5a-mentions"}
	require.NoError(t, s.CreateProject(ctx, project))
	primary := &store.Agent{ID: tid("u5a-mentions-primary"), Name: "primary", Slug: "primary", ProjectID: project.ID, Phase: "running", RuntimeBrokerID: "broker"}
	require.NoError(t, s.CreateAgent(ctx, primary))
	mentioned := &store.Agent{ID: tid("u5a-mentions-mentioned"), Name: "mentioned", Slug: "mentioned", ProjectID: project.ID, Phase: "running", RuntimeBrokerID: "broker"}
	require.NoError(t, s.CreateAgent(ctx, mentioned))

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	rr := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/agents/"+primary.Slug+"/message",
		MessageRequest{
			StructuredMessage: &messages.StructuredMessage{
				Version:   messages.Version,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Sender:    "user:dev",
				Recipient: "agent:" + primary.Slug,
				Msg:       "hello @" + mentioned.Slug,
				Type:      messages.TypeInstruction,
				Metadata:  spoofedReservedMetadata(),
			},
			Mentions: []string{mentioned.Slug},
		})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	calls := dispatchesTo(dispatcher, mentioned.ID)
	require.Len(t, calls, 1)
	require.NotNil(t, calls[0].StructuredMessage)
	// Not assertReservedStripped: messages.NewMention builds an entirely
	// fresh metadata map (mention_source/mention_position only) and never
	// copies originalMsg's metadata at all — "harmless" doesn't survive
	// either, which is expected and unrelated to this PR. The property
	// pinned here is AC9's outcome: no reserved key reaches the mention
	// recipient, and the mention's metadata is exactly the two mention keys.
	msg := calls[0].StructuredMessage
	md := msg.Metadata
	_, hasOffloaded := md[messaging.MetaBodyOffloaded]
	_, hasChars := md[messaging.MetaBodyChars]
	_, hasSHA := md[messaging.MetaBodySHA256]
	assert.False(t, hasOffloaded, "client-supplied body_offloaded must be stripped")
	assert.False(t, hasChars, "client-supplied body_chars must be stripped")
	assert.False(t, hasSHA, "client-supplied body_sha256 must be stripped")
	assert.Equal(t, map[string]string{"mention_source": "agent:" + primary.Slug, "mention_position": "body"}, md,
		"a mention's metadata must be exactly NewMention's own two keys, never anything from originalMsg")

	// r2 finding 2: also check the rendered output, not just the metadata
	// map (see assertReservedStripped's doc comment for why).
	// r3 finding 1: require.NotEmpty first — see assertReservedStripped.
	require.NotEmpty(t, msg.DeliveryText, "test setup must enable the envelope switch so this check exercises real rendered content")
	assert.NotContains(t, msg.DeliveryText, "deadbeef", "spoofed body_sha256 value must not reach the rendered (switch-ON) envelope")
	assert.NotContains(t, msg.DeliveryText, "999999", "spoofed body_chars value must not reach the rendered (switch-ON) envelope")
	legacy := messages.FormatForDelivery(msg)
	assert.NotContains(t, legacy, "deadbeef", "spoofed body_sha256 value must not reach the switch-OFF (legacy) rendering")
	assert.NotContains(t, legacy, "999999", "spoofed body_chars value must not reach the switch-OFF (legacy) rendering")
}

// U5(a) coverage note — outbound and #2083 fan-out: both route through
// ExecuteAgentDM, whose single strip call (agent_dm_operation.go) is already
// covered end-to-end by TestOffload_ClientSuppliedReservedMetadataAlwaysStripped
// (the /message agent-fork branch) and TestOffload_I1_OutboundEndpoint_StubDispatched
// (the /outbound-message branch reaches the same strip line — there is only
// one ExecuteAgentDM, so a separate outbound-specific metadata test would
// exercise identical code).
