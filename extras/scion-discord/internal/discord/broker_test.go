package discord

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
