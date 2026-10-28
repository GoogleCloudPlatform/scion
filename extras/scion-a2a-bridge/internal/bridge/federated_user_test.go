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

package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/identity"
	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
)

// Fixed identifiers of the federated user tests. IDs that end up in a DM key
// must be canonical UUIDs.
const (
	fedUserIssuer    = "https://hub-a.example.com"
	fedUserSubject   = "alice-on-hub-a"
	fedUserEmail     = "alice@corp.example.com"
	fedUserAccountID = "11111111-2222-4333-8444-555555555555"
	fedUserProjectID = "99999999-8888-4777-8666-555555555555"
	fedUserAgentID   = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	fedUserReplyText = "reply from agent1 to the federated user"
)

// fedUserToken is a user access credential from a peer hub, as the bridge
// receives it (unsigned here: the bridge never verifies it, the hub does).
func fedUserToken(t *testing.T) string {
	t.Helper()
	return buildTestJWT(t, map[string]interface{}{
		"iss":              fedUserIssuer,
		"sub":              fedUserSubject,
		"aud":              "https://hub-b.example.com",
		"email":            fedUserEmail,
		"email_verified":   true,
		"scion_token_kind": "user",
	})
}

// fedAgentToken is an agent identity credential from a peer hub.
func fedAgentToken(t *testing.T) string {
	t.Helper()
	return buildTestJWT(t, map[string]interface{}{
		"iss":              fedUserIssuer,
		"sub":              "fed-agent-1",
		"project_id":       "remote-project",
		"agent_name":       "remote-agent",
		"ancestry":         []string{"root-user"},
		"root_user":        "root-user",
		"scion_token_kind": "agent",
	})
}

// --- classification ---

func TestDecodeFederationToken_Classification(t *testing.T) {
	tests := []struct {
		name      string
		claims    map[string]interface{}
		wantErr   bool
		wantUser  bool
		wantAgent string
	}{
		{
			name: "user kind",
			claims: map[string]interface{}{
				"iss": fedUserIssuer, "sub": fedUserSubject, "email": fedUserEmail,
				"scion_token_kind": "user",
			},
			wantUser: true,
		},
		{
			name: "agent kind",
			claims: map[string]interface{}{
				"iss": fedUserIssuer, "sub": "agent-1", "project_id": "p", "ancestry": []string{"u"},
				"scion_token_kind": "agent",
			},
			wantAgent: "agent-1",
		},
		{
			name: "agent kind with an email stays an agent",
			claims: map[string]interface{}{
				"iss": fedUserIssuer, "sub": "agent-2", "email": "x@example.com",
				"scion_token_kind": "agent",
			},
			wantAgent: "agent-2",
		},
		{
			name: "no kind with agent claims is an agent",
			claims: map[string]interface{}{
				"iss": fedUserIssuer, "sub": "agent-3", "project_id": "p", "root_user": "u",
			},
			wantAgent: "agent-3",
		},
		{
			name: "no kind with agent claims and an email is an agent",
			claims: map[string]interface{}{
				"iss": fedUserIssuer, "sub": "agent-4", "agent_name": "a", "email": "x@example.com",
			},
			wantAgent: "agent-4",
		},
		{
			name: "no kind with only an email is a user",
			claims: map[string]interface{}{
				"iss": fedUserIssuer, "sub": fedUserSubject, "email": fedUserEmail,
			},
			wantUser: true,
		},
		{
			name: "no kind and neither email nor agent claims is an agent",
			claims: map[string]interface{}{
				"iss": fedUserIssuer, "sub": "agent-5",
			},
			wantAgent: "agent-5",
		},
		{
			name: "user kind with an agent claim is refused",
			claims: map[string]interface{}{
				"iss": fedUserIssuer, "sub": fedUserSubject, "email": fedUserEmail, "project_id": "p",
				"scion_token_kind": "user",
			},
			wantErr: true,
		},
		{
			name: "user kind with ancestry is refused",
			claims: map[string]interface{}{
				"iss": fedUserIssuer, "sub": fedUserSubject, "email": fedUserEmail, "ancestry": []string{"u"},
				"scion_token_kind": "user",
			},
			wantErr: true,
		},
		{
			name: "user kind without an email is refused",
			claims: map[string]interface{}{
				"iss": fedUserIssuer, "sub": fedUserSubject,
				"scion_token_kind": "user",
			},
			wantErr: true,
		},
		{
			name: "unknown kind is refused",
			claims: map[string]interface{}{
				"iss": fedUserIssuer, "sub": fedUserSubject, "email": fedUserEmail,
				"scion_token_kind": "service",
			},
			wantErr: true,
		},
		{
			name: "kind that is not a string is refused",
			claims: map[string]interface{}{
				"iss": fedUserIssuer, "sub": fedUserSubject, "email": fedUserEmail,
				"scion_token_kind": []string{"user", "agent"},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			caller, err := decodeFederationToken(buildTestJWT(t, tt.claims))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("decodeFederationToken = %+v, want an error", caller)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeFederationToken: %v", err)
			}
			if caller.TokenType != "federation" {
				t.Errorf("TokenType = %q, want federation", caller.TokenType)
			}
			if got := caller.IsFederatedUser(); got != tt.wantUser {
				t.Errorf("IsFederatedUser() = %v, want %v", got, tt.wantUser)
			}
			if tt.wantUser {
				if caller.IsAgent() {
					t.Errorf("IsAgent() = true for a user credential")
				}
				if caller.Subject != fedUserSubject || caller.Email != fedUserEmail || caller.IssuerURL != fedUserIssuer {
					t.Errorf("user caller = %+v, want subject %q, email %q, issuer %q", caller, fedUserSubject, fedUserEmail, fedUserIssuer)
				}
				if caller.UserID != "" {
					t.Errorf("UserID = %q before the hub named the account, want empty", caller.UserID)
				}
				return
			}
			if caller.AgentID != tt.wantAgent || !caller.IsAgent() {
				t.Errorf("AgentID = %q (IsAgent %v), want %q", caller.AgentID, caller.IsAgent(), tt.wantAgent)
			}
			if caller.Subject != "" || caller.Email != "" {
				t.Errorf("agent caller carries user fields: %+v", caller)
			}
		})
	}
}

// TestDecodeFederationToken_AgentUnchanged pins the agent caller built from
// an agent credential: exactly the fields set before credential kinds.
func TestDecodeFederationToken_AgentUnchanged(t *testing.T) {
	token := fedAgentToken(t)
	caller, err := decodeFederationToken(token)
	if err != nil {
		t.Fatalf("decodeFederationToken: %v", err)
	}
	want := CallerIdentity{
		TokenType: "federation",
		RawToken:  token,
		AgentID:   "fed-agent-1",
		IssuerURL: fedUserIssuer,
		ProjectID: "remote-project",
		Ancestry:  []string{"root-user"},
	}
	if caller.TokenType != want.TokenType || caller.RawToken != want.RawToken || caller.AgentID != want.AgentID ||
		caller.IssuerURL != want.IssuerURL || caller.ProjectID != want.ProjectID ||
		len(caller.Ancestry) != 1 || caller.Ancestry[0] != "root-user" ||
		caller.UserID != "" || caller.Email != "" || caller.Subject != "" || caller.Role != "" {
		t.Fatalf("agent caller = %+v, want %+v", caller, want)
	}
	if caller.SenderLabel() != "agent:fed-agent-1" {
		t.Errorf("SenderLabel() = %q, want agent:fed-agent-1", caller.SenderLabel())
	}
	if caller.CallerKey() != "agent:hub-a.example.com:fed-agent-1" {
		t.Errorf("CallerKey() = %q, want agent:hub-a.example.com:fed-agent-1", caller.CallerKey())
	}
}

// --- caller identity ---

func TestCallerIdentity_FederatedUser(t *testing.T) {
	caller := &CallerIdentity{
		TokenType: "federation",
		UserID:    fedUserAccountID,
		Email:     fedUserEmail,
		IssuerURL: fedUserIssuer,
		Subject:   fedUserSubject,
	}
	if !caller.IsFederatedUser() || caller.IsAgent() {
		t.Fatalf("IsFederatedUser = %v, IsAgent = %v; want true, false", caller.IsFederatedUser(), caller.IsAgent())
	}
	if got, want := caller.SenderLabel(), "user:"+fedUserAccountID; got != want {
		t.Errorf("SenderLabel() = %q, want %q (the account ID, never the email)", got, want)
	}
	if got, want := caller.CallerKey(), "fed:"+fedUserAccountID; got != want {
		t.Errorf("CallerKey() = %q, want %q (the hub account ID)", got, want)
	}

	unnamed := *caller
	unnamed.UserID = ""
	if got := unnamed.CallerKey(); got != "" {
		t.Errorf("CallerKey() before the hub named the account = %q, want empty (refused by the task store)", got)
	}

	var nilCaller *CallerIdentity
	if nilCaller.IsFederatedUser() {
		t.Error("nil caller is a federated user")
	}
	if (&CallerIdentity{UserID: "u", Email: "e", TokenType: "uat"}).IsFederatedUser() {
		t.Error("a UAT caller is a federated user")
	}
	if (&CallerIdentity{AgentID: "a", Subject: "s", TokenType: "federation"}).IsFederatedUser() {
		t.Error("an agent caller is a federated user")
	}
}

// TestCallerKey_FederatedUserIssuersOnOneHost: the task-isolation key is
// the hub account, so users of two issuers on the same host with the same
// subject (realms on one host, or a port in the issuer or the subject) get
// different keys when the hub resolved different accounts for them.
func TestCallerKey_FederatedUserIssuersOnOneHost(t *testing.T) {
	const otherAccountID = "22222222-3333-4444-8555-666666666666"
	pairs := []struct {
		name string
		a, b CallerIdentity
	}{
		{
			name: "two realms on one host, same subject",
			a:    CallerIdentity{TokenType: "federation", IssuerURL: "https://idp.example.com/realms/a", Subject: "same-sub", UserID: fedUserAccountID},
			b:    CallerIdentity{TokenType: "federation", IssuerURL: "https://idp.example.com/realms/b", Subject: "same-sub", UserID: otherAccountID},
		},
		{
			name: "port in the issuer or in the subject",
			a:    CallerIdentity{TokenType: "federation", IssuerURL: "https://idp.example.com", Subject: "8443:same-sub", UserID: fedUserAccountID},
			b:    CallerIdentity{TokenType: "federation", IssuerURL: "https://idp.example.com:8443", Subject: "same-sub", UserID: otherAccountID},
		},
	}
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			keyA, keyB := p.a.CallerKey(), p.b.CallerKey()
			if keyA == keyB {
				t.Fatalf("CallerKey() = %q for both users, want different keys", keyA)
			}
			if keyA != "fed:"+fedUserAccountID || keyB != "fed:"+otherAccountID {
				t.Errorf("CallerKey() = %q and %q, want fed:<account ID> for each", keyA, keyB)
			}
		})
	}
}

// --- hub test double ---

// fedUserHub stands in for the hub the bridge talks to, for both the
// bridge admin client and the federated user's own credential.
type fedUserHub struct {
	*httptest.Server
	userToken string
	// otherUsers maps further user access credentials the hub accepts to
	// their account IDs (other visitor accounts).
	otherUsers map[string]string

	mu           sync.Mutex
	meStatus     int    // 0: 200 with the visitor account
	meID         string // account ID in the /auth/me answer
	meRole       string // role in the /auth/me answer
	sendStatus   int    // 0: 200
	adminStatus  int    // 0: 200 for the bridge admin agent list
	dmReadStatus int    // 0: 200
	grants       []map[string]interface{}
	adminAgents  []map[string]interface{}
	projAgents   []map[string]interface{}
	meCalls      int
	meFedTokens  []string
	sent         []fedUserHubSend
	listQueries  []url.Values
	listTokens   []string
	dmReadKeys   []string
	dmReadTokens []string
	createCalls  int
	// agentGet is the agent GET /api/v1/agents/{id} answers with (404 when
	// nil); agentGets records the IDs read.
	agentGet  map[string]interface{}
	agentGets []string
	// order records, in arrival order, the caller's DM reads ("dm-read"),
	// the admin agent reads ("agent-get") and the sends ("send").
	order []string
}

type fedUserHubSend struct {
	AgentID  string
	Body     map[string]interface{}
	FedToken string
}

func writeHubError(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{"code": "forbidden", "message": federatedUserRefusalText},
	})
}

func newFedUserHub(t *testing.T, userToken string) *fedUserHub {
	t.Helper()
	h := &fedUserHub{
		userToken: userToken,
		meID:      fedUserAccountID,
		meRole:    "viewer",
		grants: []map[string]interface{}{
			{"projectId": fedUserProjectID, "projectSlug": "proj1", "projectName": "Project 1", "status": "active"},
		},
		adminAgents: []map[string]interface{}{
			{"id": fedUserAgentID, "name": "agent1", "slug": "agent1", "projectId": fedUserProjectID, "status": "running"},
		},
		projAgents: []map[string]interface{}{
			{"name": "agent1", "slug": "agent1", "status": "idle"},
			{"name": "agent3", "slug": "agent3", "status": "idle"},
		},
	}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fedToken := r.Header.Get(FederationTokenHeader)
		h.mu.Lock()
		defer h.mu.Unlock()
		writeJSON := func(v interface{}) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(v)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/me":
			h.meCalls++
			h.meFedTokens = append(h.meFedTokens, fedToken)
			if !h.knownUser(fedToken) {
				writeHubError(w, http.StatusUnauthorized)
				return
			}
			if h.meStatus != 0 {
				writeHubError(w, h.meStatus)
				return
			}
			meID := h.meID
			if fedToken != h.userToken {
				meID = h.otherUsers[fedToken]
			}
			writeJSON(map[string]interface{}{
				"id": meID, "email": "visitor+synthetic@hub-b.invalid", "display_name": "Alice", "role": h.meRole,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/users/me/visitor-grants":
			if !h.knownUser(fedToken) {
				writeHubError(w, http.StatusForbidden)
				return
			}
			writeJSON(map[string]interface{}{"grants": h.grants})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+fedUserProjectID+"/agents":
			h.listQueries = append(h.listQueries, r.URL.Query())
			h.listTokens = append(h.listTokens, fedToken)
			if !h.knownUser(fedToken) {
				writeHubError(w, http.StatusForbidden)
				return
			}
			writeJSON(map[string]interface{}{"agents": h.projAgents})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agents":
			// Agent create (auto-provisioning): recorded and refused.
			h.createCalls++
			writeHubError(w, http.StatusForbidden)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agents":
			// Bridge admin agent list (resolveContext, lookupAgent).
			if h.adminStatus != 0 {
				writeHubError(w, h.adminStatus)
				return
			}
			writeJSON(map[string]interface{}{"agents": h.adminAgents, "totalCount": len(h.adminAgents)})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/agents/") && strings.HasSuffix(r.URL.Path, "/message"):
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			agentID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/agents/"), "/message")
			h.sent = append(h.sent, fedUserHubSend{AgentID: agentID, Body: body, FedToken: fedToken})
			h.order = append(h.order, "send")
			if h.sendStatus != 0 {
				writeHubError(w, h.sendStatus)
				return
			}
			writeJSON(map[string]interface{}{"message_id": "msg-1", "status": "delivered"})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/agents/") && !strings.Contains(strings.TrimPrefix(r.URL.Path, "/api/v1/agents/"), "/"):
			// Bridge admin agent read (the launch wait).
			h.agentGets = append(h.agentGets, strings.TrimPrefix(r.URL.Path, "/api/v1/agents/"))
			h.order = append(h.order, "agent-get")
			if h.agentGet == nil {
				writeHubError(w, http.StatusNotFound)
				return
			}
			writeJSON(h.agentGet)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/chat/conversations/") && strings.HasSuffix(r.URL.Path, "/messages"):
			key := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/chat/conversations/"), "/messages")
			h.dmReadKeys = append(h.dmReadKeys, key)
			h.dmReadTokens = append(h.dmReadTokens, fedToken)
			h.order = append(h.order, "dm-read")
			if !h.knownUser(fedToken) {
				writeHubError(w, http.StatusForbidden)
				return
			}
			if h.dmReadStatus != 0 {
				writeHubError(w, h.dmReadStatus)
				return
			}
			writeJSON(map[string]interface{}{"messages": []interface{}{}})
		default:
			http.Error(w, "not found: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(h.Close)
	return h
}

// knownUser reports whether the hub accepts fedToken as a visitor's user
// access credential. The caller holds h.mu.
func (h *fedUserHub) knownUser(fedToken string) bool {
	if fedToken == h.userToken {
		return true
	}
	_, ok := h.otherUsers[fedToken]
	return ok
}

func (h *fedUserHub) set(fn func(h *fedUserHub)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn(h)
}

// fedUserHubCalls is a copy of what the hub double has seen.
type fedUserHubCalls struct {
	meCalls      int
	meFedTokens  []string
	sent         []fedUserHubSend
	listQueries  []url.Values
	listTokens   []string
	dmReadKeys   []string
	dmReadTokens []string
	createCalls  int
	agentGets    []string
	order        []string
}

func (h *fedUserHub) snapshot() fedUserHubCalls {
	h.mu.Lock()
	defer h.mu.Unlock()
	return fedUserHubCalls{
		meCalls:      h.meCalls,
		meFedTokens:  append([]string(nil), h.meFedTokens...),
		sent:         append([]fedUserHubSend(nil), h.sent...),
		listQueries:  append([]url.Values(nil), h.listQueries...),
		listTokens:   append([]string(nil), h.listTokens...),
		dmReadKeys:   append([]string(nil), h.dmReadKeys...),
		dmReadTokens: append([]string(nil), h.dmReadTokens...),
		createCalls:  h.createCalls,
		agentGets:    append([]string(nil), h.agentGets...),
		order:        append([]string(nil), h.order...),
	}
}

func testMinter(t *testing.T) *identity.TokenMinter {
	t.Helper()
	m, err := identity.NewTokenMinter([]byte("federated-user-test-signing-key-0123456789"))
	if err != nil {
		t.Fatalf("NewTokenMinter: %v", err)
	}
	return m
}

// newFedUserTestServer builds a bridge with the federation scheme, a real
// SDK handler and executor, a broker server (for subscriptions), and the hub
// double as the hub. It mirrors newIntegrationTestServer, plus a signing key
// (the federation scheme mints the bridge credential it sends alongside the
// caller's).
func newFedUserTestServer(t *testing.T, hub *fedUserHub, configure ...func(*Config)) (*Bridge, *BrokerServer, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	cfg := &Config{
		Bridge: BridgeConfig{ExternalURL: "https://bridge.example.com"},
		Hub:    HubConfig{Endpoint: hub.URL, User: "admin@test"},
		Auth:   AuthConfig{Scheme: "federation"},
		Projects: []ProjectConfig{
			{Slug: "proj1", ExposedAgents: []string{"agent1", "agent2"}},
		},
		Timeouts: TimeoutConfig{SendMessage: 3 * time.Second},
	}
	for _, fn := range configure {
		fn(cfg)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	adminClient, err := hubclient.New(hub.URL, hubclient.WithBearerToken("admin-token"))
	if err != nil {
		t.Fatal(err)
	}
	b := New(store, adminClient, testMinter(t), cfg, nil, log)
	t.Cleanup(b.Shutdown)
	broker := NewBrokerServer(nil, log, context.Background())
	b.SetBroker(broker)

	executor := NewScionExecutor(b, log)
	innerStore := taskstore.NewInMemory(&taskstore.InMemoryStoreConfig{Authenticator: RouteKeyAuthenticator()})
	sdkRequestHandler := a2asrv.NewHandler(
		executor,
		a2asrv.WithLogger(log),
		a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}),
		a2asrv.WithAgentInactivityTimeout(10*time.Second),
		a2asrv.WithTaskStore(NewScopedTaskStore(innerStore)),
	)
	b.SetSDKRequestHandler(sdkRequestHandler)
	srv := NewServer(b, cfg, nil, log, a2asrv.NewJSONRPCHandler(sdkRequestHandler))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return b, broker, ts
}

const fedUserSendBody = `{
	"jsonrpc": "2.0",
	"id": "fed-user-1",
	"method": "SendMessage",
	"params": {
		"message": {
			"messageId": "msg-fed-user-001",
			"role": "ROLE_USER",
			"parts": [{"text": "Hello from hub A"}]
		}
	}
}`

// sendAsync sends the JSON-RPC SendMessage in the background and returns a
// channel with the status and body.
func sendAsync(t *testing.T, ts *httptest.Server, agentSlug, token string) <-chan [2]interface{} {
	t.Helper()
	return sendAsyncTo(t, ts, "proj1", agentSlug, token)
}

// sendAsyncTo is sendAsync for an agent of any project.
func sendAsyncTo(t *testing.T, ts *httptest.Server, projectSlug, agentSlug, token string) <-chan [2]interface{} {
	t.Helper()
	done := make(chan [2]interface{}, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/projects/"+projectSlug+"/agents/"+agentSlug+"/jsonrpc", strings.NewReader(fedUserSendBody))
		if err != nil {
			done <- [2]interface{}{0, err.Error()}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- [2]interface{}{0, err.Error()}
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		done <- [2]interface{}{resp.StatusCode, string(body)}
	}()
	return done
}

func waitForSend(t *testing.T, hub *fedUserHub) fedUserHubSend {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := hub.snapshot(); len(s.sent) > 0 {
			return s.sent[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("hub received no message send")
	return fedUserHubSend{}
}

func awaitResult(t *testing.T, done <-chan [2]interface{}) (int, string) {
	t.Helper()
	select {
	case r := <-done:
		status, _ := r[0].(int)
		body, _ := r[1].(string)
		return status, body
	case <-time.After(10 * time.Second):
		t.Fatal("JSON-RPC send did not return")
		return 0, ""
	}
}

// deliverReply delivers agent1's reply to the federated user the way the hub
// does: on the user's own topic, with no a2aTaskId metadata.
func deliverReply(t *testing.T, b *Bridge) {
	t.Helper()
	reply := &messages.StructuredMessage{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "agent:agent1",
		Recipient: "user:" + fedUserAccountID,
		Msg:       fedUserReplyText,
		Type:      messages.TypeInstruction,
	}
	topic := projectkeys.UserTopic(fedUserProjectID, fedUserAccountID)
	if err := b.HandleBrokerMessage(context.Background(), topic, reply); err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}
}

func brokerSubscribed(broker *BrokerServer, pattern string) bool {
	broker.mu.RLock()
	defer broker.mu.RUnlock()
	return broker.subscriptions[pattern]
}

// --- auth middleware: the user caller path ---

func TestAuthMiddleware_FederatedUserIdentity(t *testing.T) {
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	b, _, _ := newFedUserTestServer(t, hub)

	var seen *CallerIdentity
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = callerIdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	srv := NewServer(b, b.config, nil, b.log, echo)
	mw := srv.authMiddleware(echo)

	serve := func(bearer string) *httptest.ResponseRecorder {
		seen = nil
		req := httptest.NewRequest(http.MethodPost, "/projects/proj1/agents/agent1/jsonrpc", nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		w := httptest.NewRecorder()
		mw.ServeHTTP(w, req)
		return w
	}

	t.Run("user credential is identified by the hub", func(t *testing.T) {
		w := serve(token)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
		}
		if seen == nil || !seen.IsFederatedUser() {
			t.Fatalf("caller = %+v, want a federated user", seen)
		}
		if seen.UserID != fedUserAccountID {
			t.Errorf("UserID = %q, want the account the hub named (%q)", seen.UserID, fedUserAccountID)
		}
		if seen.Email != fedUserEmail {
			t.Errorf("Email = %q, want the credential's email %q", seen.Email, fedUserEmail)
		}
		if seen.TokenType != "federation" || seen.RawToken != token {
			t.Errorf("TokenType/RawToken = %q/%q, want federation and the caller's credential", seen.TokenType, seen.RawToken)
		}
		if got, want := seen.SenderLabel(), "user:"+fedUserAccountID; got != want {
			t.Errorf("SenderLabel() = %q, want %q", got, want)
		}
		if got, want := seen.CallerKey(), "fed:"+fedUserAccountID; got != want {
			t.Errorf("CallerKey() = %q, want %q", got, want)
		}
		snap := hub.snapshot()
		if snap.meCalls != 1 || snap.meFedTokens[0] != token {
			t.Errorf("auth/me calls = %d with credentials %v, want 1 with the caller's credential", snap.meCalls, snap.meFedTokens)
		}
	})

	t.Run("hub does not accept the credential", func(t *testing.T) {
		hub.set(func(h *fedUserHub) { h.meStatus = http.StatusUnauthorized })
		defer hub.set(func(h *fedUserHub) { h.meStatus = 0 })
		if w := serve(token); w.Code != http.StatusUnauthorized || seen != nil {
			t.Fatalf("status = %d (caller %+v), want 401 and no caller", w.Code, seen)
		}
	})

	// The hub refuses a user with no project visitor account (for example,
	// an identity issuer user not linked to an account) with 403
	// (GoogleCloudPlatform/scion#3577): the bridge answers with the uniform
	// refusal and admits no caller.
	t.Run("hub refuses the account", func(t *testing.T) {
		hub.set(func(h *fedUserHub) { h.meStatus = http.StatusForbidden })
		defer hub.set(func(h *fedUserHub) { h.meStatus = 0 })
		w := serve(token)
		if w.Code != http.StatusForbidden || seen != nil {
			t.Fatalf("status = %d (caller %+v), want 403 and no caller", w.Code, seen)
		}
		if got := strings.TrimSpace(w.Body.String()); got != federatedUserRefusalText {
			t.Errorf("body = %q, want the uniform refusal %q", got, federatedUserRefusalText)
		}
	})

	// A hub answer that names no project visitor account (an agent, or an
	// ID that is not an account ID) is refused the same way: the caller is
	// never admitted under another identity.
	for _, tc := range []struct {
		name   string
		meID   string
		meRole string
	}{
		{"hub answer naming an agent is refused", fedUserAccountID, "agent"},
		{"hub answer naming no account is refused", fedUserIssuer + ":" + fedUserSubject, "member"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub.set(func(h *fedUserHub) { h.meID, h.meRole = tc.meID, tc.meRole })
			defer hub.set(func(h *fedUserHub) { h.meID, h.meRole = fedUserAccountID, "viewer" })
			w := serve(token)
			if w.Code != http.StatusForbidden || seen != nil {
				t.Fatalf("status = %d (caller %+v), want 403 and no caller", w.Code, seen)
			}
			if got := strings.TrimSpace(w.Body.String()); got != federatedUserRefusalText {
				t.Errorf("body = %q, want the uniform refusal %q", got, federatedUserRefusalText)
			}
		})
	}

	t.Run("hub could not be asked", func(t *testing.T) {
		hub.set(func(h *fedUserHub) { h.meStatus = http.StatusServiceUnavailable })
		defer hub.set(func(h *fedUserHub) { h.meStatus = 0 })
		w := serve(token)
		if w.Code != http.StatusBadGateway || seen != nil {
			t.Fatalf("status = %d (caller %+v), want 502 and no caller", w.Code, seen)
		}
		if strings.Contains(w.Body.String(), federatedUserRefusalText) {
			t.Errorf("body = %q: a hub outage is answered with the access refusal", w.Body.String())
		}
	})

	t.Run("agent credential is not sent to auth/me", func(t *testing.T) {
		before := hub.snapshot().meCalls
		agentToken := fedAgentToken(t)
		if w := serve(agentToken); w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if seen == nil || !seen.IsAgent() || seen.IsFederatedUser() || seen.AgentID != "fed-agent-1" || seen.UserID != "" {
			t.Fatalf("caller = %+v, want the unchanged agent caller", seen)
		}
		if after := hub.snapshot().meCalls; after != before {
			t.Errorf("auth/me calls %d -> %d for an agent credential, want none", before, after)
		}
	})
}

// --- executor: send, reply correlation, revocation ---

func TestExecutor_FederatedUserReplyThroughUserTopic(t *testing.T) {
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	b, broker, ts := newFedUserTestServer(t, hub)

	done := sendAsync(t, ts, "agent1", token)
	sent := waitForSend(t, hub)

	if sent.AgentID != fedUserAgentID {
		t.Errorf("send went to agent %q, want %q", sent.AgentID, fedUserAgentID)
	}
	if sent.FedToken != token {
		t.Errorf("send carried federation credential %q, want the caller's", sent.FedToken)
	}
	sm, _ := sent.Body["structured_message"].(map[string]interface{})
	if sm == nil {
		t.Fatalf("send body has no structured_message: %v", sent.Body)
	}
	if _, has := sm["metadata"]; has {
		t.Errorf("structured_message carries metadata %v; the hub accepts only a text message from a federated user", sm["metadata"])
	}
	for _, field := range []string{"interrupt", "notify", "wake", "mentions"} {
		if _, has := sent.Body[field]; has {
			t.Errorf("send body sets %q", field)
		}
	}
	if got, want := sm["sender"], "user:"+fedUserAccountID; got != want {
		t.Errorf("sender = %v, want %q", got, want)
	}
	if !brokerSubscribed(broker, projectkeys.UserTopic(fedUserProjectID, fedUserAccountID)) {
		t.Errorf("no subscription to the federated user's own topic (%s)", projectkeys.UserTopic(fedUserProjectID, fedUserAccountID))
	}
	if brokerSubscribed(broker, projectkeys.AllUserTopic(fedUserProjectID)) {
		t.Errorf("federated user caller subscribed to all user topics (%s)", projectkeys.AllUserTopic(fedUserProjectID))
	}

	deliverReply(t, b)

	status, body := awaitResult(t, done)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, fedUserReplyText) {
		t.Fatalf("response does not carry the agent's reply; body %s", body)
	}

	snap := hub.snapshot()
	wantKey, _ := messages.DMConversationKey("agent", fedUserAgentID, "user", fedUserAccountID)
	if len(snap.dmReadKeys) != 1 || snap.dmReadKeys[0] != wantKey || snap.dmReadTokens[0] != token {
		t.Errorf("access re-checks = %v with %v, want one read of %q with the caller's credential", snap.dmReadKeys, snap.dmReadTokens, wantKey)
	}
}

func TestExecutor_FederatedUserRevokedEndsTask(t *testing.T) {
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	b, _, ts := newFedUserTestServer(t, hub)

	done := sendAsync(t, ts, "agent1", token)
	waitForSend(t, hub)

	// The grant is revoked while the agent works: the hub now refuses the
	// caller's read of its conversation with the agent.
	hub.set(func(h *fedUserHub) { h.dmReadStatus = http.StatusForbidden })
	deliverReply(t, b)

	status, body := awaitResult(t, done)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", status, body)
	}
	if strings.Contains(body, fedUserReplyText) {
		t.Fatalf("reply forwarded after the hub refused access; body %s", body)
	}
	if !strings.Contains(body, federatedUserRefusalText) {
		t.Fatalf("response does not carry the uniform refusal; body %s", body)
	}
	if !strings.Contains(body, "FAILED") && !strings.Contains(body, "failed") {
		t.Errorf("task did not end failed; body %s", body)
	}
}

func TestExecutor_FederatedUserRecheckUnavailableStopsReply(t *testing.T) {
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	b, _, ts := newFedUserTestServer(t, hub)

	done := sendAsync(t, ts, "agent1", token)
	waitForSend(t, hub)
	hub.set(func(h *fedUserHub) { h.dmReadStatus = http.StatusInternalServerError })
	deliverReply(t, b)

	_, body := awaitResult(t, done)
	if strings.Contains(body, fedUserReplyText) {
		t.Fatalf("reply forwarded although the hub could not confirm access; body %s", body)
	}
}

// TestExecutor_FederatedUserRefusalsAreUniform: an agent the hub does not let
// the caller message, and an agent that does not exist, give the same
// answer, and the bridge never creates an agent for a federated user.
func TestExecutor_FederatedUserRefusalsAreUniform(t *testing.T) {
	token := fedUserToken(t)

	refusedHub := newFedUserHub(t, token)
	refusedHub.set(func(h *fedUserHub) { h.sendStatus = http.StatusForbidden })
	_, _, refusedTS := newFedUserTestServer(t, refusedHub)
	refusedStatus, refusedBody := awaitResult(t, sendAsync(t, refusedTS, "agent1", token))

	unknownHub := newFedUserHub(t, token)
	unknownHub.set(func(h *fedUserHub) { h.adminAgents = nil })
	// agent2 is exposed but does not exist; auto-provisioning is on.
	_, _, unknownTS := newFedUserTestServer(t, unknownHub, func(cfg *Config) {
		cfg.Projects[0].AutoProvision = true
		cfg.Projects[0].DefaultTemplate = "default"
	})
	unknownStatus, unknownBody := awaitResult(t, sendAsync(t, unknownTS, "agent2", token))

	for name, r := range map[string]struct {
		status int
		body   string
	}{"refused agent": {refusedStatus, refusedBody}, "unknown agent": {unknownStatus, unknownBody}} {
		if r.status != http.StatusOK {
			t.Errorf("%s: status = %d, want 200; body %s", name, r.status, r.body)
		}
		if !strings.Contains(r.body, federatedUserRefusalText) {
			t.Errorf("%s: response does not carry the uniform refusal; body %s", name, r.body)
		}
		if strings.Contains(r.body, `"error"`) {
			t.Errorf("%s: answered with a JSON-RPC error instead of a failed task; body %s", name, r.body)
		}
	}
	if calls := unknownHub.snapshot(); len(calls.sent) != 0 || calls.createCalls != 0 {
		t.Errorf("unknown agent: %d sends and %d agent creates reached the hub, want 0 and 0", len(calls.sent), calls.createCalls)
	}
}

// TestExecutor_FederatedUserHubAccessOff: when the hub does not accept the
// credential (401, as a hub that refuses user access credentials from peers
// answers), the bridge answers 401 and sends nothing.
func TestExecutor_FederatedUserHubAccessOff(t *testing.T) {
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	hub.set(func(h *fedUserHub) { h.meStatus = http.StatusUnauthorized })
	_, _, ts := newFedUserTestServer(t, hub)

	status, body := awaitResult(t, sendAsync(t, ts, "agent1", token))
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body %s", status, body)
	}
	if n := len(hub.snapshot().sent); n != 0 {
		t.Errorf("%d sends reached the hub, want 0", n)
	}
}

// TestExecutor_FederatedAgentUnchanged is the regression for agent
// credentials: metadata, sender and admin topic subscription as before, and
// no auth/me call or access re-check.
func TestExecutor_FederatedAgentUnchanged(t *testing.T) {
	agentToken := fedAgentToken(t)
	hub := newFedUserHub(t, "unused-user-token")
	_, broker, ts := newFedUserTestServer(t, hub)

	done := sendAsync(t, ts, "agent1", agentToken)
	sent := waitForSend(t, hub)
	awaitResult(t, done) // no reply: the send times out

	if sent.FedToken != agentToken {
		t.Errorf("send carried federation credential %q, want the agent's", sent.FedToken)
	}
	sm, _ := sent.Body["structured_message"].(map[string]interface{})
	md, _ := sm["metadata"].(map[string]interface{})
	if md == nil || md["a2aTaskId"] == nil || md["a2aTaskId"] == "" {
		t.Errorf("agent send metadata = %v, want a2aTaskId", sm["metadata"])
	}
	if sm["sender"] != "agent:fed-agent-1" {
		t.Errorf("sender = %v, want agent:fed-agent-1", sm["sender"])
	}
	if !brokerSubscribed(broker, projectkeys.UserTopic(fedUserProjectID, "admin@test")) {
		t.Error("agent caller did not subscribe to the bridge admin's user topic")
	}
	if brokerSubscribed(broker, projectkeys.AllUserTopic(fedUserProjectID)) {
		t.Error("agent caller subscribed to all user topics")
	}
	snap := hub.snapshot()
	if snap.meCalls != 0 || len(snap.dmReadKeys) != 0 {
		t.Errorf("auth/me calls = %d, access re-checks = %d for an agent credential, want 0 and 0", snap.meCalls, len(snap.dmReadKeys))
	}
}

// TestValidateTopicUser_FederatedUser: the caller ID the task stores for a
// federated user is its account ID, so a reply on its own user topic is
// accepted and one on another user's topic is not.
func TestValidateTopicUser_FederatedUser(t *testing.T) {
	b, _ := newLifecycleTestBridge(t)
	caller := &CallerIdentity{TokenType: "federation", UserID: fedUserAccountID, Email: fedUserEmail, IssuerURL: fedUserIssuer, Subject: fedUserSubject}
	own := projectkeys.UserTopic(fedUserProjectID, fedUserAccountID)
	other := projectkeys.UserTopic(fedUserProjectID, "22222222-3333-4444-8555-666666666666")
	if err := b.validateTopicUser(own, caller.UserID, "task-1"); err != nil {
		t.Errorf("own user topic refused: %v", err)
	}
	if err := b.validateTopicUser(other, caller.UserID, "task-1"); err == nil {
		t.Error("another user's topic accepted")
	}
}

// --- agent cards ---

func TestFederatedUserAgentCards(t *testing.T) {
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	_, _, ts := newFedUserTestServer(t, hub)

	get := func(path, bearer string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	t.Run("lists only messageable, exposed agents", func(t *testing.T) {
		status, body := get("/projects/proj1/agents", token)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200; body %s", status, body)
		}
		var resp struct {
			Agents []map[string]interface{} `json:"agents"`
		}
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("decode: %v; body %s", err, body)
		}
		if len(resp.Agents) != 1 {
			t.Fatalf("cards = %d, want 1 (agent1; agent3 is not exposed, agent2 not messageable); body %s", len(resp.Agents), body)
		}
		if resp.Agents[0]["name"] != "agent1" || resp.Agents[0]["url"] != "https://bridge.example.com/projects/proj1/agents/agent1" {
			t.Errorf("card = %v, want agent1's", resp.Agents[0])
		}
		snap := hub.snapshot()
		if len(snap.listQueries) != 1 || snap.listQueries[0].Get("messageable") != "true" {
			t.Errorf("hub list queries = %v, want one with messageable=true", snap.listQueries)
		}
		if snap.listTokens[0] != token {
			t.Errorf("hub list used federation credential %q, want the caller's", snap.listTokens[0])
		}
	})

	t.Run("no active grant on the project is refused", func(t *testing.T) {
		hub.set(func(h *fedUserHub) { h.grants[0]["status"] = "expired" })
		defer hub.set(func(h *fedUserHub) { h.grants[0]["status"] = "active" })
		status, body := get("/projects/proj1/agents", token)
		if status != http.StatusForbidden || !strings.Contains(body, federatedUserRefusalText) {
			t.Fatalf("status = %d body %q, want 403 with the uniform refusal", status, body)
		}
	})

	t.Run("unknown project is refused the same way", func(t *testing.T) {
		status, body := get("/projects/nope/agents", token)
		if status != http.StatusForbidden || !strings.Contains(body, federatedUserRefusalText) {
			t.Fatalf("status = %d body %q, want 403 with the uniform refusal", status, body)
		}
	})

	t.Run("agent caller gets no per-caller list", func(t *testing.T) {
		if status, _ := get("/projects/proj1/agents", fedAgentToken(t)); status != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", status)
		}
	})
}

// --- resubscribe re-check ---

func TestDurableHandler_FederatedUserStreamRefusal(t *testing.T) {
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	b, _, _ := newFedUserTestServer(t, hub)
	caller := &CallerIdentity{TokenType: "federation", RawToken: token, UserID: fedUserAccountID, Email: fedUserEmail, IssuerURL: fedUserIssuer, Subject: fedUserSubject}
	ctx := WithRouteInfo(context.Background(), RouteInfo{ProjectSlug: "proj1", AgentSlug: "agent1"})

	h := &DurableRequestHandler{}
	if h.federatedUserStreamRefusal(ctx, caller) == nil {
		t.Error("without a bridge the reply is streamed; want it held back")
	}
	h.SetBridge(b)
	if msg := h.federatedUserStreamRefusal(context.Background(), caller); msg == nil {
		t.Error("without route info the reply is streamed; want it held back")
	}
	if msg := h.federatedUserStreamRefusal(ctx, caller); msg != nil {
		t.Errorf("access granted, but the reply is held back: %v", msg)
	}
	hub.set(func(h *fedUserHub) { h.dmReadStatus = http.StatusForbidden })
	msg := h.federatedUserStreamRefusal(ctx, caller)
	if msg == nil {
		t.Fatal("access revoked, but the reply is streamed")
	}
	if len(msg.Parts) != 1 || msg.Parts[0].Text() != federatedUserRefusalText {
		t.Errorf("refusal message = %v, want the uniform refusal", msg.Parts)
	}
}

// --- reply correlation: a federated user hears only its own replies ---

// deliverReplyTo delivers agent1's reply with text msg on userID's user
// topic, with no a2aTaskId metadata.
func deliverReplyTo(t *testing.T, b *Bridge, userID, msg string) {
	t.Helper()
	reply := &messages.StructuredMessage{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "agent:agent1",
		Recipient: "user:" + userID,
		Msg:       msg,
		Type:      messages.TypeInstruction,
	}
	if err := b.HandleBrokerMessage(context.Background(), projectkeys.UserTopic(fedUserProjectID, userID), reply); err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}
}

// TestExecutor_FederatedUserNeverGetsAnotherUsersReply: another user (a
// member of the project) also talks to agent1. In the default (plugin) mode
// the bridge correlates replies without a2aTaskId through its local cache;
// agent1's reply to the other user must not reach the federated user's
// task, and the federated user's own reply still does.
func TestExecutor_FederatedUserNeverGetsAnotherUsersReply(t *testing.T) {
	const (
		otherUserID    = "33333333-4444-4555-8666-777777777777"
		otherReplyText = "reply from agent1 to another user"
	)
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	b, _, ts := newFedUserTestServer(t, hub)

	// The other user's task on agent1 started first.
	aKey := agentKey(fedUserProjectID, "agent1")
	b.registerActiveTask("other-user-task", aKey)
	t.Cleanup(func() { b.unregisterActiveTask("other-user-task", aKey) })

	done := sendAsync(t, ts, "agent1", token)
	waitForSend(t, hub)

	// The federated user's task is now the latest on agent1. The other
	// user's reply arrives first, on the other user's topic.
	deliverReplyTo(t, b, otherUserID, otherReplyText)
	deliverReplyTo(t, b, fedUserAccountID, fedUserReplyText)

	status, body := awaitResult(t, done)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", status, body)
	}
	if strings.Contains(body, otherReplyText) {
		t.Fatalf("the federated user received the reply addressed to another user; body %s", body)
	}
	if !strings.Contains(body, fedUserReplyText) {
		t.Fatalf("response does not carry the federated user's own reply; body %s", body)
	}
}

// TestExecutor_AgentCallerTakesNoReplyAddressedToAUser: an agent caller's
// task, in the default (plugin) mode, where replies without a2aTaskId are
// correlated through the local cache, takes none of agent1's replies
// addressed to a member of the project or to a federated user (whose task
// has ended), and still takes agent1's reply on the bridge admin's topic,
// where the replies to an agent caller arrive.
func TestExecutor_AgentCallerTakesNoReplyAddressedToAUser(t *testing.T) {
	const (
		memberID         = "33333333-4444-4555-8666-777777777777"
		memberReplyText  = "reply from agent1 to a member"
		visitorReplyText = "reply from agent1 to a federated user"
		adminReplyText   = "reply from agent1 to the agent caller"
	)
	agentToken := fedAgentToken(t)
	hub := newFedUserHub(t, "unused-user-token")
	b, _, ts := newFedUserTestServer(t, hub)

	done := sendAsync(t, ts, "agent1", agentToken)
	waitForSend(t, hub)

	deliverReplyTo(t, b, memberID, memberReplyText)
	deliverReplyTo(t, b, fedUserAccountID, visitorReplyText)
	deliverReplyTo(t, b, "admin@test", adminReplyText)

	status, body := awaitResult(t, done)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", status, body)
	}
	if strings.Contains(body, memberReplyText) || strings.Contains(body, visitorReplyText) {
		t.Fatalf("the agent caller received a reply addressed to a user; body %s", body)
	}
	if !strings.Contains(body, adminReplyText) {
		t.Fatalf("response does not carry the reply on the bridge admin's topic; body %s", body)
	}
}

// TestExecutor_FederatedUserNeverGetsAnotherVisitorsReply: a second
// federated user, whose task on agent1 has ended, gets another reply from
// agent1. With the first federated user's task and an agent caller's task
// active on agent1, that reply reaches neither; the first federated user
// still gets its own reply.
func TestExecutor_FederatedUserNeverGetsAnotherVisitorsReply(t *testing.T) {
	const (
		secondVisitorID   = "44444444-5555-4666-8777-888888888888"
		secondVisitorText = "reply from agent1 to the second federated user"
	)
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	b, _, ts := newFedUserTestServer(t, hub)

	aKey := agentKey(fedUserProjectID, "agent1")
	b.registerActiveTaskFor("agent-task", aKey, &CallerIdentity{TokenType: "federation", AgentID: "peer-agent", IssuerURL: fedUserIssuer})
	t.Cleanup(func() { b.unregisterActiveTask("agent-task", aKey) })

	done := sendAsync(t, ts, "agent1", token)
	waitForSend(t, hub)

	secondTopic := projectkeys.UserTopic(fedUserProjectID, secondVisitorID)
	msg := &messages.StructuredMessage{Sender: "agent:agent1", Type: messages.TypeInstruction, Msg: secondVisitorText}
	if got, err := b.correlateToTask(context.Background(), fedUserProjectID, "agent1", secondTopic, msg); err == nil {
		t.Errorf("the second federated user's reply correlated to task %q, want none", got)
	}

	deliverReplyTo(t, b, secondVisitorID, secondVisitorText)
	deliverReplyTo(t, b, fedUserAccountID, fedUserReplyText)

	status, body := awaitResult(t, done)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", status, body)
	}
	if strings.Contains(body, secondVisitorText) {
		t.Fatalf("the federated user received the reply addressed to another federated user; body %s", body)
	}
	if !strings.Contains(body, fedUserReplyText) {
		t.Fatalf("response does not carry the federated user's own reply; body %s", body)
	}
}

// TestCorrelate_PluginModeAgentCallerTask: in plugin mode, an agent
// caller's task takes a reply without a2aTaskId only on the bridge admin's
// topic or a topic with no user, both from the local cache and from the
// legacy task store.
func TestCorrelate_PluginModeAgentCallerTask(t *testing.T) {
	b, store := newLifecycleTestBridge(t) // the bridge admin is "test-user"
	const projID, agentSlug = "proj1", "agent1"
	agentCaller := &CallerIdentity{TokenType: "federation", AgentID: "peer-agent", IssuerURL: fedUserIssuer}
	reply := &messages.StructuredMessage{Sender: "agent:" + agentSlug, Type: messages.TypeInstruction, Msg: "hi"}
	cases := []struct {
		name   string
		topic  string
		wantOK bool
	}{
		{"a member's topic", projectkeys.UserTopic(projID, "33333333-4444-4555-8666-777777777777"), false},
		{"a federated user's topic", projectkeys.UserTopic(projID, fedUserAccountID), false},
		{"the bridge admin's topic", projectkeys.UserTopic(projID, "test-user"), true},
		{"topic without a user", projectkeys.AgentTopic(projID, "some-agent"), true},
	}
	check := func(t *testing.T, wantTask string) {
		t.Helper()
		for _, tc := range cases {
			got, err := b.correlateToTask(context.Background(), projID, agentSlug, tc.topic, reply)
			if tc.wantOK && (err != nil || got != wantTask) {
				t.Errorf("%s: correlateToTask = %q, %v; want %q", tc.name, got, err, wantTask)
			}
			if !tc.wantOK && err == nil {
				t.Errorf("%s: correlateToTask = %q, want no task", tc.name, got)
			}
		}
	}

	t.Run("local cache", func(t *testing.T) {
		aKey := agentKey(projID, agentSlug)
		b.registerActiveTaskFor("agent-task", aKey, agentCaller)
		defer b.unregisterActiveTask("agent-task", aKey)
		check(t, "agent-task")
	})

	t.Run("legacy task store", func(t *testing.T) {
		now := time.Now()
		if err := store.CreateTask(context.Background(), &state.Task{
			ID:           "legacy-agent-task",
			ContextID:    "ctx-agent-caller",
			ProjectID:    projID,
			AgentSlug:    agentSlug,
			AgentID:      fedUserAgentID,
			State:        TaskStateWorking,
			CallerUserID: agentCaller.CallerKey(),
			CreatedAt:    now,
			UpdatedAt:    now,
			Metadata:     "{}",
		}); err != nil {
			t.Fatalf("create legacy task: %v", err)
		}
		check(t, "legacy-agent-task")
	})
}

func TestLegacyTaskTakesReply(t *testing.T) {
	b, _ := newLifecycleTestBridge(t) // the bridge admin is "test-user"
	const memberID = "33333333-4444-4555-8666-777777777777"
	agentCallerKey := (&CallerIdentity{TokenType: "federation", AgentID: "peer-agent", IssuerURL: fedUserIssuer}).CallerKey()
	fedKey := federatedUserCallerKey(fedUserAccountID)
	tests := []struct {
		name, callerKey, topicUser string
		want                       bool
	}{
		{"agent caller, bridge admin's topic", agentCallerKey, "test-user", true},
		{"agent caller, topic without a user", agentCallerKey, "", true},
		{"agent caller, a member's topic", agentCallerKey, memberID, false},
		{"agent caller, a federated user's topic", agentCallerKey, fedUserAccountID, false},
		{"federated user, own topic", fedKey, fedUserAccountID, true},
		{"federated user, a member's topic", fedKey, memberID, false},
		{"federated user, topic without a user", fedKey, "", false},
		{"member, another user's topic: as before", memberID, fedUserAccountID, true},
		{"no caller identity, a member's topic: as before", "", memberID, true},
	}
	for _, tt := range tests {
		if got := b.legacyTaskTakesReply(tt.callerKey, tt.topicUser); got != tt.want {
			t.Errorf("%s: legacyTaskTakesReply(%q, %q) = %v, want %v", tt.name, tt.callerKey, tt.topicUser, got, tt.want)
		}
	}
}

func TestPickPluginModeTask(t *testing.T) {
	const visitorA, visitorB = "aaaaaaaa-0000-4000-8000-000000000001", "bbbbbbbb-0000-4000-8000-000000000002"
	fed := map[string]string{"fed-a": visitorA, "fed-b": visitorB}
	tests := []struct {
		name      string
		taskIDs   []string
		fed       map[string]string
		topicUser string
		want      string
		wantOK    bool
	}{
		{"no federated tasks: latest task, as before", []string{"t1", "t2"}, nil, "someone", "t2", true},
		{"no federated tasks, topic without a user: latest task", []string{"t1", "t2"}, nil, "", "t2", true},
		{"own topic picks the federated user's task", []string{"t1", "fed-a", "t2"}, fed, visitorA, "fed-a", true},
		{"another user's topic skips the latest federated task", []string{"t1", "fed-a"}, fed, "someone", "t1", true},
		{"another visitor's topic skips the federated task", []string{"t1", "fed-a"}, fed, visitorB, "t1", true},
		{"topic without a user skips federated tasks", []string{"t1", "fed-a"}, fed, "", "t1", true},
		{"only another user's federated task: none", []string{"fed-a"}, fed, "someone", "", false},
		{"two federated tasks: the topic's account", []string{"fed-a", "fed-b"}, fed, visitorA, "fed-a", true},
		{"no tasks: none", nil, nil, visitorA, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := pickPluginModeTask(tt.taskIDs, tt.fed, tt.topicUser)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("pickPluginModeTask = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestRegisterActiveTaskFor_KeepsFederatedUser(t *testing.T) {
	b, _ := newLifecycleTestBridge(t)
	aKey := agentKey(fedUserProjectID, "agent1")
	fedCaller := &CallerIdentity{TokenType: "federation", Subject: fedUserSubject, IssuerURL: fedUserIssuer, UserID: fedUserAccountID}
	b.registerActiveTaskFor("fed-task", aKey, fedCaller)
	b.registerActiveTask("fed-task", aKey) // a later registration without the account
	b.tasksMu.RLock()
	entry := b.activeTasks["fed-task"]
	n := len(b.agentTasks[aKey])
	b.tasksMu.RUnlock()
	if entry.federatedUserID != fedUserAccountID {
		t.Errorf("federatedUserID = %q after re-registration, want %q", entry.federatedUserID, fedUserAccountID)
	}
	if entry.agentCaller {
		t.Error("federated user's task recorded as an agent caller's")
	}
	if n != 1 {
		t.Errorf("agent task list has %d entries, want 1", n)
	}
}

func TestRegisterActiveTaskFor_KeepsAgentCaller(t *testing.T) {
	b, _ := newLifecycleTestBridge(t)
	aKey := agentKey(fedUserProjectID, "agent1")
	agentCaller := &CallerIdentity{TokenType: "federation", AgentID: "peer-agent", IssuerURL: fedUserIssuer}
	b.registerActiveTaskFor("agent-task", aKey, agentCaller)
	b.registerActiveTask("agent-task", aKey) // a later registration without the caller
	b.registerActiveTaskFor("member-task", aKey, &CallerIdentity{UserID: "member-1"})
	b.registerActiveTask("legacy-task", aKey)
	b.tasksMu.RLock()
	agentEntry, memberEntry, legacyEntry := b.activeTasks["agent-task"], b.activeTasks["member-task"], b.activeTasks["legacy-task"]
	b.tasksMu.RUnlock()
	if !agentEntry.agentCaller || agentEntry.federatedUserID != "" {
		t.Errorf("agent caller's task = %+v after re-registration, want agentCaller and no account", agentEntry)
	}
	if memberEntry.agentCaller || memberEntry.federatedUserID != "" {
		t.Errorf("member's task = %+v, want neither agentCaller nor an account", memberEntry)
	}
	if legacyEntry.agentCaller || legacyEntry.federatedUserID != "" {
		t.Errorf("task with no caller identity = %+v, want neither agentCaller nor an account", legacyEntry)
	}
}

// TestCorrelate_FederatedUserTaskStandalone covers standalone mode (the SDK
// task store is authoritative): a federated user's task takes a reply only
// on its own account's user topic, with or without a2aTaskId. Needs
// PostgreSQL (TEST_DATABASE_URL).
func TestCorrelate_FederatedUserTaskStandalone(t *testing.T) {
	store := testPostgresTaskStore(t)
	b, _ := newLifecycleTestBridge(t)
	b.SetSDKTaskStore(store)

	projID := "proj-fed-" + randomSuffix()
	const agentSlug = "agent-fed"
	t.Cleanup(func() {
		store.db.ExecContext(context.Background(), `DELETE FROM a2a_sdk_tasks WHERE project_id = $1`, projID)
	})

	fedCaller := &CallerIdentity{TokenType: "federation", Subject: fedUserSubject, IssuerURL: fedUserIssuer, Email: fedUserEmail, UserID: fedUserAccountID}
	ctx := withCallerIdentity(ctxForRoute(projID, agentSlug), fedCaller)
	taskID := "fed-task-" + randomSuffix()
	if _, err := store.Create(ctx, &a2a.Task{ID: a2a.TaskID(taskID), Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	federated, err := store.IsFederatedUserTask(context.Background(), taskID)
	if err != nil || !federated {
		t.Fatalf("IsFederatedUserTask = %v, %v; want true", federated, err)
	}

	reply := func(metadata map[string]string) *messages.StructuredMessage {
		return &messages.StructuredMessage{Sender: "agent:" + agentSlug, Type: messages.TypeInstruction, Msg: "hi", Metadata: metadata}
	}
	own := projectkeys.UserTopic(projID, fedUserAccountID)
	other := projectkeys.UserTopic(projID, "33333333-4444-4555-8666-777777777777")
	agentTopic := projectkeys.AgentTopic(projID, "some-agent")
	withID := map[string]string{"a2aTaskId": taskID}

	for _, tc := range []struct {
		name     string
		topic    string
		metadata map[string]string
		wantOK   bool
	}{
		{"own topic, no metadata", own, nil, true},
		{"own topic, a2aTaskId", own, withID, true},
		{"another user's topic, no metadata", other, nil, false},
		{"another user's topic, a2aTaskId", other, withID, false},
		{"topic without a user, no metadata", agentTopic, nil, false},
		{"topic without a user, a2aTaskId", agentTopic, withID, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := b.correlateToTask(context.Background(), projID, agentSlug, tc.topic, reply(tc.metadata))
			if tc.wantOK && (err != nil || got != taskID) {
				t.Fatalf("correlateToTask = %q, %v; want %q", got, err, taskID)
			}
			if !tc.wantOK && err == nil {
				t.Fatalf("correlateToTask = %q, want no task", got)
			}
		})
	}

	// A task of a member of the project is not a federated user's task.
	memberTaskID := "member-task-" + randomSuffix()
	memberCtx := ctxForRouteAndCaller(projID, "agent-member", fedUserAccountID)
	if _, err := store.Create(memberCtx, &a2a.Task{ID: a2a.TaskID(memberTaskID), Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}); err != nil {
		t.Fatalf("create member task: %v", err)
	}
	if federated, err := store.IsFederatedUserTask(context.Background(), memberTaskID); err != nil || federated {
		t.Errorf("IsFederatedUserTask(member task) = %v, %v; want false", federated, err)
	}
}

func TestIsFederatedUserOwnerKey(t *testing.T) {
	tests := []struct {
		owner, callerUserID string
		want                bool
	}{
		{"proj1:agent1:fed:" + fedUserAccountID, fedUserAccountID, true},
		{"proj1:agent1:" + fedUserAccountID, fedUserAccountID, false},
		{"proj1:agent1:agent:hub-a.example.com:a1", "", false},
		{"proj1:agent1", "", false},
		{"proj1:agent1:fed:" + fedUserAccountID, "", false},
	}
	for _, tt := range tests {
		if got := isFederatedUserOwnerKey(tt.owner, tt.callerUserID); got != tt.want {
			t.Errorf("isFederatedUserOwnerKey(%q, %q) = %v, want %v", tt.owner, tt.callerUserID, got, tt.want)
		}
	}
}

// TestCorrelate_AgentCallerTaskStandalone covers standalone mode (the SDK
// task store is authoritative): an agent caller's task takes a reply only
// on the bridge admin's topic or a topic with no user, with or without
// a2aTaskId; a task with no caller identity is unchanged. Needs PostgreSQL
// (TEST_DATABASE_URL).
func TestCorrelate_AgentCallerTaskStandalone(t *testing.T) {
	store := testPostgresTaskStore(t)
	b, _ := newLifecycleTestBridge(t) // the bridge admin is "test-user"
	b.SetSDKTaskStore(store)

	projID := "proj-agentcaller-" + randomSuffix()
	const agentSlug, legacySlug = "agent-ac", "agent-nocaller"
	t.Cleanup(func() {
		store.db.ExecContext(context.Background(), `DELETE FROM a2a_sdk_tasks WHERE project_id = $1`, projID)
	})

	agentCaller := &CallerIdentity{TokenType: "federation", AgentID: "peer-agent", IssuerURL: fedUserIssuer}
	taskID := "agent-task-" + randomSuffix()
	if _, err := store.Create(withCallerIdentity(ctxForRoute(projID, agentSlug), agentCaller), &a2a.Task{ID: a2a.TaskID(taskID), Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if agent, err := store.IsAgentCallerTask(context.Background(), taskID); err != nil || !agent {
		t.Fatalf("IsAgentCallerTask = %v, %v; want true", agent, err)
	}

	reply := func(metadata map[string]string) *messages.StructuredMessage {
		return &messages.StructuredMessage{Sender: "agent:" + agentSlug, Type: messages.TypeInstruction, Msg: "hi", Metadata: metadata}
	}
	member := projectkeys.UserTopic(projID, "33333333-4444-4555-8666-777777777777")
	visitor := projectkeys.UserTopic(projID, fedUserAccountID)
	admin := projectkeys.UserTopic(projID, "test-user")
	agentTopic := projectkeys.AgentTopic(projID, "some-agent")
	withID := map[string]string{"a2aTaskId": taskID}

	for _, tc := range []struct {
		name     string
		topic    string
		metadata map[string]string
		wantOK   bool
	}{
		{"bridge admin's topic, no metadata", admin, nil, true},
		{"bridge admin's topic, a2aTaskId", admin, withID, true},
		{"topic without a user, no metadata", agentTopic, nil, true},
		{"a member's topic, no metadata", member, nil, false},
		{"a member's topic, a2aTaskId", member, withID, false},
		{"a federated user's topic, no metadata", visitor, nil, false},
		{"a federated user's topic, a2aTaskId", visitor, withID, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := b.correlateToTask(context.Background(), projID, agentSlug, tc.topic, reply(tc.metadata))
			if tc.wantOK && (err != nil || got != taskID) {
				t.Fatalf("correlateToTask = %q, %v; want %q", got, err, taskID)
			}
			if !tc.wantOK && err == nil {
				t.Fatalf("correlateToTask = %q, want no task", got)
			}
		})
	}

	// A task with no caller identity is not an agent caller's task and
	// keeps its rule.
	legacyTaskID := "nocaller-task-" + randomSuffix()
	if _, err := store.Create(ctxForRoute(projID, legacySlug), &a2a.Task{ID: a2a.TaskID(legacyTaskID), Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}); err != nil {
		t.Fatalf("create task with no caller identity: %v", err)
	}
	if agent, err := store.IsAgentCallerTask(context.Background(), legacyTaskID); err != nil || agent {
		t.Errorf("IsAgentCallerTask(task with no caller identity) = %v, %v; want false", agent, err)
	}
	legacyReply := &messages.StructuredMessage{Sender: "agent:" + legacySlug, Type: messages.TypeInstruction, Msg: "hi", Metadata: map[string]string{"a2aTaskId": legacyTaskID}}
	if got, err := b.correlateToTask(context.Background(), projID, legacySlug, member, legacyReply); err != nil || got != legacyTaskID {
		t.Errorf("correlateToTask(task with no caller identity) = %q, %v; want %q", got, err, legacyTaskID)
	}
}

func TestIsAgentCallerOwnerKey(t *testing.T) {
	tests := []struct {
		owner, callerUserID string
		want                bool
	}{
		{"proj1:agent1:agent:hub-a.example.com:a1", "", true},
		{"proj1:agent1:agent:a1", "", true},
		{"proj1:agent1", "", false},
		{"proj1:agent1:fed:" + fedUserAccountID, fedUserAccountID, false},
		{"proj1:agent1:member-1", "member-1", false},
	}
	for _, tt := range tests {
		if got := isAgentCallerOwnerKey(tt.owner, tt.callerUserID, "proj1", "agent1"); got != tt.want {
			t.Errorf("isAgentCallerOwnerKey(%q, %q) = %v, want %v", tt.owner, tt.callerUserID, got, tt.want)
		}
	}
}

// --- an identity issuer user the hub names no visitor account for ---

// TestExecutor_IdentityIssuerUserWithoutVisitorAccountRefused: a user
// access credential from an identity issuer whose user has no project
// visitor account on the hub is refused by the hub (403 on GET
// /api/v1/auth/me, GoogleCloudPlatform/scion#3577). The bridge answers the
// JSON-RPC send with the uniform refusal before any task starts: nothing
// is sent, no topic is subscribed and no access re-check is made.
func TestExecutor_IdentityIssuerUserWithoutVisitorAccountRefused(t *testing.T) {
	const (
		issuer  = "https://accounts.example.com"
		subject = "idp-user-1"
	)
	token := buildTestJWT(t, map[string]interface{}{
		"iss":   issuer,
		"sub":   subject,
		"aud":   "https://hub-b.example.com",
		"email": "person@example.com",
	})
	hub := newFedUserHub(t, token)
	hub.set(func(h *fedUserHub) { h.meStatus = http.StatusForbidden })
	_, broker, ts := newFedUserTestServer(t, hub)

	status, body := awaitResult(t, sendAsync(t, ts, "agent1", token))
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body %s", status, body)
	}
	if got := strings.TrimSpace(body); got != federatedUserRefusalText {
		t.Errorf("body = %q, want the uniform refusal %q", got, federatedUserRefusalText)
	}
	snap := hub.snapshot()
	if snap.meCalls != 1 || snap.meFedTokens[0] != token {
		t.Errorf("auth/me calls = %d with credentials %v, want 1 with the caller's credential", snap.meCalls, snap.meFedTokens)
	}
	if len(snap.sent) != 0 || snap.createCalls != 0 || len(snap.listQueries) != 0 || len(snap.dmReadKeys) != 0 {
		t.Errorf("hub calls after the refusal: sends %d, creates %d, agent lists %d, access re-checks %d; want none",
			len(snap.sent), snap.createCalls, len(snap.listQueries), len(snap.dmReadKeys))
	}
	if brokerSubscribed(broker, projectkeys.UserTopic(fedUserProjectID, "admin@test")) ||
		brokerSubscribed(broker, projectkeys.AllUserTopic(fedUserProjectID)) {
		t.Error("subscribed to a user topic for a refused caller")
	}
}

// --- uniform answers and outages ---

// TestExecutor_FederatedUserUnexposedAgentRefusedLikeUnknown: an agent that
// is not exposed and a project that is not configured get the same failed
// task with the uniform refusal as an unknown agent, with nothing sent;
// other callers keep the JSON-RPC "agent not found" error.
func TestExecutor_FederatedUserUnexposedAgentRefusedLikeUnknown(t *testing.T) {
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	_, _, ts := newFedUserTestServer(t, hub)

	for _, target := range [][2]string{{"proj1", "agent3"}, {"nope", "agent1"}} {
		status, body := awaitResult(t, sendAsyncTo(t, ts, target[0], target[1], token))
		if status != http.StatusOK {
			t.Errorf("%s/%s: status = %d, want 200; body %s", target[0], target[1], status, body)
		}
		if !strings.Contains(body, federatedUserRefusalText) {
			t.Errorf("%s/%s: response does not carry the uniform refusal; body %s", target[0], target[1], body)
		}
		if strings.Contains(body, `"error"`) {
			t.Errorf("%s/%s: answered with a JSON-RPC error instead of a failed task; body %s", target[0], target[1], body)
		}
	}
	if n := len(hub.snapshot().sent); n != 0 {
		t.Errorf("%d sends reached the hub, want 0", n)
	}

	_, body := awaitResult(t, sendAsyncTo(t, ts, "proj1", "agent3", fedAgentToken(t)))
	if !strings.Contains(body, "agent not found") {
		t.Errorf("agent caller: body %s, want the JSON-RPC agent not found error as before", body)
	}
}

// TestExecutor_FederatedUserHubOutageIsNotARefusal: when the bridge cannot
// resolve the agent because the hub fails, the federated user gets a routing
// failure, not the access refusal.
func TestExecutor_FederatedUserHubOutageIsNotARefusal(t *testing.T) {
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	hub.set(func(h *fedUserHub) { h.adminStatus = http.StatusInternalServerError })
	_, _, ts := newFedUserTestServer(t, hub)

	status, body := awaitResult(t, sendAsync(t, ts, "agent1", token))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", status, body)
	}
	if strings.Contains(body, federatedUserRefusalText) {
		t.Fatalf("a hub failure is answered with the access refusal; body %s", body)
	}
	if !strings.Contains(body, "Failed to route message to agent") {
		t.Fatalf("response does not carry the routing failure; body %s", body)
	}
	if n := len(hub.snapshot().sent); n != 0 {
		t.Errorf("%d sends reached the hub, want 0", n)
	}
}

// --- cancel and the legacy send paths ---

// TestExecutor_FederatedUserCancelSendsNoInterrupt: tasks/cancel for a
// federated user cancels the task and sends the agent nothing; an agent
// caller still sends the interrupt.
func TestExecutor_FederatedUserCancelSendsNoInterrupt(t *testing.T) {
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	b, _, _ := newFedUserTestServer(t, hub)
	executor := NewScionExecutor(b, b.log)
	route := WithRouteInfo(context.Background(), RouteInfo{ProjectSlug: "proj1", AgentSlug: "agent1"})

	cancel := func(caller *CallerIdentity) []a2a.Event {
		execCtx := &a2asrv.ExecutorContext{TaskID: "cancel-task", StoredTask: &a2a.Task{ID: "cancel-task"}}
		var events []a2a.Event
		for ev, err := range executor.Cancel(withCallerIdentity(route, caller), execCtx) {
			if err != nil {
				t.Fatalf("Cancel: %v", err)
			}
			events = append(events, ev)
		}
		return events
	}

	fedCaller := &CallerIdentity{TokenType: "federation", RawToken: token, UserID: fedUserAccountID, Email: fedUserEmail, IssuerURL: fedUserIssuer, Subject: fedUserSubject}
	events := cancel(fedCaller)
	if len(events) != 1 {
		t.Fatalf("Cancel yielded %d events, want 1", len(events))
	}
	if ev, ok := events[0].(*a2a.TaskStatusUpdateEvent); !ok || ev.Status.State != a2a.TaskStateCanceled {
		t.Errorf("Cancel event = %#v, want a canceled status", events[0])
	}
	if n := len(hub.snapshot().sent); n != 0 {
		t.Fatalf("%d sends reached the hub for a federated user's cancel, want 0", n)
	}

	agentToken := fedAgentToken(t)
	agentCaller, err := decodeFederationToken(agentToken)
	if err != nil {
		t.Fatalf("decodeFederationToken: %v", err)
	}
	cancel(agentCaller)
	sent := hub.snapshot().sent
	if len(sent) != 1 || sent[0].Body["interrupt"] != true {
		t.Errorf("agent caller cancel sends = %v, want one interrupt as before", sent)
	}
}

// TestBridge_LegacySendsRefuseFederatedUser: the legacy send paths (one of
// them sends as the bridge admin) refuse a federated user without reaching
// the hub.
func TestBridge_LegacySendsRefuseFederatedUser(t *testing.T) {
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	b, _, _ := newFedUserTestServer(t, hub)
	caller := &CallerIdentity{TokenType: "federation", RawToken: token, UserID: fedUserAccountID, Email: fedUserEmail, IssuerURL: fedUserIssuer, Subject: fedUserSubject}
	ctx := withCallerIdentity(context.Background(), caller)
	parts := []Part{{Text: "Hello from hub A"}}

	if _, err := b.SendMessage(ctx, "proj1", "agent1", "", "", parts, true); !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("SendMessage error = %v, want ErrAgentNotFound", err)
	}
	// A follow-up to a real task of agent1 that any caller may continue
	// (it stores no caller): only the refusal of a federated user stops it.
	seedTask(t, b.store, "existing-task", "ctx-existing", "proj1", "agent1", fedUserAgentID, TaskStateWorking)
	if _, err := b.SendMessage(ctx, "proj1", "agent1", "", "existing-task", parts, false); !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("SendMessage follow-up error = %v, want ErrAgentNotFound", err)
	}
	if _, _, _, err := b.SendStreamingMessage(ctx, "proj1", "agent1", "", parts); !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("SendStreamingMessage error = %v, want ErrAgentNotFound", err)
	}
	if snap := hub.snapshot(); len(snap.sent) != 0 || snap.createCalls != 0 {
		t.Errorf("%d sends and %d agent creates reached the hub, want 0 and 0", len(snap.sent), snap.createCalls)
	}
}

// --- resubscribe: the per-event decision ---

// TestDurableHandler_FederatedUserResubscribeEvents covers the events
// SubscribeToTask streams (yieldResubscribeEvents): a federated user's
// reply is streamed while the hub grants access and replaced by the
// refusal once it does not; an agent caller's reply is streamed with no
// re-check.
func TestDurableHandler_FederatedUserResubscribeEvents(t *testing.T) {
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	b, _, _ := newFedUserTestServer(t, hub)
	h := &DurableRequestHandler{}
	h.SetBridge(b)
	ctx := WithRouteInfo(context.Background(), RouteInfo{ProjectSlug: "proj1", AgentSlug: "agent1"})
	fedCaller := &CallerIdentity{TokenType: "federation", RawToken: token, UserID: fedUserAccountID, Email: fedUserEmail, IssuerURL: fedUserIssuer, Subject: fedUserSubject}

	payload, err := json.Marshal(TaskStatusUpdate{
		TaskID: "resub-task",
		Status: TaskStatus{State: TaskStateCompleted, Message: &Message{Role: "agent", Parts: []Part{{Text: fedUserReplyText}}}},
		Final:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	events := []state.TaskEvent{{ID: 7, TaskID: "resub-task", Kind: "message", Payload: payload, Final: true}}

	stream := func(caller *CallerIdentity) ([]a2a.Event, int64, bool) {
		var got []a2a.Event
		cursor, done := h.yieldResubscribeEvents(ctx, "resub-task", caller, events, 3, func(ev a2a.Event, err error) bool {
			if err != nil {
				t.Fatalf("yield error: %v", err)
			}
			got = append(got, ev)
			return true
		})
		return got, cursor, done
	}
	text := func(ev a2a.Event) (a2a.TaskState, string) {
		su, ok := ev.(*a2a.TaskStatusUpdateEvent)
		if !ok || su.Status.Message == nil || len(su.Status.Message.Parts) == 0 {
			t.Fatalf("event = %#v, want a status update with a message", ev)
		}
		return su.Status.State, su.Status.Message.Parts[0].Text()
	}

	got, cursor, done := stream(fedCaller)
	if len(got) != 1 || cursor != 7 || !done {
		t.Fatalf("access granted: %d events, cursor %d, done %v; want 1, 7, true", len(got), cursor, done)
	}
	if _, msg := text(got[0]); msg != fedUserReplyText {
		t.Errorf("access granted: streamed %q, want the reply", msg)
	}
	if n := len(hub.snapshot().dmReadKeys); n != 1 {
		t.Errorf("access re-checks = %d, want 1", n)
	}

	hub.set(func(h *fedUserHub) { h.dmReadStatus = http.StatusForbidden })
	got, _, done = stream(fedCaller)
	if len(got) != 1 || !done {
		t.Fatalf("access revoked: %d events, done %v; want 1, true", len(got), done)
	}
	if st, msg := text(got[0]); st != a2a.TaskStateFailed || msg != federatedUserRefusalText {
		t.Errorf("access revoked: streamed %v %q, want a failed status with the uniform refusal", st, msg)
	}

	agentCaller, err := decodeFederationToken(fedAgentToken(t))
	if err != nil {
		t.Fatalf("decodeFederationToken: %v", err)
	}
	before := len(hub.snapshot().dmReadKeys)
	got, _, _ = stream(agentCaller)
	if len(got) != 1 {
		t.Fatalf("agent caller: %d events, want 1", len(got))
	}
	if _, msg := text(got[0]); msg != fedUserReplyText {
		t.Errorf("agent caller: streamed %q, want the reply", msg)
	}
	if after := len(hub.snapshot().dmReadKeys); after != before {
		t.Errorf("agent caller: access re-checks %d -> %d, want none", before, after)
	}
}

// --- an agent that is still launching ---

// fedUserLaunchingAgent is agent1 in the bridge admin list while its launch
// is still active.
func fedUserLaunchingAgent() map[string]interface{} {
	return map[string]interface{}{
		"id": fedUserAgentID, "name": "agent1", "slug": "agent1", "projectId": fedUserProjectID,
		"phase": "provisioning", "status": "provisioning",
		"launch": map[string]interface{}{"id": "launch-1", "state": "active", "active": true, "kind": "create", "step": "pod_create"},
	}
}

// fedUserTaskOutcome returns a JSON-RPC SendMessage answer's task state and
// every text in the task, in order, so that two answers can be compared
// without their task and context IDs.
func fedUserTaskOutcome(t *testing.T, body string) (string, []string) {
	t.Helper()
	var resp struct {
		Result map[string]interface{} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode JSON-RPC answer: %v; body %s", err, body)
	}
	var texts []string
	var walk func(v interface{})
	walk = func(v interface{}) {
		switch x := v.(type) {
		case map[string]interface{}:
			if text, ok := x["text"].(string); ok {
				texts = append(texts, text)
			}
			for _, k := range sortedKeys(x) {
				walk(x[k])
			}
		case []interface{}:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(resp.Result)
	state := ""
	if task, ok := resp.Result["task"].(map[string]interface{}); ok {
		if st, ok := task["status"].(map[string]interface{}); ok {
			state, _ = st["state"].(string)
		}
	}
	if st, ok := resp.Result["status"].(map[string]interface{}); ok && state == "" {
		state, _ = st["state"].(string)
	}
	return state, texts
}

func sortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestExecutor_FederatedUserLaunchingAgentRefusalIsUniform: a federated user
// the hub refuses gets the same answer for an agent that is still launching
// as for a running agent or one whose launch failed: the uniform refusal at
// once, with no launch step or outcome, no wait (no read of the agent) and
// nothing sent.
func TestExecutor_FederatedUserLaunchingAgentRefusalIsUniform(t *testing.T) {
	token := fedUserToken(t)
	refusing := func(h *fedUserHub) {
		h.sendStatus = http.StatusForbidden
		h.dmReadStatus = http.StatusForbidden
	}
	cases := []struct {
		name  string
		agent map[string]interface{}
	}{
		{"launching", fedUserLaunchingAgent()},
		{"running", map[string]interface{}{
			"id": fedUserAgentID, "name": "agent1", "slug": "agent1", "projectId": fedUserProjectID, "phase": "running", "status": "running",
		}},
		{"launch failed", map[string]interface{}{
			"id": fedUserAgentID, "name": "agent1", "slug": "agent1", "projectId": fedUserProjectID, "phase": "error", "status": "error",
			"message": "image pull back-off",
			"launch":  map[string]interface{}{"id": "launch-1", "state": "ended", "kind": "create", "error": "image_pull_failed", "endReason": "failed"},
		}},
	}
	type outcome struct {
		state string
		texts []string
	}
	outcomes := map[string]outcome{}
	for _, tc := range cases {
		hub := newFedUserHub(t, token)
		hub.set(refusing)
		hub.set(func(h *fedUserHub) { h.adminAgents = []map[string]interface{}{tc.agent} })
		b, _, ts := newFedUserTestServer(t, hub)
		b.launchPollInterval = 5 * time.Millisecond

		status, body := awaitResult(t, sendAsync(t, ts, "agent1", token))
		if status != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200; body %s", tc.name, status, body)
		}
		if !strings.Contains(body, federatedUserRefusalText) {
			t.Errorf("%s: response does not carry the uniform refusal; body %s", tc.name, body)
		}
		for _, banned := range []string{"provisioning agent", "Agent failed to start", "pod_create", "image_pull_failed", "image pull back-off"} {
			if strings.Contains(body, banned) {
				t.Errorf("%s: response contains %q; body %s", tc.name, banned, body)
			}
		}
		snap := hub.snapshot()
		if tc.name == "launching" && len(snap.sent) != 0 {
			t.Errorf("%s: %d sends reached the hub, want 0", tc.name, len(snap.sent))
		}
		if len(snap.agentGets) != 0 {
			t.Errorf("%s: the bridge read the agent %v (a launch wait), want no read", tc.name, snap.agentGets)
		}
		state, texts := fedUserTaskOutcome(t, body)
		outcomes[tc.name] = outcome{state, texts}
	}
	want := outcomes["running"]
	if want.state == "" {
		t.Fatalf("running: no task state in the answer")
	}
	for _, name := range []string{"launching", "launch failed"} {
		if got := outcomes[name]; got.state != want.state || strings.Join(got.texts, "\n") != strings.Join(want.texts, "\n") {
			t.Errorf("%s: answer (%s, %q) differs from the running agent's (%s, %q)", name, got.state, got.texts, want.state, want.texts)
		}
	}
}

// TestExecutor_FederatedUserLaunchingAgentAllowedWaits: when the hub allows
// the federated user to message an agent that is still launching, the
// bridge asks the hub first (the caller's own read of its conversation with
// the agent), then waits for the launch, then sends as the user.
func TestExecutor_FederatedUserLaunchingAgentAllowedWaits(t *testing.T) {
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	hub.set(func(h *fedUserHub) {
		h.adminAgents = []map[string]interface{}{fedUserLaunchingAgent()}
		h.agentGet = map[string]interface{}{
			"id": fedUserAgentID, "name": "agent1", "slug": "agent1", "projectId": fedUserProjectID, "phase": "running", "status": "running",
			"launch": map[string]interface{}{"id": "launch-1", "state": "ended", "kind": "create"},
		}
	})
	b, _, ts := newFedUserTestServer(t, hub)
	b.launchPollInterval = 5 * time.Millisecond

	done := sendAsync(t, ts, "agent1", token)
	sent := waitForSend(t, hub)

	snap := hub.snapshot()
	wantKey, _ := messages.DMConversationKey("agent", fedUserAgentID, "user", fedUserAccountID)
	if len(snap.dmReadKeys) != 1 || snap.dmReadKeys[0] != wantKey || snap.dmReadTokens[0] != token {
		t.Errorf("access checks before the send = %v with %v, want one read of %q with the caller's credential", snap.dmReadKeys, snap.dmReadTokens, wantKey)
	}
	if len(snap.agentGets) == 0 {
		t.Errorf("the bridge did not wait for the launch (no read of the agent)")
	}
	if dm, get := slices.Index(snap.order, "dm-read"), slices.Index(snap.order, "agent-get"); dm < 0 || get < 0 || dm > get {
		t.Errorf("hub requests in order %v, want the caller's DM read before the first agent read", snap.order)
	}
	if sent.AgentID != fedUserAgentID {
		t.Errorf("send went to agent %q, want %q", sent.AgentID, fedUserAgentID)
	}
	if sent.FedToken != token {
		t.Errorf("send carried federation credential %q, want the caller's", sent.FedToken)
	}
	sm, _ := sent.Body["structured_message"].(map[string]interface{})
	if got, want := sm["sender"], "user:"+fedUserAccountID; got != want {
		t.Errorf("sender = %v, want %q", got, want)
	}

	deliverReply(t, b)
	status, body := awaitResult(t, done)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, fedUserReplyText) {
		t.Fatalf("response does not carry the agent's reply; body %s", body)
	}
}

// TestExecutor_FederatedUserLaunchingAgentSendRefusedAfterWait: the hub
// allows the federated user at the check before the launch wait, the agent
// starts, and the hub then refuses the send (for example, the grant was
// revoked during the wait). The task ends with the uniform refusal, no reply
// is forwarded, and no launch outcome is shown. The launch progress the
// caller saw while the hub allowed it stays in the task history.
func TestExecutor_FederatedUserLaunchingAgentSendRefusedAfterWait(t *testing.T) {
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	hub.set(func(h *fedUserHub) {
		h.adminAgents = []map[string]interface{}{fedUserLaunchingAgent()}
		h.agentGet = map[string]interface{}{
			"id": fedUserAgentID, "name": "agent1", "slug": "agent1", "projectId": fedUserProjectID, "phase": "running", "status": "running",
			"launch": map[string]interface{}{"id": "launch-1", "state": "ended", "kind": "create"},
		}
		h.sendStatus = http.StatusForbidden
	})
	b, _, ts := newFedUserTestServer(t, hub)
	b.launchPollInterval = 5 * time.Millisecond

	done := sendAsync(t, ts, "agent1", token)
	waitForSend(t, hub)
	deliverReply(t, b)

	status, body := awaitResult(t, done)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", status, body)
	}
	if state, texts := fedUserTaskOutcome(t, body); state != string(a2a.TaskStateFailed) || !slices.Contains(texts, federatedUserRefusalText) {
		t.Errorf("answer (%s, %q), want failed with the uniform refusal %q", state, texts, federatedUserRefusalText)
	}
	if strings.Contains(body, fedUserReplyText) {
		t.Errorf("reply forwarded although the hub refused the send; body %s", body)
	}
	for _, banned := range []string{"Agent failed to start", "Could not confirm that the agent started", "Request cancelled while waiting"} {
		if strings.Contains(body, banned) {
			t.Errorf("response contains launch text %q; body %s", banned, body)
		}
	}
	snap := hub.snapshot()
	if len(snap.dmReadKeys) == 0 || len(snap.agentGets) == 0 || len(snap.sent) != 1 {
		t.Errorf("DM reads %v, agent reads %v, %d sends; want the check, the launch wait and one send", snap.dmReadKeys, snap.agentGets, len(snap.sent))
	}
}

// TestExecutor_FederatedUserLaunchingAgentAccessCheckError: a hub error
// other than a refusal on the check before the launch wait is a routing
// failure, not a refusal, and nothing is waited for or sent.
func TestExecutor_FederatedUserLaunchingAgentAccessCheckError(t *testing.T) {
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	hub.set(func(h *fedUserHub) {
		h.adminAgents = []map[string]interface{}{fedUserLaunchingAgent()}
		h.dmReadStatus = http.StatusInternalServerError
	})
	_, _, ts := newFedUserTestServer(t, hub)

	status, body := awaitResult(t, sendAsync(t, ts, "agent1", token))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "Failed to route message to agent") {
		t.Errorf("response does not carry the routing failure; body %s", body)
	}
	if strings.Contains(body, federatedUserRefusalText) || strings.Contains(body, "provisioning agent") {
		t.Errorf("response carries the refusal or launch progress; body %s", body)
	}
	snap := hub.snapshot()
	if len(snap.sent) != 0 || len(snap.agentGets) != 0 {
		t.Errorf("%d sends and agent reads %v, want none", len(snap.sent), snap.agentGets)
	}
}

// TestExecutor_FederatedUserLaunchFailureShowsCodeOnly: a federated user the
// hub allows, whose agent's launch fails, is told the launch outcome code
// only; the agent's status message is not shown.
func TestExecutor_FederatedUserLaunchFailureShowsCodeOnly(t *testing.T) {
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	hub.set(func(h *fedUserHub) {
		h.adminAgents = []map[string]interface{}{fedUserLaunchingAgent()}
		h.agentGet = map[string]interface{}{
			"id": fedUserAgentID, "name": "agent1", "slug": "agent1", "projectId": fedUserProjectID, "phase": "error", "status": "error",
			"message": "image pull back-off",
			"launch":  map[string]interface{}{"id": "launch-1", "state": "ended", "kind": "create", "error": "image_pull_failed", "endReason": "failed"},
		}
	})
	b, _, ts := newFedUserTestServer(t, hub)
	b.launchPollInterval = 5 * time.Millisecond

	status, body := awaitResult(t, sendAsync(t, ts, "agent1", token))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "Agent failed to start (image_pull_failed)") {
		t.Errorf("response does not carry the launch outcome code; body %s", body)
	}
	if strings.Contains(body, "image pull back-off") {
		t.Errorf("response carries the agent's status message; body %s", body)
	}
	if n := len(hub.snapshot().sent); n != 0 {
		t.Errorf("%d sends reached the hub, want 0", n)
	}
}

func TestFederatedUserLaunchFailureText(t *testing.T) {
	notRunning := &hubclient.LaunchNotRunningError{Outcome: hubclient.LaunchOutcome{Code: "image_pull_failed", Phase: "error", Message: "image pull back-off"}}
	noCode := &hubclient.LaunchNotRunningError{Outcome: hubclient.LaunchOutcome{Phase: "stopped", Message: "stopped by user"}}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"outcome code", notRunning, "Agent failed to start (image_pull_failed)"},
		{"phase only", noCode, "Agent failed to start (phase: stopped)"},
		{"cancelled", context.Canceled, "Request cancelled while waiting for the agent to start"},
		{"read error", errors.New("GET /api/v1/agents/x: 500 internal detail"), "Could not confirm that the agent started"},
	} {
		if got := federatedUserLaunchFailureText(tc.err); got != tc.want {
			t.Errorf("%s: federatedUserLaunchFailureText = %q, want %q", tc.name, got, tc.want)
		}
	}
	// Other callers keep the full text.
	if got, want := launchFailureText(notRunning), "Agent failed to start (image_pull_failed): image pull back-off"; got != want {
		t.Errorf("launchFailureText = %q, want %q", got, want)
	}
	if got, want := launchFailureText(noCode), "Agent failed to start (phase: stopped): stopped by user"; got != want {
		t.Errorf("launchFailureText (phase only) = %q, want %q", got, want)
	}
}

// Second visitor account of the per-account task tests.
const (
	fedUser2Subject   = "bob-on-hub-a"
	fedUser2Email     = "bob@corp.example.com"
	fedUser2AccountID = "22222222-3333-4444-8555-666666666666"
)

// fedUser2Token is the second visitor's user access credential, from the
// same peer hub as fedUserToken.
func fedUser2Token(t *testing.T) string {
	t.Helper()
	return buildTestJWT(t, map[string]interface{}{
		"iss":              fedUserIssuer,
		"sub":              fedUser2Subject,
		"aud":              "https://hub-b.example.com",
		"email":            fedUser2Email,
		"email_verified":   true,
		"scion_token_kind": "user",
	})
}

// TestExecutor_FederatedUsersLaunchingAgentTasksKeptPerAccount: two
// visitor accounts message agent1 while it is still launching. In the
// default (plugin) mode, where replies without a2aTaskId are correlated
// through the local cache, each task is registered for its own account
// once the launch ends: each account's reply reaches only that account's
// task, and the second account can neither read nor cancel the first
// account's task.
func TestExecutor_FederatedUsersLaunchingAgentTasksKeptPerAccount(t *testing.T) {
	const (
		firstReplyText  = "reply from agent1 to the first visitor"
		secondReplyText = "reply from agent1 to the second visitor"
	)
	token1, token2 := fedUserToken(t), fedUser2Token(t)
	hub := newFedUserHub(t, token1)
	hub.set(func(h *fedUserHub) {
		h.otherUsers = map[string]string{token2: fedUser2AccountID}
		h.adminAgents = []map[string]interface{}{fedUserLaunchingAgent()}
		h.agentGet = map[string]interface{}{
			"id": fedUserAgentID, "name": "agent1", "slug": "agent1", "projectId": fedUserProjectID, "phase": "running", "status": "running",
			"launch": map[string]interface{}{"id": "launch-1", "state": "ended", "kind": "create"},
		}
	})
	b, _, ts := newFedUserTestServer(t, hub)
	b.launchPollInterval = 5 * time.Millisecond

	waitForSends := func(n int) []fedUserHubSend {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if s := hub.snapshot(); len(s.sent) >= n {
				return s.sent
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("hub received fewer than %d message sends", n)
		return nil
	}

	done1 := sendAsync(t, ts, "agent1", token1)
	waitForSends(1)
	done2 := sendAsync(t, ts, "agent1", token2)
	sent := waitForSends(2)

	if len(hub.snapshot().agentGets) == 0 {
		t.Fatalf("the bridge did not wait for the launch (no read of the agent)")
	}
	for i, want := range []struct{ token, sender string }{
		{token1, "user:" + fedUserAccountID},
		{token2, "user:" + fedUser2AccountID},
	} {
		sm, _ := sent[i].Body["structured_message"].(map[string]interface{})
		if sent[i].FedToken != want.token || sm["sender"] != want.sender {
			t.Errorf("send %d: sender %v (own credential: %v), want %q with its own credential", i, sm["sender"], sent[i].FedToken == want.token, want.sender)
		}
	}

	// Both tasks are active on agent1, the first account's first; each is
	// registered for its own account.
	aKey := agentKey(fedUserProjectID, "agent1")
	b.tasksMu.RLock()
	taskIDs := append([]string(nil), b.agentTasks[aKey]...)
	owners := make([]string, len(taskIDs))
	for i, id := range taskIDs {
		owners[i] = b.activeTasks[id].federatedUserID
	}
	b.tasksMu.RUnlock()
	if len(taskIDs) != 2 {
		t.Fatalf("active tasks on agent1 = %v, want the two visitors' tasks", taskIDs)
	}
	if owners[0] != fedUserAccountID || owners[1] != fedUser2AccountID {
		t.Errorf("tasks registered for accounts %q, want [%q %q]", owners, fedUserAccountID, fedUser2AccountID)
	}
	firstTask := taskIDs[0]

	// Each account's reply correlates to that account's task.
	reply := &messages.StructuredMessage{Sender: "agent:agent1", Type: messages.TypeInstruction, Msg: "hi"}
	for i, userID := range []string{fedUserAccountID, fedUser2AccountID} {
		got, err := b.correlateToTask(context.Background(), fedUserProjectID, "agent1", projectkeys.UserTopic(fedUserProjectID, userID), reply)
		if err != nil || got != taskIDs[i] {
			t.Errorf("reply on account %s's topic correlated to (%q, %v), want its task %q", userID, got, err, taskIDs[i])
		}
	}

	// The second account cannot read or cancel the first account's task.
	path := "/projects/proj1/agents/agent1/jsonrpc"
	auth2 := map[string]string{"Authorization": "Bearer " + token2}
	for _, method := range []string{"GetTask", "CancelTask"} {
		payload := `{"jsonrpc":"2.0","id":"other-account","method":"` + method + `","params":{"id":"` + firstTask + `"}}`
		status, raw := doRPCRaw(t, ts, path, payload, auth2)
		var resp map[string]interface{}
		_ = json.Unmarshal(raw, &resp)
		if status != http.StatusOK || resp["error"] == nil || resp["result"] != nil {
			t.Errorf("%s by the second account on the first account's task: status %d, body %s; want a JSON-RPC error", method, status, raw)
		}
	}
	b.tasksMu.RLock()
	_, firstActive := b.activeTasks[firstTask]
	b.tasksMu.RUnlock()
	if !firstActive {
		t.Errorf("the first account's task %q is no longer active after the second account's cancel", firstTask)
	}

	// The first account's reply arrives first, while both tasks are active.
	deliverReplyTo(t, b, fedUserAccountID, firstReplyText)
	deliverReplyTo(t, b, fedUser2AccountID, secondReplyText)

	for _, tc := range []struct {
		name       string
		done       <-chan [2]interface{}
		own, other string
	}{
		{"first account", done1, firstReplyText, secondReplyText},
		{"second account", done2, secondReplyText, firstReplyText},
	} {
		status, body := awaitResult(t, tc.done)
		if status != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200; body %s", tc.name, status, body)
		}
		if strings.Contains(body, tc.other) {
			t.Errorf("%s: received the reply addressed to the other account; body %s", tc.name, body)
		}
		if !strings.Contains(body, tc.own) {
			t.Errorf("%s: response does not carry its own reply; body %s", tc.name, body)
		}
	}
}
