package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
)

// routedTestFixture sets up a real SQLite store seeded with a channel link and
// user mapping, two httptest.Server instances acting as the hub's legacy and
// routed inbound endpoints, and a SlackBroker wired to those endpoints. Each
// test can flip routedInboundEnabled and call deliverUserMessage directly on
// the eventServer to observe which endpoint receives the request and what
// payload shape is sent.
type routedTestFixture struct {
	store  Store
	broker *SlackBroker

	// Captured payloads from the two hub endpoints.
	mu              sync.Mutex
	legacyCalls     []inboundPayload
	routedCalls     []routedInboundPayload
	legacyRawBodies []json.RawMessage
	routedRawBodies []json.RawMessage

	// Servers.
	hubServer *httptest.Server

	// Response overrides (default: 200 OK).
	routedStatus int
	routedBody   string
}

func newRoutedTestFixture(t *testing.T) *routedTestFixture {
	t.Helper()
	f := &routedTestFixture{
		store:        newTestStore(t),
		routedStatus: http.StatusOK,
	}

	// Seed channel link.
	require.NoError(t, f.store.CreateChannelLink(context.Background(), &ChannelLink{
		ChannelID:    "C-TEST",
		TeamID:       "T-TEAM",
		ProjectID:    "proj-001",
		DefaultAgent: "alpha",
		LinkedBy:     "test",
		LinkedAt:     time.Now(),
		Active:       true,
	}))

	// Seed user mapping.
	require.NoError(t, f.store.CreateUserMapping(context.Background(), &SlackUserMapping{
		SlackUserID:   "U-SENDER",
		SlackUsername: "testuser",
		ScionEmail:    "testuser@example.com",
		LinkedAt:      time.Now(),
	}))

	// Hub server that handles both legacy and routed endpoints.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/broker/inbound", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p inboundPayload
		json.Unmarshal(body, &p)
		f.mu.Lock()
		f.legacyCalls = append(f.legacyCalls, p)
		f.legacyRawBodies = append(f.legacyRawBodies, json.RawMessage(body))
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /api/v1/broker/inbound/routed", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p routedInboundPayload
		json.Unmarshal(body, &p)
		f.mu.Lock()
		f.routedCalls = append(f.routedCalls, p)
		f.routedRawBodies = append(f.routedRawBodies, json.RawMessage(body))
		status := f.routedStatus
		respBody := f.routedBody
		f.mu.Unlock()
		w.WriteHeader(status)
		if respBody != "" {
			w.Write([]byte(respBody))
		} else {
			// Default: successful delivery with primary_agent from the request.
			defaultResp := fmt.Sprintf(`{"delivered":true,"primary_agent":%q}`, p.DefaultAgent)
			w.Write([]byte(defaultResp))
		}
	})
	f.hubServer = httptest.NewServer(mux)
	t.Cleanup(f.hubServer.Close)

	// Build broker using the real Configure path so we exercise config wiring.
	f.broker = NewBroker(slog.Default())
	// Phase 1: bot_token + routed_inbound_enabled. We set socket_mode=false with
	// a signing_secret so Configure doesn't complain, and provide a temp db path.
	require.NoError(t, f.broker.Configure(map[string]string{
		"bot_token":      "xoxb-fake-token",
		"signing_secret": "fake-signing-secret",
		"db_path":        t.TempDir() + "/broker.db",
	}))

	// Inject the test store in place of the auto-created one.
	f.broker.mu.Lock()
	f.broker.store = f.store
	f.broker.mu.Unlock()

	// Phase 2: hub_url triggers event server creation with delivery functions.
	require.NoError(t, f.broker.Configure(map[string]string{
		"hub_url":   f.hubServer.URL,
		"hmac_key":  "",
		"broker_id": "",
	}))

	return f
}

// events returns the eventServer from the broker. Must be called after Configure.
func (f *routedTestFixture) events() *eventServer {
	f.broker.mu.RLock()
	defer f.broker.mu.RUnlock()
	return f.broker.events
}

// enableRouted reconfigures the eventServer for routed inbound delivery.
func (f *routedTestFixture) enableRouted() {
	es := f.events()
	es.routedInboundEnabled = true
}

// disableRouted reconfigures the eventServer for legacy inbound delivery.
func (f *routedTestFixture) disableRouted() {
	es := f.events()
	es.routedInboundEnabled = false
}

// -------------------------------------------------------------------
// Tests
// -------------------------------------------------------------------

func TestDeliverUserMessage_RoutedEnabled_CorrectPayload(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.enableRouted()

	f.events().deliverUserMessage("C-TEST", "1726099200.000100", "U-SENDER", "hello @beta what's up")

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Len(t, f.routedCalls, 1, "exactly one routed request expected")
	require.Len(t, f.legacyCalls, 0, "no legacy request expected when routed is enabled")

	p := f.routedCalls[0]
	assert.Equal(t, "proj-001", p.ProjectID)
	assert.Equal(t, "alpha", p.DefaultAgent)
	assert.NotNil(t, p.Message)
	assert.Equal(t, "hello @beta what's up", p.Message.Msg, "body must be raw — mentions not stripped by adapter")
	assert.Equal(t, "user:testuser@example.com", p.Message.Sender)
	assert.Equal(t, "U-SENDER", p.Message.SenderID)
	assert.Equal(t, "1726099200.000100", p.Message.ThreadID)
	assert.Equal(t, "slack", p.Message.Channel)
	assert.Equal(t, messages.TypeInstruction, p.Message.Type)
	assert.Equal(t, "C-TEST", p.Message.Metadata["slack_channel_id"])
	assert.Equal(t, "1726099200.000100", p.Message.Metadata["slack_thread_ts"])

	// Routed messages must NOT include project_id in metadata (it's a top-level field).
	_, hasProjectID := p.Message.Metadata["project_id"]
	assert.False(t, hasProjectID, "project_id must not appear in routed message metadata")

	// Routed messages must NOT set Recipient (hub resolves routing).
	assert.Empty(t, p.Message.Recipient, "recipient must not be set by adapter on routed path")
}

func TestDeliverUserMessage_RoutedDisabled_LegacyPayload(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.disableRouted()

	f.events().deliverUserMessage("C-TEST", "1726099200.000200", "U-SENDER", "hello legacy")

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Len(t, f.legacyCalls, 1, "exactly one legacy request expected")
	require.Len(t, f.routedCalls, 0, "no routed request expected when routed is disabled")

	p := f.legacyCalls[0]
	expectedTopic := projectcompat.AgentTopic("proj-001", "alpha")
	assert.Equal(t, expectedTopic, p.Topic)
	assert.NotNil(t, p.Message)
	assert.Equal(t, "hello legacy", p.Message.Msg)
	assert.Equal(t, "agent:alpha", p.Message.Recipient)
	assert.Equal(t, "proj-001", p.Message.Metadata["project_id"])
}

func TestDeliverUserMessage_RoutedEnabled_MentionsRemainRaw(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.enableRouted()

	// Leading mention pattern: "@gamma do something" — adapter must NOT strip or
	// resolve mentions; the hub planner does that.
	f.events().deliverUserMessage("C-TEST", "1726099200.000300", "U-SENDER", "@gamma do something")

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Len(t, f.routedCalls, 1)
	assert.Equal(t, "@gamma do something", f.routedCalls[0].Message.Msg,
		"leading mention must remain raw for hub planner")
}

func TestDeliverUserMessage_RoutedEnabled_MultipleMentionsRaw(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.enableRouted()

	// Multiple @-mentions: adapter must forward verbatim.
	text := "hey @alpha and @beta please review this @gamma"
	f.events().deliverUserMessage("C-TEST", "1726099200.000350", "U-SENDER", text)

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Len(t, f.routedCalls, 1)
	assert.Equal(t, text, f.routedCalls[0].Message.Msg,
		"all mentions must be forwarded raw to hub")
}

func TestDeliverUserMessage_RoutedError_NoLegacyFallback(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.enableRouted()

	// Make the routed endpoint return an error.
	f.mu.Lock()
	f.routedStatus = http.StatusInternalServerError
	f.routedBody = `{"error":{"code":"internal","message":"boom"}}`
	f.mu.Unlock()

	f.events().deliverUserMessage("C-TEST", "1726099200.000400", "U-SENDER", "test error path")

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Len(t, f.routedCalls, 1, "routed endpoint must be called")
	require.Len(t, f.legacyCalls, 0,
		"must NOT fall back to legacy on routed error — routed path returns early")
}

func TestDeliverUserMessage_RoutedPartialError_NoLegacyFallback(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.enableRouted()

	// 409 Conflict (e.g. agent stopped) — must NOT retry via legacy.
	f.mu.Lock()
	f.routedStatus = http.StatusConflict
	f.routedBody = `{"error":{"code":"agent_not_running","message":"primary agent is stopped"}}`
	f.mu.Unlock()

	f.events().deliverUserMessage("C-TEST", "1726099200.000450", "U-SENDER", "test partial error")

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Len(t, f.routedCalls, 1)
	require.Len(t, f.legacyCalls, 0,
		"must NOT fall back to legacy on partial error")
}

func TestDeliverUserMessage_RoutedEnabled_NoDefaultAgent_StillDelivers(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.enableRouted()

	// Create a channel link with no default agent — routed path can derive from
	// mentions, so it should still send the request.
	require.NoError(t, f.store.CreateChannelLink(context.Background(), &ChannelLink{
		ChannelID:    "C-NODEFAULT",
		TeamID:       "T-TEAM",
		ProjectID:    "proj-002",
		DefaultAgent: "", // no default
		LinkedBy:     "test",
		LinkedAt:     time.Now(),
		Active:       true,
	}))

	f.events().deliverUserMessage("C-NODEFAULT", "1726099200.000500", "U-SENDER", "@gamma help me")

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Len(t, f.routedCalls, 1, "routed path must proceed even without default agent")
	assert.Equal(t, "proj-002", f.routedCalls[0].ProjectID)
	assert.Empty(t, f.routedCalls[0].DefaultAgent)
	assert.Equal(t, "@gamma help me", f.routedCalls[0].Message.Msg)
}

func TestDeliverUserMessage_LegacyPath_NoDefaultAgent_Drops(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.disableRouted()

	// Create a channel link with no default agent — legacy path requires one.
	require.NoError(t, f.store.CreateChannelLink(context.Background(), &ChannelLink{
		ChannelID:    "C-NODEFAULT2",
		TeamID:       "T-TEAM",
		ProjectID:    "proj-003",
		DefaultAgent: "",
		LinkedBy:     "test",
		LinkedAt:     time.Now(),
		Active:       true,
	}))

	f.events().deliverUserMessage("C-NODEFAULT2", "1726099200.000600", "U-SENDER", "hello")

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Len(t, f.legacyCalls, 0, "legacy path must drop message when no default agent")
	require.Len(t, f.routedCalls, 0)
}

func TestDeliverUserMessage_RoutedEnabled_ConversationContextSaved(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.enableRouted()

	f.events().deliverUserMessage("C-TEST", "1726099200.000700", "U-SENDER", "context test")

	// Verify conversation context was persisted.
	ctx := context.Background()
	cc, err := f.store.GetConversationContext(ctx, "U-SENDER", "proj-001", "alpha")
	require.NoError(t, err)
	require.NotNil(t, cc)
	assert.Equal(t, "C-TEST", cc.LastChannelID)
	assert.Equal(t, "1726099200.000700", cc.LastThreadTS)
}

func TestDeliverUserMessage_RoutedEnabled_ContextUsesRoutedPrimary(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.enableRouted()

	// Hub returns primary_agent="beta" (leading mention overrides default "alpha").
	f.mu.Lock()
	f.routedBody = `{"delivered":true,"primary_agent":"beta"}`
	f.mu.Unlock()

	f.events().deliverUserMessage("C-TEST", "1726099200.001800", "U-SENDER", "@beta hello")

	// Context must be saved with the routed primary "beta", not the configured default "alpha".
	ctx := context.Background()
	cc, err := f.store.GetConversationContext(ctx, "U-SENDER", "proj-001", "beta")
	require.NoError(t, err)
	require.NotNil(t, cc, "context must be saved with hub's primary_agent")
	assert.Equal(t, "C-TEST", cc.LastChannelID)
	assert.Equal(t, "1726099200.001800", cc.LastThreadTS)

	// Verify no context was saved for the configured default "alpha".
	ccAlpha, err := f.store.GetConversationContext(ctx, "U-SENDER", "proj-001", "alpha")
	require.NoError(t, err)
	assert.Nil(t, ccAlpha, "context must NOT be saved for the overridden default")
}

func TestDeliverUserMessage_RoutedEnabled_ContextNoDefault_UsesPrimary(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.enableRouted()

	// Create channel with no default agent.
	require.NoError(t, f.store.CreateChannelLink(context.Background(), &ChannelLink{
		ChannelID:    "C-NODEFAULT3",
		TeamID:       "T-TEAM",
		ProjectID:    "proj-005",
		DefaultAgent: "",
		LinkedBy:     "test",
		LinkedAt:     time.Now(),
		Active:       true,
	}))

	// Hub returns primary_agent="gamma" (resolved from mention).
	f.mu.Lock()
	f.routedBody = `{"delivered":true,"primary_agent":"gamma"}`
	f.mu.Unlock()

	f.events().deliverUserMessage("C-NODEFAULT3", "1726099200.001900", "U-SENDER", "@gamma help me")

	// Context saved with hub's primary_agent even though no default was configured.
	ctx := context.Background()
	cc, err := f.store.GetConversationContext(ctx, "U-SENDER", "proj-005", "gamma")
	require.NoError(t, err)
	require.NotNil(t, cc, "context must be saved with hub's primary_agent when no default")
	assert.Equal(t, "C-NODEFAULT3", cc.LastChannelID)
}

func TestDeliverUserMessage_RoutedEnabled_ContextNotSavedOnFailure(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.enableRouted()

	// Hub returns error.
	f.mu.Lock()
	f.routedStatus = http.StatusConflict
	f.routedBody = `{"error":{"code":"agent_not_running","message":"primary agent is stopped"}}`
	f.mu.Unlock()

	f.events().deliverUserMessage("C-TEST", "1726099200.002000", "U-SENDER", "should fail")

	// No context should be saved on delivery failure.
	ctx := context.Background()
	cc, err := f.store.GetConversationContext(ctx, "U-SENDER", "proj-001", "alpha")
	require.NoError(t, err)
	assert.Nil(t, cc, "context must NOT be saved when delivery fails")
}

func TestDeliverUserMessage_RoutedEnabled_ContextNotSavedOnEmptyPrimary(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.enableRouted()

	// Hub returns delivered=true but empty primary_agent.
	f.mu.Lock()
	f.routedBody = `{"delivered":true,"primary_agent":""}`
	f.mu.Unlock()

	f.events().deliverUserMessage("C-TEST", "1726099200.002100", "U-SENDER", "empty primary")

	ctx := context.Background()
	// No context for configured default.
	cc, err := f.store.GetConversationContext(ctx, "U-SENDER", "proj-001", "alpha")
	require.NoError(t, err)
	assert.Nil(t, cc, "context must NOT be saved when primary_agent is empty")
}

func TestDeliverUserMessage_RoutedEnabled_ContextNotSavedOnDeliveredFalse(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.enableRouted()

	// Hub returns 200 but delivered=false (e.g. primary failed, all secondaries skipped).
	f.mu.Lock()
	f.routedBody = `{"delivered":false,"primary_agent":"alpha"}`
	f.mu.Unlock()

	f.events().deliverUserMessage("C-TEST", "1726099200.002200", "U-SENDER", "not delivered")

	ctx := context.Background()
	cc, err := f.store.GetConversationContext(ctx, "U-SENDER", "proj-001", "alpha")
	require.NoError(t, err)
	assert.Nil(t, cc, "context must NOT be saved when delivered=false")
}

func TestDeliverUserMessage_RoutedEnabled_ContextNotSavedOnMalformedResponse(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.enableRouted()

	// Hub returns 200 with malformed body. deliverRoutedInbound returns
	// nil + hubError (decode failure), so deliverUserMessage hits the
	// he != nil path and shows ephemeral feedback. No context saved.
	f.mu.Lock()
	f.routedBody = `not json at all`
	f.mu.Unlock()

	f.events().deliverUserMessage("C-TEST", "1726099200.002300", "U-SENDER", "malformed response")

	ctx := context.Background()
	cc, err := f.store.GetConversationContext(ctx, "U-SENDER", "proj-001", "alpha")
	require.NoError(t, err)
	assert.Nil(t, cc, "context must NOT be saved on malformed response")

	// No legacy fallback.
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Len(t, f.legacyCalls, 0, "must NOT fall back to legacy on decode error")
}

func TestDeliverUserMessage_RoutedEnabled_ContextNotSavedOnUnmarshalTypeError(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.enableRouted()

	// Hub returns 200 with a duplicate key where the second value has a wrong
	// type. json.Unmarshal may partially populate the struct (delivered=true,
	// primary_agent="alpha") before hitting the type error on the second
	// "delivered" key. deliverRoutedInbound must return nil + hubError rather
	// than a partially populated result.
	f.mu.Lock()
	f.routedBody = `{"delivered":true,"primary_agent":"alpha","delivered":"bad"}`
	f.mu.Unlock()

	f.events().deliverUserMessage("C-TEST", "1726099200.002500", "U-SENDER", "type error response")

	ctx := context.Background()
	// Despite partial decode having delivered=true and primary_agent="alpha",
	// the decode error must prevent context save.
	cc, err := f.store.GetConversationContext(ctx, "U-SENDER", "proj-001", "alpha")
	require.NoError(t, err)
	assert.Nil(t, cc, "context must NOT be saved when response has UnmarshalTypeError")

	// No legacy fallback or retry.
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Len(t, f.legacyCalls, 0, "must NOT fall back to legacy on type error")
}

func TestDeliverUserMessage_RoutedEnabled_ContextNotSavedOnEmptyObjectResponse(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.enableRouted()

	// Hub returns 200 with an empty JSON object — valid JSON but missing
	// delivered and primary_agent (both default to zero values: false/"").
	f.mu.Lock()
	f.routedBody = `{}`
	f.mu.Unlock()

	f.events().deliverUserMessage("C-TEST", "1726099200.002400", "U-SENDER", "empty object response")

	ctx := context.Background()
	cc, err := f.store.GetConversationContext(ctx, "U-SENDER", "proj-001", "alpha")
	require.NoError(t, err)
	assert.Nil(t, cc, "context must NOT be saved when hub returns empty JSON object")
}

func TestDeliverUserMessage_UnknownChannel_NoCalls(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.enableRouted()

	// Channel not linked — should silently return.
	f.events().deliverUserMessage("C-UNKNOWN", "1726099200.000800", "U-SENDER", "hello")

	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Len(t, f.routedCalls, 0)
	assert.Len(t, f.legacyCalls, 0)
}

func TestDeliverUserMessage_UnregisteredUser_NoCalls(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.enableRouted()

	// User not mapped — should silently return (and try to post ephemeral, which
	// will fail silently since we have a fake Slack client).
	f.events().deliverUserMessage("C-TEST", "1726099200.000900", "U-UNKNOWN", "hello")

	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Len(t, f.routedCalls, 0)
	assert.Len(t, f.legacyCalls, 0)
}

func TestDeliverUserMessage_InactiveChannel_NoCalls(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.enableRouted()

	require.NoError(t, f.store.CreateChannelLink(context.Background(), &ChannelLink{
		ChannelID:    "C-INACTIVE",
		TeamID:       "T-TEAM",
		ProjectID:    "proj-004",
		DefaultAgent: "alpha",
		LinkedBy:     "test",
		LinkedAt:     time.Now(),
		Active:       false,
	}))

	f.events().deliverUserMessage("C-INACTIVE", "1726099200.001000", "U-SENDER", "hello")

	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Len(t, f.routedCalls, 0)
	assert.Len(t, f.legacyCalls, 0)
}

// TestConfigureRoutedInboundEnabled verifies the config path: the
// routed_inbound_enabled setting is correctly plumbed from Configure into the
// eventServer.
func TestConfigureRoutedInboundEnabled(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		expected bool
	}{
		{"true", "true", true},
		{"one", "1", true},
		{"false", "false", false},
		{"empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewBroker(slog.Default())
			cfg := map[string]string{
				"bot_token":              "xoxb-test",
				"signing_secret":         "secret",
				"db_path":                t.TempDir() + "/test.db",
				"routed_inbound_enabled": tt.value,
			}
			require.NoError(t, b.Configure(cfg))

			// Phase 2 — triggers eventServer creation.
			require.NoError(t, b.Configure(map[string]string{
				"hub_url": "http://localhost:9999",
			}))

			b.mu.RLock()
			es := b.events
			b.mu.RUnlock()
			require.NotNil(t, es, "eventServer must be created after phase 2")
			assert.Equal(t, tt.expected, es.routedInboundEnabled)

			b.Close()
		})
	}
}

// TestDeliverUserMessage_RoutedEnabled_HTTPPayloadShape validates the raw JSON
// body shape sent to the routed endpoint — ensuring the adapter does not inject
// fields that belong to the legacy path (e.g. "topic") or leak metadata that
// the hub should set (e.g. "recipient").
func TestDeliverUserMessage_RoutedEnabled_HTTPPayloadShape(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.enableRouted()

	f.events().deliverUserMessage("C-TEST", "1726099200.001100", "U-SENDER", "payload shape test")

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Len(t, f.routedRawBodies, 1)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(f.routedRawBodies[0], &raw))

	// Must have these top-level fields.
	assert.Contains(t, raw, "project_id")
	assert.Contains(t, raw, "default_agent")
	assert.Contains(t, raw, "message")

	// Must NOT have legacy "topic" field.
	assert.NotContains(t, raw, "topic", "routed payload must not contain legacy 'topic' field")

	// Inspect inner message.
	var msg map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw["message"], &msg))

	// Message must not have a "recipient" set.
	if recipientRaw, ok := msg["recipient"]; ok {
		var recipient string
		json.Unmarshal(recipientRaw, &recipient)
		assert.Empty(t, recipient, "recipient must be empty on routed path")
	}
}

// TestDeliverUserMessage_LegacyPath_HTTPPayloadShape validates that the legacy
// path sends the expected "topic" + "message" shape.
func TestDeliverUserMessage_LegacyPath_HTTPPayloadShape(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.disableRouted()

	f.events().deliverUserMessage("C-TEST", "1726099200.001200", "U-SENDER", "legacy shape test")

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Len(t, f.legacyRawBodies, 1)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(f.legacyRawBodies[0], &raw))

	// Must have legacy fields.
	assert.Contains(t, raw, "topic")
	assert.Contains(t, raw, "message")

	// Must NOT have routed fields.
	assert.NotContains(t, raw, "project_id",
		"legacy payload must not contain 'project_id' top-level field")
	assert.NotContains(t, raw, "default_agent",
		"legacy payload must not contain 'default_agent' top-level field")
}

// TestDeliverUserMessage_SenderFallback_Legacy verifies that on the legacy path,
// when the user mapping has no scion_email, the sender field falls back to
// "slack:<username>". The legacy hub handleBrokerInbound uses the broker
// identity for non-"user:" senders, and authorizeAgentMessage denies the
// broker identity type — so this message is also denied on legacy, but it
// reaches the hub (the adapter doesn't guard against it).
func TestDeliverUserMessage_SenderFallback_Legacy(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.disableRouted()

	require.NoError(t, f.store.CreateUserMapping(context.Background(), &SlackUserMapping{
		SlackUserID:   "U-NOEMAIL",
		SlackUsername: "slackonly",
		ScionEmail:    "",
		LinkedAt:      time.Now(),
	}))

	f.events().deliverUserMessage("C-TEST", "1726099200.001300", "U-NOEMAIL", "no email")

	f.mu.Lock()
	defer f.mu.Unlock()

	// Legacy path sends the message — the hub will deny it via authorizeAgentMessage
	// since "slack:" is not a "user:" sender.
	require.Len(t, f.legacyCalls, 1)
	assert.Equal(t, "slack:slackonly", f.legacyCalls[0].Message.Sender)
}

// TestDeliverUserMessage_RoutedNoEmail_BlockedBeforeHub verifies that on the
// routed path, when the user mapping has no scion_email, the adapter blocks
// the message before reaching the hub and shows an ephemeral registration
// prompt. The hub requires "user:<email>" sender format and would reject
// "slack:<username>" with 400.
func TestDeliverUserMessage_RoutedNoEmail_BlockedBeforeHub(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.enableRouted()

	require.NoError(t, f.store.CreateUserMapping(context.Background(), &SlackUserMapping{
		SlackUserID:   "U-NOEMAIL2",
		SlackUsername: "slackonly2",
		ScionEmail:    "",
		LinkedAt:      time.Now(),
	}))

	f.events().deliverUserMessage("C-TEST", "1726099200.001400", "U-NOEMAIL2", "blocked sender")

	f.mu.Lock()
	defer f.mu.Unlock()

	// No hub call — the adapter blocks the message before delivery.
	assert.Len(t, f.routedCalls, 0,
		"routed endpoint must NOT be called for email-empty user")
	assert.Len(t, f.legacyCalls, 0,
		"legacy endpoint must NOT be called when routed is enabled")
	// The adapter posts an ephemeral to the user via s.client.PostEphemeral.
	// We can't easily assert the ephemeral content without a mock Slack client,
	// but the absence of hub calls proves the guard fired.
}

// TestDeliverUserMessage_RoutedMixedResults_NoRetry verifies that when the hub
// returns 200 with delivered=true and a response body containing a delivered
// primary and an unauthorized secondary, the adapter treats it as success
// (single request, no retry, no legacy fallback).
func TestDeliverUserMessage_RoutedMixedResults_NoRetry(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.enableRouted()

	// Hub returns 200 with mixed results: primary delivered, secondary unauthorized.
	f.mu.Lock()
	f.routedStatus = http.StatusOK
	f.routedBody = `{
		"delivered": true,
		"primary_agent": "alpha",
		"results": [
			{"agent_slug": "alpha", "type": "message", "status": "delivered", "message_id": "msg-001"},
			{"agent_slug": "beta", "type": "mention", "status": "unauthorized", "error": "not a project member"}
		],
		"unresolved_mentions": []
	}`
	f.mu.Unlock()

	f.events().deliverUserMessage("C-TEST", "1726099200.001500", "U-SENDER", "hello @beta review this")

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Len(t, f.routedCalls, 1, "exactly one routed request — no retry on mixed results")
	require.Len(t, f.legacyCalls, 0, "no legacy fallback on mixed results")

	// Verify the adapter sent the raw message including the mention.
	assert.Equal(t, "hello @beta review this", f.routedCalls[0].Message.Msg)
	assert.Equal(t, "alpha", f.routedCalls[0].DefaultAgent)
}

// TestDeliverUserMessage_RoutedTransportFailure_NoLegacyFallback proves that
// when the routed hub endpoint is unreachable (transport error), the adapter
// makes exactly one attempt and does NOT fall back to legacy delivery.
// Uses a closed server for instant connection-refused — no 6-minute wait.
func TestDeliverUserMessage_RoutedTransportFailure_NoLegacyFallback(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.enableRouted()

	// Close the hub server to force transport error (connection refused).
	f.hubServer.Close()

	f.events().deliverUserMessage("C-TEST", "1726099200.001600", "U-SENDER", "transport fail")

	f.mu.Lock()
	defer f.mu.Unlock()

	// The routed call never reaches the handler (server is closed), so no
	// routedCalls are captured. The key assertion: no legacy fallback either.
	assert.Len(t, f.routedCalls, 0, "routed handler never reached (server closed)")
	assert.Len(t, f.legacyCalls, 0,
		"must NOT fall back to legacy on transport failure")
}

// TestDeliverUserMessage_RoutedTimeout_NoLegacyFallback proves that when the
// routed hub endpoint hangs and a bounded transport times out, the adapter makes
// one attempt and does NOT fall back to legacy. Uses a custom deliverRoutedInbound
// with a short-timeout transport to avoid the production 6-minute wait.
func TestDeliverUserMessage_RoutedTimeout_NoLegacyFallback(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.enableRouted()

	// Track whether the custom delivery function was called.
	var called int
	var calledMu sync.Mutex

	// Create a hub that hangs on routed requests. The handler blocks until the
	// client gives up (200ms) or until cleanupDone fires during test cleanup.
	cleanupDone := make(chan struct{})
	slowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-cleanupDone:
		}
	}))
	t.Cleanup(func() {
		close(cleanupDone)
		slowServer.Close()
	})

	// Override the eventServer's deliverRoutedInbound with a version that uses
	// a short timeout — consistent with the production code path but bounded for
	// testing.
	es := f.events()
	es.deliverRoutedInbound = func(projectID, defaultAgent string, msg *messages.StructuredMessage) (*routedInboundResult, *hubError) {
		calledMu.Lock()
		called++
		calledMu.Unlock()

		payload := routedInboundPayload{
			ProjectID:    projectID,
			DefaultAgent: defaultAgent,
			Message:      msg,
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, nil
		}

		// Short timeout: 200ms instead of 6 minutes.
		client := &http.Client{Timeout: 200 * time.Millisecond}
		req, _ := http.NewRequest("POST", slowServer.URL+"/api/v1/broker/inbound/routed",
			bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			// Transport/timeout error — matches O-5 production behavior:
			// surface transport failures so the caller can show ephemeral feedback.
			return nil, &hubError{Code: "transport_error", Message: "Message delivery could not be confirmed — the service may be temporarily unavailable."}
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			return nil, parseHubError(resp)
		}
		io.Copy(io.Discard, resp.Body)
		return &routedInboundResult{Delivered: true, PrimaryAgent: defaultAgent}, nil
	}

	f.events().deliverUserMessage("C-TEST", "1726099200.001700", "U-SENDER", "timeout test")

	calledMu.Lock()
	defer calledMu.Unlock()

	assert.Equal(t, 1, called, "routed delivery must be attempted exactly once")

	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Len(t, f.legacyCalls, 0,
		"must NOT fall back to legacy on timeout")
}

// --- R-1 regression: context save after expired preflight context ---

// TestDeliverUserMessage_RoutedEnabled_ContextSavedAfterPreflightExpiry is
// the regression test for R-1: the 10-second preflight context created in
// deliverUserMessage must NOT be reused for the store.SetConversationContext
// call after deliverRoutedInbound returns. deliverRoutedInbound uses its own
// 6-minute http.Client timeout, so the original context will be expired on
// slow hub responses.
//
// This test shortens the preflight timeout to 50ms and replaces the delivery
// function with one that sleeps 150ms (outliving the preflight context),
// then returns a successful delivery. The fix creates a fresh bounded
// context for the store call, so the save must succeed despite the expired
// parent.
func TestDeliverUserMessage_RoutedEnabled_ContextSavedAfterPreflightExpiry(t *testing.T) {
	f := newRoutedTestFixture(t)
	f.enableRouted()

	es := f.events()

	// Shorten the preflight timeout to 50ms so we don't wait 10s in the test.
	es.preflightTimeout = 50 * time.Millisecond

	// Override deliverRoutedInbound to simulate a slow hub response that
	// outlives the short preflight context. deliverRoutedInbound in production
	// creates its own http.Client (6-min timeout) and does NOT propagate the
	// parent ctx to the HTTP call, so the request itself succeeds even though
	// the parent ctx is long expired by return time.
	origDeliver := es.deliverRoutedInbound
	es.deliverRoutedInbound = func(projectID, defaultAgent string, msg *messages.StructuredMessage) (*routedInboundResult, *hubError) {
		// Sleep longer than the 50ms preflight context.
		time.Sleep(150 * time.Millisecond)
		return origDeliver(projectID, defaultAgent, msg)
	}

	f.events().deliverUserMessage("C-TEST", "1726099200.009900", "U-SENDER", "preflight expiry regression")

	// The context save must succeed despite the expired parent ctx, because
	// deliverUserMessage now creates a fresh bounded context for the store call.
	checkCtx := context.Background()
	cc, err := f.store.GetConversationContext(checkCtx, "U-SENDER", "proj-001", "alpha")
	require.NoError(t, err)
	require.NotNil(t, cc, "conversation context must be saved even when preflight context is expired (R-1 regression)")
	assert.Equal(t, "C-TEST", cc.LastChannelID)
	assert.Equal(t, "1726099200.009900", cc.LastThreadTS)
}
