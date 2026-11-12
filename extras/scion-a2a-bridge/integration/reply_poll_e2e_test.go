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

package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/bridge"
	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/identity"
	bridgestate "github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// Agent replies reach A2A callers (upstream issue 4603). A real hub serves a
// project with one agent; the A2A bridge sits in front of it, receiving the
// hub's broker publications as a broker spoke. An A2A client calls the
// agent as a local hub user with a hub user JWT (the bridge's hubJWT
// scheme), or, in legacy mode, with the bridge's API key. The agent's reply
// carries the "web" channel by default (or the channel reply affinity
// chose), which the hub hands only to the in-process bus and the spoke that
// serves that channel, so the bridge never receives it as a push; the call
// must still return it, read from the caller's own conversation.
//
// The agent is the one stand-in: a dispatcher records what the hub delivers
// to it, and the test answers as the agent through the hub's
// outbound-message and status routes with an agent credential the hub
// signed.
//
// Standalone variants run the bridge with the durable PostgreSQL stores
// (TEST_DATABASE_URL), as standalone deployments do.

const (
	rplDevToken   = "scion_dev_reply_poll_e2e_token_0123456789abcdef"
	rplSigningKey = "reply-poll-e2e-user-signing-key-0123456789abcdef"
	rplAPIKey     = "reply-poll-e2e-legacy-api-key-0123456789"
	rplAgentSlug  = "helper"
	// rplWait bounds every wait on a delivery or an A2A answer. It is sized
	// for the A2A integration job's -race and -count=3 runs.
	rplWait = 60 * time.Second
	// rplBridgeSendTimeout is the bridge's own wait for an agent reply.
	rplBridgeSendTimeout = 5 * time.Second
	// rplLongSendTimeout is the bridge's wait in tests that take several
	// hub round trips before the agent replies.
	rplLongSendTimeout = 15 * time.Second

	// The bridge's refusal texts, as an A2A client sees them.
	rplMsgForbidden = "This access token can't read your conversation with this agent (it needs the inbox:read scope and access to the agent's project); agent replies can't be returned."
	rplMsgRouteFail = "Failed to route message to agent"
)

// rplRequest is one request the hub served.
type rplRequest struct {
	method string
	path   string
	at     time.Time
}

// rplRecorder records the requests the hub serves. It can rewrite a
// request before the hub serves it, and can answer sends without the
// message ID, as an older hub does.
type rplRecorder struct {
	mu                 sync.Mutex
	requests           []rplRequest
	rewrite            func(*http.Request)
	stripSendMessageID bool
}

func (r *rplRecorder) setRewrite(fn func(*http.Request)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rewrite = fn
}

func (r *rplRecorder) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.requests = append(r.requests, rplRequest{method: req.Method, path: req.URL.Path, at: time.Now()})
		rewrite := r.rewrite
		strip := r.stripSendMessageID
		r.mu.Unlock()
		if rewrite != nil {
			rewrite(req)
		}
		if strip && req.Method == http.MethodPost && strings.HasPrefix(req.URL.Path, "/api/v1/agents/") && strings.HasSuffix(req.URL.Path, "/message") {
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, req)
			body := rec.Body.Bytes()
			var fields map[string]any
			if rec.Code == http.StatusOK && json.Unmarshal(body, &fields) == nil {
				delete(fields, "message_id")
				if data, err := json.Marshal(fields); err == nil {
					body = data
				}
			}
			for k, vs := range rec.Header() {
				if k == "Content-Length" {
					continue
				}
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(rec.Code)
			_, _ = w.Write(body)
			return
		}
		next.ServeHTTP(w, req)
	})
}

func (r *rplRecorder) snapshot() []rplRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]rplRequest(nil), r.requests...)
}

// rplLogBuffer collects the bridge's log lines at every level.
type rplLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *rplLogBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *rplLogBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// rplDispatcher stands in for the hub's runtime broker: it records each
// message the hub delivers to an agent. Every other dispatch answers an
// error; embedding the interface keeps the type complete if it grows.
type rplDispatcher struct {
	hub.AgentDispatcher
	delivered chan *messages.StructuredMessage
}

var errRPLDispatchUnavailable = errors.New("not available in this test")

func (d *rplDispatcher) DispatchAgentMessage(_ context.Context, _ *store.Agent, message string, _ bool, structuredMsg *messages.StructuredMessage) error {
	var got messages.StructuredMessage
	if structuredMsg != nil {
		got = *structuredMsg
	} else {
		got.Msg = message
	}
	select {
	case d.delivered <- &got:
		return nil
	default:
		return errors.New("delivery buffer full")
	}
}

func (d *rplDispatcher) DispatchAgentCreate(context.Context, *store.Agent) (*hub.CreateDispatchResult, error) {
	return nil, errRPLDispatchUnavailable
}

func (d *rplDispatcher) DispatchAgentProvision(context.Context, *store.Agent) error {
	return errRPLDispatchUnavailable
}

func (d *rplDispatcher) DispatchAgentReprovision(context.Context, *store.Agent, hub.ReprovisionOptions) error {
	return errRPLDispatchUnavailable
}

func (d *rplDispatcher) DispatchAgentStart(context.Context, *store.Agent, string, bool) error {
	return errRPLDispatchUnavailable
}

func (d *rplDispatcher) DispatchAgentStop(context.Context, *store.Agent) error {
	return errRPLDispatchUnavailable
}

func (d *rplDispatcher) DispatchAgentRestart(context.Context, *store.Agent) error {
	return errRPLDispatchUnavailable
}

func (d *rplDispatcher) DispatchAgentResetAuth(context.Context, *store.Agent) error {
	return errRPLDispatchUnavailable
}

func (d *rplDispatcher) DispatchAgentDelete(context.Context, *store.Agent, bool, bool, bool, time.Time) error {
	return errRPLDispatchUnavailable
}

func (d *rplDispatcher) DispatchAgentLogs(context.Context, *store.Agent, int) (string, error) {
	return "", errRPLDispatchUnavailable
}

func (d *rplDispatcher) DispatchAgentExec(context.Context, *store.Agent, []string, int) (string, int, error) {
	return "", 0, errRPLDispatchUnavailable
}

func (d *rplDispatcher) DispatchCheckAgentPrompt(context.Context, *store.Agent) (bool, error) {
	return false, errRPLDispatchUnavailable
}

func (d *rplDispatcher) DispatchAgentCreateWithGather(context.Context, *store.Agent) (*hub.CreateDispatchResult, error) {
	return nil, errRPLDispatchUnavailable
}

func (d *rplDispatcher) DispatchFinalizeEnv(context.Context, *store.Agent, map[string]string) (*hub.CreateDispatchResult, error) {
	return nil, errRPLDispatchUnavailable
}

// rplBrokerSpoke is the bridge's broker as a spoke of the hub's fan-out
// event bus, in place of the plugin RPC adapter: Publish hands the message
// to the bridge, Subscribe passes the pattern on. The spoke has no channel,
// so a message with a channel goes only to the in-process bus and the
// spoke of that channel.
// It counts, per topic, the messages the bridge has finished handling.
type rplBrokerSpoke struct {
	broker *bridge.BrokerServer

	mu      sync.Mutex
	handled map[string]int
}

func (s *rplBrokerSpoke) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	err := s.broker.Publish(ctx, topic, msg)
	s.mu.Lock()
	if s.handled == nil {
		s.handled = map[string]int{}
	}
	s.handled[topic]++
	s.mu.Unlock()
	return err
}

// handledWhere returns the number of handled messages whose topic matches.
func (s *rplBrokerSpoke) handledWhere(match func(topic string) bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for topic, count := range s.handled {
		if match(topic) {
			n += count
		}
	}
	return n
}

func (s *rplBrokerSpoke) Subscribe(pattern string, _ eventbus.EventHandler) (eventbus.Subscription, error) {
	if err := s.broker.Subscribe(pattern); err != nil {
		return nil, err
	}
	return rplSpokeSubscription{broker: s.broker, pattern: pattern}, nil
}

func (s *rplBrokerSpoke) Close() error { return nil }

type rplSpokeSubscription struct {
	broker  *bridge.BrokerServer
	pattern string
}

func (s rplSpokeSubscription) Unsubscribe() error { return s.broker.Unsubscribe(s.pattern) }

// rplChannelSpoke is a named channel spoke (a chat integration) that
// records what the hub hands it.
type rplChannelSpoke struct {
	mu        sync.Mutex
	published []messages.StructuredMessage
}

func (s *rplChannelSpoke) Publish(_ context.Context, _ string, msg *messages.StructuredMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.published = append(s.published, *msg)
	return nil
}

func (s *rplChannelSpoke) Subscribe(string, eventbus.EventHandler) (eventbus.Subscription, error) {
	return rplNoSubscription{}, nil
}

func (s *rplChannelSpoke) Close() error { return nil }

func (s *rplChannelSpoke) saw(channel, text string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.published {
		if m.Channel == channel && m.Msg == text {
			return true
		}
	}
	return false
}

type rplNoSubscription struct{}

func (rplNoSubscription) Unsubscribe() error { return nil }

// rplUserSpec names a local user of a test.
type rplUserSpec struct {
	email string
	// noMessage leaves the user out of the agent's ancestry: the hub
	// refuses the user's messages to the agent.
	noMessage bool
}

// rplOptions configures a test topology.
type rplOptions struct {
	users []rplUserSpec
	// legacyAdmin, when set, runs the bridge in legacy mode (bearer API
	// key, no caller identity) with this local user as the bridge admin.
	legacyAdmin string
	// standalone runs the bridge with the durable PostgreSQL stores;
	// replicas is the number of bridge replicas on those stores (the hub
	// publishes to the first).
	standalone bool
	replicas   int
	// slackSpoke registers a "slack" channel spoke and a reply affinity
	// store.
	slackSpoke bool
	// stripSendMessageID answers sends without the message ID.
	stripSendMessageID bool
	sendTimeout        time.Duration
	// replyPollOff runs the bridge with reply_poll.enabled false.
	replyPollOff bool
}

// rplReplica is one bridge process.
type rplReplica struct {
	b      *bridge.Bridge
	broker *bridge.BrokerServer
	url    string
	events bridgestate.Store
}

// rplUser is a local hub user and the hub user JWT an A2A client holds.
type rplUser struct {
	user  *store.User
	token string
}

// rplEnv is the whole topology of a test.
type rplEnv struct {
	hub          *hub.Server
	hubStore     store.Store
	hubURL       string
	recorder     *rplRecorder
	logs         *rplLogBuffer
	projectID    string
	projectSlug  string
	routeProject string
	agent        *store.Agent
	agentToken   string
	dispatcher   *rplDispatcher
	client       *http.Client
	users        map[string]rplUser
	admin        rplUser
	replicas     []*rplReplica
	webChat      hub.WebChatStore
	slack        *rplChannelSpoke
	spoke        *rplBrokerSpoke
}

func rplListener(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	ts := httptest.NewUnstartedServer(http.NotFoundHandler())
	t.Cleanup(ts.Close)
	return ts, "http://" + ts.Listener.Addr().String()
}

// rplDo sends one request and returns the status and body.
func (e *rplEnv) rplDo(t *testing.T, method, url string, header map[string]string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode %s %s: %v", method, url, err)
		}
		reader = bytes.NewReader(data)
	}
	ctx, cancel := context.WithTimeout(context.Background(), rplWait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, url, err)
	}
	return resp.StatusCode, data
}

func rplBearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func rplDecode(t *testing.T, what string, body []byte, out any) {
	t.Helper()
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("decode %s: %v; body %s", what, err, body)
	}
}

// rplDatabaseURL returns TEST_DATABASE_URL, or skips the test when it is
// unset outside the fail-closed CI mode.
func rplDatabaseURL(t *testing.T) string {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		if os.Getenv("TEST_REQUIRE_DATABASE") == "1" {
			t.Fatal("TEST_DATABASE_URL is required in fail-closed mode")
		}
		t.Skip("TEST_DATABASE_URL not set")
	}
	return dbURL
}

// rplNewEnv starts the hub, the project, the agent and the bridge.
func rplNewEnv(t *testing.T, opts rplOptions) *rplEnv {
	t.Helper()
	var dbURL string
	if opts.standalone {
		dbURL = rplDatabaseURL(t)
	}
	if opts.replicas < 1 {
		opts.replicas = 1
	}
	if opts.sendTimeout == 0 {
		opts.sendTimeout = rplBridgeSendTimeout
	}
	// The hub creates its project's directory under the global directory
	// ($HOME/.scion); a per-test HOME keeps every run off the real one.
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	e := &rplEnv{
		recorder:   &rplRecorder{stripSendMessageID: opts.stripSendMessageID},
		logs:       &rplLogBuffer{},
		dispatcher: &rplDispatcher{delivered: make(chan *messages.StructuredMessage, 16)},
		client:     &http.Client{Timeout: rplWait},
		users:      map[string]rplUser{},
	}
	tsHub, hubURL := rplListener(t)
	e.hubURL = hubURL

	dsn := "file:" + filepath.Join(t.TempDir(), "hub.db") + "?_journal_mode=WAL&_busy_timeout=5000"
	entClient, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open hub store: %v", err)
	}
	st := entadapter.NewCompositeStore(entClient)
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate hub store: %v", err)
	}
	// As the pkg/hub test servers do: agent rows written directly to the
	// store have no delegation edges, so the backfill marker Migrate sets
	// is removed.
	_ = st.DeleteHubSetting(ctx, "migration_delegation_edge_backfill_v1")
	e.hubStore = st

	cfg := hub.DefaultServerConfig()
	cfg.HubEndpoint = hubURL
	cfg.CORSEnabled = false
	cfg.DisableCloudLogQuery = true
	cfg.DevAuthToken = rplDevToken
	cfg.DevUserConfig = hub.DevUserConfig{Username: "dev", DisplayName: "Development User", Email: "dev@localhost"}
	cfg.UserTokenConfig.SigningKey = []byte(rplSigningKey)
	srv, err := hub.New(cfg, st)
	if err != nil {
		t.Fatalf("hub.New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	e.hub = srv
	tsHub.Config.Handler = e.recorder.wrap(srv.Handler())
	tsHub.Start()

	if opts.slackSpoke {
		// Reply affinity lives in the webchat store.
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "webchat.db"))
		if err != nil {
			t.Fatalf("open webchat store: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		wcs := hub.NewWebChatStore(db, "sqlite")
		if err := wcs.Init(); err != nil {
			t.Fatalf("init webchat store: %v", err)
		}
		srv.SetWebChatStore(wcs)
		e.webChat = wcs
		e.slack = &rplChannelSpoke{}
	}

	// The project (owned by the dev user), a runtime broker, the users,
	// and a running agent on the broker.
	status, body := e.rplDo(t, http.MethodPost, hubURL+"/api/v1/projects", rplBearer(rplDevToken),
		map[string]string{"name": "reply-poll-e2e"})
	if status != http.StatusCreated {
		t.Fatalf("create project: status %d, want 201; body %s", status, body)
	}
	var project struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	}
	rplDecode(t, "project", body, &project)
	if project.ID == "" || project.Slug == "" {
		t.Fatalf("create project: no id or slug; body %s", body)
	}
	e.projectID, e.projectSlug = project.ID, project.Slug
	// Standalone push correlation compares the topic's project with the
	// route's, so the standalone bridge exposes the project by its ID.
	e.routeProject = e.projectSlug
	if opts.standalone {
		e.routeProject = e.projectID
	}

	brokerID := uuid.NewString()
	if err := st.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "reply-poll-e2e-broker", Slug: "reply-poll-e2e-broker", Status: store.BrokerStatusOnline,
	}); err != nil {
		t.Fatalf("create runtime broker: %v", err)
	}
	var ancestry []string
	for _, spec := range opts.users {
		u := e.rplLocalUser(t, spec.email)
		e.users[spec.email] = u
		if !spec.noMessage {
			ancestry = append(ancestry, u.user.ID)
		}
	}
	if opts.legacyAdmin != "" {
		e.admin = e.rplLocalUser(t, opts.legacyAdmin)
		ancestry = append(ancestry, e.admin.user.ID)
	}
	e.agent = &store.Agent{
		ID: uuid.NewString(), Name: rplAgentSlug, Slug: rplAgentSlug, ProjectID: e.projectID,
		RuntimeBrokerID: brokerID, Phase: "running", MessageMode: store.MessageModeProject,
		OwnerID: hub.DevUserID, Ancestry: ancestry,
	}
	if err := st.CreateAgent(ctx, e.agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	token, cred, err := srv.GetAgentTokenService().SignAgentToken(hub.AgentTokenGrant{
		AgentID: e.agent.ID, ProjectID: e.projectID, Scopes: hub.ScopesForRole(hub.AgentRoleFull),
	}, "")
	if err != nil {
		t.Fatalf("sign agent credential: %v", err)
	}
	if err := st.CreateAgentCredential(ctx, cred); err != nil {
		t.Fatalf("record agent credential: %v", err)
	}
	e.agentToken = token

	// The bridge replicas.
	minter, err := identity.NewTokenMinter([]byte(rplSigningKey))
	if err != nil {
		t.Fatalf("bridge token minter: %v", err)
	}
	hubCfg := bridge.HubConfig{Endpoint: hubURL, User: "a2a-bridge@example.invalid"}
	authCfg := bridge.AuthConfig{Scheme: "hubJWT"}
	adminClient, err := hubclient.New(hubURL, hubclient.WithBearerToken(rplDevToken))
	if err != nil {
		t.Fatalf("bridge hub client: %v", err)
	}
	if opts.legacyAdmin != "" {
		// Legacy mode: the admin credential is minted for the bridge admin
		// user, whose conversation with the agent carries the replies.
		hubCfg.User, hubCfg.UserID = e.admin.user.Email, e.admin.user.ID
		authCfg = bridge.AuthConfig{Scheme: "bearer", APIKey: rplAPIKey}
		adminClient, err = hubclient.New(hubURL, hubclient.WithAuthenticator(
			identity.NewMintingAuth(minter, e.admin.user.ID, e.admin.user.Email, "admin", 15*time.Minute)))
		if err != nil {
			t.Fatalf("bridge admin hub client: %v", err)
		}
	}
	logger := slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	for i := 0; i < opts.replicas; i++ {
		ts, url := rplListener(t)
		bridgeCfg := &bridge.Config{
			Bridge:   bridge.BridgeConfig{ExternalURL: url},
			Hub:      hubCfg,
			Auth:     authCfg,
			Projects: []bridge.ProjectConfig{{Slug: e.routeProject, ExposedAgents: []string{rplAgentSlug}}},
			Timeouts: bridge.TimeoutConfig{SendMessage: opts.sendTimeout},
		}
		if opts.replyPollOff {
			off := false
			bridgeCfg.ReplyPoll = bridge.ReplyPollConfig{Enabled: &off}
		}
		e.replicas = append(e.replicas, rplStartReplica(t, ts, url, bridgeCfg, adminClient, minter, logger, dbURL))
	}

	// The hub's message broker: the in-process bus plus the first bridge
	// replica as a broker spoke (and the channel spoke when requested),
	// with the broker proxy as that replica's host callbacks. The
	// notification dispatcher starts first so it gets the broker proxy.
	publisher := hub.NewChannelEventPublisher()
	t.Cleanup(publisher.Close)
	srv.SetEventPublisher(publisher)
	srv.SetDispatcher(e.dispatcher)
	srv.StartNotificationDispatcher()
	// The hub's buses log to their own sink: their subscription lines
	// carry topic patterns, and the log check (D9) covers the bridge's
	// lines only.
	hubBusLogger := slog.New(slog.NewTextHandler(io.Discard, nil))
	e.spoke = &rplBrokerSpoke{broker: e.replicas[0].broker}
	buses := []eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: eventbus.NewInProcessEventBus(hubBusLogger)},
		{Name: "a2a-bridge", Bus: e.spoke},
	}
	if e.slack != nil {
		buses = append(buses, eventbus.NamedEventBus{Name: "slack", Bus: e.slack})
	}
	srv.StartMessageBroker(eventbus.NewFanOutEventBus(buses, hubBusLogger))
	proxy := srv.GetMessageBrokerProxy()
	if proxy == nil {
		t.Fatal("hub message broker not started")
	}
	e.replicas[0].broker.SetHostCallbacks(proxy)
	if err := proxy.EnsureProjectSubscriptions(ctx, e.projectID); err != nil {
		t.Fatalf("hub project subscriptions: %v", err)
	}
	return e
}

// rplStartReplica starts one bridge process: plugin mode (in-memory SDK
// task store, SQLite event log) without a database URL, standalone mode
// (durable PostgreSQL stores) with one.
func rplStartReplica(t *testing.T, ts *httptest.Server, url string, cfg *bridge.Config, adminClient hubclient.Client, minter *identity.TokenMinter, logger *slog.Logger, dbURL string) *rplReplica {
	t.Helper()
	sdkLogger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := &rplReplica{url: url}
	var sdkHandler a2asrv.RequestHandler
	var jsonrpc http.Handler
	if dbURL == "" {
		stateStore, err := bridgestate.NewSQLite(filepath.Join(t.TempDir(), "bridge.db"))
		if err != nil {
			t.Fatalf("bridge state store: %v", err)
		}
		t.Cleanup(func() { _ = stateStore.Close() })
		r.events = stateStore
		r.b = bridge.New(stateStore, adminClient, minter, cfg, nil, logger)
		t.Cleanup(r.b.Shutdown)
		r.broker = bridge.NewBrokerServer(r.b.HandleBrokerMessage, logger, context.Background())
		r.b.SetBroker(r.broker)
		innerStore := taskstore.NewInMemory(&taskstore.InMemoryStoreConfig{Authenticator: bridge.RouteKeyAuthenticator()})
		sdkHandler = a2asrv.NewHandler(bridge.NewScionExecutor(r.b, logger),
			a2asrv.WithLogger(sdkLogger),
			a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}),
			a2asrv.WithAgentInactivityTimeout(rplWait),
			a2asrv.WithTaskStore(bridge.NewScopedTaskStore(innerStore)),
		)
		jsonrpc = a2asrv.NewJSONRPCHandler(sdkHandler)
	} else {
		stateStore, err := bridgestate.NewPostgres(dbURL)
		if err != nil {
			t.Fatalf("bridge state store: %v", err)
		}
		t.Cleanup(func() { _ = stateStore.Close() })
		sdkStore, err := bridge.NewPostgresTaskStoreWithDB(stateStore.DB())
		if err != nil {
			t.Fatalf("bridge SDK task store: %v", err)
		}
		t.Cleanup(func() { _ = sdkStore.Close() })
		r.events = stateStore
		r.b = bridge.New(stateStore, adminClient, minter, cfg, nil, logger)
		t.Cleanup(r.b.Shutdown)
		r.b.SetSDKTaskStore(sdkStore)
		barrierStore := bridge.NewBarrierTaskStore(sdkStore)
		r.b.SetBarrierStore(barrierStore)
		r.broker = bridge.NewBrokerServer(r.b.HandleBrokerMessage, logger, context.Background())
		r.b.SetBroker(r.broker)
		sdkHandler = a2asrv.NewHandler(bridge.NewScionExecutor(r.b, logger),
			a2asrv.WithLogger(sdkLogger),
			a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}),
			a2asrv.WithAgentInactivityTimeout(rplWait),
			a2asrv.WithTaskStore(barrierStore),
		)
		jsonrpc = a2asrv.NewJSONRPCHandler(bridge.NewDurableRequestHandler(sdkHandler, sdkStore, stateStore, nil))
	}
	r.b.SetSDKRequestHandler(sdkHandler)
	server := bridge.NewServer(r.b, cfg, nil, logger, jsonrpc)
	server.SetJWTValidator(bridge.NewJWTValidator([]byte(rplSigningKey)))
	ts.Config.Handler = server.Handler()
	ts.Start()
	return r
}

// rplLocalUser stores an active local hub user, makes them a member of the
// project, and returns them with a hub user JWT. rplNewEnv calls it before
// the agent exists, so the agent can record the user in its ancestry.
func (e *rplEnv) rplLocalUser(t *testing.T, email string) rplUser {
	t.Helper()
	ctx := context.Background()
	u := &store.User{
		ID: uuid.NewString(), Email: email, DisplayName: email,
		Role: "member", Status: store.UserStatusActive, Kind: store.UserKindHuman,
	}
	if err := e.hubStore.CreateUser(ctx, u); err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	role, err := e.hubStore.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	if err != nil {
		t.Fatalf("project member role: %v", err)
	}
	status, body := e.rplDo(t, http.MethodPost, e.hubURL+"/api/v1/projects/"+e.projectID+"/members", rplBearer(rplDevToken),
		map[string]string{"roleDefinitionId": role.ID, "principalType": "user", "principalId": u.ID})
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("add %s to the project: status %d, want 201; body %s", email, status, body)
	}
	token, _, err := e.hub.GetUserTokenService().MintAccessToken(u.ID, u.Email, u.DisplayName, u.Role, u.SessionGeneration, hub.ClientTypeWeb)
	if err != nil {
		t.Fatalf("user JWT for %s: %v", email, err)
	}
	return rplUser{user: u, token: token}
}

// rplSendResult is the bridge's answer to one JSON-RPC send.
type rplSendResult struct {
	status int
	body   []byte
	err    error
}

// rplSendAsync sends text to the agent through a bridge replica as an A2A
// client holding token, and delivers the answer on the returned channel.
func (e *rplEnv) rplSendAsync(ctx context.Context, replica int, token, text string) <-chan rplSendResult {
	done := make(chan rplSendResult, 1)
	go func() {
		reqBody, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      uuid.NewString(),
			"method":  "SendMessage",
			"params": map[string]any{
				"message": map[string]any{
					"messageId": uuid.NewString(),
					"role":      "ROLE_USER",
					"parts":     []map[string]any{{"text": text}},
				},
			},
		})
		if err != nil {
			done <- rplSendResult{err: err}
			return
		}
		url := e.replicas[replica].url + "/projects/" + e.routeProject + "/agents/" + rplAgentSlug + "/jsonrpc"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
		if err != nil {
			done <- rplSendResult{err: err}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := e.client.Do(req)
		if err != nil {
			done <- rplSendResult{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		done <- rplSendResult{status: resp.StatusCode, body: body, err: err}
	}()
	return done
}

// rplOutcome is how a task ended, as the A2A client sees it.
type rplOutcome struct {
	TaskID string
	State  a2a.TaskState
	Text   string
}

// rplTaskOutcome decodes a JSON-RPC send answer into the task's final state
// and status message text.
func rplTaskOutcome(t *testing.T, label string, res rplSendResult) rplOutcome {
	t.Helper()
	if res.err != nil {
		t.Fatalf("%s: send failed: %v", label, res.err)
	}
	if res.status != http.StatusOK {
		t.Fatalf("%s: bridge answered HTTP %d, want 200; body %s", label, res.status, res.body)
	}
	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	rplDecode(t, label+" JSON-RPC answer", res.body, &rpc)
	if rpc.Error != nil {
		t.Fatalf("%s: JSON-RPC error %d %q; body %s", label, rpc.Error.Code, rpc.Error.Message, res.body)
	}
	var wrapped struct {
		Task json.RawMessage `json:"task"`
	}
	rplDecode(t, label+" send result", rpc.Result, &wrapped)
	raw := []byte(rpc.Result)
	if len(wrapped.Task) > 0 && string(wrapped.Task) != "null" {
		raw = wrapped.Task
	}
	var task a2a.Task
	rplDecode(t, label+" task", raw, &task)
	var text []string
	if msg := task.Status.Message; msg != nil {
		for _, p := range msg.Parts {
			if p != nil {
				text = append(text, p.Text())
			}
		}
	}
	return rplOutcome{TaskID: string(task.ID), State: task.Status.State, Text: strings.Join(text, "\n")}
}

// rplAgentReply answers as the agent to recipient through the hub's
// outbound-message route.
func (e *rplEnv) rplAgentReply(t *testing.T, label, recipient, text, msgType string) {
	t.Helper()
	status, body := e.rplDo(t, http.MethodPost, e.hubURL+"/api/v1/agents/"+e.agent.ID+"/outbound-message",
		map[string]string{"X-Scion-Agent-Token": e.agentToken},
		hub.OutboundMessageRequest{Recipient: recipient, Msg: text, Type: msgType})
	if status != http.StatusOK {
		t.Fatalf("%s: agent reply: status %d, want 200; body %s", label, status, body)
	}
}

// rplAgentStatus reports an agent activity through the hub's status route.
func (e *rplEnv) rplAgentStatus(t *testing.T, label, activity, message string) {
	t.Helper()
	status, body := e.rplDo(t, http.MethodPost, e.hubURL+"/api/v1/agents/"+e.agent.ID+"/status",
		map[string]string{"X-Scion-Agent-Token": e.agentToken},
		map[string]string{"activity": activity, "message": message})
	if status != http.StatusOK && status != http.StatusNoContent {
		t.Fatalf("%s: agent status %s: status %d, want 200; body %s", label, activity, status, body)
	}
}

// rplAwaitDelivery waits for the hub to deliver the caller's message to the
// agent and returns it.
func (e *rplEnv) rplAwaitDelivery(t *testing.T, label, text string, done <-chan rplSendResult) *messages.StructuredMessage {
	t.Helper()
	select {
	case got := <-e.dispatcher.delivered:
		if got.Msg != text {
			t.Fatalf("%s: agent received %q, want %q", label, got.Msg, text)
		}
		if !strings.HasPrefix(got.Sender, "user:") {
			t.Fatalf("%s: envelope sender %q, want a user address", label, got.Sender)
		}
		return got
	case res := <-done:
		t.Fatalf("%s: the bridge answered before the hub delivered the message to the agent: HTTP %d, error %v, body %s",
			label, res.status, res.err, res.body)
	case <-time.After(rplWait):
		t.Fatalf("%s: the hub delivered nothing to the agent within %s", label, rplWait)
	}
	return nil
}

// rplAwaitAnswer waits for the bridge's answer to a send.
func rplAwaitAnswer(t *testing.T, label string, done <-chan rplSendResult) rplOutcome {
	t.Helper()
	select {
	case res := <-done:
		return rplTaskOutcome(t, label, res)
	case <-time.After(rplWait):
		t.Fatalf("%s: the bridge did not answer within %s", label, rplWait)
	}
	return rplOutcome{}
}

// rplSentMessages returns the messages the user sent, as the hub stored
// them.
func (e *rplEnv) rplSentMessages(t *testing.T, label string, u rplUser) []store.Message {
	t.Helper()
	res, err := e.hubStore.ListMessages(context.Background(), store.MessageFilter{SenderID: u.user.ID}, store.ListOptions{})
	if err != nil {
		t.Fatalf("%s: list the user's messages: %v", label, err)
	}
	return res.Items
}

// rplConversationOf returns the conversation of the user's stored
// messages.
func (e *rplEnv) rplConversationOf(t *testing.T, label string, u rplUser) string {
	t.Helper()
	msgs := e.rplSentMessages(t, label, u)
	if len(msgs) == 0 || msgs[0].ConversationID == "" {
		t.Fatalf("%s: the user's messages carry no conversation: %+v", label, msgs)
	}
	return msgs[0].ConversationID
}

// rplAssertNeutralLogs checks that no bridge log line, at any level,
// carries message text, user IDs or emails, conversation IDs, DM keys or
// topic strings.
func (e *rplEnv) rplAssertNeutralLogs(t *testing.T) {
	t.Helper()
	res, err := e.hubStore.ListMessages(context.Background(), store.MessageFilter{ProjectID: e.projectID}, store.ListOptions{})
	if err != nil {
		t.Fatalf("list the project's messages: %v", err)
	}
	forbidden := []string{"dm:", "scion.project."}
	for _, m := range res.Items {
		if len(m.Msg) >= 8 {
			forbidden = append(forbidden, m.Msg)
		}
		if m.ConversationID != "" {
			forbidden = append(forbidden, m.ConversationID)
		}
	}
	for _, u := range e.users {
		forbidden = append(forbidden, u.user.ID, u.user.Email)
	}
	if e.admin.user != nil {
		forbidden = append(forbidden, e.admin.user.ID, e.admin.user.Email)
	}
	out := e.logs.String()
	for _, f := range forbidden {
		if f != "" && strings.Contains(out, f) {
			t.Errorf("a bridge log line contains %q", f)
		}
	}
}

// rplRunReplyReachesCaller is D1: the call returns the agent's default
// reply, read from the caller's own conversation anchored on the message
// the send returned.
func rplRunReplyReachesCaller(t *testing.T, e *rplEnv) {
	alice := e.users["alice@example.com"]
	const label = "local user"
	ctx, cancel := context.WithTimeout(context.Background(), rplWait)
	defer cancel()
	done := e.rplSendAsync(ctx, 0, alice.token, "hello from a local user")
	got := e.rplAwaitDelivery(t, label, "hello from a local user", done)
	e.rplAgentReply(t, label, got.Sender, "the agent's reply", messages.TypeInstruction)

	outcome := rplAwaitAnswer(t, label, done)
	if outcome.State != a2a.TaskStateCompleted || outcome.Text != "the agent's reply" {
		t.Fatalf("%s: task ended %v with %q, want %v with the agent's reply %q",
			label, outcome.State, outcome.Text, a2a.TaskStateCompleted, "the agent's reply")
	}

	// The anchor is the message the send returned: the bridge read exactly
	// that message of the conversation.
	sent := e.rplSentMessages(t, label, alice)
	if len(sent) != 1 {
		t.Fatalf("%s: the hub stored %d messages from the user, want 1", label, len(sent))
	}
	anchorID := sent[0].ID
	var anchorReads int
	for _, r := range e.recorder.snapshot() {
		if r.method == http.MethodGet && strings.HasPrefix(r.path, "/api/v1/conversations/") &&
			strings.Contains(r.path, "/messages/") {
			if !strings.HasSuffix(r.path, "/messages/"+anchorID) {
				t.Fatalf("%s: the bridge read message %s, want only the anchor %s", label, r.path, anchorID)
			}
			anchorReads++
		}
	}
	if anchorReads != 1 {
		t.Fatalf("%s: the bridge read the anchor %d times, want 1", label, anchorReads)
	}
}

// TestAgentReplyReachesLocalUser_PluginMode is D1 in plugin mode, with the
// log check (D9).
func TestAgentReplyReachesLocalUser_PluginMode(t *testing.T) {
	e := rplNewEnv(t, rplOptions{users: []rplUserSpec{{email: "alice@example.com"}}})
	rplRunReplyReachesCaller(t, e)
	e.rplAssertNeutralLogs(t)
}

// TestAgentReplyReachesLocalUser_Standalone is D1 in standalone mode.
func TestAgentReplyReachesLocalUser_Standalone(t *testing.T) {
	e := rplNewEnv(t, rplOptions{users: []rplUserSpec{{email: "alice@example.com"}}, standalone: true})
	rplRunReplyReachesCaller(t, e)
}

// TestAgentReplyOnAffinityChannelReachesCaller is D2: reply affinity routes
// the agent's reply to a chat channel; it carries that channel, reaches
// that channel's spoke, and the call still returns it.
func TestAgentReplyOnAffinityChannelReachesCaller(t *testing.T) {
	e := rplNewEnv(t, rplOptions{users: []rplUserSpec{{email: "alice@example.com"}}, slackSpoke: true})
	alice := e.users["alice@example.com"]
	if err := e.webChat.RecordChannel(context.Background(), alice.user.ID, e.projectID, e.agent.ID, "slack", time.Now()); err != nil {
		t.Fatalf("record reply affinity: %v", err)
	}
	const label = "affinity"
	ctx, cancel := context.WithTimeout(context.Background(), rplWait)
	defer cancel()
	done := e.rplSendAsync(ctx, 0, alice.token, "hello, answer me where I last spoke")
	got := e.rplAwaitDelivery(t, label, "hello, answer me where I last spoke", done)
	e.rplAgentReply(t, label, got.Sender, "a reply routed to the chat channel", messages.TypeInstruction)

	outcome := rplAwaitAnswer(t, label, done)
	if outcome.State != a2a.TaskStateCompleted || outcome.Text != "a reply routed to the chat channel" {
		t.Fatalf("%s: task ended %v with %q, want completed with the routed reply", label, outcome.State, outcome.Text)
	}
	if !e.slack.saw("slack", "a reply routed to the chat channel") {
		t.Fatalf("%s: the slack spoke did not receive the reply with channel slack", label)
	}
}

// TestConcurrentCallersReadOnlyTheirOwnConversations is D4: two users call
// the agent at once and each gets their own reply; a hook that points
// Alice's poll at Bob's conversation is refused by the hub (403), and
// Alice's call fails with no content from Bob.
func TestConcurrentCallersReadOnlyTheirOwnConversations(t *testing.T) {
	e := rplNewEnv(t, rplOptions{
		users:       []rplUserSpec{{email: "alice@example.com"}, {email: "bob@example.com"}},
		sendTimeout: rplLongSendTimeout,
	})
	alice, bob := e.users["alice@example.com"], e.users["bob@example.com"]
	ctx, cancel := context.WithTimeout(context.Background(), rplWait)
	defer cancel()

	texts := map[string]string{"hello from alice": "the reply for alice", "hello from bob": "the reply for bob"}
	aliceDone := e.rplSendAsync(ctx, 0, alice.token, "hello from alice")
	bobDone := e.rplSendAsync(ctx, 0, bob.token, "hello from bob")
	for i := 0; i < 2; i++ {
		select {
		case got := <-e.dispatcher.delivered:
			reply, ok := texts[got.Msg]
			if !ok {
				t.Fatalf("unexpected delivery %q", got.Msg)
			}
			e.rplAgentReply(t, "two users", got.Sender, reply, messages.TypeInstruction)
		case <-time.After(rplWait):
			t.Fatal("two users: the hub did not deliver both messages")
		}
	}
	if out := rplAwaitAnswer(t, "alice", aliceDone); out.State != a2a.TaskStateCompleted || out.Text != "the reply for alice" {
		t.Fatalf("alice's task ended %v with %q, want her own reply", out.State, out.Text)
	}
	if out := rplAwaitAnswer(t, "bob", bobDone); out.State != a2a.TaskStateCompleted || out.Text != "the reply for bob" {
		t.Fatalf("bob's task ended %v with %q, want his own reply", out.State, out.Text)
	}
	e.rplAssertNeutralLogs(t)

	// The hook: after Alice's next send, her poll reads Bob's conversation.
	aliceConv := e.rplConversationOf(t, "alice", alice)
	bobConv := e.rplConversationOf(t, "bob", bob)
	done := e.rplSendAsync(ctx, 0, alice.token, "hello again from alice")
	e.rplAwaitDelivery(t, "hook", "hello again from alice", done)
	alicePage := "/api/v1/conversations/" + aliceConv + "/messages"
	e.recorder.setRewrite(func(req *http.Request) {
		if req.Method == http.MethodGet && req.URL.Path == alicePage {
			req.URL.Path = "/api/v1/conversations/" + bobConv + "/messages"
			req.URL.RawPath = ""
		}
	})
	defer e.recorder.setRewrite(nil)
	out := rplAwaitAnswer(t, "hook", done)
	if out.State != a2a.TaskStateFailed || out.Text != rplMsgForbidden {
		t.Fatalf("hook: alice's task ended %v with %q, want failed with %q", out.State, out.Text, rplMsgForbidden)
	}
	if strings.Contains(out.Text, "the reply for bob") {
		t.Fatalf("hook: alice's answer carries bob's reply")
	}
}

// rplRunOtherUsersPushes is the end-to-end case of D4b: on Bob's topic,
// the hub's input-needed and COMPLETED notifications (Bob subscribes to the
// agent) and an agent message arrive while Alice's call waits; the push
// path's user check drops them all (topic_user), none reaches Alice's
// task's event log, and Alice's call returns her own reply. With reply
// polling on the reply is read from her conversation; with it off there is
// no poller mark, so the user check alone keeps Bob's pushes out, and her
// reply arrives on her own topic.
func rplRunOtherUsersPushes(t *testing.T, replyPollOff bool) {
	e := rplNewEnv(t, rplOptions{
		users:        []rplUserSpec{{email: "alice@example.com"}, {email: "bob@example.com"}},
		sendTimeout:  rplLongSendTimeout,
		replyPollOff: replyPollOff,
	})
	alice, bob := e.users["alice@example.com"], e.users["bob@example.com"]
	bobTopic := projectkeys.UserTopic(e.projectID, bob.user.ID)
	ctx := context.Background()
	if err := e.hubStore.CreateNotificationSubscription(ctx, &store.NotificationSubscription{
		ID: uuid.NewString(), Scope: "agent", AgentID: e.agent.ID, SubscriberType: "user", SubscriberID: bob.user.ID,
		ProjectID: e.projectID, TriggerActivities: []string{"COMPLETED", "WAITING_FOR_INPUT"},
		CreatedAt: time.Now(), CreatedBy: bob.user.ID,
	}); err != nil {
		t.Fatalf("subscribe bob: %v", err)
	}

	const label = "other user's pushes"
	waitCtx, cancel := context.WithTimeout(ctx, rplWait)
	defer cancel()
	done := e.rplSendAsync(waitCtx, 0, alice.token, "hello from alice")
	got := e.rplAwaitDelivery(t, label, "hello from alice", done)

	e.rplAgentStatus(t, label, "waiting_for_input", "a question for bob")
	e.rplAgentStatus(t, label, "completed", "finished bob's turn")
	if err := e.replicas[0].broker.Publish(ctx, bobTopic, &messages.StructuredMessage{
		Version: 1, Sender: "agent:" + rplAgentSlug, SenderID: e.agent.ID, Recipient: "user:" + bob.user.ID,
		Msg: "an agent message for bob", Type: messages.TypeInstruction, Timestamp: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("%s: publish on bob's topic: %v", label, err)
	}
	// Wait until the bridge has handled the hub's two notifications on
	// another user's topic, whatever it decided; the decision is checked
	// below.
	rplWaitFor(t, label+": the hub's notifications on bob's topic handled", func() bool {
		return e.spoke.handledWhere(func(topic string) bool {
			parsed, err := projectkeys.ParseTopic(topic)
			return err == nil && parsed.Kind == projectkeys.TopicKindUser && parsed.Actor != alice.user.ID
		}) >= 2
	})

	if replyPollOff {
		if err := e.replicas[0].broker.Publish(ctx, projectkeys.UserTopic(e.projectID, alice.user.ID), &messages.StructuredMessage{
			Version: 1, Sender: "agent:" + rplAgentSlug, SenderID: e.agent.ID, Recipient: "user:" + alice.user.ID,
			Msg: "the reply for alice", Type: messages.TypeInstruction, Timestamp: time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			t.Fatalf("%s: publish on alice's topic: %v", label, err)
		}
	} else {
		e.rplAgentReply(t, label, got.Sender, "the reply for alice", messages.TypeInstruction)
	}
	out := rplAwaitAnswer(t, label, done)
	if out.State != a2a.TaskStateCompleted || out.Text != "the reply for alice" {
		t.Fatalf("%s: alice's task ended %v with %q, want her own reply", label, out.State, out.Text)
	}
	events, err := e.replicas[0].events.ReadTaskEvents(ctx, out.TaskID, 0, 100)
	if err != nil {
		t.Fatalf("%s: read alice's task events: %v", label, err)
	}
	for _, ev := range events {
		for _, text := range []string{"a question for bob", "bob's turn", "an agent message for bob"} {
			if strings.Contains(string(ev.Payload), text) {
				t.Fatalf("%s: alice's task event %q carries %q from bob's topic", label, ev.DedupKey, text)
			}
		}
	}
	// All three pushes on Bob's topic were dropped by the user check, not
	// by the poller mark.
	if n := strings.Count(e.logs.String(), "reason=topic_user"); n < 3 {
		t.Fatalf("%s: %d topic_user drop lines, want at least 3", label, n)
	}
	e.rplAssertNeutralLogs(t)
}

// TestPushFromOtherUsersNeverReachesCaller is D4b end to end, with reply
// polling on and the log check (D9).
func TestPushFromOtherUsersNeverReachesCaller(t *testing.T) {
	rplRunOtherUsersPushes(t, false)
}

// TestPushFromOtherUsersNeverReachesCaller_ReplyPollOff is D4b end to end
// with reply_poll.enabled false: no poller mark drops the pushes, so only
// the user check keeps them out of Alice's call.
func TestPushFromOtherUsersNeverReachesCaller_ReplyPollOff(t *testing.T) {
	rplRunOtherUsersPushes(t, true)
}

// rplWaitFor polls cond until it holds or rplWait passes.
func rplWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(rplWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestLegacyCallReturnsReplyFromAdminConversation is D6 (a): in legacy mode
// (no caller identity; hub.user an email and hub.user_id the admin's ID)
// the call returns the reply from the admin's own conversation with the
// agent, found by its dmPeer.
func TestLegacyCallReturnsReplyFromAdminConversation(t *testing.T) {
	e := rplNewEnv(t, rplOptions{legacyAdmin: "bridge-admin@example.com"})
	const label = "legacy"
	ctx, cancel := context.WithTimeout(context.Background(), rplWait)
	defer cancel()
	done := e.rplSendAsync(ctx, 0, rplAPIKey, "hello through the legacy bridge")
	got := e.rplAwaitDelivery(t, label, "hello through the legacy bridge", done)
	e.rplAgentReply(t, label, got.Sender, "the reply to the bridge admin", messages.TypeInstruction)
	out := rplAwaitAnswer(t, label, done)
	if out.State != a2a.TaskStateCompleted || out.Text != "the reply to the bridge admin" {
		t.Fatalf("%s: task ended %v with %q, want completed with the reply", label, out.State, out.Text)
	}
	e.rplAssertNeutralLogs(t)
}

// TestCallerWithoutMessagePermissionIsRefusedWithoutPolling: a project
// member the hub does not let message the agent calls it; the hub refuses
// the send, and the call ends at once with no conversation read after the
// send (no poll is started) and nothing delivered to the agent.
func TestCallerWithoutMessagePermissionIsRefusedWithoutPolling(t *testing.T) {
	e := rplNewEnv(t, rplOptions{users: []rplUserSpec{{email: "carol@example.com", noMessage: true}}})
	carol := e.users["carol@example.com"]
	const label = "no message permission"
	ctx, cancel := context.WithTimeout(context.Background(), rplWait)
	defer cancel()
	start := time.Now()
	out := rplAwaitAnswer(t, label, e.rplSendAsync(ctx, 0, carol.token, "hello without permission"))
	if out.State != a2a.TaskStateFailed || out.Text != rplMsgRouteFail {
		t.Fatalf("%s: task ended %v with %q, want failed with %q", label, out.State, out.Text, rplMsgRouteFail)
	}
	if elapsed := time.Since(start); elapsed >= rplBridgeSendTimeout {
		t.Fatalf("%s: the call took %v, want it to end at once, not at the deadline", label, elapsed)
	}
	select {
	case got := <-e.dispatcher.delivered:
		t.Fatalf("%s: the hub delivered %q to the agent", label, got.Msg)
	default:
	}
	requests := e.recorder.snapshot()
	sendIdx := -1
	for i, r := range requests {
		if r.method == http.MethodPost && strings.HasSuffix(r.path, "/message") {
			sendIdx = i
		}
	}
	if sendIdx < 0 {
		t.Fatalf("%s: the bridge never sent the message", label)
	}
	// The call has already ended, so no read can follow it.
	for _, r := range e.recorder.snapshot()[sendIdx+1:] {
		if strings.HasPrefix(r.path, "/api/v1/conversations") {
			t.Fatalf("%s: conversation read %s %s after the refused send, want none", label, r.method, r.path)
		}
	}
}

// rplRunAgentWideStatus is D12: Alice subscribes to the agent. While her
// message waits, the agent's COMPLETED (another conversation's turn)
// reaches Alice's topic, an ERROR is pushed, and the agent asks Bob a
// question, so the hub's WAITING_FOR_INPUT notification lands in Alice's
// conversation after her anchor. Alice's call keeps waiting and returns
// her reply, not input-required. In standalone mode the pushed statuses
// also reach the second replica, which has no poller mark and appends
// them; the wait on the first replica skips them.
func rplRunAgentWideStatus(t *testing.T, e *rplEnv) {
	alice := e.users["alice@example.com"]
	ctx := context.Background()
	if err := e.hubStore.CreateNotificationSubscription(ctx, &store.NotificationSubscription{
		ID: uuid.NewString(), Scope: "agent", AgentID: e.agent.ID, SubscriberType: "user", SubscriberID: alice.user.ID,
		ProjectID: e.projectID, TriggerActivities: []string{"COMPLETED", "WAITING_FOR_INPUT", "ERROR"},
		CreatedAt: time.Now(), CreatedBy: alice.user.ID,
	}); err != nil {
		t.Fatalf("subscribe alice: %v", err)
	}
	const label = "agent-wide status"
	waitCtx, cancel := context.WithTimeout(ctx, rplWait)
	defer cancel()
	done := e.rplSendAsync(waitCtx, 0, alice.token, "hello from alice")
	got := e.rplAwaitDelivery(t, label, "hello from alice", done)
	taskID := got.Metadata["a2aTaskId"]
	aliceTopic := projectkeys.UserTopic(e.projectID, alice.user.ID)

	e.rplAgentStatus(t, label, "completed", "finished another conversation's turn")
	pushedStatus := func(status string) *messages.StructuredMessage {
		m := &messages.StructuredMessage{
			Version: 1, Sender: "agent:" + rplAgentSlug, SenderID: e.agent.ID, Recipient: "user:" + alice.user.ID,
			Msg: "helper has reached a state of " + status, Type: messages.TypeStateChange, Status: status,
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		}
		if taskID != "" {
			m.Metadata = map[string]string{"a2aTaskId": taskID}
		}
		return m
	}
	if err := e.replicas[0].broker.Publish(ctx, aliceTopic, pushedStatus("ERROR")); err != nil {
		t.Fatalf("%s: push ERROR: %v", label, err)
	}
	for _, r := range e.replicas[1:] {
		for _, status := range []string{"COMPLETED", "ERROR"} {
			if err := r.b.HandleBrokerMessage(ctx, aliceTopic, pushedStatus(status)); err != nil {
				t.Fatalf("%s: push %s to another replica: %v", label, status, err)
			}
		}
	}
	e.rplAgentStatus(t, label, "waiting_for_input", "a question for bob")

	// The hub files the WAITING_FOR_INPUT notification in Alice's
	// conversation with no thread ID.
	var notifiedAt time.Time
	rplWaitFor(t, label+": the WAITING_FOR_INPUT notification in alice's conversation", func() bool {
		res, err := e.hubStore.ListMessages(ctx, store.MessageFilter{RecipientID: alice.user.ID}, store.ListOptions{})
		if err != nil {
			return false
		}
		for _, m := range res.Items {
			if m.Type == messages.TypeInputNeeded && m.ThreadID == "" && m.SenderID == e.agent.ID && m.ConversationID != "" {
				notifiedAt = m.CreatedAt
				return true
			}
		}
		return false
	})
	// The bridge has read the conversation since the notification landed.
	aliceConv := e.rplConversationOf(t, label, alice)
	rplWaitFor(t, label+": a poll after the notification", func() bool {
		for _, r := range e.recorder.snapshot() {
			if r.method == http.MethodGet && r.path == "/api/v1/conversations/"+aliceConv+"/messages" && r.at.After(notifiedAt) {
				return true
			}
		}
		return false
	})

	e.rplAgentReply(t, label, got.Sender, "the reply for alice", messages.TypeInstruction)
	out := rplAwaitAnswer(t, label, done)
	if out.State != a2a.TaskStateCompleted || out.Text != "the reply for alice" {
		t.Fatalf("%s: alice's task ended %v with %q, want completed with her reply", label, out.State, out.Text)
	}
}

// TestAgentWideStatusDoesNotEndCall_PluginMode is D12 in plugin mode, with
// the log check (D9).
func TestAgentWideStatusDoesNotEndCall_PluginMode(t *testing.T) {
	e := rplNewEnv(t, rplOptions{users: []rplUserSpec{{email: "alice@example.com"}}, sendTimeout: rplLongSendTimeout})
	rplRunAgentWideStatus(t, e)
	e.rplAssertNeutralLogs(t)
}

// TestAgentWideStatusDoesNotEndCall_Standalone is D12 in standalone mode,
// with a second replica.
func TestAgentWideStatusDoesNotEndCall_Standalone(t *testing.T) {
	e := rplNewEnv(t, rplOptions{
		users: []rplUserSpec{{email: "alice@example.com"}}, sendTimeout: rplLongSendTimeout,
		standalone: true, replicas: 2,
	})
	rplRunAgentWideStatus(t, e)
}

// TestOlderHubPushedQuestionEndsCall_Standalone is D15 (b) in standalone
// mode: a hub that answers sends without the message ID; no poller runs,
// and a pushed input-needed still ends the call input-required.
func TestOlderHubPushedQuestionEndsCall_Standalone(t *testing.T) {
	e := rplNewEnv(t, rplOptions{users: []rplUserSpec{{email: "alice@example.com"}}, standalone: true, stripSendMessageID: true})
	alice := e.users["alice@example.com"]
	const label = "older hub"
	ctx, cancel := context.WithTimeout(context.Background(), rplWait)
	defer cancel()
	done := e.rplSendAsync(ctx, 0, alice.token, "hello to an older hub")
	got := e.rplAwaitDelivery(t, label, "hello to an older hub", done)
	push := &messages.StructuredMessage{
		Version: 1, Sender: "agent:" + rplAgentSlug, SenderID: e.agent.ID, Recipient: "user:" + alice.user.ID,
		Msg: "a pushed question", Type: messages.TypeInputNeeded, Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	if taskID := got.Metadata["a2aTaskId"]; taskID != "" {
		push.Metadata = map[string]string{"a2aTaskId": taskID}
	}
	// The executor clears its mark right after the send answers, then logs
	// that no poller runs.
	rplWaitFor(t, label+": the executor's no-anchor line", func() bool {
		return strings.Contains(e.logs.String(), "reason=no_anchor")
	})
	if err := e.replicas[0].broker.Publish(ctx, projectkeys.UserTopic(e.projectID, alice.user.ID), push); err != nil {
		t.Fatalf("%s: push the question: %v", label, err)
	}
	out := rplAwaitAnswer(t, label, done)
	if out.State != a2a.TaskStateInputRequired || out.Text != "a pushed question" {
		t.Fatalf("%s: task ended %v with %q, want input-required with the pushed question", label, out.State, out.Text)
	}
	for _, r := range e.recorder.snapshot() {
		if r.method == http.MethodGet && strings.Contains(r.path, "/messages/") {
			t.Fatalf("%s: the bridge read an anchor (%s) without a message ID", label, r.path)
		}
	}
}

// TestStandalonePushCopyNeverDuplicatesResponse is D18: standalone, a
// poller live, the agent asks a question (input-needed) and a pushed copy
// arrives too. (a) On the same replica the push is dropped and one message
// event is in the log. (b) On another replica (no mark) the push copy is
// appended, and the client still receives exactly one response, the polled
// one. (c) A pushed message whose msgId is "poll:x" is appended under its
// content hash, and the wait skips it. The pushed copies carry their own
// text so the response shows which copy the wait returned.
func TestStandalonePushCopyNeverDuplicatesResponse(t *testing.T) {
	e := rplNewEnv(t, rplOptions{users: []rplUserSpec{{email: "alice@example.com"}}, standalone: true, replicas: 2})
	alice := e.users["alice@example.com"]
	aliceTopic := projectkeys.UserTopic(e.projectID, alice.user.ID)
	ctx := context.Background()

	run := func(label string, pushTo int, push func(taskID string) *messages.StructuredMessage, question string) (rplOutcome, []bridgestate.TaskEvent) {
		t.Helper()
		waitCtx, cancel := context.WithTimeout(ctx, rplWait)
		defer cancel()
		done := e.rplSendAsync(waitCtx, 0, alice.token, "hello for "+label)
		got := e.rplAwaitDelivery(t, label, "hello for "+label, done)
		taskID := got.Metadata["a2aTaskId"]
		if taskID == "" {
			t.Fatalf("%s: the delivered message carries no task ID", label)
		}
		if err := e.replicas[pushTo].b.HandleBrokerMessage(ctx, aliceTopic, push(taskID)); err != nil {
			t.Fatalf("%s: push: %v", label, err)
		}
		e.rplAgentReply(t, label, got.Sender, question, messages.TypeInputNeeded)
		out := rplAwaitAnswer(t, label, done)
		events, err := e.replicas[0].events.ReadTaskEvents(ctx, taskID, 0, 100)
		if err != nil {
			t.Fatalf("%s: read the task's events: %v", label, err)
		}
		return out, events
	}
	pushedQuestion := func(text string, meta map[string]string) func(string) *messages.StructuredMessage {
		return func(taskID string) *messages.StructuredMessage {
			md := map[string]string{"a2aTaskId": taskID}
			for k, v := range meta {
				md[k] = v
			}
			return &messages.StructuredMessage{
				Version: 1, Sender: "agent:" + rplAgentSlug, SenderID: e.agent.ID, Recipient: "user:" + alice.user.ID,
				Msg: text, Type: messages.TypeInputNeeded, Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Metadata: md,
			}
		}
	}
	messageEvents := func(events []bridgestate.TaskEvent) (poll, other []string) {
		for _, ev := range events {
			if ev.Kind != "message" {
				continue
			}
			if strings.HasPrefix(ev.DedupKey, "poll:") {
				poll = append(poll, ev.DedupKey)
			} else {
				other = append(other, ev.DedupKey)
			}
		}
		return poll, other
	}

	// (a) Same replica: the pushed copy is dropped as owned by the poller.
	ownedBefore := strings.Count(e.logs.String(), "reason=poller_owns")
	out, events := run("same replica", 0, pushedQuestion("the first question", nil), "the first question")
	if strings.Count(e.logs.String(), "reason=poller_owns") <= ownedBefore {
		t.Fatalf("same replica: no poller_owns drop line for the pushed copy")
	}
	if out.State != a2a.TaskStateInputRequired || out.Text != "the first question" {
		t.Fatalf("same replica: task ended %v with %q, want input-required with the question", out.State, out.Text)
	}
	if poll, other := messageEvents(events); len(poll) != 1 || len(other) != 0 {
		t.Fatalf("same replica: message events poll=%v other=%v, want exactly one, from the poller", poll, other)
	}

	// (b) Another replica: the copy is appended; the response is the polled one.
	out, events = run("other replica", 1, pushedQuestion("a pushed copy of the second question", nil), "the second question")
	if out.State != a2a.TaskStateInputRequired || out.Text != "the second question" {
		t.Fatalf("other replica: task ended %v with %q, want input-required with the polled question", out.State, out.Text)
	}
	if poll, other := messageEvents(events); len(poll) != 1 || len(other) != 1 {
		t.Fatalf("other replica: message events poll=%v other=%v, want one polled and one pushed copy", poll, other)
	}

	// (c) A pushed msgId with the poll: prefix gets the content hash key.
	out, events = run("reserved prefix", 1, pushedQuestion("a pushed message claiming a poll key", map[string]string{"msgId": "poll:x"}), "the third question")
	if out.State != a2a.TaskStateInputRequired || out.Text != "the third question" {
		t.Fatalf("reserved prefix: task ended %v with %q, want input-required with the polled question", out.State, out.Text)
	}
	for _, ev := range events {
		if strings.HasPrefix(ev.DedupKey, "poll:x") {
			t.Fatalf("reserved prefix: an event carries the pushed key %q", ev.DedupKey)
		}
	}
	if poll, other := messageEvents(events); len(poll) != 1 || len(other) != 1 {
		t.Fatalf("reserved prefix: message events poll=%v other=%v, want one polled and one pushed copy", poll, other)
	}
}
