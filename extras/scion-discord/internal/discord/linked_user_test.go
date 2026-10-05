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
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	luDiscordUser = "du-alice"
	luPrincipal   = "user:alice@example.com"
	luChannel     = "chan-1"
	luProject     = "p1"
)

// linkedUserHub is a fake hub that, like the real one, answers 403 on the
// user-scoped endpoints unless X-Scion-On-Behalf-Of is present. It records
// every request so tests can assert which calls carried the header.
type linkedUserHub struct {
	mu    sync.Mutex
	calls []recordedHubCall
}

func (h *linkedUserHub) snapshot() []recordedHubCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]recordedHubCall(nil), h.calls...)
}

// callsTo returns the recorded calls matching method and path.
func (h *linkedUserHub) callsTo(method, path string) []recordedHubCall {
	var out []recordedHubCall
	for _, c := range h.snapshot() {
		if c.Method == method && c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

func (h *linkedUserHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	onBehalfOf := r.Header.Get("X-Scion-On-Behalf-Of")
	h.mu.Lock()
	h.calls = append(h.calls, recordedHubCall{
		Method:        r.Method,
		Path:          r.URL.Path,
		RawQuery:      r.URL.RawQuery,
		OnBehalfOf:    onBehalfOf,
		SignedHeaders: r.Header.Get("X-Scion-Signed-Headers"),
	})
	h.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")

	// The broker-scoped project list does not need a linked user.
	if r.URL.Path == "/api/v1/broker/projects" {
		_ = json.NewEncoder(w).Encode(hubProjectsResponse{Projects: []hubProject{{ID: "other", Slug: "other-project"}}})
		return
	}

	if onBehalfOf == "" {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"forbidden","message":"linked user required"}}`)
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects":
		_ = json.NewEncoder(w).Encode(hubProjectsResponse{Projects: []hubProject{{ID: luProject, Slug: "proj-one"}}})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+luProject+"/agents":
		_ = json.NewEncoder(w).Encode(hubAgentsResponse{Agents: []hubAgent{{ID: "a1", Slug: "worker", Phase: "running", Activity: "idle"}}})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/"+luProject+"/agents":
		var body CreateAgentRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		resp := hubCreateAgentResponse{}
		resp.Agent.Slug = body.Name
		resp.Agent.Name = body.Name
		_ = json.NewEncoder(w).Encode(resp)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/templates":
		_ = json.NewEncoder(w).Encode(hubTemplatesResponse{Templates: []hubTemplate{{Slug: "default", Name: "Default"}}})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/secrets":
		_ = json.NewEncoder(w).Encode(hubListSecretsResponse{Secrets: []SecretInfo{{Key: "API_KEY"}}})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/secrets/API_KEY":
		_ = json.NewEncoder(w).Encode(SecretInfo{Key: "API_KEY", Scope: "project", ScopeID: luProject})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// discordStub is an http.RoundTripper standing in for the Discord REST API.
// It answers every request with a minimal channel/message object and records
// request bodies so tests can read the bot's replies.
type discordStub struct {
	mu     sync.Mutex
	bodies []string
}

func (d *discordStub) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	d.mu.Lock()
	d.bodies = append(d.bodies, string(body))
	d.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"thread-1","channel_id":"thread-1","parent_id":"` + luChannel + `"}`)),
		Request:    req,
	}, nil
}

func (d *discordStub) allBodies() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return strings.Join(d.bodies, "\n")
}

type linkedUserEnv struct {
	hub      *linkedUserHub
	discord  *discordStub
	session  *discordgo.Session
	store    Store
	commands *CommandHandler
	callback *CallbackHandler
}

func newLinkedUserEnv(t *testing.T) *linkedUserEnv {
	t.Helper()

	hub := &linkedUserHub{}
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)

	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "discord.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	require.NoError(t, store.CreateUserMapping(ctx, &DiscordUserMapping{
		DiscordUserID:   luDiscordUser,
		DiscordUsername: "alice",
		ScionUserID:     "scion-user-1",
		ScionEmail:      "alice@example.com",
		LinkedAt:        time.Now(),
	}))

	stub := &discordStub{}
	session, err := discordgo.New("Bot test-token")
	require.NoError(t, err)
	session.Client = &http.Client{Transport: stub}
	session.State = discordgo.NewState()
	require.NoError(t, session.State.GuildAdd(&discordgo.Guild{ID: testGuildID}))
	require.NoError(t, session.State.ChannelAdd(&discordgo.Channel{ID: luChannel, GuildID: testGuildID, Type: discordgo.ChannelTypeGuildText}))

	hubClient := NewHTTPHubClient(srv.URL, "", "", nil)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &linkedUserEnv{
		hub:      hub,
		discord:  stub,
		session:  session,
		store:    store,
		commands: NewCommandHandler(store, session, hubClient, nil, "app-1", nil, 0, "", log),
		callback: NewCallbackHandler(store, session, hubClient, nil, log),
	}
}

func (e *linkedUserEnv) linkChannel(t *testing.T) {
	t.Helper()
	require.NoError(t, e.store.CreateChannelLink(context.Background(), &ChannelLink{
		ChannelID:   luChannel,
		GuildID:     testGuildID,
		ProjectID:   luProject,
		ProjectSlug: "proj-one",
		LinkedBy:    luDiscordUser,
		LinkedAt:    time.Now(),
		Active:      true,
	}))
}

// luInteraction builds a /scion <sub> interaction from the linked user.
func luInteraction(itype discordgo.InteractionType, data discordgo.InteractionData) *discordgo.InteractionCreate {
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:        "int-1",
		AppID:     "app-1",
		Token:     "tok",
		Type:      itype,
		GuildID:   testGuildID,
		ChannelID: luChannel,
		Member: &discordgo.Member{
			User:        &discordgo.User{ID: luDiscordUser, Username: "alice"},
			Permissions: discordgo.PermissionAdministrator,
		},
		Data: data,
	}}
}

func luStringOpt(name, value string) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionString, Value: value}
}

func luCommand(sub string, opts ...*discordgo.ApplicationCommandInteractionDataOption) *discordgo.InteractionCreate {
	return luInteraction(discordgo.InteractionApplicationCommand, discordgo.ApplicationCommandInteractionData{
		Name: "scion",
		Options: []*discordgo.ApplicationCommandInteractionDataOption{{
			Name: sub, Type: discordgo.ApplicationCommandOptionSubCommand, Options: opts,
		}},
	})
}

func luSecretCommand(sub string, opts ...*discordgo.ApplicationCommandInteractionDataOption) *discordgo.InteractionCreate {
	return luInteraction(discordgo.InteractionApplicationCommand, discordgo.ApplicationCommandInteractionData{
		Name: "scion",
		Options: []*discordgo.ApplicationCommandInteractionDataOption{{
			Name: "secret", Type: discordgo.ApplicationCommandOptionSubCommandGroup,
			Options: []*discordgo.ApplicationCommandInteractionDataOption{{
				Name: sub, Type: discordgo.ApplicationCommandOptionSubCommand, Options: opts,
			}},
		}},
	})
}

func luAutocomplete(sub, focused string) *discordgo.InteractionCreate {
	opt := luStringOpt(focused, "")
	opt.Focused = true
	return luInteraction(discordgo.InteractionApplicationCommandAutocomplete, discordgo.ApplicationCommandInteractionData{
		Name: "scion",
		Options: []*discordgo.ApplicationCommandInteractionDataOption{{
			Name: sub, Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandInteractionDataOption{opt},
		}},
	})
}

// assertLinkedUserCalls checks that at least one method+path request reached
// the hub and that every such request carried the linked user.
func assertLinkedUserCalls(t *testing.T, hub *linkedUserHub, method, path string) {
	t.Helper()
	calls := hub.callsTo(method, path)
	require.NotEmpty(t, calls, "expected a %s %s request; got %+v", method, path, hub.snapshot())
	for _, c := range calls {
		assert.Equal(t, luPrincipal, c.OnBehalfOf, "%s %s", method, path)
		assert.Equal(t, "x-scion-on-behalf-of", c.SignedHeaders, "%s %s", method, path)
	}
}

func TestHandlers_HubReadsCarryLinkedUser(t *testing.T) {
	agentsPath := "/api/v1/projects/" + luProject + "/agents"

	tests := []struct {
		name     string
		run      func(e *linkedUserEnv)
		wantPath string
		wantText string
	}{
		{
			name:     "agents",
			run:      func(e *linkedUserEnv) { e.commands.HandleAgents(e.session, luCommand("agents")) },
			wantPath: agentsPath,
			wantText: "worker",
		},
		{
			name: "status",
			run: func(e *linkedUserEnv) {
				e.commands.HandleStatus(e.session, luCommand("status", luStringOpt("agent", "worker")))
			},
			wantPath: agentsPath,
			wantText: "idle",
		},
		{
			name: "message",
			run: func(e *linkedUserEnv) {
				e.commands.HandleMessage(e.session, luCommand("message", luStringOpt("agent", "worker"), luStringOpt("text", "hi")))
			},
			wantPath: agentsPath,
			// No delivery func is configured, so the handler stops after
			// confirming the agent exists.
			wantText: "Message delivery is not configured",
		},
		{
			name: "terminal",
			run: func(e *linkedUserEnv) {
				e.commands.HandleTerminal(e.session, luCommand("terminal", luStringOpt("agent", "worker")))
			},
			wantPath: agentsPath,
			wantText: "/agents/a1/terminal",
		},
		{
			name:     "default",
			run:      func(e *linkedUserEnv) { e.commands.HandleDefault(e.session, luCommand("default")) },
			wantPath: agentsPath,
			wantText: "worker",
		},
		{
			name:     "secret list",
			run:      func(e *linkedUserEnv) { e.commands.HandleSecretList(e.session, luSecretCommand("list")) },
			wantPath: "/api/v1/secrets",
			wantText: "API_KEY",
		},
		{
			name: "secret get",
			run: func(e *linkedUserEnv) {
				e.commands.HandleSecretGet(e.session, luSecretCommand("get", luStringOpt("key", "API_KEY")))
			},
			wantPath: "/api/v1/secrets/API_KEY",
			wantText: "API_KEY",
		},
		{
			name:     "autocomplete agent",
			run:      func(e *linkedUserEnv) { e.commands.HandleAutocomplete(e.session, luAutocomplete("status", "agent")) },
			wantPath: agentsPath,
			wantText: "worker",
		},
		{
			name:     "autocomplete template",
			run:      func(e *linkedUserEnv) { e.commands.HandleAutocomplete(e.session, luAutocomplete("thread", "template")) },
			wantPath: "/api/v1/templates",
			wantText: "default",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newLinkedUserEnv(t)
			e.linkChannel(t)

			tt.run(e)

			assertLinkedUserCalls(t, e.hub, http.MethodGet, tt.wantPath)
			assert.Contains(t, e.discord.allBodies(), tt.wantText)
		})
	}
}

func TestHandleSetup_ListsLinkedUserProjects(t *testing.T) {
	e := newLinkedUserEnv(t)

	e.commands.HandleSetup(e.session, luCommand("setup"))

	assertLinkedUserCalls(t, e.hub, http.MethodGet, "/api/v1/projects")
	for _, c := range e.hub.callsTo(http.MethodGet, "/api/v1/projects") {
		assert.Empty(t, c.RawQuery, "project list is scoped by the linked user, not an ownerId filter")
	}
	assert.Empty(t, e.hub.callsTo(http.MethodGet, "/api/v1/broker/projects"),
		"the broker-wide project list is only a fallback when the user list is empty")
	assert.Contains(t, e.discord.allBodies(), "setup:proj:"+luProject)
}

func TestHandleSetupProject_ListsAgentsAsLinkedUser(t *testing.T) {
	e := newLinkedUserEnv(t)

	i := luInteraction(discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{
		CustomID: "setup:proj:" + luProject,
	})
	e.callback.Dispatch(e.session, i, "setup:proj:"+luProject, nil)

	assertLinkedUserCalls(t, e.hub, http.MethodGet, "/api/v1/projects/"+luProject+"/agents")
	assert.Contains(t, e.discord.allBodies(), "setup:dflt:worker")
}

func TestHandleThread_ReachesAgentCreation(t *testing.T) {
	e := newLinkedUserEnv(t)
	e.linkChannel(t)

	e.commands.HandleThread(e.session, luCommand("thread",
		luStringOpt("title", "Fix the build"),
		luStringOpt("template", "default"),
	))

	agentsPath := "/api/v1/projects/" + luProject + "/agents"
	assertLinkedUserCalls(t, e.hub, http.MethodGet, agentsPath)
	assertLinkedUserCalls(t, e.hub, http.MethodGet, "/api/v1/templates")
	assertLinkedUserCalls(t, e.hub, http.MethodPost, agentsPath)
	assert.Contains(t, e.discord.allBodies(), "Thread created with agent **fix-the-build**")
}
