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
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/bridge"
	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/identity"
	bridgestate "github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// The phase 1 project visitor slice end to end, in one process (upstream issue 4603):
//
//   - hub A issues a user access credential for its signed-in user, signed
//     with its own OIDC key and verified by hub B through hub A's published
//     JWKS;
//   - hub B trusts hub A as a hub issuer with accept_user_tokens and
//     allow_visitor_grants, and its project owner grants visitor access by
//     email and by email domain;
//   - the A2A bridge sits in front of hub B, receiving hub B's broker
//     publications as a broker spoke the way a broker plugin does;
//   - an A2A client (plain JSON-RPC) sends tasks with the hub A credential
//     and no sign-in on hub B.
//
// The agent on hub B is the one stand-in: a dispatcher records what hub B
// delivers to it, and the test answers as the agent through hub B's
// outbound-message route with an agent credential hub B signed.

const (
	fvDevToken       = "scion_dev_fed_visitor_e2e_token_0123456789abcdef"
	fvMinterKey      = "fed-visitor-e2e-bridge-signing-key-0123456789"
	fvAgentSlug      = "helper"
	fvUnexposedAgent = "not-exposed"
	fvRuleDomain     = "partner.example"
	// fvWait bounds every wait on a delivery or an A2A answer. It is sized
	// for the A2A integration job's -race and -count=3 runs.
	fvWait = 60 * time.Second
	// fvBridgeSendTimeout is the bridge's own wait for an agent reply. It
	// is below fvWait, so a reply that never comes ends the task inside the
	// test's bound.
	fvBridgeSendTimeout = 45 * time.Second
)

// fvHub is one in-process hub server, its store, and its listener.
type fvHub struct {
	srv   *hub.Server
	store store.Store
	url   string
}

// fvEnv is the whole topology of the test.
type fvEnv struct {
	hubA, hubB  *fvHub
	bridgeURL   string
	projectID   string
	projectSlug string
	agent       *store.Agent
	agentToken  string
	dispatcher  *fvDispatcher
	client      *http.Client
}

// fvDispatcher stands in for hub B's runtime broker: it records each
// message hub B delivers to an agent. Every other dispatch answers an
// error; embedding the interface keeps the type complete if the interface
// grows.
type fvDispatcher struct {
	hub.AgentDispatcher
	delivered chan *messages.StructuredMessage
}

var errFVDispatchUnavailable = errors.New("not available in this test")

func (d *fvDispatcher) DispatchAgentMessage(_ context.Context, _ *store.Agent, message string, _ bool, structuredMsg *messages.StructuredMessage) error {
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

func (d *fvDispatcher) DispatchAgentCreate(context.Context, *store.Agent) (*hub.CreateDispatchResult, error) {
	return nil, errFVDispatchUnavailable
}

func (d *fvDispatcher) DispatchAgentProvision(context.Context, *store.Agent) error {
	return errFVDispatchUnavailable
}

func (d *fvDispatcher) DispatchAgentReprovision(context.Context, *store.Agent) error {
	return errFVDispatchUnavailable
}

func (d *fvDispatcher) DispatchAgentStart(context.Context, *store.Agent, string, bool) error {
	return errFVDispatchUnavailable
}

func (d *fvDispatcher) DispatchAgentStop(context.Context, *store.Agent) error {
	return errFVDispatchUnavailable
}

func (d *fvDispatcher) DispatchAgentRestart(context.Context, *store.Agent) error {
	return errFVDispatchUnavailable
}

func (d *fvDispatcher) DispatchAgentResetAuth(context.Context, *store.Agent) error {
	return errFVDispatchUnavailable
}

func (d *fvDispatcher) DispatchAgentDelete(context.Context, *store.Agent, bool, bool, bool, time.Time) error {
	return errFVDispatchUnavailable
}

func (d *fvDispatcher) DispatchAgentLogs(context.Context, *store.Agent, int) (string, error) {
	return "", errFVDispatchUnavailable
}

func (d *fvDispatcher) DispatchAgentExec(context.Context, *store.Agent, []string, int) (string, int, error) {
	return "", 0, errFVDispatchUnavailable
}

func (d *fvDispatcher) DispatchCheckAgentPrompt(context.Context, *store.Agent) (bool, error) {
	return false, errFVDispatchUnavailable
}

func (d *fvDispatcher) DispatchAgentCreateWithGather(context.Context, *store.Agent) (*hub.CreateDispatchResult, error) {
	return nil, errFVDispatchUnavailable
}

func (d *fvDispatcher) DispatchFinalizeEnv(context.Context, *store.Agent, map[string]string) (*hub.CreateDispatchResult, error) {
	return nil, errFVDispatchUnavailable
}

// fvBrokerSpoke is the bridge's broker as a spoke of hub B's fan-out event
// bus, in place of the plugin RPC adapter (plugin.BrokerPluginAdapter):
// Publish hands the message to the bridge, Subscribe passes the pattern on
// and discards the handler. The spoke has no ChannelID, so the fan-out bus
// hands it every message without a channel; a message with a channel goes
// only to the in-process bus and the spoke whose routing key matches it.
type fvBrokerSpoke struct {
	broker *bridge.BrokerServer
}

func (s *fvBrokerSpoke) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	return s.broker.Publish(ctx, topic, msg)
}

func (s *fvBrokerSpoke) Subscribe(pattern string, _ eventbus.EventHandler) (eventbus.Subscription, error) {
	if err := s.broker.Subscribe(pattern); err != nil {
		return nil, err
	}
	return fvSpokeSubscription{broker: s.broker, pattern: pattern}, nil
}

func (s *fvBrokerSpoke) Close() error { return nil }

type fvSpokeSubscription struct {
	broker  *bridge.BrokerServer
	pattern string
}

func (s fvSpokeSubscription) Unsubscribe() error { return s.broker.Unsubscribe(s.pattern) }

// fvListener returns an unstarted server on a loopback port and its URL.
func fvListener(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	ts := httptest.NewUnstartedServer(http.NotFoundHandler())
	t.Cleanup(ts.Close)
	return ts, "http://" + ts.Listener.Addr().String()
}

// fvExperiments returns the experiment registry with
// federation.project_visitors on.
func fvExperiments(t *testing.T) *experiments.Registry {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		if e.Name == experiments.FederationProjectVisitors {
			e.Default = true
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	if err != nil {
		t.Fatalf("experiment registry: %v", err)
	}
	return reg
}

// fvStartHub builds a hub on a fresh SQLite store with the visitor
// experiment on and dev auth, lets configure adjust the config, and serves
// it on ts.
func fvStartHub(t *testing.T, name string, ts *httptest.Server, url string, configure func(*hub.ServerConfig)) *fvHub {
	t.Helper()
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), name+".db") + "?_journal_mode=WAL&_busy_timeout=5000"
	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("%s: open store: %v", name, err)
	}
	st := entadapter.NewCompositeStore(client)
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("%s: migrate store: %v", name, err)
	}
	// As the pkg/hub test servers do: agent rows written directly to the
	// store have no delegation edges, so the backfill marker Migrate sets
	// is removed.
	_ = st.DeleteHubSetting(ctx, "migration_delegation_edge_backfill_v1")

	cfg := hub.DefaultServerConfig()
	cfg.HubEndpoint = url
	cfg.CORSEnabled = false
	cfg.DisableCloudLogQuery = true
	cfg.DevAuthToken = fvDevToken
	cfg.DevUserConfig = hub.DevUserConfig{Username: "dev", DisplayName: "Development User", Email: "dev@localhost"}
	cfg.Experiments = fvExperiments(t)
	configure(&cfg)

	srv, err := hub.New(cfg, st)
	if err != nil {
		t.Fatalf("%s: hub.New: %v", name, err)
	}
	// DB-backed operational settings, built the way the server's startup
	// builds them (cmd/server_foreground.go initOperationalSettings). hub.New
	// leaves them unset, and a hub without them reads the switches that need
	// settings to be present as off, including the conversation envelope
	// switch that the bridge's visitor reply poller relies on. With no stored sections each
	// switch takes its compiled default. The startup's snapshot apply is
	// left out so the federation configuration set above stays in force.
	ops := hub.NewOperationalSettings(st, config.LoadServerBootstrapKoanf("", nil), config.LoadEnvKoanf())
	if _, err := ops.Refresh(ctx); err != nil {
		t.Fatalf("%s: operational settings refresh: %v", name, err)
	}
	srv.SetOperationalSettings(ops)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	ts.Config.Handler = srv.Handler()
	ts.Start()
	return &fvHub{srv: srv, store: st, url: url}
}

// fvPeerIssuer is hub B's trusted issuer entry for hub A.
func fvPeerIssuer(hubAURL, hubBURL string, acceptUserTokens bool) config.TrustedIssuerConfig {
	return config.TrustedIssuerConfig{
		IssuerURL:          hubAURL,
		IssuerType:         string(hub.IssuerTypeHub),
		JWKSURL:            hubAURL + "/.well-known/jwks.json",
		ExpectedAudience:   hubBURL,
		AcceptUserTokens:   acceptUserTokens,
		AllowVisitorGrants: true,
	}
}

// fvDo sends one request and returns the status and body.
func (e *fvEnv) fvDo(t *testing.T, method, url string, header map[string]string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode %s %s: %v", method, url, err)
		}
		reader = bytes.NewReader(data)
	}
	ctx, cancel := context.WithTimeout(context.Background(), fvWait)
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

func fvBearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// fvDecode decodes body into out, failing the test with the body on error.
func fvDecode(t *testing.T, what string, body []byte, out any) {
	t.Helper()
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("decode %s: %v; body %s", what, err, body)
	}
}

// fvHubAUser stores an active local user of hub A and returns it with a
// session credential (a hub user JWT, an interactive credential).
func (e *fvEnv) fvHubAUser(t *testing.T, email string) (*store.User, string) {
	t.Helper()
	u := &store.User{
		ID: uuid.NewString(), Email: email, DisplayName: email,
		Role: "member", Status: store.UserStatusActive, Kind: store.UserKindHuman,
	}
	if err := e.hubA.store.CreateUser(context.Background(), u); err != nil {
		t.Fatalf("create hub A user %s: %v", email, err)
	}
	session, _, err := e.hubA.srv.GetUserTokenService().MintAccessToken(u.ID, u.Email, u.DisplayName, u.Role, u.SessionGeneration, hub.ClientTypeWeb)
	if err != nil {
		t.Fatalf("session credential for %s: %v", email, err)
	}
	return u, session
}

// fvMint obtains a user access credential for hub B from hub A with the
// user's session credential.
func (e *fvEnv) fvMint(t *testing.T, session string) string {
	t.Helper()
	status, body := e.fvDo(t, http.MethodPost, e.hubA.url+"/api/v1/auth/federation-token", fvBearer(session),
		map[string]string{"audience": e.hubB.url})
	if status != http.StatusOK {
		t.Fatalf("hub A federation-token: status %d, want 200; body %s", status, body)
	}
	var resp struct {
		Token string `json:"token"`
	}
	fvDecode(t, "federation-token response", body, &resp)
	if resp.Token == "" {
		t.Fatalf("hub A federation-token: empty token; body %s", body)
	}
	return resp.Token
}

func (e *fvEnv) fvVisitorsPath() string {
	return e.hubB.url + "/api/v1/projects/" + e.projectID + "/visitors"
}

// fvVisitors lists hub B's project visitors as the owner.
func (e *fvEnv) fvVisitors(t *testing.T) []hub.ProjectVisitor {
	t.Helper()
	status, body := e.fvDo(t, http.MethodGet, e.fvVisitorsPath(), fvBearer(fvDevToken), nil)
	if status != http.StatusOK {
		t.Fatalf("list visitors: status %d, want 200; body %s", status, body)
	}
	var resp hub.ListProjectVisitorsResponse
	fvDecode(t, "visitor list", body, &resp)
	return resp.Visitors
}

// fvVisitor returns hub B's visitor entry for email.
func (e *fvEnv) fvVisitor(t *testing.T, email string) hub.ProjectVisitor {
	t.Helper()
	visitors := e.fvVisitors(t)
	for _, v := range visitors {
		if v.Email == email {
			return v
		}
	}
	t.Fatalf("no visitor %s in %+v", email, visitors)
	return hub.ProjectVisitor{}
}

// fvSendResult is the bridge's answer to one JSON-RPC send.
type fvSendResult struct {
	status int
	body   []byte
	err    error
}

// fvSendAsync sends text to agentSlug through the bridge as an A2A client
// holding the user access credential, and delivers the answer on the
// returned channel.
func (e *fvEnv) fvSendAsync(ctx context.Context, agentSlug, token, text string) <-chan fvSendResult {
	done := make(chan fvSendResult, 1)
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
			done <- fvSendResult{err: err}
			return
		}
		url := e.bridgeURL + "/projects/" + e.projectSlug + "/agents/" + agentSlug + "/jsonrpc"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
		if err != nil {
			done <- fvSendResult{err: err}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := e.client.Do(req)
		if err != nil {
			done <- fvSendResult{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		done <- fvSendResult{status: resp.StatusCode, body: body, err: err}
	}()
	return done
}

// fvOutcome is how a task ended, as the A2A client sees it.
type fvOutcome struct {
	State a2a.TaskState
	Text  string
}

// fvTaskOutcome decodes a JSON-RPC send answer into the task's final state
// and status message text.
func fvTaskOutcome(t *testing.T, label string, res fvSendResult) fvOutcome {
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
	fvDecode(t, label+" JSON-RPC answer", res.body, &rpc)
	if rpc.Error != nil {
		t.Fatalf("%s: JSON-RPC error %d %q; body %s", label, rpc.Error.Code, rpc.Error.Message, res.body)
	}
	// The result is the task wrapped as {"task": {...}}; a bare task is
	// accepted too (the shapes taskIdentity reads).
	var wrapped struct {
		Task json.RawMessage `json:"task"`
	}
	fvDecode(t, label+" send result", rpc.Result, &wrapped)
	raw := []byte(rpc.Result)
	if len(wrapped.Task) > 0 && string(wrapped.Task) != "null" {
		raw = wrapped.Task
	}
	var task a2a.Task
	fvDecode(t, label+" task", raw, &task)
	var text []string
	if msg := task.Status.Message; msg != nil {
		for _, p := range msg.Parts {
			if p != nil {
				text = append(text, p.Text())
			}
		}
	}
	return fvOutcome{State: task.Status.State, Text: strings.Join(text, "\n")}
}

// fvSendAdmitted sends text with the credential and expects hub B to
// deliver it to the agent, from sender (any user address when sender is
// empty); the agent replies with reply to the envelope's sender, and the
// task must complete with that reply. It returns the envelope's sender.
func (e *fvEnv) fvSendAdmitted(t *testing.T, label, token, sender, text, reply string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fvWait)
	defer cancel()
	done := e.fvSendAsync(ctx, fvAgentSlug, token, text)

	var got *messages.StructuredMessage
	select {
	case got = <-e.dispatcher.delivered:
	case res := <-done:
		t.Fatalf("%s: the bridge answered before hub B delivered the message to the agent: HTTP %d, error %v, body %s",
			label, res.status, res.err, res.body)
	case <-ctx.Done():
		t.Fatalf("%s: hub B delivered nothing to the agent within %s", label, fvWait)
	}
	if got.Msg != text {
		t.Errorf("%s: agent received %q, want %q", label, got.Msg, text)
	}
	if sender != "" && got.Sender != sender {
		t.Fatalf("%s: envelope sender %q, want %q", label, got.Sender, sender)
	}
	if !strings.HasPrefix(got.Sender, "user:") {
		t.Fatalf("%s: envelope sender %q, want a user address", label, got.Sender)
	}

	// The agent answers the address its envelope shows as the sender.
	status, body := e.fvDo(t, http.MethodPost, e.hubB.url+"/api/v1/agents/"+e.agent.ID+"/outbound-message",
		map[string]string{"X-Scion-Agent-Token": e.agentToken},
		hub.OutboundMessageRequest{Recipient: got.Sender, Msg: reply, Type: messages.TypeInstruction})
	if status != http.StatusOK {
		t.Fatalf("%s: agent reply: status %d, want 200; body %s", label, status, body)
	}

	var res fvSendResult
	select {
	case res = <-done:
	case <-ctx.Done():
		t.Fatalf("%s: the bridge did not answer within %s", label, fvWait)
	}
	outcome := fvTaskOutcome(t, label, res)
	if outcome.State != a2a.TaskStateCompleted || outcome.Text != reply {
		t.Fatalf("%s: task ended %v with %q, want %v with the agent's reply %q",
			label, outcome.State, outcome.Text, a2a.TaskStateCompleted, reply)
	}
	e.fvNoDelivery(t, label)
	return got.Sender
}

// fvSendNotDelivered sends text to agentSlug with the credential and
// expects an answer with nothing delivered to the agent.
func (e *fvEnv) fvSendNotDelivered(t *testing.T, label, agentSlug, token, text string) fvSendResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fvWait)
	defer cancel()
	done := e.fvSendAsync(ctx, agentSlug, token, text)
	var res fvSendResult
	select {
	case got := <-e.dispatcher.delivered:
		t.Fatalf("%s: hub B delivered %q from %s to the agent, want no delivery", label, got.Msg, got.Sender)
	case res = <-done:
	case <-ctx.Done():
		t.Fatalf("%s: the bridge did not answer within %s", label, fvWait)
	}
	e.fvNoDelivery(t, label)
	if e.fvStored(t, text) {
		t.Fatalf("%s: hub B stored the message %q, want it refused before it is stored", label, text)
	}
	return res
}

// fvStored reports whether hub B's store holds a message with text. Hub B
// writes a message before it answers the send, so the check does not depend
// on delivery timing.
func (e *fvEnv) fvStored(t *testing.T, text string) bool {
	t.Helper()
	page, err := e.hubB.store.ListMessages(context.Background(), store.MessageFilter{},
		store.ListOptions{Limit: 200, SkipTotalCount: true})
	if err != nil {
		t.Fatalf("list hub B messages: %v", err)
	}
	for _, m := range page.Items {
		if m.Msg == text {
			return true
		}
	}
	return false
}

// fvProjectRoles returns the names of the roles the user holds on hub B's
// project through project-scoped role bindings, one entry per binding.
// These are the user's direct project roles; membership also counts
// group-derived bindings, and the visitor role confers none.
func (e *fvEnv) fvProjectRoles(t *testing.T, userID string) []string {
	t.Helper()
	ctx := context.Background()
	bindings, err := e.hubB.store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	if err != nil {
		t.Fatalf("role bindings of %s: %v", userID, err)
	}
	var roles []string
	for _, rb := range bindings {
		if rb.ScopeType != store.RoleScopeProject || rb.ScopeID != e.projectID {
			continue
		}
		role, err := e.hubB.store.GetRoleDefinition(ctx, rb.RoleDefinitionID)
		if err != nil {
			t.Fatalf("role definition %s: %v", rb.RoleDefinitionID, err)
		}
		roles = append(roles, role.Name)
	}
	return roles
}

// fvVisitorDMPath is the history route of the DM between the agent and the
// user account, the read the bridge's visitor reply poller makes.
func (e *fvEnv) fvVisitorDMPath(t *testing.T, userID string) string {
	t.Helper()
	key, err := messages.DMConversationKey("agent", e.agent.ID, "user", userID)
	if err != nil {
		t.Fatalf("DM key: %v", err)
	}
	return e.hubB.url + "/api/v1/chat/conversations/" + url.PathEscape(key) + "/messages?limit=10"
}

// fvVisitorDMHeader is the bridge's request shape for a federation caller:
// the bridge's own bearer credential plus the user access credential in the
// federation header (bridge.federationHeaderTransport).
func fvVisitorDMHeader(token string) map[string]string {
	h := fvBearer(fvDevToken)
	h[hub.FederationTokenHeader] = token
	return h
}

// fvNoDelivery checks that nothing more reached the agent.
func (e *fvEnv) fvNoDelivery(t *testing.T, label string) {
	t.Helper()
	select {
	case got := <-e.dispatcher.delivered:
		t.Fatalf("%s: unexpected delivery to the agent: %q from %s", label, got.Msg, got.Sender)
	default:
	}
}

// fvSetAcceptUserTokens reloads hub B's federation configuration with
// accept_user_tokens set to accept on hub A's issuer, the way a settings
// reload does. ApplySnapshot lists "federation" in applied only after the
// authenticator was rebuilt from the new configuration, so its presence is
// the check that the reload took effect.
func (e *fvEnv) fvSetAcceptUserTokens(t *testing.T, accept bool) {
	t.Helper()
	result := hub.ApplySnapshot(e.hubB.srv, hub.Layer1Snapshot{FederationConfig: &config.FederationConfig{
		Enabled:        true,
		TrustedIssuers: []config.TrustedIssuerConfig{fvPeerIssuer(e.hubA.url, e.hubB.url, accept)},
	}})
	applied, _ := result["applied"].([]string)
	for _, a := range applied {
		if a == "federation" {
			return
		}
	}
	t.Fatalf("federation configuration not applied (accept_user_tokens=%t): %v", accept, result)
}

func fvNewEnv(t *testing.T) *fvEnv {
	t.Helper()
	// Hub B creates its project's directory under the global directory
	// ($HOME/.scion); a per-test HOME keeps every run off the real one.
	t.Setenv("HOME", t.TempDir())
	// The operational settings read SCION_SERVER_* and SCION_SEED_*
	// variables; an empty value counts as unset, so these keep an inherited
	// variable from changing what either hub reads.
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "SCION_SERVER_") || strings.HasPrefix(name, "SCION_SEED_") {
			t.Setenv(name, "")
		}
	}
	ctx := context.Background()
	e := &fvEnv{
		dispatcher: &fvDispatcher{delivered: make(chan *messages.StructuredMessage, 16)},
		client:     &http.Client{Timeout: fvWait},
	}
	tsA, urlA := fvListener(t)
	tsB, urlB := fvListener(t)
	tsBridge, urlBridge := fvListener(t)
	e.bridgeURL = urlBridge

	// Hub A: OIDC provider on, user access credentials for hub B only.
	e.hubA = fvStartHub(t, "hub-a", tsA, urlA, func(cfg *hub.ServerConfig) {
		cfg.OIDCConfig = config.OIDCProviderConfig{
			Enabled:            true,
			IssuerURL:          urlA,
			UserTokenAudiences: []string{urlB},
		}
	})
	if status, body := e.fvDo(t, http.MethodGet, urlA+"/.well-known/jwks.json", nil, nil); status != http.StatusOK {
		t.Fatalf("hub A JWKS: status %d, want 200; body %s", status, body)
	}

	// Hub B: trusts hub A as a hub issuer that may present user access
	// credentials, with visitor grants allowed. Dev mode, because the
	// federation authenticator accepts an http issuer URL (hub A's
	// loopback listener) only in dev and workstation modes.
	e.hubB = fvStartHub(t, "hub-b", tsB, urlB, func(cfg *hub.ServerConfig) {
		cfg.Mode = "dev"
		cfg.Federation = config.FederationConfig{
			Enabled:        true,
			TrustedIssuers: []config.TrustedIssuerConfig{fvPeerIssuer(urlA, urlB, true)},
		}
	})

	// Hub B's project (created by the owner who also grants), a runtime
	// broker, and a running agent on it.
	status, body := e.fvDo(t, http.MethodPost, urlB+"/api/v1/projects", fvBearer(fvDevToken),
		map[string]string{"name": "visitor-e2e"})
	if status != http.StatusCreated {
		t.Fatalf("create project on hub B: status %d, want 201; body %s", status, body)
	}
	var project struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	}
	fvDecode(t, "project", body, &project)
	if project.ID == "" || project.Slug == "" {
		t.Fatalf("create project on hub B: no id or slug; body %s", body)
	}
	e.projectID, e.projectSlug = project.ID, project.Slug

	brokerID := uuid.NewString()
	if err := e.hubB.store.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "visitor-e2e-broker", Slug: "visitor-e2e-broker", Status: store.BrokerStatusOnline,
	}); err != nil {
		t.Fatalf("create runtime broker: %v", err)
	}
	e.agent = &store.Agent{
		ID: uuid.NewString(), Name: fvAgentSlug, Slug: fvAgentSlug, ProjectID: e.projectID,
		RuntimeBrokerID: brokerID, Phase: "running", MessageMode: store.MessageModeProject,
		OwnerID: hub.DevUserID,
	}
	if err := e.hubB.store.CreateAgent(ctx, e.agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	token, cred, err := e.hubB.srv.GetAgentTokenService().SignAgentToken(hub.AgentTokenGrant{
		AgentID: e.agent.ID, ProjectID: e.projectID, Scopes: hub.ScopesForRole(hub.AgentRoleFull),
	}, "")
	if err != nil {
		t.Fatalf("sign agent credential: %v", err)
	}
	if err := e.hubB.store.CreateAgentCredential(ctx, cred); err != nil {
		t.Fatalf("record agent credential: %v", err)
	}
	e.agentToken = token

	// The A2A bridge in front of hub B.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	stateStore, err := bridgestate.NewSQLite(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("bridge state store: %v", err)
	}
	t.Cleanup(func() { _ = stateStore.Close() })
	adminClient, err := hubclient.New(urlB, hubclient.WithBearerToken(fvDevToken))
	if err != nil {
		t.Fatalf("bridge hub client: %v", err)
	}
	minter, err := identity.NewTokenMinter([]byte(fvMinterKey))
	if err != nil {
		t.Fatalf("bridge token minter: %v", err)
	}
	bridgeCfg := &bridge.Config{
		Bridge:   bridge.BridgeConfig{ExternalURL: urlBridge},
		Hub:      bridge.HubConfig{Endpoint: urlB, User: "a2a-bridge@example.invalid"},
		Auth:     bridge.AuthConfig{Scheme: "federation"},
		Projects: []bridge.ProjectConfig{{Slug: e.projectSlug, ExposedAgents: []string{fvAgentSlug}}},
		Timeouts: bridge.TimeoutConfig{SendMessage: fvBridgeSendTimeout},
	}
	b := bridge.New(stateStore, adminClient, minter, bridgeCfg, nil, logger)
	t.Cleanup(b.Shutdown)
	broker := bridge.NewBrokerServer(b.HandleBrokerMessage, logger, context.Background())
	b.SetBroker(broker)
	executor := bridge.NewScionExecutor(b, logger)
	innerStore := taskstore.NewInMemory(&taskstore.InMemoryStoreConfig{Authenticator: bridge.RouteKeyAuthenticator()})
	sdkHandler := a2asrv.NewHandler(executor,
		a2asrv.WithLogger(logger),
		a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}),
		a2asrv.WithAgentInactivityTimeout(fvWait),
		a2asrv.WithTaskStore(bridge.NewScopedTaskStore(innerStore)),
	)
	b.SetSDKRequestHandler(sdkHandler)
	server := bridge.NewServer(b, bridgeCfg, nil, logger, a2asrv.NewJSONRPCHandler(sdkHandler))
	tsBridge.Config.Handler = server.Handler()
	tsBridge.Start()

	// Hub B's message broker: the in-process bus plus the bridge as a
	// broker spoke, with the broker proxy as the bridge's host callbacks
	// (cmd/server_foreground.go wires a broker plugin the same way).
	publisher := hub.NewChannelEventPublisher()
	t.Cleanup(publisher.Close)
	e.hubB.srv.SetEventPublisher(publisher)
	e.hubB.srv.SetDispatcher(e.dispatcher)
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: eventbus.NewInProcessEventBus(logger)},
		{Name: "a2a-bridge", Bus: &fvBrokerSpoke{broker: broker}},
	}, logger)
	e.hubB.srv.StartMessageBroker(fanout)
	proxy := e.hubB.srv.GetMessageBrokerProxy()
	if proxy == nil {
		t.Fatal("hub B message broker not started")
	}
	broker.SetHostCallbacks(proxy)
	if err := proxy.EnsureProjectSubscriptions(ctx, e.projectID); err != nil {
		t.Fatalf("hub B project subscriptions: %v", err)
	}
	return e
}

func TestFederatedVisitorEndToEnd(t *testing.T) {
	e := fvNewEnv(t)
	ctx := context.Background()

	// Step 1: Alice, signed in to hub A, obtains a user access credential
	// for hub B from hub A.
	_, aliceSession := e.fvHubAUser(t, "alice@corp.example")
	aliceToken := e.fvMint(t, aliceSession)

	// Step 2: hub B's owner grants alice@corp.example from hub A visitor
	// access. Before her first request the grant is pending.
	status, body := e.fvDo(t, http.MethodPost, e.fvVisitorsPath(), fvBearer(fvDevToken),
		map[string]string{"issuer": e.hubA.url, "email": "alice@corp.example"})
	if status != http.StatusCreated {
		t.Fatalf("grant alice: status %d, want 201; body %s", status, body)
	}
	var aliceGrant hub.ProjectVisitor
	fvDecode(t, "alice's grant", body, &aliceGrant)
	if got := e.fvVisitor(t, "alice@corp.example"); got.Status != store.UserStatusPending {
		t.Fatalf("alice's grant before her first request: status %q, want %q", got.Status, store.UserStatusPending)
	}
	aliceSender := "user:" + aliceGrant.ID

	// Step 3: an A2A client holding Alice's hub A credential sends a task
	// through the bridge, with no sign-in on hub B; the agent's reply
	// completes the task.
	e.fvSendAdmitted(t, "first task", aliceToken, aliceSender, "hello from hub A", "reply to the first task")
	aliceLinked := e.fvVisitor(t, "alice@corp.example")
	if aliceLinked.Status != store.UserStatusActive || aliceLinked.ID != aliceGrant.ID {
		t.Fatalf("alice's grant after her first request: %+v, want status %q on account %s", aliceLinked, store.UserStatusActive, aliceGrant.ID)
	}
	linked, _, err := e.hubB.store.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: "visitor_linked", TargetID: aliceGrant.ID})
	if err != nil {
		t.Fatalf("list link audit records: %v", err)
	}
	if len(linked) != 1 {
		t.Fatalf("link audit records for alice's account: %d, want 1", len(linked))
	}
	if !e.fvStored(t, "hello from hub A") {
		t.Fatal("first task: hub B has no stored message with the task text")
	}

	// The reply is in Alice's own DM with the agent, which hub B serves to
	// the bridge's request shape (the federation header carrying Alice's
	// credential) and refuses to the bridge's bearer credential alone.
	dmPath := e.fvVisitorDMPath(t, aliceGrant.ID)
	status, body = e.fvDo(t, http.MethodGet, dmPath, fvVisitorDMHeader(aliceToken), nil)
	if status != http.StatusOK {
		t.Fatalf("alice's DM read: status %d, want 200; body %s", status, body)
	}
	var dm struct {
		Messages []store.Message `json:"messages"`
	}
	fvDecode(t, "alice's DM", body, &dm)
	replyFound := false
	for _, m := range dm.Messages {
		if m.SenderID == e.agent.ID && m.Msg == "reply to the first task" {
			replyFound = true
		}
	}
	if !replyFound {
		t.Fatalf("alice's DM read: no reply from the agent in %s", body)
	}
	if status, body = e.fvDo(t, http.MethodGet, dmPath, fvBearer(fvDevToken), nil); status != http.StatusNotFound {
		t.Fatalf("alice's DM read without the federation header: status %d, want 404; body %s", status, body)
	}

	// Alice reaches the agent only through her visitor grant: the agent
	// row does not name her account, her only direct role on the project
	// is the visitor role, and the hub's membership rule, which also counts
	// group-derived bindings, finds her a non-member. The agent row names
	// the project's creator, and the creator holds the owner role and is a
	// member, which shows each lookup finds what it looks for.
	agentRow, err := e.hubB.store.GetAgent(ctx, e.agent.ID)
	if err != nil {
		t.Fatalf("read agent: %v", err)
	}
	if agentRow.OwnerID != hub.DevUserID {
		t.Fatalf("agent row owner %q, want the project's creator %q", agentRow.OwnerID, hub.DevUserID)
	}
	if agentRow.OwnerID == aliceGrant.ID || agentRow.CreatedBy == aliceGrant.ID {
		t.Fatalf("agent row names alice's account: owner %q, created by %q", agentRow.OwnerID, agentRow.CreatedBy)
	}
	for _, a := range agentRow.Ancestry {
		if a == aliceGrant.ID {
			t.Fatalf("agent ancestry %v names alice's account %s", agentRow.Ancestry, aliceGrant.ID)
		}
	}
	creatorRoles := e.fvProjectRoles(t, hub.DevUserID)
	hasOwner := false
	for _, name := range creatorRoles {
		hasOwner = hasOwner || name == store.ProjectRoleOwner
	}
	if !hasOwner {
		t.Fatalf("project creator %s holds project roles %v, want %s among them", hub.DevUserID, creatorRoles, store.ProjectRoleOwner)
	}
	aliceRoles := e.fvProjectRoles(t, aliceGrant.ID)
	if len(aliceRoles) == 0 {
		t.Fatalf("alice's account %s holds no role on the project, want %s", aliceGrant.ID, store.ProjectRoleVisitor)
	}
	for _, name := range aliceRoles {
		if name != store.ProjectRoleVisitor {
			t.Fatalf("alice's account %s holds project roles %v, want only %s", aliceGrant.ID, aliceRoles, store.ProjectRoleVisitor)
		}
	}
	if m := e.hubB.srv.CheckEffectiveMembership(ctx, hub.DevUserID, e.projectID); m.Err != nil || !m.IsMember || m.Role != store.ProjectRoleOwner {
		t.Fatalf("project creator %s: membership %+v, want a member with role %s", hub.DevUserID, m, store.ProjectRoleOwner)
	}
	if m := e.hubB.srv.CheckEffectiveMembership(ctx, aliceGrant.ID, e.projectID); m.Err != nil || m.IsMember {
		t.Fatalf("alice's account %s: membership %+v, want a non-member", aliceGrant.ID, m)
	}

	// The bridge lists the agent Alice may message.
	status, body = e.fvDo(t, http.MethodGet, e.bridgeURL+"/projects/"+e.projectSlug+"/agents", fvBearer(aliceToken), nil)
	if status != http.StatusOK {
		t.Fatalf("agent cards: status %d, want 200; body %s", status, body)
	}
	var cards struct {
		Agents []map[string]any `json:"agents"`
	}
	fvDecode(t, "agent cards", body, &cards)
	if len(cards.Agents) != 1 || cards.Agents[0]["name"] != fvAgentSlug {
		t.Fatalf("agent cards: %v, want the one card of %s", cards.Agents, fvAgentSlug)
	}

	// The refusal every refused task must match: a task for an agent the
	// bridge does not expose.
	baseline := fvTaskOutcome(t, "unexposed agent",
		e.fvSendNotDelivered(t, "unexposed agent", fvUnexposedAgent, aliceToken, "hello, unknown agent"))
	if baseline.State != a2a.TaskStateFailed || baseline.Text == "" {
		t.Fatalf("unexposed agent: task ended %v with %q, want a failed task with a message", baseline.State, baseline.Text)
	}

	// Step 4: hub B's issuer gate is live. With accept_user_tokens off on
	// hub A's issuer, hub B no longer accepts the credential: its
	// GET /api/v1/auth/me answers 401, and the bridge's auth middleware
	// answers the A2A request with HTTP 401 before any task exists. Turned
	// back on, the next task is admitted with no grant change.
	e.fvSetAcceptUserTokens(t, false)
	res := e.fvSendNotDelivered(t, "accept_user_tokens off", fvAgentSlug, aliceToken, "hello while off")
	if res.err != nil || res.status != http.StatusUnauthorized ||
		strings.TrimSpace(string(res.body)) != "unauthorized: credential rejected by Scion Hub" {
		t.Fatalf("accept_user_tokens off: HTTP %d, error %v, body %q; want HTTP 401 %q from the bridge",
			res.status, res.err, res.body, "unauthorized: credential rejected by Scion Hub")
	}
	e.fvSetAcceptUserTokens(t, true)
	e.fvSendAdmitted(t, "accept_user_tokens back on", aliceToken, aliceSender, "hello again", "reply after the issuer gate")
	if got := e.fvVisitor(t, "alice@corp.example"); got.ID != aliceLinked.ID || got.BindingID != aliceLinked.BindingID ||
		got.Status != aliceLinked.Status || got.Source != aliceLinked.Source || !got.IssuerAccepted {
		t.Fatalf("alice's grant changed across the issuer gate: %+v, want %+v", got, aliceLinked)
	}

	// Step 5: revoking the grant on hub B refuses Alice's next task with
	// the uniform refusal, and nothing reaches the agent.
	status, body = e.fvDo(t, http.MethodDelete, e.fvVisitorsPath()+"/"+aliceGrant.ID, fvBearer(fvDevToken), nil)
	if status != http.StatusNoContent {
		t.Fatalf("revoke alice: status %d, want 204; body %s", status, body)
	}
	revoked := fvTaskOutcome(t, "after revoke",
		e.fvSendNotDelivered(t, "after revoke", fvAgentSlug, aliceToken, "hello after revoke"))
	if revoked != baseline {
		t.Fatalf("after revoke: task ended %+v, want the same answer as for an unexposed agent %+v", revoked, baseline)
	}
	if status, body = e.fvDo(t, http.MethodGet, dmPath, fvVisitorDMHeader(aliceToken), nil); status != http.StatusForbidden {
		t.Fatalf("alice's DM read after revoke: status %d, want 403; body %s", status, body)
	}

	// Step 6: a domain grant. Bob (bob@partner.example on hub A) is
	// admitted on his first request through the visitor rule; removing the
	// rule refuses his next task with the uniform refusal.
	status, body = e.fvDo(t, http.MethodPost, e.fvVisitorsPath()+"/rules", fvBearer(fvDevToken),
		map[string]string{"issuer": e.hubA.url, "domain": fvRuleDomain})
	if status != http.StatusCreated {
		t.Fatalf("add visitor rule: status %d, want 201; body %s", status, body)
	}
	var rule hub.ProjectVisitorRuleView
	fvDecode(t, "visitor rule", body, &rule)
	_, bobSession := e.fvHubAUser(t, "bob@"+fvRuleDomain)
	bobToken := e.fvMint(t, bobSession)

	// Bob's account does not exist before his first request, so his sender
	// address is learnt from the delivery; it must name the account the
	// rule bound, as the visitor list shows it.
	bobSender := e.fvSendAdmitted(t, "domain grant", bobToken, "", "hello from a partner", "reply to the partner")
	bob := e.fvVisitor(t, "bob@"+fvRuleDomain)
	if bob.Status != store.UserStatusActive || bob.Source != "rule" || bob.SourceRuleID != rule.ID {
		t.Fatalf("bob's visitor entry: %+v, want an active visitor through rule %s", bob, rule.ID)
	}
	if bobSender != "user:"+bob.ID {
		t.Fatalf("domain grant: envelope sender %q, want %q", bobSender, "user:"+bob.ID)
	}

	status, body = e.fvDo(t, http.MethodDelete, e.fvVisitorsPath()+"/rules/"+rule.ID, fvBearer(fvDevToken), nil)
	if status != http.StatusNoContent {
		t.Fatalf("remove visitor rule: status %d, want 204; body %s", status, body)
	}
	ruleRemoved := fvTaskOutcome(t, "after rule removal",
		e.fvSendNotDelivered(t, "after rule removal", fvAgentSlug, bobToken, "hello after rule removal"))
	if ruleRemoved != baseline {
		t.Fatalf("after rule removal: task ended %+v, want the same answer as for an unexposed agent %+v", ruleRemoved, baseline)
	}
}
