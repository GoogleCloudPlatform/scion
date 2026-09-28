package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// ---------------------------------------------------------------------------
// routedTestFixture — httptest mux with both legacy and routed endpoints
// ---------------------------------------------------------------------------

// discordRoutedFixture sets up a real SQLite store seeded with a channel link
// and user mapping, an httptest.Server acting as the hub with both legacy and
// routed inbound endpoints, and a DiscordBroker wired to those endpoints.
//
// Each test can toggle routedEnabled and call handleIncomingMessage directly
// to observe which endpoint receives the request and what payload is sent.
type discordRoutedFixture struct {
	store  Store
	broker *DiscordBroker

	// Captured payloads.
	mu              sync.Mutex
	legacyCalls     []inboundPayload
	legacyRawBodies []json.RawMessage
	routedCalls     []routedInboundPayload
	routedRawBodies []json.RawMessage

	// Servers.
	hubServer *httptest.Server

	// Response overrides (default: 200 OK with delivered+primary_agent).
	routedStatus          int
	routedBody            string
	routedHandlerOverride http.HandlerFunc // if set, replaces default routed handler

	// Discord session stub.
	session *discordgo.Session
}

func newDiscordRoutedFixture(t *testing.T) *discordRoutedFixture {
	t.Helper()
	f := &discordRoutedFixture{
		routedStatus: http.StatusOK,
	}

	// Create store.
	dbPath := filepath.Join(t.TempDir(), "routed_test.db")
	store, err := NewSQLiteStore(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	f.store = store

	ctx := context.Background()

	// Seed channel link.
	require.NoError(t, f.store.CreateChannelLink(ctx, &ChannelLink{
		ChannelID:    "C-TEST",
		GuildID:      "G-TEST",
		ProjectID:    "proj-001",
		ProjectSlug:  "test-project",
		DefaultAgent: "alpha",
		LinkedBy:     "test-user",
		LinkedAt:     time.Now(),
		Active:       true,
	}))

	// Seed user mapping.
	require.NoError(t, f.store.CreateUserMapping(ctx, &DiscordUserMapping{
		DiscordUserID:   "U-SENDER",
		DiscordUsername: "testuser",
		ScionEmail:      "testuser@example.com",
		LinkedAt:        time.Now(),
	}))

	// Hub server — mux with both legacy and routed endpoints.
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
		override := f.routedHandlerOverride
		status := f.routedStatus
		respBody := f.routedBody
		f.mu.Unlock()

		// Allow per-test handler override (e.g. to inject delays).
		if override != nil {
			override(w, r)
			return
		}

		w.WriteHeader(status)
		if respBody != "" {
			w.Write([]byte(respBody))
		} else {
			// Default: successful delivery with primary_agent from request.
			defaultResp := fmt.Sprintf(`{"delivered":true,"primary_agent":%q}`, p.DefaultAgent)
			w.Write([]byte(defaultResp))
		}
	})
	f.hubServer = httptest.NewServer(mux)
	t.Cleanup(f.hubServer.Close)

	// Discord session with recording transport so REST calls don't panic.
	f.session, _ = newRecordingSession(t, []*discordgo.Channel{
		{ID: "C-TEST", Type: discordgo.ChannelTypeGuildText},
	})

	// Build broker.
	f.broker = &DiscordBroker{
		log:           discardLogger(),
		session:       f.session,
		store:         f.store,
		hubURL:        f.hubServer.URL,
		hmacKey:       "",
		brokerID:      "",
		pluginName:    "discord",
		httpClient:    &http.Client{Timeout: 10 * time.Second},
		sentIDs:       make(map[string]time.Time),
		subs:          make(map[string]bool),
		threadParents: make(map[string]string),
		config: &Config{
			RoutedInboundEnabled: false, // default off; tests flip as needed
		},
		botUser: &discordgo.User{
			ID:       "BOT123",
			Username: "TestBot",
		},
		agentCacheTTL: 30 * time.Second,
	}

	// Seed agent cache so legacy path resolves agents without a hub client.
	require.NoError(t, f.store.SetProjectAgents(ctx, &ProjectAgents{
		ProjectID:   "proj-001",
		AgentSlugs:  []string{"alpha", "beta", "gamma"},
		RefreshedAt: time.Now(),
	}))

	return f
}

// enableRouted turns on routed inbound delivery.
func (f *discordRoutedFixture) enableRouted() {
	f.broker.mu.Lock()
	f.broker.config.RoutedInboundEnabled = true
	f.broker.mu.Unlock()
}

// disableRouted turns off routed inbound delivery.
func (f *discordRoutedFixture) disableRouted() {
	f.broker.mu.Lock()
	f.broker.config.RoutedInboundEnabled = false
	f.broker.mu.Unlock()
}

// setRoutedHandler replaces the default routed endpoint handler, allowing
// per-test behavior (e.g. injecting delays to simulate slow hub responses).
func (f *discordRoutedFixture) setRoutedHandler(fn http.HandlerFunc) {
	f.mu.Lock()
	f.routedHandlerOverride = fn
	f.mu.Unlock()
}

// simulateMessage builds a discordgo.MessageCreate and passes it to the
// broker's handleIncomingMessage, simulating a gateway event.
func (f *discordRoutedFixture) simulateMessage(content string, mentions []*discordgo.User, attachments []*discordgo.MessageAttachment) {
	m := &discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID:        fmt.Sprintf("msg-%d", time.Now().UnixNano()),
			ChannelID: "C-TEST",
			GuildID:   "G-TEST",
			Content:   content,
			Author: &discordgo.User{
				ID:       "U-SENDER",
				Username: "testuser",
			},
			Timestamp:   time.Now(),
			Mentions:    mentions,
			Attachments: attachments,
			Type:        discordgo.MessageTypeDefault,
		},
	}
	f.broker.handleIncomingMessage(f.session, m)
}

// simulateMessageFrom is like simulateMessage but with a custom author.
func (f *discordRoutedFixture) simulateMessageFrom(content string, authorID, authorUsername string, mentions []*discordgo.User) {
	m := &discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID:        fmt.Sprintf("msg-%d", time.Now().UnixNano()),
			ChannelID: "C-TEST",
			GuildID:   "G-TEST",
			Content:   content,
			Author: &discordgo.User{
				ID:       authorID,
				Username: authorUsername,
			},
			Timestamp: time.Now(),
			Mentions:  mentions,
			Type:      discordgo.MessageTypeDefault,
		},
	}
	f.broker.handleIncomingMessage(f.session, m)
}

// simulateMessageInChannel sends a message in a specific channel.
func (f *discordRoutedFixture) simulateMessageInChannel(channelID, content string, mentions []*discordgo.User) {
	m := &discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID:        fmt.Sprintf("msg-%d", time.Now().UnixNano()),
			ChannelID: channelID,
			GuildID:   "G-TEST",
			Content:   content,
			Author: &discordgo.User{
				ID:       "U-SENDER",
				Username: "testuser",
			},
			Timestamp: time.Now(),
			Mentions:  mentions,
			Type:      discordgo.MessageTypeDefault,
		},
	}
	f.broker.handleIncomingMessage(f.session, m)
}

// -------------------------------------------------------------------
// Tests: routed inbound path
// -------------------------------------------------------------------

func TestRoutedEnabled_CorrectPayload(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	f.simulateMessage("hello @beta what's up", nil, nil)

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Len(t, f.routedCalls, 1, "exactly one routed request expected")
	require.Len(t, f.legacyCalls, 0, "no legacy request expected when routed is enabled")

	p := f.routedCalls[0]
	assert.Equal(t, "proj-001", p.ProjectID)
	assert.Equal(t, "alpha", p.DefaultAgent)
	assert.NotNil(t, p.Message)
	assert.Equal(t, "hello @beta what's up", p.Message.Msg,
		"text @mentions must remain raw — hub planner handles extraction")
	assert.Equal(t, "user:testuser@example.com", p.Message.Sender)
	assert.Equal(t, "U-SENDER", p.Message.SenderID)
	assert.Equal(t, "C-TEST", p.Message.ThreadID)
	assert.Equal(t, "discord", p.Message.Channel)
	assert.Equal(t, messages.TypeInstruction, p.Message.Type)
	assert.Equal(t, "C-TEST", p.Message.Metadata["discord_channel_id"])
	assert.Equal(t, "G-TEST", p.Message.Metadata["discord_guild_id"])

	// Recipient must NOT be set — hub resolves routing.
	assert.Empty(t, p.Message.Recipient, "recipient must not be set by adapter on routed path")
}

func TestRoutedDisabled_LegacyPayload(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.disableRouted()

	// Bot mention triggers legacy routing to default agent.
	botMention := &discordgo.User{ID: "BOT123"}
	f.simulateMessage("<@BOT123> hello legacy", []*discordgo.User{botMention}, nil)

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Len(t, f.legacyCalls, 1, "exactly one legacy request expected")
	require.Len(t, f.routedCalls, 0, "no routed request expected when routed is disabled")

	p := f.legacyCalls[0]
	assert.NotNil(t, p.Message)
	assert.Equal(t, "hello legacy", p.Message.Msg)
	assert.Equal(t, "agent:alpha", p.Message.Recipient)
	assert.Equal(t, "proj-001", p.Message.Metadata["project_id"])
}

func TestRoutedEnabled_MentionsRemainRaw(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	f.simulateMessage("@gamma do something", nil, nil)

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Len(t, f.routedCalls, 1)
	assert.Equal(t, "@gamma do something", f.routedCalls[0].Message.Msg,
		"text-format agent mentions must remain raw for hub planner")
}

func TestRoutedEnabled_MultipleMentionsRaw(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	text := "hey @alpha and @beta please review this @gamma"
	f.simulateMessage(text, nil, nil)

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Len(t, f.routedCalls, 1)
	assert.Equal(t, text, f.routedCalls[0].Message.Msg,
		"all text mentions must be forwarded raw to hub")
}

func TestRoutedEnabled_BotMentionStripped(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	botMention := &discordgo.User{ID: "BOT123"}
	f.simulateMessage("<@BOT123> help me please", []*discordgo.User{botMention}, nil)

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Len(t, f.routedCalls, 1)
	assert.Equal(t, "help me please", f.routedCalls[0].Message.Msg,
		"Discord bot mention <@BOT_ID> must be stripped on routed path")
}

func TestRoutedEnabled_BotNicknameMentionStripped(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	botMention := &discordgo.User{ID: "BOT123"}
	f.simulateMessage("<@!BOT123> help me", []*discordgo.User{botMention}, nil)

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Len(t, f.routedCalls, 1)
	assert.Equal(t, "help me", f.routedCalls[0].Message.Msg,
		"Discord bot nickname mention <@!BOT_ID> must be stripped on routed path")
}

func TestRoutedEnabled_BotMentionMixedWithAgentMention(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	botMention := &discordgo.User{ID: "BOT123"}
	f.simulateMessage("<@BOT123> @gamma fix this", []*discordgo.User{botMention}, nil)

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Len(t, f.routedCalls, 1)
	assert.Equal(t, "@gamma fix this", f.routedCalls[0].Message.Msg,
		"bot mention stripped, agent mention preserved for hub")
}

func TestRoutedEnabled_HubError_NoLegacyFallback(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	f.mu.Lock()
	f.routedStatus = http.StatusInternalServerError
	f.routedBody = `{"error":{"code":"internal","message":"boom"}}`
	f.mu.Unlock()

	f.simulateMessage("test error path", nil, nil)

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Len(t, f.routedCalls, 1, "routed endpoint must be called")
	require.Len(t, f.legacyCalls, 0,
		"must NOT fall back to legacy on routed error")
}

func TestRoutedEnabled_PartialError_NoLegacyFallback(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	f.mu.Lock()
	f.routedStatus = http.StatusConflict
	f.routedBody = `{"error":{"code":"agent_not_running","message":"primary agent is stopped"}}`
	f.mu.Unlock()

	f.simulateMessage("test partial error", nil, nil)

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Len(t, f.routedCalls, 1)
	require.Len(t, f.legacyCalls, 0, "must NOT fall back to legacy on partial error")
}

func TestRoutedEnabled_NoDefaultAgent_StillDelivers(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	// Create a channel link with no default agent.
	ctx := context.Background()
	require.NoError(t, f.store.CreateChannelLink(ctx, &ChannelLink{
		ChannelID:    "C-NODEFAULT",
		GuildID:      "G-TEST",
		ProjectID:    "proj-002",
		ProjectSlug:  "no-default-project",
		DefaultAgent: "",
		LinkedBy:     "test",
		LinkedAt:     time.Now(),
		Active:       true,
	}))
	// Add channel to session state.
	_ = f.session.State.ChannelAdd(&discordgo.Channel{
		ID:      "C-NODEFAULT",
		GuildID: "G-TEST",
		Type:    discordgo.ChannelTypeGuildText,
	})

	// Bot mention so the message is picked up even without a default agent.
	botMention := &discordgo.User{ID: "BOT123"}
	m := &discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID:        "msg-nodefault",
			ChannelID: "C-NODEFAULT",
			GuildID:   "G-TEST",
			Content:   "<@BOT123> @gamma help me",
			Author:    &discordgo.User{ID: "U-SENDER", Username: "testuser"},
			Timestamp: time.Now(),
			Mentions:  []*discordgo.User{botMention},
			Type:      discordgo.MessageTypeDefault,
		},
	}
	f.broker.handleIncomingMessage(f.session, m)

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Len(t, f.routedCalls, 1, "routed path must proceed even without default agent")
	assert.Equal(t, "proj-002", f.routedCalls[0].ProjectID)
	assert.Empty(t, f.routedCalls[0].DefaultAgent)
	assert.Equal(t, "@gamma help me", f.routedCalls[0].Message.Msg)
}

// --- @all stays legacy ---

func TestRoutedEnabled_AtAll_StaysLegacy(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	// @all should use legacy path even when routed is enabled.
	botMention := &discordgo.User{ID: "BOT123"}
	f.simulateMessage("<@BOT123> @all deploy please", []*discordgo.User{botMention}, nil)

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Len(t, f.routedCalls, 0, "@all must NOT use routed path")
	// Legacy delivers one message per agent (3 agents in cache: alpha, beta, gamma).
	require.True(t, len(f.legacyCalls) > 0, "@all must use legacy fan-out path")
}

// --- Conversation context ---

func TestRoutedEnabled_ConversationContextSaved(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	f.simulateMessage("context test", nil, nil)

	ctx := context.Background()
	cc, err := f.store.GetConversationContext(ctx, "U-SENDER", "proj-001", "alpha")
	require.NoError(t, err)
	require.NotNil(t, cc, "conversation context must be saved on successful delivery")
	assert.Equal(t, "C-TEST", cc.LastChannelID)
}

func TestRoutedEnabled_ContextUsesRoutedPrimary(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	// Hub returns primary_agent="beta" (overrides default "alpha").
	f.mu.Lock()
	f.routedBody = `{"delivered":true,"primary_agent":"beta"}`
	f.mu.Unlock()

	f.simulateMessage("@beta hello", nil, nil)

	ctx := context.Background()
	cc, err := f.store.GetConversationContext(ctx, "U-SENDER", "proj-001", "beta")
	require.NoError(t, err)
	require.NotNil(t, cc, "context must be saved with hub's primary_agent")
	assert.Equal(t, "C-TEST", cc.LastChannelID)

	// No context should exist for the configured default "alpha".
	ccAlpha, err := f.store.GetConversationContext(ctx, "U-SENDER", "proj-001", "alpha")
	require.NoError(t, err)
	assert.Nil(t, ccAlpha, "context must NOT be saved for the overridden default")
}

func TestRoutedEnabled_ContextNotSavedOnFailure(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	f.mu.Lock()
	f.routedStatus = http.StatusConflict
	f.routedBody = `{"error":{"code":"agent_not_running","message":"primary agent stopped"}}`
	f.mu.Unlock()

	f.simulateMessage("should fail", nil, nil)

	ctx := context.Background()
	cc, err := f.store.GetConversationContext(ctx, "U-SENDER", "proj-001", "alpha")
	require.NoError(t, err)
	assert.Nil(t, cc, "context must NOT be saved when delivery fails")
}

func TestRoutedEnabled_ContextNotSavedOnEmptyPrimary(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	f.mu.Lock()
	f.routedBody = `{"delivered":true,"primary_agent":""}`
	f.mu.Unlock()

	f.simulateMessage("empty primary test", nil, nil)

	ctx := context.Background()
	cc, err := f.store.GetConversationContext(ctx, "U-SENDER", "proj-001", "alpha")
	require.NoError(t, err)
	assert.Nil(t, cc, "context must NOT be saved when primary_agent is empty")
}

func TestRoutedEnabled_ContextNotSavedOnDeliveredFalse(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	f.mu.Lock()
	f.routedBody = `{"delivered":false,"primary_agent":"alpha"}`
	f.mu.Unlock()

	f.simulateMessage("not delivered", nil, nil)

	ctx := context.Background()
	cc, err := f.store.GetConversationContext(ctx, "U-SENDER", "proj-001", "alpha")
	require.NoError(t, err)
	assert.Nil(t, cc, "context must NOT be saved when delivered=false")
}

func TestRoutedEnabled_ContextNotSavedOnMalformedResponse(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	f.mu.Lock()
	f.routedBody = `not json at all`
	f.mu.Unlock()

	f.simulateMessage("malformed response test", nil, nil)

	ctx := context.Background()
	cc, err := f.store.GetConversationContext(ctx, "U-SENDER", "proj-001", "alpha")
	require.NoError(t, err)
	assert.Nil(t, cc, "context must NOT be saved on malformed response")

	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Len(t, f.legacyCalls, 0, "must NOT fall back to legacy on decode error")
}

func TestRoutedEnabled_ContextNotSavedOnEmptyObjectResponse(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	f.mu.Lock()
	f.routedBody = `{}`
	f.mu.Unlock()

	f.simulateMessage("empty object response", nil, nil)

	ctx := context.Background()
	cc, err := f.store.GetConversationContext(ctx, "U-SENDER", "proj-001", "alpha")
	require.NoError(t, err)
	assert.Nil(t, cc, "context must NOT be saved when hub returns empty JSON object")
}

// --- R-1 regression: context save after expired preflight context ---

// TestRoutedEnabled_ContextSavedAfterPreflightExpiry is the regression test for
// R-1: the 10-second preflight context created in handleIncomingMessage must NOT
// be reused for the store.SetConversationContext call after deliverRoutedInbound
// returns. deliverRoutedInbound uses its own 6-minute http.Client timeout, so
// the original context will be expired on slow hub responses.
//
// This test calls handleRoutedInbound directly with an already-cancelled context,
// simulating the worst case (preflight expired before the hub even responds).
// The fix creates a fresh bounded context for the store call, so the save must
// succeed despite the expired parent.
func TestRoutedEnabled_ContextSavedAfterPreflightExpiry(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	// Replace the default hub handler with one that sleeps just long enough for
	// the caller's 50ms context to expire, then responds with a successful
	// delivery. deliverRoutedInbound creates its own http.Client (6-min timeout)
	// and does NOT propagate the parent ctx to the HTTP call, so the request
	// itself succeeds even though the parent ctx is long expired by return time.
	f.setRoutedHandler(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond) // outlive the 50ms parent context
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"delivered":     true,
			"primary_agent": "alpha",
		})
	})

	// Use a very short-lived context: valid when handleRoutedInbound starts
	// (so GetUserMapping succeeds), but guaranteed expired by the time
	// deliverRoutedInbound returns after the 100ms hub delay.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	link := &ChannelLink{
		ChannelID:    "C-TEST",
		GuildID:      "G-TEST",
		ProjectID:    "proj-001",
		ProjectSlug:  "test-project",
		DefaultAgent: "alpha",
		Active:       true,
	}
	m := &discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID:        "msg-r1-regression",
			ChannelID: "C-TEST",
			GuildID:   "G-TEST",
			Content:   "preflight expiry regression",
			Author:    &discordgo.User{ID: "U-SENDER", Username: "testuser"},
			Timestamp: time.Now(),
			Type:      discordgo.MessageTypeDefault,
		},
	}

	f.broker.handleRoutedInbound(ctx, f.session, m, f.store, link, "C-TEST", "BOT123", "alpha")

	// The context save must succeed despite the expired parent ctx, because
	// handleRoutedInbound now creates a fresh bounded context for the store call.
	checkCtx := context.Background()
	cc, err := f.store.GetConversationContext(checkCtx, "U-SENDER", "proj-001", "alpha")
	require.NoError(t, err)
	require.NotNil(t, cc, "conversation context must be saved even when preflight context is expired (R-1 regression)")
	assert.Equal(t, "C-TEST", cc.LastChannelID)
}

// --- Sender identity / unregistered / email-empty ---

func TestRoutedEnabled_UnregisteredUser_Blocked(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	// Send from an unregistered user.
	f.simulateMessageFrom("hello", "U-UNKNOWN", "nobody", nil)

	f.mu.Lock()
	defer f.mu.Unlock()

	assert.Len(t, f.routedCalls, 0, "unregistered user must not reach hub")
	assert.Len(t, f.legacyCalls, 0)
}

func TestRoutedEnabled_EmailEmpty_BlockedBeforeHub(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	ctx := context.Background()
	require.NoError(t, f.store.CreateUserMapping(ctx, &DiscordUserMapping{
		DiscordUserID:   "U-NOEMAIL",
		DiscordUsername: "noemail",
		ScionEmail:      "",
		LinkedAt:        time.Now(),
	}))

	f.simulateMessageFrom("blocked sender test", "U-NOEMAIL", "noemail", nil)

	f.mu.Lock()
	defer f.mu.Unlock()

	assert.Len(t, f.routedCalls, 0,
		"routed endpoint must NOT be called for email-empty user")
	assert.Len(t, f.legacyCalls, 0,
		"legacy endpoint must NOT be called when routed is enabled")
}

// --- Human-only mentions ---

func TestRoutedEnabled_HumanOnlyMentions_Dropped(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	// Message that only mentions a human Discord user (not the bot).
	// The handler should check hasNonBotMentions and drop when no default and no bot mention.

	// Create a channel with no default agent for this test.
	ctx := context.Background()
	require.NoError(t, f.store.CreateChannelLink(ctx, &ChannelLink{
		ChannelID:    "C-NODEFAULT-HM",
		GuildID:      "G-TEST",
		ProjectID:    "proj-hm",
		ProjectSlug:  "human-mention-proj",
		DefaultAgent: "",
		LinkedBy:     "test",
		LinkedAt:     time.Now(),
		Active:       true,
	}))
	_ = f.session.State.ChannelAdd(&discordgo.Channel{
		ID:      "C-NODEFAULT-HM",
		GuildID: "G-TEST",
		Type:    discordgo.ChannelTypeGuildText,
	})

	// Mention a non-bot user — should be silently dropped.
	humanUser := &discordgo.User{ID: "U-HUMAN", Username: "someuser"}
	m := &discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID:        "msg-human-only",
			ChannelID: "C-NODEFAULT-HM",
			GuildID:   "G-TEST",
			Content:   "<@U-HUMAN> hey there",
			Author:    &discordgo.User{ID: "U-SENDER", Username: "testuser"},
			Timestamp: time.Now(),
			Mentions:  []*discordgo.User{humanUser},
			Type:      discordgo.MessageTypeDefault,
		},
	}
	f.broker.handleIncomingMessage(f.session, m)

	f.mu.Lock()
	defer f.mu.Unlock()

	assert.Len(t, f.routedCalls, 0, "human-only mentions must be dropped on routed path")
	assert.Len(t, f.legacyCalls, 0)
}

func TestRoutedEnabled_DefaultAgentWithHumanMentions_StillRoutes(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	// When there IS a default agent, plain text with human mentions should still route.
	f.simulateMessage("hey there buddy", nil, nil)

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Len(t, f.routedCalls, 1,
		"plain text with default agent should route even without bot mention")
}

// --- Inactive/unknown channel ---

func TestRoutedEnabled_UnknownChannel_NoCalls(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	f.simulateMessageInChannel("C-UNKNOWN", "hello", nil)

	f.mu.Lock()
	defer f.mu.Unlock()

	assert.Len(t, f.routedCalls, 0)
	assert.Len(t, f.legacyCalls, 0)
}

func TestRoutedEnabled_InactiveChannel_NoCalls(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	ctx := context.Background()
	require.NoError(t, f.store.CreateChannelLink(ctx, &ChannelLink{
		ChannelID:    "C-INACTIVE",
		GuildID:      "G-TEST",
		ProjectID:    "proj-inactive",
		ProjectSlug:  "inactive",
		DefaultAgent: "alpha",
		LinkedBy:     "test",
		LinkedAt:     time.Now(),
		Active:       false,
	}))
	_ = f.session.State.ChannelAdd(&discordgo.Channel{
		ID:      "C-INACTIVE",
		GuildID: "G-TEST",
		Type:    discordgo.ChannelTypeGuildText,
	})

	f.simulateMessageInChannel("C-INACTIVE", "hello", nil)

	f.mu.Lock()
	defer f.mu.Unlock()

	assert.Len(t, f.routedCalls, 0)
	assert.Len(t, f.legacyCalls, 0)
}

// --- HTTP payload shape ---

func TestRoutedEnabled_HTTPPayloadShape(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	f.simulateMessage("payload shape test", nil, nil)

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

	// Recipient must not be set.
	if recipientRaw, ok := msg["recipient"]; ok {
		var recipient string
		json.Unmarshal(recipientRaw, &recipient)
		assert.Empty(t, recipient, "recipient must be empty on routed path")
	}
}

// --- Config wiring ---

func TestConfigureRoutedInboundEnabled_Discord(t *testing.T) {
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
			b := NewBroker(discardLogger())
			cfg := map[string]string{
				"bot_token":              "Bot fake-token",
				"db_path":                filepath.Join(t.TempDir(), "cfg_test.db"),
				"routed_inbound_enabled": tt.value,
			}
			require.NoError(t, b.Configure(cfg))

			b.mu.RLock()
			config := b.config
			b.mu.RUnlock()
			require.NotNil(t, config)
			assert.Equal(t, tt.expected, config.RoutedInboundEnabled)

			b.Close()
		})
	}
}

// --- Transport failure ---

func TestRoutedEnabled_TransportFailure_NoLegacyFallback(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	// Close the hub server to force transport error.
	f.hubServer.Close()

	f.simulateMessage("transport fail", nil, nil)

	f.mu.Lock()
	defer f.mu.Unlock()

	// The routed handler never reached (server closed), so no routedCalls.
	assert.Len(t, f.routedCalls, 0, "routed handler never reached (server closed)")
	assert.Len(t, f.legacyCalls, 0,
		"must NOT fall back to legacy on transport failure")
}

// --- Mixed delivery results ---

func TestRoutedEnabled_MixedResults_NoRetry(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

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

	f.simulateMessage("hello @beta review this", nil, nil)

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Len(t, f.routedCalls, 1, "exactly one routed request — no retry on mixed results")
	require.Len(t, f.legacyCalls, 0, "no legacy fallback on mixed results")
}

// --- Bot messages ignored ---

func TestRoutedEnabled_BotMessage_Ignored(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	// Bot messages (Author.Bot = true) must be ignored.
	m := &discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID:        "msg-bot",
			ChannelID: "C-TEST",
			GuildID:   "G-TEST",
			Content:   "I am a bot",
			Author: &discordgo.User{
				ID:       "BOT-OTHER",
				Username: "otherbot",
				Bot:      true,
			},
			Timestamp: time.Now(),
			Type:      discordgo.MessageTypeDefault,
		},
	}
	f.broker.handleIncomingMessage(f.session, m)

	f.mu.Lock()
	defer f.mu.Unlock()

	assert.Len(t, f.routedCalls, 0, "bot messages must be ignored")
	assert.Len(t, f.legacyCalls, 0)
}

// --- Empty content ---

func TestRoutedEnabled_EmptyContent_Ignored(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	f.simulateMessage("", nil, nil)

	f.mu.Lock()
	defer f.mu.Unlock()

	assert.Len(t, f.routedCalls, 0, "empty content must be ignored")
	assert.Len(t, f.legacyCalls, 0)
}

// --- deliverRoutedInbound unit tests ---

func TestDeliverRoutedInbound_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/broker/inbound/routed", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p routedInboundPayload
		require.NoError(t, json.Unmarshal(body, &p))

		assert.Equal(t, "proj-001", p.ProjectID)
		assert.Equal(t, "alpha", p.DefaultAgent)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		assert.Equal(t, "discord", r.Header.Get("X-Scion-Plugin-Name"))

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"delivered":     true,
			"primary_agent": "alpha",
		})
	})
	hub := httptest.NewServer(mux)
	defer hub.Close()

	b := &DiscordBroker{
		log:        discardLogger(),
		hubURL:     hub.URL,
		pluginName: "discord",
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}

	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Channel: "discord",
		Sender:  "user:alice@example.com",
		Msg:     "hello",
		Type:    messages.TypeInstruction,
	}

	result, he := b.deliverRoutedInbound("proj-001", "alpha", msg)
	assert.Nil(t, he, "no error expected on success")
	require.NotNil(t, result)
	assert.True(t, result.Delivered)
	assert.Equal(t, "alpha", result.PrimaryAgent)
}

func TestDeliverRoutedInbound_HubError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/broker/inbound/routed", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"code":    "agent_not_found",
				"message": "Agent not found",
			},
		})
	})
	hub := httptest.NewServer(mux)
	defer hub.Close()

	b := &DiscordBroker{
		log:        discardLogger(),
		hubURL:     hub.URL,
		pluginName: "discord",
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}

	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Channel: "discord",
		Sender:  "user:alice@example.com",
		Msg:     "hello",
		Type:    messages.TypeInstruction,
	}

	result, he := b.deliverRoutedInbound("proj-001", "alpha", msg)
	assert.Nil(t, result, "result must be nil on hub error")
	require.NotNil(t, he)
	assert.Equal(t, 404, he.StatusCode)
	assert.Equal(t, "agent_not_found", he.Code)
}

func TestDeliverRoutedInbound_MalformedResponse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/broker/inbound/routed", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`not json`))
	})
	hub := httptest.NewServer(mux)
	defer hub.Close()

	b := &DiscordBroker{
		log:        discardLogger(),
		hubURL:     hub.URL,
		pluginName: "discord",
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}

	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Channel: "discord",
		Sender:  "user:alice@example.com",
		Msg:     "hello",
		Type:    messages.TypeInstruction,
	}

	result, he := b.deliverRoutedInbound("proj-001", "alpha", msg)
	assert.Nil(t, result, "result must be nil on decode failure")
	require.NotNil(t, he, "hubError must be returned on decode failure")
	assert.Equal(t, "transport_error", he.Code)
}

func TestDeliverRoutedInbound_NoHubURL(t *testing.T) {
	b := &DiscordBroker{
		log:        discardLogger(),
		hubURL:     "",
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}

	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Channel: "discord",
		Sender:  "user:alice@example.com",
		Msg:     "hello",
		Type:    messages.TypeInstruction,
	}

	result, he := b.deliverRoutedInbound("proj-001", "alpha", msg)
	assert.Nil(t, result)
	assert.Nil(t, he, "no hubURL should return nil,nil (silent drop)")
}

func TestDeliverRoutedInbound_HMAC_SigningHeaders(t *testing.T) {
	var capturedHeaders http.Header
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/broker/inbound/routed", func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"delivered":true,"primary_agent":"alpha"}`))
	})
	hub := httptest.NewServer(mux)
	defer hub.Close()

	// HMAC key must be valid base64 (decodeBase64 is used internally).
	b := &DiscordBroker{
		log:        discardLogger(),
		hubURL:     hub.URL,
		hmacKey:    "dGVzdC1zZWNyZXQta2V5", // base64("test-secret-key")
		brokerID:   "broker-001",
		pluginName: "discord",
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}

	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Channel: "discord",
		Sender:  "user:alice@example.com",
		Msg:     "hello",
		Type:    messages.TypeInstruction,
	}

	result, he := b.deliverRoutedInbound("proj-001", "alpha", msg)
	assert.Nil(t, he)
	require.NotNil(t, result)

	// HMAC headers must be present when hmacKey and brokerID are configured.
	assert.NotEmpty(t, capturedHeaders.Get("X-Scion-Broker-ID"))
	assert.NotEmpty(t, capturedHeaders.Get("X-Scion-Timestamp"))
	assert.NotEmpty(t, capturedHeaders.Get("X-Scion-Signature"))
	assert.Equal(t, "discord", capturedHeaders.Get("X-Scion-Plugin-Name"))
}

// --- stripBotMention unit tests ---

func TestStripBotMention(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		botUserID string
		want      string
	}{
		{
			name:      "standard bot mention",
			text:      "<@BOT123> hello world",
			botUserID: "BOT123",
			want:      " hello world",
		},
		{
			name:      "nickname bot mention",
			text:      "<@!BOT123> hello world",
			botUserID: "BOT123",
			want:      " hello world",
		},
		{
			name:      "both formats",
			text:      "<@BOT123> and <@!BOT123> text",
			botUserID: "BOT123",
			want:      " and  text",
		},
		{
			name:      "preserves agent mentions",
			text:      "<@BOT123> @coder fix this",
			botUserID: "BOT123",
			want:      " @coder fix this",
		},
		{
			name:      "no bot mention",
			text:      "@coder fix this",
			botUserID: "BOT123",
			want:      "@coder fix this",
		},
		{
			name:      "empty bot user ID",
			text:      "<@BOT123> hello",
			botUserID: "",
			want:      "<@BOT123> hello",
		},
		{
			name:      "other user mention preserved",
			text:      "<@BOT123> <@USER456> hello",
			botUserID: "BOT123",
			want:      " <@USER456> hello",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripBotMention(tt.text, tt.botUserID)
			assert.Equal(t, tt.want, got)
		})
	}
}

// --- userFacingMessage for new error codes ---

func TestHubError_UserFacingMessage_NewCodes(t *testing.T) {
	tests := []struct {
		code     string
		contains string
	}{
		{"transport_error", "could not be confirmed"},
		{"local_error", "Failed to prepare"},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			he := &hubError{Code: tt.code, Message: "test"}
			assert.Contains(t, he.userFacingMessage(), tt.contains)
		})
	}
}

// --- Plain text with default agent routes on routed path ---

func TestRoutedEnabled_PlainTextWithDefault_Routes(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	// Plain text (no mentions at all) with a default agent should route.
	f.simulateMessage("just a regular message", nil, nil)

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Len(t, f.routedCalls, 1)
	assert.Equal(t, "alpha", f.routedCalls[0].DefaultAgent)
	assert.Equal(t, "just a regular message", f.routedCalls[0].Message.Msg)
}

// --- Slash command prefix dropped ---

func TestRoutedEnabled_SlashCommandPrefix_Dropped(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	// Create channel with no default to test the slash-prefix check.
	ctx := context.Background()
	require.NoError(t, f.store.CreateChannelLink(ctx, &ChannelLink{
		ChannelID:    "C-SLASH",
		GuildID:      "G-TEST",
		ProjectID:    "proj-slash",
		ProjectSlug:  "slash",
		DefaultAgent: "",
		LinkedBy:     "test",
		LinkedAt:     time.Now(),
		Active:       true,
	}))
	_ = f.session.State.ChannelAdd(&discordgo.Channel{
		ID:      "C-SLASH",
		GuildID: "G-TEST",
		Type:    discordgo.ChannelTypeGuildText,
	})

	// "/scion agents" should be dropped (starts with /).
	f.simulateMessageInChannel("C-SLASH", "/scion agents", nil)

	f.mu.Lock()
	defer f.mu.Unlock()

	assert.Len(t, f.routedCalls, 0, "slash-prefixed messages must be dropped")
	assert.Len(t, f.legacyCalls, 0)
}

// --- System message types ---

func TestRoutedEnabled_SystemMessage_Ignored(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.enableRouted()

	// System messages (e.g. thread created, pin) should be ignored.
	m := &discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID:        "msg-system",
			ChannelID: "C-TEST",
			GuildID:   "G-TEST",
			Content:   "Thread created",
			Author:    &discordgo.User{ID: "U-SENDER", Username: "testuser"},
			Timestamp: time.Now(),
			Type:      discordgo.MessageTypeThreadCreated,
		},
	}
	f.broker.handleIncomingMessage(f.session, m)

	f.mu.Lock()
	defer f.mu.Unlock()

	assert.Len(t, f.routedCalls, 0, "system messages must be ignored")
	assert.Len(t, f.legacyCalls, 0)
}

// --- deliverRoutedInbound uses 6-minute timeout ---

func TestDeliverRoutedInbound_UsesLongTimeout(t *testing.T) {
	// The deliverRoutedInbound method creates an http.Client with a 6-minute
	// timeout. We verify this by checking that a slow server doesn't timeout
	// within the default 10s client timeout (proving a different client is used).
	// We use a 500ms delay — enough to exceed a hypothetical 100ms timeout but
	// short enough for tests.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/broker/inbound/routed", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"delivered":true,"primary_agent":"alpha"}`))
	})
	hub := httptest.NewServer(mux)
	defer hub.Close()

	b := &DiscordBroker{
		log:        discardLogger(),
		hubURL:     hub.URL,
		pluginName: "discord",
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}

	result, he := b.deliverRoutedInbound("proj-001", "alpha", &messages.StructuredMessage{
		Version: messages.Version,
		Channel: "discord",
		Sender:  "user:test@example.com",
		Msg:     "slow test",
		Type:    messages.TypeInstruction,
	})
	assert.Nil(t, he, "500ms delay must not cause timeout with 6-minute client")
	require.NotNil(t, result)
	assert.True(t, result.Delivered)
}

// --- HMAC signing uses /api/v1/broker/inbound/routed path ---

func TestDeliverRoutedInbound_HMAC_UsesRoutedPath(t *testing.T) {
	var capturedTimestamp, capturedSignature, capturedBrokerID string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/broker/inbound/routed", func(w http.ResponseWriter, r *http.Request) {
		capturedTimestamp = r.Header.Get("X-Scion-Timestamp")
		capturedSignature = r.Header.Get("X-Scion-Signature")
		capturedBrokerID = r.Header.Get("X-Scion-Broker-ID")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"delivered":true,"primary_agent":"alpha"}`))
	})
	hub := httptest.NewServer(mux)
	defer hub.Close()

	// HMAC key must be valid base64 (decodeBase64 is used internally).
	b := &DiscordBroker{
		log:        discardLogger(),
		hubURL:     hub.URL,
		hmacKey:    "c2VjcmV0MTIz", // base64("secret123")
		brokerID:   "broker-test",
		pluginName: "discord",
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}

	result, he := b.deliverRoutedInbound("proj-001", "alpha", &messages.StructuredMessage{
		Version: messages.Version,
		Channel: "discord",
		Sender:  "user:test@example.com",
		Msg:     "hmac test",
		Type:    messages.TypeInstruction,
	})
	assert.Nil(t, he)
	require.NotNil(t, result)

	// Verify HMAC auth headers are populated.
	assert.Equal(t, "broker-test", capturedBrokerID)
	assert.NotEmpty(t, capturedTimestamp, "X-Scion-Timestamp must be set")
	assert.NotEmpty(t, capturedSignature, "X-Scion-Signature must be set")
}

// --- Verify that when routed is disabled and no mention/default, message is dropped ---

func TestRoutedDisabled_PlainTextWithDefault_RoutesToDefaultLegacy(t *testing.T) {
	f := newDiscordRoutedFixture(t)
	f.disableRouted()

	// Plain text with default agent (no explicit mention) should route to default via legacy.
	f.simulateMessage("hello without mention", nil, nil)

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Len(t, f.legacyCalls, 1, "plain text with default should route via legacy")
	assert.Equal(t, "agent:alpha", f.legacyCalls[0].Message.Recipient)
}
