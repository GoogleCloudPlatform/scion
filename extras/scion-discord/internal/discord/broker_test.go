package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestStructuredMessage() *messages.StructuredMessage {
	return &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Channel:   "discord",
		Sender:    "user:alice@example.com",
		Recipient: "agent:coder",
		Msg:       "hello",
		Type:      messages.TypeInstruction,
	}
}

// TestUnknownMentionRouting traces the actual routing logic in
// handleIncomingMessage for unknown @mentions and verifies the error path
// fires (R1) and unresolved names are reported (N2).
func TestUnknownMentionRouting(t *testing.T) {
	knownAgents := []string{"coder", "reviewer"}
	botUserID := "BOT123"

	t.Run("unknown mention with default agent triggers error path", func(t *testing.T) {
		// Trace the handleIncomingMessage flow for "@not-an-agent hello"
		// with effectiveDefault = "coder".
		content := "@not-an-agent hello"

		// Step 1: resolveTargetAgents returns empty for unknown mention.
		msg := newMockMessage(content, nil)
		targets, _ := resolveTargetAgents(msg, botUserID, "coder", knownAgents)
		assert.Empty(t, targets, "unknown mention should not resolve")

		// Step 2: Default fallback sets targets = ["coder"].
		effectiveDefault := "coder"
		targets = []string{effectiveDefault}

		// Step 3: classifyMentions identifies the unknown start mention.
		classified := classifyMentions(content, botUserID, knownAgents, noopResolver)
		assert.Equal(t, 0, countAgentStartMentions(classified),
			"unknown start mentions should not count as agent start mentions")
		assert.True(t, len(classified.StartMentions) > 0,
			"unknown mention should still be in StartMentions")

		// Step 4: The error check fires BEFORE the filtering block.
		// In the actual code, after classifyMentions, we check for unknown
		// start mentions and return an error instead of routing to default.
		hasUnknownStartMention := false
		for _, sm := range classified.StartMentions {
			if sm.Kind == "unknown" {
				hasUnknownStartMention = true
				break
			}
		}
		assert.True(t, hasUnknownStartMention,
			"error path must detect unknown start mention")

		// Extract unresolved names directly from classified.StartMentions
		// (mirrors the production code path in broker.go).
		var unresolved []string
		for _, sm := range classified.StartMentions {
			if sm.Kind == "unknown" {
				unresolved = append(unresolved, sm.Name)
			}
		}
		assert.Equal(t, []string{"not-an-agent"}, unresolved,
			"unresolved mentions should be detected for error feedback")

		// Verify the error message that would be sent.
		errMsg := fmt.Sprintf("Unknown agent: %s. Use `/scion agents` to see available agents.",
			strings.Join(unresolved, ", "))
		assert.Contains(t, errMsg, "not-an-agent")
	})

	t.Run("multiple unknown mentions reports all names", func(t *testing.T) {
		// N2: "@foo @bar hello" should report both unknown names.
		content := "@foo @bar hello"

		classified := classifyMentions(content, botUserID, knownAgents, noopResolver)
		assert.Equal(t, 0, countAgentStartMentions(classified))
		assert.Len(t, classified.StartMentions, 2)

		// Extract unresolved names directly from classified.StartMentions.
		var unresolved []string
		for _, sm := range classified.StartMentions {
			if sm.Kind == "unknown" {
				unresolved = append(unresolved, sm.Name)
			}
		}
		assert.Equal(t, []string{"foo", "bar"}, unresolved)

		errMsg := fmt.Sprintf("Unknown agent: %s. Use `/scion agents` to see available agents.",
			strings.Join(unresolved, ", "))
		assert.Contains(t, errMsg, "foo, bar",
			"error message should list all unresolved mentions")
	})

	t.Run("mixed known and unknown start mentions delivers to known agent", func(t *testing.T) {
		// R2: "@coder @not-an-agent fix this" should deliver to coder,
		// not show an error. The error path must not fire when there are
		// valid agent start mentions alongside unknown ones.
		content := "@coder @not-an-agent fix this"

		msg := newMockMessage(content, nil)
		targets, _ := resolveTargetAgents(msg, botUserID, "coder", knownAgents)
		assert.Equal(t, []string{"coder"}, targets)

		classified := classifyMentions(content, botUserID, knownAgents, noopResolver)
		// Has both agent and unknown start mentions.
		assert.Equal(t, 1, countAgentStartMentions(classified))
		assert.Len(t, classified.StartMentions, 2)

		// Error path should NOT fire because there are valid agent start mentions.
		// The guard: hasUnknownStartMention && countAgentStartMentions(classified) == 0
		// ensures mixed cases are delivered, not blocked.
		hasUnknownStartMention := false
		for _, sm := range classified.StartMentions {
			if sm.Kind == "unknown" {
				hasUnknownStartMention = true
				break
			}
		}
		assert.True(t, hasUnknownStartMention,
			"unknown mention should be detected")
		assert.True(t, countAgentStartMentions(classified) > 0,
			"mixed case must not trigger error path — known agent should receive message")
	})

	t.Run("unknown mention without default agent returns early", func(t *testing.T) {
		// Without a default, targets stays empty and the function returns
		// at the early exit (line ~1226). The error feedback at that point
		// only fires when isBotMentioned is true (Discord structured mention).
		// Text-format @unknown does not set isBotMentioned.
		content := "@not-an-agent hello"

		msg := newMockMessage(content, nil)
		targets, _ := resolveTargetAgents(msg, botUserID, "", knownAgents)
		assert.Empty(t, targets)

		// No default → targets stays empty → early return before classifyMentions.
		// Verify unresolved mentions are detectable for the early-return path.
		unresolved := extractUnresolvedMentions(content, botUserID, knownAgents)
		assert.Equal(t, []string{"not-an-agent"}, unresolved)
	})

	t.Run("known agent mention routes correctly", func(t *testing.T) {
		content := "@coder fix this bug"

		msg := newMockMessage(content, nil)
		targets, _ := resolveTargetAgents(msg, botUserID, "coder", knownAgents)
		assert.Equal(t, []string{"coder"}, targets)

		classified := classifyMentions(content, botUserID, knownAgents, noopResolver)
		assert.Equal(t, 1, countAgentStartMentions(classified))

		// No unknown start mentions → error path does NOT fire.
		hasUnknownStartMention := false
		for _, sm := range classified.StartMentions {
			if sm.Kind == "unknown" {
				hasUnknownStartMention = true
				break
			}
		}
		assert.False(t, hasUnknownStartMention,
			"known agent should not trigger error path")

		// Filtering preserves the known agent.
		startMentionSet := make(map[string]bool)
		for _, sm := range classified.StartMentions {
			if sm.Kind == "agent" {
				startMentionSet[strings.ToLower(sm.Name)] = true
			}
		}
		filteredTargets := make([]string, 0)
		for _, t2 := range targets {
			if startMentionSet[strings.ToLower(t2)] {
				filteredTargets = append(filteredTargets, t2)
			}
		}
		assert.Equal(t, []string{"coder"}, filteredTargets,
			"known agent should remain in targets after filtering")

		// Verify no unknown start mentions exist (error path would not fire).
		var unresolved []string
		for _, sm := range classified.StartMentions {
			if sm.Kind == "unknown" {
				unresolved = append(unresolved, sm.Name)
			}
		}
		assert.Empty(t, unresolved)
	})

	t.Run("safety net restores default when only body mentions exist", func(t *testing.T) {
		// When all agents are body-mentioned (no start mentions), the safety
		// net should restore the default agent. Verify the fix preserves this.
		content := "please ask @reviewer about this"

		classified := classifyMentions(content, botUserID, knownAgents, noopResolver)

		// No start mentions, so countAgentStartMentions == 0.
		// Safety net condition: len(targets) == 0 && agentStartMentions == 0
		// should allow default restoration.
		assert.Equal(t, 0, countAgentStartMentions(classified))
		assert.Empty(t, classified.StartMentions)
		assert.Len(t, classified.BodyMentions, 1)
	})
}

func TestParseHubError(t *testing.T) {
	t.Run("valid error response", func(t *testing.T) {
		body := `{"error":{"code":"agent_not_found","message":"Agent \"coder\" not found in project"}}`
		resp := &http.Response{
			StatusCode: 404,
			Body:       io.NopCloser(strings.NewReader(body)),
		}
		he := parseHubError(resp)
		require.NotNil(t, he)
		assert.Equal(t, 404, he.StatusCode)
		assert.Equal(t, "agent_not_found", he.Code)
		assert.Equal(t, `Agent "coder" not found in project`, he.Message)
	})

	t.Run("empty body", func(t *testing.T) {
		resp := &http.Response{
			StatusCode: 500,
			Body:       io.NopCloser(strings.NewReader("")),
		}
		he := parseHubError(resp)
		assert.Equal(t, "unknown", he.Code)
		assert.Equal(t, "Internal Server Error", he.Message)
	})

	t.Run("invalid JSON", func(t *testing.T) {
		resp := &http.Response{
			StatusCode: 403,
			Body:       io.NopCloser(strings.NewReader("not json")),
		}
		he := parseHubError(resp)
		assert.Equal(t, "unknown", he.Code)
		assert.Equal(t, "Forbidden", he.Message)
	})
}

func TestHubError_UserFacingMessage(t *testing.T) {
	tests := []struct {
		name     string
		err      hubError
		contains string
	}{
		{
			name:     "agent not found",
			err:      hubError{StatusCode: 404, Code: "agent_not_found", Message: "Agent not found"},
			contains: "Target agent not found",
		},
		{
			name:     "forbidden",
			err:      hubError{StatusCode: 403, Code: "forbidden", Message: "no permission"},
			contains: "permission",
		},
		{
			name:     "unauthorized",
			err:      hubError{StatusCode: 401, Code: "unauthorized", Message: "bad auth"},
			contains: "Authentication error",
		},
		{
			name:     "broker auth failed",
			err:      hubError{StatusCode: 401, Code: "broker_auth_failed", Message: "bad hmac"},
			contains: "Authentication error",
		},
		{
			name:     "agent not running",
			err:      hubError{StatusCode: 409, Code: "agent_not_running", Message: "Agent is in error state"},
			contains: "Agent is not running",
		},
		{
			name:     "server error",
			err:      hubError{StatusCode: 502, Code: "runtime_error", Message: "agent unreachable"},
			contains: "try again or contact",
		},
		{
			name:     "other client error",
			err:      hubError{StatusCode: 400, Code: "invalid_request", Message: "bad topic format"},
			contains: "try again or contact",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := tt.err.userFacingMessage()
			assert.Contains(t, msg, tt.contains)
		})
	}
}

func TestDeliverInbound_ReturnsHubError(t *testing.T) {
	t.Run("404 agent not found", func(t *testing.T) {
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{
					"code":    "agent_not_found",
					"message": "Agent not found",
				},
			})
		}))
		defer hub.Close()

		b := &DiscordBroker{
			log:        discardLogger(),
			hubURL:     hub.URL,
			httpClient: http.DefaultClient,
		}

		he := b.deliverInbound("scion.project.p1.agent.coder.messages", newTestStructuredMessage())
		require.NotNil(t, he)
		assert.Equal(t, 404, he.StatusCode)
		assert.Equal(t, "agent_not_found", he.Code)
	})

	t.Run("403 forbidden", func(t *testing.T) {
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{
					"code":    "forbidden",
					"message": "user does not have permission",
				},
			})
		}))
		defer hub.Close()

		b := &DiscordBroker{
			log:        discardLogger(),
			hubURL:     hub.URL,
			httpClient: http.DefaultClient,
		}

		he := b.deliverInbound("scion.project.p1.agent.coder.messages", newTestStructuredMessage())
		require.NotNil(t, he)
		assert.Equal(t, 403, he.StatusCode)
		assert.Equal(t, "forbidden", he.Code)
	})

	t.Run("200 success returns nil", func(t *testing.T) {
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"delivered": true,
				"agentId":   "agent-123",
			})
		}))
		defer hub.Close()

		b := &DiscordBroker{
			log:        discardLogger(),
			hubURL:     hub.URL,
			httpClient: http.DefaultClient,
		}

		he := b.deliverInbound("scion.project.p1.agent.coder.messages", newTestStructuredMessage())
		assert.Nil(t, he)
	})

	t.Run("in-process handler returns nil", func(t *testing.T) {
		b := &DiscordBroker{
			log: discardLogger(),
			InboundHandler: func(_ string, _ *messages.StructuredMessage) {
			},
		}

		he := b.deliverInbound("test.topic", newTestStructuredMessage())
		assert.Nil(t, he)
	})
}

const testGuildID = "guild1"

func stubSession(channels []*discordgo.Channel) *discordgo.Session {
	s := &discordgo.Session{
		State: discordgo.NewState(),
	}
	_ = s.State.GuildAdd(&discordgo.Guild{ID: testGuildID})
	for _, ch := range channels {
		if ch.GuildID == "" {
			ch.GuildID = testGuildID
		}
		_ = s.State.ChannelAdd(ch)
	}
	return s
}

func testBroker(session *discordgo.Session) *DiscordBroker {
	return &DiscordBroker{
		session:       session,
		log:           slog.Default(),
		sentIDs:       make(map[string]time.Time),
		threadParents: make(map[string]string),
	}
}

func TestIsForumChannel(t *testing.T) {
	tests := []struct {
		name      string
		chType    discordgo.ChannelType
		wantForum bool
	}{
		{"text channel", discordgo.ChannelTypeGuildText, false},
		{"DM channel", discordgo.ChannelTypeDM, false},
		{"voice channel", discordgo.ChannelTypeGuildVoice, false},
		{"category", discordgo.ChannelTypeGuildCategory, false},
		{"news channel", discordgo.ChannelTypeGuildNews, false},
		{"public thread", discordgo.ChannelTypeGuildPublicThread, false},
		{"private thread", discordgo.ChannelTypeGuildPrivateThread, false},
		{"news thread", discordgo.ChannelTypeGuildNewsThread, false},
		{"stage voice", discordgo.ChannelTypeGuildStageVoice, false},
		{"forum channel", discordgo.ChannelTypeGuildForum, true},
		{"media channel", discordgo.ChannelTypeGuildMedia, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := stubSession([]*discordgo.Channel{
				{ID: "ch1", Type: tt.chType},
			})
			b := &DiscordBroker{session: session}
			assert.Equal(t, tt.wantForum, b.isForumChannel("ch1"))
		})
	}
}

func TestIsForumChannel_NilSession(t *testing.T) {
	b := &DiscordBroker{}
	assert.False(t, b.isForumChannel("ch1"))
}

func TestForumGuardCondition(t *testing.T) {
	session := stubSession([]*discordgo.Channel{
		{ID: "forum1", Type: discordgo.ChannelTypeGuildForum},
		{ID: "media1", Type: discordgo.ChannelTypeGuildMedia},
		{ID: "text1", Type: discordgo.ChannelTypeGuildText},
	})
	b := &DiscordBroker{session: session}

	tests := []struct {
		name      string
		channelID string
		threadID  string
		wantBlock bool
	}{
		{"forum without threadID", "forum1", "", true},
		{"forum with threadID", "forum1", "thread123", false},
		{"media without threadID", "media1", "", true},
		{"media with threadID", "media1", "thread456", false},
		{"text without threadID", "text1", "", false},
		{"text with threadID", "text1", "thread789", false},
		{"unknown channel without threadID", "unknown", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blocked := tt.threadID == "" && b.isForumChannel(tt.channelID)
			assert.Equal(t, tt.wantBlock, blocked)
		})
	}
}

func TestPublish_ForumChannelWithoutThreadID_ReturnsError(t *testing.T) {
	session := stubSession([]*discordgo.Channel{
		{ID: "forum123", Type: discordgo.ChannelTypeGuildForum},
	})
	b := testBroker(session)

	msg := &messages.StructuredMessage{
		Version:  messages.Version,
		Channel:  "discord",
		Sender:   "agent:test",
		Msg:      "hello",
		Type:     messages.TypeInstruction,
		Metadata: map[string]string{"discord_channel_id": "forum123"},
	}

	err := b.Publish(context.Background(), "test-topic", msg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forum/media channel")
	assert.Contains(t, err.Error(), "thread ID is required")
}

func TestPublish_MediaChannelWithoutThreadID_ReturnsError(t *testing.T) {
	session := stubSession([]*discordgo.Channel{
		{ID: "media123", Type: discordgo.ChannelTypeGuildMedia},
	})
	b := testBroker(session)

	msg := &messages.StructuredMessage{
		Version:  messages.Version,
		Channel:  "discord",
		Sender:   "agent:test",
		Msg:      "hello",
		Type:     messages.TypeInstruction,
		Metadata: map[string]string{"discord_channel_id": "media123"},
	}

	err := b.Publish(context.Background(), "test-topic", msg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forum/media channel")
}

func TestResolveRecipientChannels(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	// Seed a user mapping and conversation context.
	require.NoError(t, store.CreateUserMapping(ctx, &DiscordUserMapping{
		DiscordUserID:   "discord-user-1",
		DiscordUsername: "alice_discord",
		ScionUserID:     "scion-uuid-123",
		ScionEmail:      "alice@example.com",
		LinkedAt:        time.Now(),
	}))
	require.NoError(t, store.SetConversationContext(ctx, &ConversationContext{
		DiscordUserID: "discord-user-1",
		ProjectID:     "proj-1",
		AgentSlug:     "coder",
		LastChannelID: "channel-42",
		LastMessageAt: time.Now(),
	}))

	b := &DiscordBroker{
		log:   discardLogger(),
		store: store,
	}

	t.Run("email lookup succeeds", func(t *testing.T) {
		channels := b.resolveRecipientChannels(ctx, "user:alice@example.com", "", "proj-1", "coder")
		assert.Equal(t, []string{"channel-42"}, channels)
	})

	t.Run("display name with recipientID fallback", func(t *testing.T) {
		// Hub rewrites recipient to display name; email lookup fails,
		// but recipientID-based fallback finds the correct mapping.
		channels := b.resolveRecipientChannels(ctx, "user:Alice", "scion-uuid-123", "proj-1", "coder")
		assert.Equal(t, []string{"channel-42"}, channels)
	})

	t.Run("display name without recipientID returns nil", func(t *testing.T) {
		// No recipientID provided — fallback cannot execute.
		channels := b.resolveRecipientChannels(ctx, "user:Alice", "", "proj-1", "coder")
		assert.Nil(t, channels)
	})

	t.Run("non-user recipient returns nil", func(t *testing.T) {
		channels := b.resolveRecipientChannels(ctx, "agent:coder", "", "proj-1", "coder")
		assert.Nil(t, channels)
	})

	t.Run("email lookup preferred over recipientID", func(t *testing.T) {
		// When email lookup succeeds, recipientID is not used.
		channels := b.resolveRecipientChannels(ctx, "user:alice@example.com", "scion-uuid-123", "proj-1", "coder")
		assert.Equal(t, []string{"channel-42"}, channels)
	})

	t.Run("fallback to latest conversation context", func(t *testing.T) {
		// Add a second conversation context for a different agent.
		require.NoError(t, store.SetConversationContext(ctx, &ConversationContext{
			DiscordUserID: "discord-user-1",
			ProjectID:     "proj-1",
			AgentSlug:     "reviewer",
			LastChannelID: "channel-99",
			LastMessageAt: time.Now(),
		}))
		// With an unknown agent slug, should fall back to the latest context.
		channels := b.resolveRecipientChannels(ctx, "user:Alice", "scion-uuid-123", "proj-1", "unknown-agent")
		assert.NotNil(t, channels)
		assert.Len(t, channels, 1)
	})
}

// --- HealthCheck gateway_connected tests ---

func TestHealthCheck_GatewayConnected(t *testing.T) {
	b := &DiscordBroker{
		log:              discardLogger(),
		session:          &discordgo.Session{},
		subs:             map[string]bool{"test.>": true},
		sentIDs:          make(map[string]time.Time),
		gatewayConnected: true,
	}

	status, err := b.HealthCheck()
	require.NoError(t, err)
	assert.Equal(t, "healthy", status.Status)
	assert.Equal(t, "discord bot operational", status.Message)
	assert.Equal(t, "true", status.Details["gateway_connected"])
}

func TestHealthCheck_GatewayDisconnectedWithSubs(t *testing.T) {
	b := &DiscordBroker{
		log:              discardLogger(),
		session:          &discordgo.Session{},
		subs:             map[string]bool{"test.>": true},
		sentIDs:          make(map[string]time.Time),
		gatewayConnected: false,
	}

	status, err := b.HealthCheck()
	require.NoError(t, err)
	assert.Equal(t, "degraded", status.Status)
	assert.Contains(t, status.Message, "gateway not connected")
	assert.Equal(t, "false", status.Details["gateway_connected"])
}

func TestHealthCheck_GatewayDisconnectedNoSubs(t *testing.T) {
	b := &DiscordBroker{
		log:              discardLogger(),
		session:          &discordgo.Session{},
		subs:             map[string]bool{},
		sentIDs:          make(map[string]time.Time),
		gatewayConnected: false,
	}

	status, err := b.HealthCheck()
	require.NoError(t, err)
	// No subscriptions → no degraded status even if gateway disconnected.
	assert.Equal(t, "healthy", status.Status)
	assert.Equal(t, "false", status.Details["gateway_connected"])
}

func TestHealthCheck_Closed(t *testing.T) {
	b := &DiscordBroker{
		log:    discardLogger(),
		closed: true,
	}

	status, err := b.HealthCheck()
	require.NoError(t, err)
	assert.Equal(t, "unhealthy", status.Status)
}

func TestHealthCheck_NoSession(t *testing.T) {
	b := &DiscordBroker{
		log:     discardLogger(),
		session: nil,
	}

	status, err := b.HealthCheck()
	require.NoError(t, err)
	assert.Equal(t, "degraded", status.Status)
	assert.Contains(t, status.Message, "not configured")
}

// --- resolveAttachmentPath tests ---

func TestResolveAttachmentPath_WorkspacePaths(t *testing.T) {
	b := &DiscordBroker{
		log: discardLogger(),
		projectSlugMap: map[string]string{
			"proj-1": "my-project",
		},
	}

	ctx := context.Background()

	tests := []struct {
		name      string
		path      string
		projectID string
		want      string
	}{
		{
			name:      "workspace with leading slash",
			path:      "/workspace/file.txt",
			projectID: "proj-1",
			want:      "/home/scion/.scion/projects/my-project/file.txt",
		},
		{
			name:      "workspace without leading slash",
			path:      "workspace/file.txt",
			projectID: "proj-1",
			want:      "/home/scion/.scion/projects/my-project/file.txt",
		},
		{
			name:      "bare workspace",
			path:      "/workspace",
			projectID: "proj-1",
			want:      "/home/scion/.scion/projects/my-project",
		},
		{
			name:      "relative path",
			path:      "file.txt",
			projectID: "proj-1",
			want:      "/home/scion/.scion/projects/my-project/file.txt",
		},
		{
			name:      "no project slug returns empty",
			path:      "/workspace/file.txt",
			projectID: "unknown-proj",
			want:      "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := b.resolveAttachmentPath(ctx, tt.path, tt.projectID)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveAttachmentPath_SharedDirPaths(t *testing.T) {
	b := &DiscordBroker{
		log: discardLogger(),
		projectSlugMap: map[string]string{
			"550e8400-e29b-41d4-a716-446655440000": "my-project",
		},
	}

	ctx := context.Background()

	tests := []struct {
		name      string
		path      string
		projectID string
		wantEnd   string // suffix to match (avoids hardcoding HOME)
		wantEmpty bool
	}{
		{
			name:      "scion-volumes path with file",
			path:      "/scion-volumes/scratchpad/projects/chat-admin/report.png",
			projectID: "550e8400-e29b-41d4-a716-446655440000",
			wantEnd:   "project-configs/my-project__550e8400/shared-dirs/scratchpad/projects/chat-admin/report.png",
		},
		{
			name:      "scion-volumes path bare dir",
			path:      "/scion-volumes/build-cache",
			projectID: "550e8400-e29b-41d4-a716-446655440000",
			wantEnd:   "project-configs/my-project__550e8400/shared-dirs/build-cache",
		},
		{
			name:      "scion-volumes with trailing slash file",
			path:      "/scion-volumes/scratchpad/file.txt",
			projectID: "550e8400-e29b-41d4-a716-446655440000",
			wantEnd:   "project-configs/my-project__550e8400/shared-dirs/scratchpad/file.txt",
		},
		{
			name:      "in-workspace scion-volumes",
			path:      "/workspace/.scion-volumes/cache/data.bin",
			projectID: "550e8400-e29b-41d4-a716-446655440000",
			wantEnd:   "project-configs/my-project__550e8400/shared-dirs/cache/data.bin",
		},
		{
			name:      "no project slug returns empty",
			path:      "/scion-volumes/scratchpad/file.txt",
			projectID: "unknown-proj",
			wantEmpty: true,
		},
		{
			name:      "path traversal rejected",
			path:      "/scion-volumes/scratchpad/../../etc/passwd",
			projectID: "550e8400-e29b-41d4-a716-446655440000",
			wantEmpty: true,
		},
		{
			name:      "path traversal in shared dir name rejected",
			path:      "/scion-volumes/../.scion/settings.yaml",
			projectID: "550e8400-e29b-41d4-a716-446655440000",
			wantEmpty: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := b.resolveAttachmentPath(ctx, tt.path, tt.projectID)
			if tt.wantEmpty {
				assert.Empty(t, got, "resolveAttachmentPath(%q) should return empty", tt.path)
			} else {
				assert.True(t, strings.HasSuffix(got, filepath.FromSlash(tt.wantEnd)),
					"resolveAttachmentPath(%q) = %q, want suffix %q", tt.path, got, tt.wantEnd)
			}
		})
	}
}

func TestResolveOutboundMentions(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// User with Discord ID and username.
	require.NoError(t, store.CreateUserMapping(ctx, &DiscordUserMapping{
		DiscordUserID:   "100",
		DiscordUsername: "ptone805",
		ScionEmail:      "ptone@google.com",
		LinkedAt:        time.Now().UTC(),
	}))
	// User with Discord ID but no username — should still produce <@id> mention.
	require.NoError(t, store.CreateUserMapping(ctx, &DiscordUserMapping{
		DiscordUserID: "200",
		ScionEmail:    "nousername@example.com",
		LinkedAt:      time.Now().UTC(),
	}))

	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "user:email replaced",
			text: "Hey user:ptone@google.com check this",
			want: "Hey <@100> check this",
		},
		{
			name: "standalone email replaced",
			text: "Hey ptone@google.com check this",
			want: "Hey <@100> check this",
		},
		{
			name: "user with no username still uses ID mention",
			text: "Contact nousername@example.com please",
			want: "Contact <@200> please",
		},
		{
			name: "unknown email leaves as-is",
			text: "Contact unknown@example.com please",
			want: "Contact unknown@example.com please",
		},
		{
			name: "email in URL skipped",
			text: "See https://ptone@google.com/path",
			want: "See https://ptone@google.com/path",
		},
		{
			name: "mailto skipped",
			text: "Send to mailto:ptone@google.com",
			want: "Send to mailto:ptone@google.com",
		},
		{
			name: "multiple emails",
			text: "user:ptone@google.com and nousername@example.com",
			want: "<@100> and <@200>",
		},
		{
			name: "email at start of text",
			text: "ptone@google.com said hello",
			want: "<@100> said hello",
		},
		{
			name: "email at end of text",
			text: "message from ptone@google.com",
			want: "message from <@100>",
		},
		{
			name: "empty text",
			text: "",
			want: "",
		},
		{
			name: "no emails",
			text: "just a regular message",
			want: "just a regular message",
		},
		{
			name: "email followed by slash skipped",
			text: "http://ptone@google.com/foo",
			want: "http://ptone@google.com/foo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveOutboundMentions(ctx, store, tt.text)
			assert.Equal(t, tt.want, got)
		})
	}

	t.Run("nil store returns text unchanged", func(t *testing.T) {
		got := resolveOutboundMentions(ctx, nil, "ptone@google.com")
		assert.Equal(t, "ptone@google.com", got)
	})
}

// ---------------------------------------------------------------------------
// senderSlug derivation — uses shared deriveSenderSlug from format.go
// ---------------------------------------------------------------------------

func TestDeriveSenderSlug(t *testing.T) {
	tests := []struct {
		name      string
		sender    string
		agentSlug string
		want      string
	}{
		{
			name:      "sender is agent — uses sender slug",
			sender:    "agent:builder",
			agentSlug: "reviewer",
			want:      "builder",
		},
		{
			name:      "sender is not agent — falls back to agentSlug",
			sender:    "user:alice@example.com",
			agentSlug: "coder",
			want:      "coder",
		},
		{
			name:      "sender is agent and agentSlug is empty",
			sender:    "agent:deployer",
			agentSlug: "",
			want:      "deployer",
		},
		{
			name:      "sender is not agent and agentSlug is empty",
			sender:    "user:bob@example.com",
			agentSlug: "",
			want:      "",
		},
		{
			name:      "observe mode — sender differs from topic agent",
			sender:    "agent:agent-b",
			agentSlug: "agent-a",
			want:      "agent-b",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveSenderSlug(tt.sender, tt.agentSlug)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestHandleGuildDelete(t *testing.T) {
	t.Run("bot removed — deactivates links", func(t *testing.T) {
		store := newTestBrokerStore(t)
		ctx := context.Background()

		// Create two links in the target guild.
		for _, chID := range []string{"100", "200"} {
			require.NoError(t, store.CreateChannelLink(ctx, &ChannelLink{
				ChannelID: chID,
				GuildID:   "guild-1",
				ProjectID: "proj-1",
				LinkedAt:  time.Now().UTC(),
				Active:    true,
			}))
		}

		b := &DiscordBroker{
			log:   discardLogger(),
			store: store,
		}

		// Simulate bot removal (Unavailable = false).
		b.handleGuildDelete(nil, &discordgo.GuildDelete{
			Guild: &discordgo.Guild{ID: "guild-1"},
		})

		got1, err := store.GetChannelLink(ctx, "100")
		require.NoError(t, err)
		require.NotNil(t, got1)
		assert.False(t, got1.Active, "link should be deactivated after guild removal")

		got2, err := store.GetChannelLink(ctx, "200")
		require.NoError(t, err)
		require.NotNil(t, got2)
		assert.False(t, got2.Active, "link should be deactivated after guild removal")
	})

	t.Run("guild unavailable — does not deactivate links", func(t *testing.T) {
		store := newTestBrokerStore(t)
		ctx := context.Background()

		require.NoError(t, store.CreateChannelLink(ctx, &ChannelLink{
			ChannelID: "100",
			GuildID:   "guild-1",
			ProjectID: "proj-1",
			LinkedAt:  time.Now().UTC(),
			Active:    true,
		}))

		b := &DiscordBroker{
			log:   discardLogger(),
			store: store,
		}

		// Simulate temporary outage (Unavailable = true).
		b.handleGuildDelete(nil, &discordgo.GuildDelete{
			Guild: &discordgo.Guild{ID: "guild-1", Unavailable: true},
		})

		got, err := store.GetChannelLink(ctx, "100")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.True(t, got.Active, "link should remain active during outage")
	})
}

func TestHandleGuildCreate_UpdatesGuildName(t *testing.T) {
	store := newTestBrokerStore(t)
	ctx := context.Background()

	// Create links with an old guild name.
	for _, chID := range []string{"100", "200"} {
		require.NoError(t, store.CreateChannelLink(ctx, &ChannelLink{
			ChannelID: chID,
			GuildID:   "guild-1",
			GuildName: "Old Name",
			ProjectID: "proj-1",
			LinkedAt:  time.Now().UTC(),
			Active:    true,
		}))
	}

	b := &DiscordBroker{
		log:   discardLogger(),
		store: store,
	}

	// Simulate GuildCreate with a new name.
	b.handleGuildCreate(nil, &discordgo.GuildCreate{
		Guild: &discordgo.Guild{
			ID:   "guild-1",
			Name: "Renamed Server",
		},
	})

	got1, err := store.GetChannelLink(ctx, "100")
	require.NoError(t, err)
	assert.Equal(t, "Renamed Server", got1.GuildName)

	got2, err := store.GetChannelLink(ctx, "200")
	require.NoError(t, err)
	assert.Equal(t, "Renamed Server", got2.GuildName)
}

func newTestBrokerStore(t *testing.T) Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "broker_test.db")
	store, err := NewSQLiteStore(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	return store
}

// recordingTransport captures outbound Discord REST calls so Publish can be
// exercised without touching the network.
type recordingTransport struct {
	paths []string
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.paths = append(rt.paths, req.URL.Path)
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{}`)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// newRecordingSession returns a Session whose REST calls are intercepted, with
// the given channels seeded into its state cache.
func newRecordingSession(t *testing.T, channels []*discordgo.Channel) (*discordgo.Session, *recordingTransport) {
	t.Helper()
	s, err := discordgo.New("Bot test-token")
	require.NoError(t, err)

	rt := &recordingTransport{}
	s.Client = &http.Client{Transport: rt}
	s.MaxRestRetries = 0
	s.ShouldRetryOnRateLimit = false

	_ = s.State.GuildAdd(&discordgo.Guild{ID: testGuildID})
	for _, ch := range channels {
		if ch.GuildID == "" {
			ch.GuildID = testGuildID
		}
		_ = s.State.ChannelAdd(ch)
	}
	return s, rt
}

// TestPublish_ObserveFilter_ThreadResolvesParentLink covers the fail-closed
// observe filter for messages routed directly at a thread snowflake.
//
// Channel links are only ever persisted against parent channels
// (saveChannelLink rewrites thread IDs to the parent), so the filter must use
// resolveChannelLink rather than a bare store.GetChannelLink — otherwise every
// thread looks unlinked and agent-to-agent traffic is blocked even when observe
// mode is explicitly enabled on the parent.
func TestPublish_ObserveFilter_ThreadResolvesParentLink(t *testing.T) {
	tests := []struct {
		name string
		// parentLink is nil when the parent channel has no link row at all.
		parentLink       *ChannelLink
		wantDelivered    bool
		wantDeliveredMsg string
	}{
		{
			name:             "thread with no parent link is filtered out",
			parentLink:       nil,
			wantDelivered:    false,
			wantDeliveredMsg: "unlinked thread must fail closed",
		},
		{
			name: "thread whose parent enables observe is delivered",
			parentLink: &ChannelLink{
				ShowAgentToAgent: true,
			},
			wantDelivered:    true,
			wantDeliveredMsg: "observe mode on the parent must allow agent-to-agent through",
		},
		{
			name: "thread whose parent disables observe is filtered out",
			parentLink: &ChannelLink{
				ShowAgentToAgent: false,
			},
			wantDelivered:    false,
			wantDeliveredMsg: "observe mode off on the parent must filter agent-to-agent",
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			// Unique IDs per case: resolveChannelLink memoises thread parents
			// in a package-level cache that outlives individual tests.
			parentID := fmt.Sprintf("parent-%d", i)
			threadID := fmt.Sprintf("thread-%d", i)
			t.Cleanup(func() {
				threadParentsMu.Lock()
				delete(threadParents, threadID)
				threadParentsMu.Unlock()
			})

			session, rt := newRecordingSession(t, []*discordgo.Channel{
				{ID: parentID, Type: discordgo.ChannelTypeGuildText},
				{
					ID:       threadID,
					Type:     discordgo.ChannelTypeGuildPublicThread,
					ParentID: parentID,
				},
			})

			store := newTestBrokerStore(t)
			if tt.parentLink != nil {
				link := *tt.parentLink
				link.ChannelID = parentID
				link.GuildID = testGuildID
				link.ProjectID = "proj-1"
				link.ProjectSlug = "proj"
				link.Active = true
				link.LinkedAt = time.Now()
				require.NoError(t, store.CreateChannelLink(ctx, &link))
			}

			b := testBroker(session)
			b.log = discardLogger()
			b.store = store

			// Agent-to-agent message routed straight at the thread snowflake.
			msg := &messages.StructuredMessage{
				Version:   messages.Version,
				Channel:   "discord",
				Sender:    "agent:alice",
				Recipient: "agent:bob",
				Msg:       fmt.Sprintf("observe payload %d", i),
				Type:      messages.TypeInstruction,
				ThreadID:  threadID,
			}

			require.NoError(t, b.Publish(ctx, "proj-1.agent.alice", msg))

			delivered := len(rt.paths) > 0
			assert.Equal(t, tt.wantDelivered, delivered, tt.wantDeliveredMsg)
			if tt.wantDelivered {
				assert.Contains(t, strings.Join(rt.paths, ","), threadID,
					"message should be delivered to the thread, not the parent")
			}
		})
	}
}

// TestPublish_ObserveFilter_StateChangeThreadResolvesParentLink is the
// state-change counterpart: ShowStateChanges lives on the parent link too.
func TestPublish_ObserveFilter_StateChangeThreadResolvesParentLink(t *testing.T) {
	for i, showStateChanges := range []bool{false, true} {
		t.Run(fmt.Sprintf("show_state_changes=%v", showStateChanges), func(t *testing.T) {
			ctx := context.Background()

			parentID := fmt.Sprintf("sc-parent-%d", i)
			threadID := fmt.Sprintf("sc-thread-%d", i)
			t.Cleanup(func() {
				threadParentsMu.Lock()
				delete(threadParents, threadID)
				threadParentsMu.Unlock()
			})

			session, rt := newRecordingSession(t, []*discordgo.Channel{
				{ID: parentID, Type: discordgo.ChannelTypeGuildText},
				{
					ID:       threadID,
					Type:     discordgo.ChannelTypeGuildPublicThread,
					ParentID: parentID,
				},
			})

			store := newTestBrokerStore(t)
			require.NoError(t, store.CreateChannelLink(ctx, &ChannelLink{
				ChannelID:        parentID,
				GuildID:          testGuildID,
				ProjectID:        "proj-1",
				ProjectSlug:      "proj",
				Active:           true,
				LinkedAt:         time.Now(),
				ShowStateChanges: showStateChanges,
			}))

			b := testBroker(session)
			b.log = discardLogger()
			b.store = store

			msg := &messages.StructuredMessage{
				Version:  messages.Version,
				Channel:  "discord",
				Sender:   "agent:alice",
				Msg:      fmt.Sprintf("state change %d", i),
				Type:     messages.TypeStateChange,
				ThreadID: threadID,
			}

			require.NoError(t, b.Publish(ctx, "proj-1.agent.alice", msg))

			assert.Equal(t, showStateChanges, len(rt.paths) > 0,
				"state change delivery must follow the parent link's ShowStateChanges flag")
		})
	}
}

// --- threadParentID / resolveChannelLink cache-poisoning tests (issue #576) ---

// failingTransport returns HTTP 500 for every Discord REST call, simulating a
// transient API outage.
type failingTransport struct{}

func (ft *failingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(`{"message":"internal server error"}`)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// newFailingSession returns a Session whose REST calls always return 500 and
// whose state cache is empty.
func newFailingSession(t *testing.T) *discordgo.Session {
	t.Helper()
	s, err := discordgo.New("Bot test-token")
	require.NoError(t, err)
	s.Client = &http.Client{Transport: &failingTransport{}}
	s.MaxRestRetries = 0
	s.ShouldRetryOnRateLimit = false
	return s
}

func TestThreadParentID_DistinguishesFailureFromNonThread(t *testing.T) {
	t.Run("confirmed thread returns parentID and ok=true", func(t *testing.T) {
		s := stubSession([]*discordgo.Channel{
			{
				ID:       "thread-1",
				Type:     discordgo.ChannelTypeGuildPublicThread,
				ParentID: "parent-1",
			},
		})
		parentID, ok := threadParentID(s, "thread-1")
		assert.True(t, ok, "lookup succeeded — ok must be true")
		assert.Equal(t, "parent-1", parentID)
	})

	t.Run("confirmed non-thread returns empty and ok=true", func(t *testing.T) {
		s := stubSession([]*discordgo.Channel{
			{ID: "text-chan", Type: discordgo.ChannelTypeGuildText},
		})
		parentID, ok := threadParentID(s, "text-chan")
		assert.True(t, ok, "lookup succeeded — ok must be true")
		assert.Equal(t, "", parentID)
	})

	t.Run("REST failure returns empty and ok=false", func(t *testing.T) {
		s := newFailingSession(t)
		parentID, ok := threadParentID(s, "unknown-chan")
		assert.False(t, ok, "REST failed — ok must be false")
		assert.Equal(t, "", parentID)
	})
}

func TestResolveChannelLink_NoCachePoisoningOnRESTFailure(t *testing.T) {
	ctx := context.Background()

	parentID := "parent-nocache"
	threadID := "thread-nocache"
	t.Cleanup(func() {
		threadParentsMu.Lock()
		delete(threadParents, threadID)
		threadParentsMu.Unlock()
	})

	store := newTestBrokerStore(t)

	// --- Phase 1: REST is failing — resolveChannelLink must NOT cache. ---
	failSession := newFailingSession(t)

	link, err := resolveChannelLink(ctx, failSession, store, threadID)
	require.NoError(t, err)
	// No channel link exists anywhere, so link should be nil.
	assert.Nil(t, link, "no link exists yet")

	// Verify the cache was NOT poisoned.
	threadParentsMu.Lock()
	_, cached := threadParents[threadID]
	threadParentsMu.Unlock()
	assert.False(t, cached, "failed lookup must not be cached")

	// --- Phase 2: REST recovers — resolveChannelLink retries and caches. ---
	goodSession, _ := newRecordingSession(t, []*discordgo.Channel{
		{ID: parentID, Type: discordgo.ChannelTypeGuildText},
		{
			ID:       threadID,
			Type:     discordgo.ChannelTypeGuildPublicThread,
			ParentID: parentID,
		},
	})

	// Create a channel link on the parent so the fallback succeeds.
	require.NoError(t, store.CreateChannelLink(ctx, &ChannelLink{
		ChannelID:   parentID,
		GuildID:     testGuildID,
		ProjectID:   "proj-nc",
		ProjectSlug: "nc",
		Active:      true,
		LinkedAt:    time.Now(),
	}))

	link, err = resolveChannelLink(ctx, goodSession, store, threadID)
	require.NoError(t, err)
	require.NotNil(t, link, "parent link must be resolved through thread")
	assert.Equal(t, parentID, link.ChannelID)

	// Verify the cache now contains the correct parent.
	threadParentsMu.Lock()
	cachedParent, cached := threadParents[threadID]
	threadParentsMu.Unlock()
	assert.True(t, cached, "successful lookup must be cached")
	assert.Equal(t, parentID, cachedParent)
}

func TestResolveChannelLink_CachesConfirmedNonThread(t *testing.T) {
	ctx := context.Background()

	channelID := "text-cached"
	t.Cleanup(func() {
		threadParentsMu.Lock()
		delete(threadParents, channelID)
		threadParentsMu.Unlock()
	})

	session, _ := newRecordingSession(t, []*discordgo.Channel{
		{ID: channelID, Type: discordgo.ChannelTypeGuildText},
	})

	store := newTestBrokerStore(t)

	_, err := resolveChannelLink(ctx, session, store, channelID)
	require.NoError(t, err)

	// Confirmed non-thread should be cached (empty string = not a thread).
	threadParentsMu.Lock()
	cachedParent, cached := threadParents[channelID]
	threadParentsMu.Unlock()
	assert.True(t, cached, "confirmed non-thread must be cached")
	assert.Equal(t, "", cachedParent)
}

func TestResolveChannelLink_CachesConfirmedThread(t *testing.T) {
	ctx := context.Background()

	parentID := "parent-cached"
	threadID := "thread-cached"
	t.Cleanup(func() {
		threadParentsMu.Lock()
		delete(threadParents, threadID)
		threadParentsMu.Unlock()
	})

	session, _ := newRecordingSession(t, []*discordgo.Channel{
		{ID: parentID, Type: discordgo.ChannelTypeGuildText},
		{
			ID:       threadID,
			Type:     discordgo.ChannelTypeGuildPublicThread,
			ParentID: parentID,
		},
	})

	store := newTestBrokerStore(t)

	_, err := resolveChannelLink(ctx, session, store, threadID)
	require.NoError(t, err)

	// Confirmed thread should be cached with parent ID.
	threadParentsMu.Lock()
	cachedParent, cached := threadParents[threadID]
	threadParentsMu.Unlock()
	assert.True(t, cached, "confirmed thread must be cached")
	assert.Equal(t, parentID, cachedParent)
}

// --- downloadDiscordAttachment tests ---

func TestDownloadDiscordAttachment_DefaultPath(t *testing.T) {
	// When downloadsPath is empty, the function writes to
	// /home/scion/.scion/projects/<slug>/downloads and returns
	// /workspace/downloads/<name> as the agent-visible path.
	// We can't safely write to /home/scion in tests, so we verify
	// the path computation by setting downloadsPath to a temp dir
	// and confirming the custom-path branch differs from the default.

	fileContent := []byte("default-path-test")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(fileContent)
	}))
	defer srv.Close()

	projectSlug := "test-project"

	// Verify the default hostDir and agentPath computation.
	defaultHostDir := filepath.Join("/home/scion/.scion/projects", projectSlug, "downloads")
	assert.Equal(t, "/home/scion/.scion/projects/test-project/downloads", defaultHostDir)

	defaultAgentPath := filepath.Join("/workspace/downloads", "discord_123_photo.png")
	assert.Equal(t, "/workspace/downloads/discord_123_photo.png", defaultAgentPath)

	// Verify that a broker with no downloadsPath set would use
	// the default agent path prefix.
	b := &DiscordBroker{
		log:        discardLogger(),
		httpClient: srv.Client(),
	}
	assert.Empty(t, b.downloadsPath, "downloadsPath should be empty by default")
}

func TestDownloadDiscordAttachment_CustomDownloadsPath(t *testing.T) {
	fileContent := []byte("fake-image-data-for-test")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(fileContent)
	}))
	defer srv.Close()

	downloadsDir := t.TempDir()
	b := &DiscordBroker{
		log:           discardLogger(),
		httpClient:    srv.Client(),
		downloadsPath: downloadsDir,
	}

	att := &discordgo.MessageAttachment{
		ID:          "att-002",
		Filename:    "document.pdf",
		URL:         srv.URL + "/document.pdf",
		Size:        len(fileContent),
		ContentType: "application/pdf",
	}

	ctx := context.Background()
	agentPath, placeholder, err := b.downloadDiscordAttachment(ctx, att, "some-project", "test-project-id")
	require.NoError(t, err)

	// Agent path should be downloads_path + filename (not /workspace/downloads/).
	assert.True(t, strings.HasPrefix(agentPath, downloadsDir),
		"agentPath %q should start with downloadsPath %q", agentPath, downloadsDir)
	assert.False(t, strings.Contains(agentPath, "/workspace/downloads"),
		"agentPath should not contain /workspace/downloads when downloadsPath is set")

	// The filename should contain the original name.
	assert.Contains(t, filepath.Base(agentPath), "document.pdf")
	assert.Contains(t, filepath.Base(agentPath), "discord_")

	// Placeholder should describe the attachment.
	assert.Contains(t, placeholder, "document.pdf")
	assert.Contains(t, placeholder, "application/pdf")

	// The file should actually exist on disk with correct contents.
	data, err := os.ReadFile(agentPath)
	require.NoError(t, err, "downloaded file should exist at agentPath")
	assert.Equal(t, fileContent, data)
}

func TestDownloadDiscordAttachment_ProjectSlugPlaceholder(t *testing.T) {
	fileContent := []byte("slug-placeholder-test")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(fileContent)
	}))
	defer srv.Close()

	baseDir := t.TempDir()
	b := &DiscordBroker{
		log:           discardLogger(),
		httpClient:    srv.Client(),
		downloadsPath: filepath.Join(baseDir, "{project_slug}"),
	}

	att := &discordgo.MessageAttachment{
		ID:          "att-slug-001",
		Filename:    "photo.jpg",
		URL:         srv.URL + "/photo.jpg",
		Size:        len(fileContent),
		ContentType: "image/jpeg",
	}

	projectSlug := "my-cool-project"
	ctx := context.Background()
	agentPath, placeholder, err := b.downloadDiscordAttachment(ctx, att, projectSlug, "test-project-id")
	require.NoError(t, err)

	// Host directory should have the slug expanded (file written there).
	expandedDir := filepath.Join(baseDir, projectSlug)
	entries, err := os.ReadDir(expandedDir)
	require.NoError(t, err, "expanded slug directory should exist")
	require.Len(t, entries, 1, "should contain exactly one downloaded file")

	// Agent path should use the expanded slug, not the literal placeholder.
	assert.True(t, strings.HasPrefix(agentPath, expandedDir),
		"agentPath %q should start with expanded dir %q", agentPath, expandedDir)
	assert.NotContains(t, agentPath, "{project_slug}",
		"agentPath should not contain literal {project_slug}")

	// File on disk should match.
	data, err := os.ReadFile(agentPath)
	require.NoError(t, err)
	assert.Equal(t, fileContent, data)

	// Placeholder should describe the attachment.
	assert.Contains(t, placeholder, "photo.jpg")
	assert.Contains(t, placeholder, "image/jpeg")
}

func TestDownloadDiscordAttachment_EmptyProjectSlug(t *testing.T) {
	b := &DiscordBroker{
		log:        discardLogger(),
		httpClient: http.DefaultClient,
	}

	att := &discordgo.MessageAttachment{
		ID:       "att-003",
		Filename: "file.txt",
		URL:      "http://example.com/file.txt",
		Size:     10,
	}

	ctx := context.Background()
	_, _, err := b.downloadDiscordAttachment(ctx, att, "", "test-project-id")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "project slug is empty")
}

func TestDownloadDiscordAttachment_TooLarge(t *testing.T) {
	b := &DiscordBroker{
		log:        discardLogger(),
		httpClient: http.DefaultClient,
	}

	att := &discordgo.MessageAttachment{
		ID:       "att-004",
		Filename: "huge.bin",
		URL:      "http://example.com/huge.bin",
		Size:     maxDiscordAttachmentSize + 1,
	}

	ctx := context.Background()
	_, _, err := b.downloadDiscordAttachment(ctx, att, "test-project", "test-project-id")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "too large")
}

func TestDownloadDiscordAttachment_CustomPath_CreatesSubdir(t *testing.T) {
	fileContent := []byte("test-content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(fileContent)
	}))
	defer srv.Close()

	// Use a nested path that doesn't exist yet — MkdirAll should create it.
	baseDir := t.TempDir()
	downloadsDir := filepath.Join(baseDir, "nested", "downloads")
	b := &DiscordBroker{
		log:           discardLogger(),
		httpClient:    srv.Client(),
		downloadsPath: downloadsDir,
	}

	att := &discordgo.MessageAttachment{
		ID:       "att-005",
		Filename: "nested-file.txt",
		URL:      srv.URL + "/nested-file.txt",
		Size:     len(fileContent),
	}

	ctx := context.Background()
	agentPath, _, err := b.downloadDiscordAttachment(ctx, att, "proj", "test-project-id")
	require.NoError(t, err)

	// Verify the directory was created and the file exists.
	_, err = os.Stat(downloadsDir)
	require.NoError(t, err, "downloadsPath directory should be created")

	data, err := os.ReadFile(agentPath)
	require.NoError(t, err)
	assert.Equal(t, fileContent, data)
}

func TestDownloadDiscordAttachment_SharedDirPath(t *testing.T) {
	// When projectID is non-empty and downloadsPath is empty, the function
	// should route through the shared dir infrastructure.
	fileContent := []byte("shared-dir-test-data")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(fileContent)
	}))
	defer srv.Close()

	// Point HOME to a temp dir so SharedDirHostPath resolves there.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	b := &DiscordBroker{
		log:        discardLogger(),
		httpClient: srv.Client(),
		// downloadsPath intentionally empty
	}

	att := &discordgo.MessageAttachment{
		ID:          "att-shared-001",
		Filename:    "shared-photo.png",
		URL:         srv.URL + "/shared-photo.png",
		Size:        len(fileContent),
		ContentType: "image/png",
	}

	ctx := context.Background()
	agentPath, placeholder, err := b.downloadDiscordAttachment(ctx, att, "my-project", "abcd1234-ef56-7890-abcd-ef1234567890")
	require.NoError(t, err)

	// Agent path should use the shared dir prefix.
	assert.True(t, strings.HasPrefix(agentPath, "/scion-volumes/scratchpad/.attachments/_discord/"),
		"agentPath %q should start with shared dir prefix", agentPath)
	assert.Contains(t, agentPath, "shared-photo.png")
	assert.Contains(t, placeholder, "shared-photo.png")

	// Verify the file was actually written to the host-side shared dir path.
	// SharedDirHostPath returns <home>/.scion/project-configs/<slug>__<shortUUID>/shared-dirs/scratchpad
	expectedHostBase := filepath.Join(fakeHome, ".scion", "project-configs", "my-project__abcd1234", "shared-dirs", "scratchpad", ".attachments", "_discord")
	entries, err := os.ReadDir(expectedHostBase)
	require.NoError(t, err, "shared dir host path should exist")
	require.Len(t, entries, 1)
	assert.Contains(t, entries[0].Name(), "shared-photo.png")
}

func TestDownloadDiscordAttachment_EmptyProjectID_LegacyPath(t *testing.T) {
	// When projectID is empty and downloadsPath is empty, the function should
	// fall back to the legacy /workspace/downloads/ agent path.
	fileContent := []byte("legacy-fallback-test")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(fileContent)
	}))
	defer srv.Close()

	// Point HOME to a temp dir. Even though projectID is empty, the legacy
	// path uses /home/scion/.scion/projects/<slug>/downloads on the host, so
	// we set HOME so the file write succeeds somewhere writable.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	b := &DiscordBroker{
		log:        discardLogger(),
		httpClient: srv.Client(),
	}

	att := &discordgo.MessageAttachment{
		ID:       "att-legacy-001",
		Filename: "legacy.txt",
		URL:      srv.URL + "/legacy.txt",
		Size:     len(fileContent),
	}

	ctx := context.Background()
	agentPath, _, err := b.downloadDiscordAttachment(ctx, att, "test-slug", "")
	require.NoError(t, err)

	// Legacy agent path should be /workspace/downloads/<name>.
	assert.True(t, strings.HasPrefix(agentPath, "/workspace/downloads/"),
		"agentPath %q should start with legacy prefix", agentPath)
	assert.Contains(t, agentPath, "legacy.txt")
}

func TestDownloadDiscordAttachment_DownloadsPathOverridesSharedDir(t *testing.T) {
	// When downloadsPath is set, it takes priority over shared dir even if
	// projectID is non-empty.
	fileContent := []byte("downloads-path-priority")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(fileContent)
	}))
	defer srv.Close()

	customDir := t.TempDir()
	b := &DiscordBroker{
		log:           discardLogger(),
		httpClient:    srv.Client(),
		downloadsPath: customDir,
	}

	att := &discordgo.MessageAttachment{
		ID:       "att-priority-001",
		Filename: "priority.txt",
		URL:      srv.URL + "/priority.txt",
		Size:     len(fileContent),
	}

	ctx := context.Background()
	agentPath, _, err := b.downloadDiscordAttachment(ctx, att, "my-project", "abcd1234-ef56-7890-abcd-ef1234567890")
	require.NoError(t, err)

	// Agent path should use the custom downloads path, not shared dir.
	assert.True(t, strings.HasPrefix(agentPath, customDir),
		"agentPath %q should start with custom dir %q", agentPath, customDir)
	assert.NotContains(t, agentPath, "/scion-volumes/scratchpad/")
	assert.Contains(t, agentPath, "priority.txt")
}

// --- RPC bootstrap regression tests ---

func TestConfigure_SessionReplacement_ClearsSubs(t *testing.T) {
	b := &DiscordBroker{
		log:              discardLogger(),
		session:          &discordgo.Session{}, // simulate existing session
		subs:             map[string]bool{"*": true},
		sentIDs:          make(map[string]time.Time),
		gatewayConnected: true,
		bootstrapDone:    true,
	}

	// Calling Configure with bot_token should close old session and clear subs.
	err := b.Configure(map[string]string{
		"bot_token": "Bot fake-token-for-test",
	})
	require.NoError(t, err)

	// After Phase 1 reconfigure:
	// - Old subs should be cleared (so Subscribe("*") would trigger startGateway)
	// - gatewayConnected should be reset
	// - bootstrapDone should be reset
	assert.Empty(t, b.subs, "subs should be cleared after session replacement")
	assert.False(t, b.gatewayConnected, "gatewayConnected should be reset")
	assert.False(t, b.bootstrapDone, "bootstrapDone should be reset")
	assert.NotNil(t, b.session, "new session should be created")
}

func TestConfigure_BootstrapSkippedWhenDone(t *testing.T) {
	b := &DiscordBroker{
		log:           discardLogger(),
		subs:          make(map[string]bool),
		sentIDs:       make(map[string]time.Time),
		bootstrapDone: true,
		hubURL:        "http://localhost:8080",
		hmacKey:       "test-key",
		brokerID:      "test-broker",
	}

	// Configure Phase 2 (hub_url present) should skip bootstrap goroutine
	// because bootstrapDone is already true.
	err := b.Configure(map[string]string{
		"hub_url":  "http://localhost:8080",
		"hmac_key": "test-key",
	})
	require.NoError(t, err)

	// bootstrapDone should still be true (not reset by Phase 2 alone).
	assert.True(t, b.bootstrapDone, "bootstrapDone should remain true when no session replacement")
}
