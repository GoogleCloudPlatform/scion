package discord

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
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

	// projects selects the GET /projects answer: "" returns one project,
	// "empty" returns none, and "error" returns a 500.
	projects string
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
	projectsMode := h.projects
	h.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")

	if onBehalfOf == "" {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"forbidden","message":"linked user required"}}`)
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects" && projectsMode == "empty":
		_ = json.NewEncoder(w).Encode(hubProjectsResponse{})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects" && projectsMode == "error":
		w.WriteHeader(http.StatusInternalServerError)
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
	case r.Method == http.MethodPut && r.URL.Path == "/api/v1/secrets/API_KEY":
		_, _ = io.WriteString(w, `{}`)
	case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/secrets/API_KEY":
		w.WriteHeader(http.StatusNoContent)
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

// setProjects selects the GET /projects answer (see linkedUserHub.projects).
func (h *linkedUserHub) setProjects(mode string) {
	h.mu.Lock()
	h.projects = mode
	h.mu.Unlock()
}

// countingStore wraps a Store, counts agent-cache reads, and can fail user
// link lookups.
type countingStore struct {
	Store
	mu                 sync.Mutex
	agentCacheReads    int
	userMappingLookups int
	userMappingError   error
}

func (c *countingStore) GetProjectAgents(ctx context.Context, projectID string) (*ProjectAgents, error) {
	c.mu.Lock()
	c.agentCacheReads++
	c.mu.Unlock()
	return c.Store.GetProjectAgents(ctx, projectID)
}

func (c *countingStore) GetUserMapping(ctx context.Context, discordUserID string) (*DiscordUserMapping, error) {
	c.mu.Lock()
	c.userMappingLookups++
	err := c.userMappingError
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return c.Store.GetUserMapping(ctx, discordUserID)
}

func (c *countingStore) reads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.agentCacheReads
}

func (c *countingStore) lookups() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.userMappingLookups
}

const luOtherUser = "du-bob"

// asUser returns i with the invoking user replaced by discordUserID.
func asUser(i *discordgo.InteractionCreate, discordUserID string) *discordgo.InteractionCreate {
	i.Member.User = &discordgo.User{ID: discordUserID, Username: "bob"}
	return i
}

// useCountingStore swaps the env's handlers onto a countingStore and seeds a
// fresh agent cache for luProject.
func (e *linkedUserEnv) useCountingStore(t *testing.T) *countingStore {
	t.Helper()
	cs := &countingStore{Store: e.store}
	e.store = cs
	e.commands.store = cs
	e.callback.store = cs
	require.NoError(t, e.store.SetProjectAgents(context.Background(), &ProjectAgents{
		ProjectID:   luProject,
		AgentSlugs:  []string{"worker"},
		RefreshedAt: time.Now(),
	}))
	return cs
}

// luSecretModalSubmit builds the secret set modal submission for API_KEY.
func luSecretModalSubmit() *discordgo.InteractionCreate {
	return luInteraction(discordgo.InteractionModalSubmit, discordgo.ModalSubmitInteractionData{
		CustomID: "secret:set:API_KEY:" + luProject,
		Components: []discordgo.MessageComponent{
			&discordgo.ActionsRow{Components: []discordgo.MessageComponent{
				&discordgo.TextInput{CustomID: "secret_value", Value: "s3cr3t"},
			}},
		},
	})
}

// linkCheckHandler runs one handler that needs the invoking user's link.
type linkCheckHandler struct {
	name string
	run  func(t *testing.T, e *linkedUserEnv, user string)
}

// linkCheckHandlers lists the handlers that check the invoking user's link
// before reading agents or calling the hub.
func linkCheckHandlers() []linkCheckHandler {
	return []linkCheckHandler{
		{"agents", func(_ *testing.T, e *linkedUserEnv, u string) {
			e.commands.HandleAgents(e.session, asUser(luCommand("agents"), u))
		}},
		{"status", func(_ *testing.T, e *linkedUserEnv, u string) {
			e.commands.HandleStatus(e.session, asUser(luCommand("status", luStringOpt("agent", "worker")), u))
		}},
		{"message", func(_ *testing.T, e *linkedUserEnv, u string) {
			e.commands.HandleMessage(e.session, asUser(luCommand("message", luStringOpt("agent", "worker"), luStringOpt("text", "hi")), u))
		}},
		{"terminal", func(_ *testing.T, e *linkedUserEnv, u string) {
			e.commands.HandleTerminal(e.session, asUser(luCommand("terminal", luStringOpt("agent", "worker")), u))
		}},
		{"default", func(_ *testing.T, e *linkedUserEnv, u string) {
			e.commands.HandleDefault(e.session, asUser(luCommand("default"), u))
		}},
		{"default with agent", func(_ *testing.T, e *linkedUserEnv, u string) {
			e.commands.HandleDefault(e.session, asUser(luCommand("default", luStringOpt("agent", "worker")), u))
		}},
		{"thread", func(_ *testing.T, e *linkedUserEnv, u string) {
			e.commands.HandleThread(e.session, asUser(luCommand("thread", luStringOpt("title", "Fix the build")), u))
		}},
		{"secret list", func(_ *testing.T, e *linkedUserEnv, u string) {
			e.commands.HandleSecretList(e.session, asUser(luSecretCommand("list"), u))
		}},
		{"secret get", func(_ *testing.T, e *linkedUserEnv, u string) {
			e.commands.HandleSecretGet(e.session, asUser(luSecretCommand("get", luStringOpt("key", "API_KEY")), u))
		}},
		{"secret set", func(_ *testing.T, e *linkedUserEnv, u string) {
			e.commands.HandleSecretSet(e.session, asUser(luSecretCommand("set", luStringOpt("key", "API_KEY")), u))
		}},
		{"secret set submit", func(_ *testing.T, e *linkedUserEnv, u string) {
			e.commands.HandleSecretModalSubmit(e.session, asUser(luSecretModalSubmit(), u))
		}},
		{"secret delete", func(_ *testing.T, e *linkedUserEnv, u string) {
			e.commands.HandleSecretDelete(e.session, asUser(luSecretCommand("delete", luStringOpt("key", "API_KEY")), u))
		}},
		{"setup", func(t *testing.T, e *linkedUserEnv, u string) {
			require.NoError(t, e.store.DeleteChannelLink(context.Background(), luChannel))
			e.commands.HandleSetup(e.session, asUser(luCommand("setup"), u))
		}},
		luButton("setup project select", "setup:proj:"+luProject),
		luButton("setup default agent button", "setup:dflt:worker"),
		luButton("set channel default button", "default:set:worker"),
		luButton("clear channel default button", "default:none"),
		luButton("set thread default button", "default:set:worker:thread-9"),
		luButton("clear thread default button", "default:none:thread-9"),
		luButton("settings observe button", "settings:observe:"+luChannel),
		luButton("settings state change button", "settings:statechange:"+luChannel),
		luButton("notifications on button", "notif:on:worker"),
		luButton("notifications off button", "notif:off:worker"),
	}
}

// luButton returns a linkCheckHandler that presses the button customID.
func luButton(name, customID string) linkCheckHandler {
	return linkCheckHandler{name, func(_ *testing.T, e *linkedUserEnv, u string) {
		i := asUser(luInteraction(discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{
			CustomID: customID,
		}), u)
		e.callback.Dispatch(e.session, i, customID, nil)
	}}
}

// useWriteCountingStore routes the handlers' store writes through a
// writeCountingStore over the env's current store.
func (e *linkedUserEnv) useWriteCountingStore() *writeCountingStore {
	ws := &writeCountingStore{Store: e.store}
	e.commands.store = ws
	e.callback.store = ws
	return ws
}

func TestHandlers_UnlinkedUserIsAskedToLink(t *testing.T) {
	users := []struct {
		name    string
		mapping *DiscordUserMapping
		want    string
	}{
		{name: "no link", want: msgLinkAccountFirst},
		{name: "link without email", mapping: &DiscordUserMapping{
			DiscordUserID: luOtherUser, DiscordUsername: "bob", ScionUserID: "scion-user-2", LinkedAt: time.Now(),
		}, want: msgReRegisterForEmail},
	}

	for _, h := range linkCheckHandlers() {
		for _, u := range users {
			t.Run(h.name+"/"+u.name, func(t *testing.T) {
				e := newLinkedUserEnv(t)
				e.linkChannel(t)
				cs := e.useCountingStore(t)
				if u.mapping != nil {
					require.NoError(t, e.store.CreateUserMapping(context.Background(), u.mapping))
				}
				ws := e.useWriteCountingStore()

				h.run(t, e, luOtherUser)

				assert.Empty(t, e.hub.snapshot(), "no hub call without a linked account")
				assert.Zero(t, cs.reads(), "no agent cache read without a linked account")
				assert.Zero(t, ws.writeCount(), "no write without a linked account")
				bodies := e.discord.allBodies()
				assert.Contains(t, bodies, jsonText(t, u.want))
				for _, other := range []string{msgLinkAccountFirst, msgReRegisterForEmail} {
					if other != u.want {
						assert.NotContains(t, bodies, jsonText(t, other))
					}
				}
			})
		}
	}
}

// jsonText returns msg as it appears inside a JSON string in a Discord
// request body.
func jsonText(t *testing.T, msg string) string {
	t.Helper()
	b, err := json.Marshal(msg)
	require.NoError(t, err)
	return strings.Trim(string(b), `"`)
}

func TestHandleSecretWrites_LinkedUser(t *testing.T) {
	t.Run("set opens the modal", func(t *testing.T) {
		e := newLinkedUserEnv(t)
		e.linkChannel(t)

		e.commands.HandleSecretSet(e.session, luSecretCommand("set", luStringOpt("key", "API_KEY")))

		assert.Contains(t, e.discord.allBodies(), "secret:set:API_KEY:"+luProject)
	})
	t.Run("set submit", func(t *testing.T) {
		e := newLinkedUserEnv(t)

		e.commands.HandleSecretModalSubmit(e.session, luSecretModalSubmit())

		assertLinkedUserCalls(t, e.hub, http.MethodPut, "/api/v1/secrets/API_KEY")
		assert.Contains(t, e.discord.allBodies(), "has been set")
	})
	t.Run("delete", func(t *testing.T) {
		e := newLinkedUserEnv(t)
		e.linkChannel(t)

		e.commands.HandleSecretDelete(e.session, luSecretCommand("delete", luStringOpt("key", "API_KEY")))

		assertLinkedUserCalls(t, e.hub, http.MethodDelete, "/api/v1/secrets/API_KEY")
		assert.Contains(t, e.discord.allBodies(), "has been deleted")
	})
}

func TestHandleAutocomplete_UnlinkedUserGetsNoChoices(t *testing.T) {
	for _, focused := range []string{"agent", "template"} {
		t.Run(focused, func(t *testing.T) {
			e := newLinkedUserEnv(t)
			e.linkChannel(t)
			cs := e.useCountingStore(t)

			e.commands.HandleAutocomplete(e.session, asUser(luAutocomplete("status", focused), luOtherUser))

			assert.Empty(t, e.hub.snapshot())
			assert.Zero(t, cs.reads())
			bodies := e.discord.allBodies()
			assert.Contains(t, bodies, `"type":8`, "an autocomplete result is sent")
			assert.NotContains(t, bodies, "worker")
			assert.NotContains(t, bodies, "default")
		})
	}
}

func TestHandlers_LinkLookupFailureAsksToRetry(t *testing.T) {
	for _, h := range linkCheckHandlers() {
		t.Run(h.name, func(t *testing.T) {
			e := newLinkedUserEnv(t)
			e.linkChannel(t)
			cs := e.useCountingStore(t)
			cs.userMappingError = assert.AnError
			ws := e.useWriteCountingStore()

			h.run(t, e, luDiscordUser)

			assert.Empty(t, e.hub.snapshot())
			assert.Zero(t, cs.reads())
			assert.Zero(t, ws.writeCount())
			assert.Contains(t, e.discord.allBodies(), msgAccountLookupFailed)
		})
	}

	// Autocomplete can only answer with choices, so a failed lookup gives an
	// empty choice list rather than the retry reply.
	for _, focused := range []string{"agent", "template"} {
		t.Run("autocomplete "+focused, func(t *testing.T) {
			e := newLinkedUserEnv(t)
			e.linkChannel(t)
			cs := e.useCountingStore(t)
			cs.userMappingError = assert.AnError

			e.commands.HandleAutocomplete(e.session, luAutocomplete("status", focused))

			assert.Empty(t, e.hub.snapshot())
			assert.Zero(t, cs.reads())
			assert.Equal(t, 1, cs.lookups())
			bodies := e.discord.allBodies()
			assert.Contains(t, bodies, `"type":8`, "an autocomplete result is sent")
			assert.NotContains(t, bodies, "worker")
		})
	}
}

func TestGetAgents_EmptyPrincipalReadsNothing(t *testing.T) {
	e := newLinkedUserEnv(t)
	cs := e.useCountingStore(t)

	agents, err := e.commands.getAgents(context.Background(), luProject, "")

	require.ErrorIs(t, err, errNoLinkedUser)
	assert.Nil(t, agents)
	assert.Zero(t, cs.reads(), "no agent cache read without a principal")
	assert.Empty(t, e.hub.snapshot(), "no hub call without a principal")

	agents, err = e.commands.getAgents(context.Background(), luProject, luPrincipal)
	require.NoError(t, err)
	assert.Equal(t, []string{"worker"}, agents)
}

func TestHandleSetup_NoProjectsTellsUserTheyAreNotAMember(t *testing.T) {
	e := newLinkedUserEnv(t)
	e.hub.setProjects("empty")

	e.commands.HandleSetup(e.session, luCommand("setup"))

	assertLinkedUserCalls(t, e.hub, http.MethodGet, "/api/v1/projects")
	bodies := e.discord.allBodies()
	assert.Contains(t, bodies, "You are not a member of any project.")
	assert.NotContains(t, bodies, "setup:proj:")
}

func TestHandleSetup_ProjectListErrorAsksToRetry(t *testing.T) {
	e := newLinkedUserEnv(t)
	e.hub.setProjects("error")

	e.commands.HandleSetup(e.session, luCommand("setup"))

	assertLinkedUserCalls(t, e.hub, http.MethodGet, "/api/v1/projects")
	bodies := e.discord.allBodies()
	assert.Contains(t, bodies, "Failed to fetch your projects. Please try `/scion setup` again.")
	assert.NotContains(t, bodies, "setup:proj:")
}

func TestHandleSetupProject_SlugFromUserProjects(t *testing.T) {
	tests := []struct {
		name     string
		projects string
		wantSlug string
	}{
		{name: "found in user projects", wantSlug: "proj-one"},
		{name: "falls back to project ID", projects: "empty", wantSlug: luProject},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newLinkedUserEnv(t)
			e.hub.setProjects(tt.projects)

			i := luInteraction(discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{
				CustomID: "setup:proj:" + luProject,
			})
			e.callback.Dispatch(e.session, i, "setup:proj:"+luProject, nil)

			assertLinkedUserCalls(t, e.hub, http.MethodGet, "/api/v1/projects")
			link, err := e.store.GetChannelLink(context.Background(), luChannel)
			require.NoError(t, err)
			require.NotNil(t, link)
			assert.Equal(t, tt.wantSlug, link.ProjectSlug)
		})
	}
}

// newLinkedUserBroker returns a broker on the env's store, session and fake
// hub, using the legacy inbound path and an empty agent cache.
func newLinkedUserBroker(t *testing.T, e *linkedUserEnv, hubURL string) *DiscordBroker {
	t.Helper()
	return &DiscordBroker{
		log:           discardLogger(),
		session:       e.session,
		store:         e.store,
		hubClient:     NewHTTPHubClient(hubURL, "", "", nil),
		hubURL:        hubURL,
		pluginName:    "discord",
		httpClient:    &http.Client{Timeout: 5 * time.Second},
		sentIDs:       make(map[string]time.Time),
		subs:          make(map[string]bool),
		threadParents: make(map[string]string),
		config:        &Config{},
		botUser:       &discordgo.User{ID: "BOT123", Username: "TestBot"},
		agentCacheTTL: 30 * time.Second,
	}
}

func luChannelMessage(authorID, content string) *discordgo.MessageCreate {
	return &discordgo.MessageCreate{Message: &discordgo.Message{
		ID:        "msg-1",
		ChannelID: luChannel,
		GuildID:   testGuildID,
		Content:   content,
		Author:    &discordgo.User{ID: authorID, Username: "someone"},
		Timestamp: time.Now(),
		Type:      discordgo.MessageTypeDefault,
	}}
}

func newLinkedUserHubServer(t *testing.T, e *linkedUserEnv) string {
	t.Helper()
	srv := httptest.NewServer(e.hub)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestHandleIncomingMessage_RefreshesAgentsAsSender(t *testing.T) {
	e := newLinkedUserEnv(t)
	e.linkChannel(t)
	b := newLinkedUserBroker(t, e, newLinkedUserHubServer(t, e))

	cs := &countingStore{Store: e.store}
	b.store = cs

	b.handleIncomingMessage(e.session, luChannelMessage(luDiscordUser, "@worker hello"))

	assertLinkedUserCalls(t, e.hub, http.MethodGet, "/api/v1/projects/"+luProject+"/agents")
	assert.Equal(t, 1, cs.lookups(), "the sender's link is looked up once")
}

func TestHandleIncomingMessage_LinkWithoutEmailIsDelivered(t *testing.T) {
	e := newLinkedUserEnv(t)
	e.linkChannel(t)
	require.NoError(t, e.store.CreateUserMapping(context.Background(), &DiscordUserMapping{
		DiscordUserID: luOtherUser, DiscordUsername: "bob", ScionUserID: "scion-user-2", LinkedAt: time.Now(),
	}))
	require.NoError(t, e.store.SetProjectAgents(context.Background(), &ProjectAgents{
		ProjectID: luProject, AgentSlugs: []string{"worker"}, RefreshedAt: time.Now(),
	}))
	b := newLinkedUserBroker(t, e, newLinkedUserHubServer(t, e))
	cs := &countingStore{Store: e.store}
	b.store = cs

	b.handleIncomingMessage(e.session, luChannelMessage(luOtherUser, "@worker hello"))

	assert.Equal(t, 1, cs.lookups(), "the sender's link is looked up once")
	assert.NotContains(t, e.discord.allBodies(), "/scion register")
}

func TestHandleIncomingMessage_UnlinkedSenderUsesAgentCacheOnly(t *testing.T) {
	tests := []struct {
		name  string
		cache *ProjectAgents
	}{
		{name: "empty cache"},
		{name: "stale cache", cache: &ProjectAgents{
			ProjectID: luProject, AgentSlugs: []string{"worker"}, RefreshedAt: time.Now().Add(-time.Hour),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newLinkedUserEnv(t)
			e.linkChannel(t)
			if tt.cache != nil {
				require.NoError(t, e.store.SetProjectAgents(context.Background(), tt.cache))
			}
			b := newLinkedUserBroker(t, e, newLinkedUserHubServer(t, e))

			b.handleIncomingMessage(e.session, luChannelMessage(luOtherUser, "@worker hello"))

			assert.Empty(t, e.hub.snapshot(), "no hub call for an unlinked sender")
			if tt.cache != nil {
				// The cached agent resolves, so the sender is asked to register.
				assert.Contains(t, e.discord.allBodies(), "/scion register")
			}
		})
	}
}

func TestLookupUserMapping_LogsFailedLookup(t *testing.T) {
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, nil))

	store := &countingStore{Store: newTestStore(t), userMappingError: assert.AnError}
	assert.Nil(t, lookupUserMapping(context.Background(), store, log, "du-carol"))
	assert.Contains(t, buf.String(), "level=WARN")
	assert.Contains(t, buf.String(), "discord_user_id=du-carol")

	buf.Reset()
	store.userMappingError = nil
	assert.Nil(t, lookupUserMapping(context.Background(), store, log, "du-carol"))
	assert.Empty(t, buf.String(), "an unlinked user is not logged")
}

// writeCountingStore wraps a Store and counts default-agent writes.
type writeCountingStore struct {
	Store
	mu     sync.Mutex
	writes int
}

func (w *writeCountingStore) count() {
	w.mu.Lock()
	w.writes++
	w.mu.Unlock()
}

func (w *writeCountingStore) UpdateChannelLink(ctx context.Context, link *ChannelLink) error {
	w.count()
	return w.Store.UpdateChannelLink(ctx, link)
}

func (w *writeCountingStore) SetThreadDefault(ctx context.Context, channelID, threadID, agentSlug string) error {
	w.count()
	return w.Store.SetThreadDefault(ctx, channelID, threadID, agentSlug)
}

func (w *writeCountingStore) DeleteThreadDefault(ctx context.Context, channelID, threadID string) error {
	w.count()
	return w.Store.DeleteThreadDefault(ctx, channelID, threadID)
}

func (w *writeCountingStore) SetNotificationPref(ctx context.Context, pref *NotificationPref) error {
	w.count()
	return w.Store.SetNotificationPref(ctx, pref)
}

func (w *writeCountingStore) writeCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes
}

func TestDefaultAgentButtons_RequireLinkedUser(t *testing.T) {
	const threadID = "thread-9"
	ctx := context.Background()

	channelDefault := func(t *testing.T, e *linkedUserEnv) string {
		t.Helper()
		link, err := e.store.GetChannelLink(ctx, luChannel)
		require.NoError(t, err)
		require.NotNil(t, link)
		return link.DefaultAgent
	}
	threadDefault := func(t *testing.T, e *linkedUserEnv) string {
		t.Helper()
		slug, err := e.store.GetThreadDefault(ctx, luChannel, threadID)
		require.NoError(t, err)
		return slug
	}

	arms := []struct {
		name     string
		customID string
		// seed sets the starting defaults; read returns the default the
		// button writes, and want is its value after a linked user's click.
		seed func(t *testing.T, e *linkedUserEnv)
		read func(t *testing.T, e *linkedUserEnv) string
		want string
	}{
		{
			name:     "setup default agent",
			customID: "setup:dflt:worker",
			read:     channelDefault,
			want:     "worker",
		},
		{
			name:     "set channel default",
			customID: "default:set:worker",
			read:     channelDefault,
			want:     "worker",
		},
		{
			name:     "clear channel default",
			customID: "default:none",
			seed: func(t *testing.T, e *linkedUserEnv) {
				link, err := e.store.GetChannelLink(ctx, luChannel)
				require.NoError(t, err)
				link.DefaultAgent = "worker"
				require.NoError(t, e.store.UpdateChannelLink(ctx, link))
			},
			read: channelDefault,
			want: "",
		},
		{
			name:     "set thread default",
			customID: "default:set:worker:" + threadID,
			read:     threadDefault,
			want:     "worker",
		},
		{
			name:     "clear thread default",
			customID: "default:none:" + threadID,
			seed: func(t *testing.T, e *linkedUserEnv) {
				require.NoError(t, e.store.SetThreadDefault(ctx, luChannel, threadID, "worker"))
			},
			read: threadDefault,
			want: "",
		},
	}

	users := []struct {
		name    string
		user    string
		mapping *DiscordUserMapping
		reply   string
	}{
		{name: "linked", user: luDiscordUser},
		{name: "no link", user: luOtherUser, reply: msgLinkAccountFirst},
		{name: "link without email", user: luOtherUser, mapping: &DiscordUserMapping{
			DiscordUserID: luOtherUser, DiscordUsername: "bob", ScionUserID: "scion-user-2", LinkedAt: time.Now(),
		}, reply: msgReRegisterForEmail},
	}

	for _, arm := range arms {
		for _, u := range users {
			t.Run(arm.name+"/"+u.name, func(t *testing.T) {
				e := newLinkedUserEnv(t)
				e.linkChannel(t)
				if u.mapping != nil {
					require.NoError(t, e.store.CreateUserMapping(ctx, u.mapping))
				}
				if arm.seed != nil {
					arm.seed(t, e)
				}
				before := arm.read(t, e)

				ws := &writeCountingStore{Store: e.store}
				e.callback.store = ws
				i := asUser(luInteraction(discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{
					CustomID: arm.customID,
				}), u.user)
				e.callback.Dispatch(e.session, i, arm.customID, nil)

				if u.reply == "" {
					assert.Equal(t, 1, ws.writeCount())
					assert.Equal(t, arm.want, arm.read(t, e))
					return
				}
				assert.Zero(t, ws.writeCount(), "no default-agent write without a linked account")
				assert.Equal(t, before, arm.read(t, e))
				assert.Contains(t, e.discord.allBodies(), jsonText(t, u.reply))
			})
		}
	}
}

func TestSettingsAndNotificationButtons_RequireLinkedUser(t *testing.T) {
	ctx := context.Background()

	readLink := func(t *testing.T, e *linkedUserEnv) *ChannelLink {
		t.Helper()
		link, err := e.store.GetChannelLink(ctx, luChannel)
		require.NoError(t, err)
		require.NotNil(t, link)
		return link
	}
	notifPrefs := func(t *testing.T, e *linkedUserEnv, user string) string {
		t.Helper()
		prefs, err := e.store.GetNotificationPrefs(ctx, user, luProject)
		require.NoError(t, err)
		var out []string
		for _, p := range prefs {
			out = append(out, p.AgentSlug+"="+map[bool]string{true: "on", false: "off"}[p.Enabled])
		}
		return strings.Join(out, ",")
	}

	arms := []struct {
		name     string
		customID string
		// read returns the state the button writes for user; want is its
		// value after a linked user's click.
		read func(t *testing.T, e *linkedUserEnv, user string) string
		want string
	}{
		{
			name:     "toggle observe",
			customID: "settings:observe:" + luChannel,
			read: func(t *testing.T, e *linkedUserEnv, _ string) string {
				return strconv.FormatBool(readLink(t, e).ShowAgentToAgent)
			},
			want: "true",
		},
		{
			name:     "toggle state changes",
			customID: "settings:statechange:" + luChannel,
			read: func(t *testing.T, e *linkedUserEnv, _ string) string {
				return strconv.FormatBool(readLink(t, e).ShowStateChanges)
			},
			want: "true",
		},
		{
			name:     "notifications on",
			customID: "notif:on:worker",
			read:     notifPrefs,
			want:     "worker=on",
		},
		{
			name:     "notifications off",
			customID: "notif:off:worker",
			read:     notifPrefs,
			want:     "worker=off",
		},
	}

	users := []struct {
		name    string
		user    string
		mapping *DiscordUserMapping
		reply   string
	}{
		{name: "linked", user: luDiscordUser},
		{name: "no link", user: luOtherUser, reply: msgLinkAccountFirst},
		{name: "link without email", user: luOtherUser, mapping: &DiscordUserMapping{
			DiscordUserID: luOtherUser, DiscordUsername: "bob", ScionUserID: "scion-user-2", LinkedAt: time.Now(),
		}, reply: msgReRegisterForEmail},
	}

	for _, arm := range arms {
		for _, u := range users {
			t.Run(arm.name+"/"+u.name, func(t *testing.T) {
				e := newLinkedUserEnv(t)
				e.linkChannel(t)
				if u.mapping != nil {
					require.NoError(t, e.store.CreateUserMapping(ctx, u.mapping))
				}
				before := arm.read(t, e, u.user)

				ws := &writeCountingStore{Store: e.store}
				e.callback.store = ws
				i := asUser(luInteraction(discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{
					CustomID: arm.customID,
				}), u.user)
				e.callback.Dispatch(e.session, i, arm.customID, nil)

				if u.reply == "" {
					assert.Equal(t, 1, ws.writeCount())
					assert.Equal(t, arm.want, arm.read(t, e, u.user))
					return
				}
				assert.Zero(t, ws.writeCount(), "no write without a linked account")
				assert.Equal(t, before, arm.read(t, e, u.user))
				assert.Contains(t, e.discord.allBodies(), jsonText(t, u.reply))
			})
		}
	}
}

func TestSettingsButtons_ActOnTheChannelTheyArePressedIn(t *testing.T) {
	const otherChannel = "chan-2"
	const threadID = "thread-9"
	ctx := context.Background()

	setup := func(t *testing.T) (*linkedUserEnv, *writeCountingStore) {
		t.Helper()
		e := newLinkedUserEnv(t)
		e.linkChannel(t)
		require.NoError(t, e.store.CreateChannelLink(ctx, &ChannelLink{
			ChannelID:   otherChannel,
			GuildID:     testGuildID,
			ProjectID:   "p2",
			ProjectSlug: "proj-two",
			LinkedBy:    luOtherUser,
			LinkedAt:    time.Now(),
			Active:      true,
		}))
		ws := &writeCountingStore{Store: e.store}
		e.callback.store = ws
		return e, ws
	}
	press := func(e *linkedUserEnv, channelID, customID string) {
		i := luInteraction(discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{
			CustomID: customID,
		})
		i.ChannelID = channelID
		e.callback.Dispatch(e.session, i, customID, nil)
	}
	observe := func(t *testing.T, e *linkedUserEnv, channelID string) bool {
		t.Helper()
		link, err := e.store.GetChannelLink(ctx, channelID)
		require.NoError(t, err)
		require.NotNil(t, link)
		return link.ShowAgentToAgent
	}

	for _, action := range []string{"observe", "statechange"} {
		t.Run(action+"/panel for another channel", func(t *testing.T) {
			e, ws := setup(t)

			press(e, luChannel, "settings:"+action+":"+otherChannel)

			assert.Zero(t, ws.writeCount(), "no write for a panel from another channel")
			for _, ch := range []string{luChannel, otherChannel} {
				link, err := e.store.GetChannelLink(ctx, ch)
				require.NoError(t, err)
				assert.False(t, link.ShowAgentToAgent, ch)
				assert.False(t, link.ShowStateChanges, ch)
			}
			assert.Contains(t, e.discord.allBodies(), jsonText(t, msgSettingsOtherChannel))
		})
	}

	t.Run("thread press updates the linked parent channel", func(t *testing.T) {
		e, ws := setup(t)
		require.NoError(t, e.session.State.ChannelAdd(&discordgo.Channel{
			ID: threadID, GuildID: testGuildID, ParentID: luChannel, Type: discordgo.ChannelTypeGuildPublicThread,
		}))

		press(e, threadID, "settings:observe:"+luChannel)

		assert.Equal(t, 1, ws.writeCount())
		assert.True(t, observe(t, e, luChannel))
		assert.False(t, observe(t, e, otherChannel))
	})
}

func TestAskUserReplies_SenderIsLinkedUser(t *testing.T) {
	ctx := context.Background()
	users := []struct {
		name string
		user string
		want string
	}{
		{name: "linked", user: luDiscordUser, want: luPrincipal},
		{name: "no link", user: luOtherUser, want: "discord:" + luOtherUser},
	}

	pending := &PendingAskUser{
		RequestID: "req-1",
		ChannelID: luChannel,
		AgentSlug: "worker",
		ProjectID: luProject,
		ExpiresAt: time.Now().Add(time.Hour),
	}

	for _, u := range users {
		t.Run("button/"+u.name, func(t *testing.T) {
			e := newLinkedUserEnv(t)
			var got *messages.StructuredMessage
			e.callback.deliverInbound = func(_ string, msg *messages.StructuredMessage) *hubError {
				got = msg
				return nil
			}
			i := asUser(luInteraction(discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{}), u.user)

			require.Nil(t, e.callback.deliverAskUserResponse(ctx, i, pending, "yes"))

			require.NotNil(t, got)
			assert.Equal(t, u.want, got.Sender)
		})
		t.Run("modal/"+u.name, func(t *testing.T) {
			e := newLinkedUserEnv(t)
			req := *pending
			require.NoError(t, e.store.CreatePendingAskUser(ctx, &req))
			var got *messages.StructuredMessage
			deliver := func(_ string, msg *messages.StructuredMessage) *hubError {
				got = msg
				return nil
			}
			i := asUser(luInteraction(discordgo.InteractionModalSubmit, discordgo.ModalSubmitInteractionData{
				CustomID: "ask:modal:" + pending.RequestID,
				Components: []discordgo.MessageComponent{
					&discordgo.ActionsRow{Components: []discordgo.MessageComponent{
						&discordgo.TextInput{CustomID: "response", Value: "yes"},
					}},
				},
			}), u.user)

			HandleModalSubmit(e.session, i, e.store, deliver, nil)

			require.NotNil(t, got)
			assert.Equal(t, u.want, got.Sender)
		})
	}
}
