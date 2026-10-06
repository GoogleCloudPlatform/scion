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

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- helpers ---------------------------------------------------------------

// markEdgeBackfillComplete records the delegation edge backfill marker, after
// which every agent caller needs an active delegation edge.
func markEdgeBackfillComplete(t *testing.T, s store.Store) {
	t.Helper()
	_, err := s.UpsertHubSetting(context.Background(), "migration_delegation_edge_backfill_v1",
		json.RawMessage(`{"schema_version":1,"completed":true}`), "migration", 0, "seeded")
	require.NoError(t, err)
}

// addProjectEdge records an active delegation edge in the project scope and
// returns its ID.
func addProjectEdge(t *testing.T, s store.Store, delegatorType, delegatorID, delegateID, projectID string) string {
	t.Helper()
	id := uuid.NewString()
	require.NoError(t, s.CreateDelegationEdge(context.Background(), &store.DelegationEdge{
		ID:            id,
		DelegatorType: delegatorType,
		DelegatorID:   delegatorID,
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    delegateID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       projectID,
		Role:          string(AgentRoleFull),
		Active:        true,
	}))
	return id
}

// revokeDelegateEdges deactivates every active delegation edge of the agent
// delegateID, attributed to a test op ID, and fails the test when there was
// none to deactivate.
func revokeDelegateEdges(t *testing.T, s store.Store, delegateID string) {
	t.Helper()
	n, err := s.DeactivateDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, delegateID,
		store.Deactivation{Cause: store.EdgeDeactivationAgentHardDelete, OpID: "test-revoke-" + uuid.NewString()})
	require.NoError(t, err)
	require.Positive(t, n, "an active edge to revoke")
}

// decodeTargetAPIError decodes an error response body.
func decodeTargetAPIError(t *testing.T, rec *httptest.ResponseRecorder) APIError {
	t.Helper()
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "body: %s", rec.Body.String())
	return resp.Error
}

// assertAgentTargetDenied asserts the neutral 403 for an agent action and
// whether it carries the delegation-ceiling detail (its only detail).
func assertAgentTargetDenied(t *testing.T, rec *httptest.ResponseRecorder, ceiling bool) {
	t.Helper()
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	apiErr := decodeTargetAPIError(t, rec)
	assert.Equal(t, ErrCodeForbidden, apiErr.Code)
	assert.Equal(t, agentTargetDenyMessage, apiErr.Message)
	if ceiling {
		assert.Equal(t, map[string]interface{}{"denied_by": "delegation_ceiling"}, apiErr.Details)
	} else {
		assert.Empty(t, apiErr.Details)
	}
}

// requestAsIdentity serves a request through the route mux with identity in
// the context (as both the caller and, for users, the user identity).
func requestAsIdentity(t *testing.T, srv *Server, identity Identity, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	ctx := contextWithIdentity(req.Context(), identity)
	if user, ok := identity.(UserIdentity); ok {
		ctx = context.WithValue(ctx, userContextKey{}, user)
	}
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req.WithContext(ctx))
	return rec
}

func hubMemberUser(t *testing.T, s store.Store, id string) *store.User {
	t.Helper()
	u := &store.User{
		ID: tid(id), Email: id + "@target.test", DisplayName: id,
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(context.Background(), u))
	ensureHubMembership(context.Background(), s, u.ID)
	return u
}

func authUser(u *store.User) *AuthenticatedUser {
	return NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, "api")
}

// connectFixtureBroker marks the fixture broker connected and points target
// at it, so a PTY preflight can reach its 200.
func connectFixtureBroker(t *testing.T, f *bypassAgentsFixture, target *store.Agent) {
	t.Helper()
	target.RuntimeBrokerID = f.broker.ID
	require.NoError(t, f.store.UpdateAgent(context.Background(), target))
	require.NotNil(t, f.srv.controlChannel)
	f.srv.controlChannel.mu.Lock()
	f.srv.controlChannel.connections[f.broker.ID] = &BrokerConnection{brokerID: f.broker.ID, streams: map[string]*StreamProxy{}}
	f.srv.controlChannel.mu.Unlock()
	t.Cleanup(func() {
		f.srv.controlChannel.mu.Lock()
		delete(f.srv.controlChannel.connections, f.broker.ID)
		f.srv.controlChannel.mu.Unlock()
	})
}

// --- denial message --------------------------------------------------------

// TestAgentTargetAction_DenialMessage pins the neutral 403 for a user caller
// without the permission, on the action, delete and project-scoped action
// paths, for a plain user and a scoped UAT.
func TestAgentTargetAction_DenialMessage(t *testing.T) {
	f := bypassAgentsSetup(t)
	outsider := hubMemberUser(t, f.store, "target-outsider")
	// The agent owner holds lifecycle and delete on its agent; a token
	// scoped to agent:read carries neither.
	readOnlyUAT := NewScopedUserIdentity(authUser(f.owner), f.proj.ID, []string{"agent:read"})

	paths := []struct {
		name, method, path string
	}{
		{"action", http.MethodPost, "/api/v1/agents/" + f.sibling.ID + "/stop"},
		{"delete", http.MethodDelete, "/api/v1/agents/" + f.sibling.ID},
		{"project action", http.MethodPost, "/api/v1/projects/" + f.proj.ID + "/agents/" + f.sibling.ID + "/stop"},
	}
	callers := []struct {
		name     string
		identity Identity
	}{
		{"plain user", authUser(outsider)},
		{"scoped UAT", readOnlyUAT},
	}
	for _, c := range callers {
		for _, p := range paths {
			t.Run(c.name+"/"+p.name, func(t *testing.T) {
				rec := requestAsIdentity(t, f.srv, c.identity, p.method, p.path, nil)
				assertAgentTargetDenied(t, rec, false)
			})
		}
	}
}

// --- delegation ceiling detail ---------------------------------------------

// TestAgentTargetAction_CeilingDeniedByDetail pins that a 403 produced by the
// delegation ceiling carries details.denied_by=delegation_ceiling and nothing
// else, that other denials carry no such detail, and that concealed 404s stay
// bare.
func TestAgentTargetAction_CeilingDeniedByDetail(t *testing.T) {
	f := bypassAgentsSetup(t)
	markEdgeBackfillComplete(t, f.store)
	stop := "/api/v1/agents/" + f.child.ID + "/stop"

	t.Run("live chain passes the gate", func(t *testing.T) {
		addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.owner.ID, f.caller.ID, f.proj.ID)
		rec := f.asAgent(t, http.MethodPost, stop, nil, ScopeAgentLifecycle)
		assert.NotEqual(t, http.StatusForbidden, rec.Code, rec.Body.String())
		revokeDelegateEdges(t, f.store, f.caller.ID)
	})

	t.Run("no active edge is a ceiling denial", func(t *testing.T) {
		rec := f.asAgent(t, http.MethodPost, stop, nil, ScopeAgentLifecycle)
		assertAgentTargetDenied(t, rec, true)
		rec = f.asAgent(t, http.MethodDelete, "/api/v1/agents/"+f.child.ID, nil, ScopeAgentLifecycle)
		assertAgentTargetDenied(t, rec, true)
	})

	t.Run("authorize sites carry the same single ceiling detail", func(t *testing.T) {
		// The generic authorize 403 keeps its resource_type and
		// denied_action details and adds only denied_by.
		addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.owner.ID, f.caller.ID, f.proj.ID)
		foreign := foreignOwnedAgent(t, f, "ceiling-authorize")
		caller := &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: f.caller.ID},
			ProjectID: f.proj.ID,
			Scopes:    []AgentTokenScope{ScopeProjectRead, ScopeAgentLifecycle},
		}}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+foreign.ID+"/stop", nil)
		req = req.WithContext(contextWithIdentity(req.Context(), caller))
		rec := httptest.NewRecorder()
		require.False(t, f.srv.authorize(rec, req, agentResource(foreign), ActionLifecycle))
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		apiErr := decodeTargetAPIError(t, rec)
		assert.Equal(t, ErrCodeForbidden, apiErr.Code)
		assert.Equal(t, map[string]interface{}{
			"resource_type": "agent",
			"denied_action": string(ActionLifecycle),
			"denied_by":     "delegation_ceiling",
		}, apiErr.Details)
	})

	t.Run("cross-project concealment stays a bare 404", func(t *testing.T) {
		rec := f.asAgent(t, http.MethodGet, "/api/v1/agents/"+f.stranger.ID, nil)
		require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		assert.Empty(t, decodeTargetAPIError(t, rec).Details)
	})

	t.Run("scope pre-gate denial has no ceiling detail", func(t *testing.T) {
		rec := f.asAgent(t, http.MethodPost, stop, nil)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "denied_by")
	})
}

// --- PTY re-authorization --------------------------------------------------

// TestAgentPTY_ReauthorizesEachConnection pins that every PTY preflight is
// authorized against current state: removing the relationship or the
// delegation edge that granted attach turns the next preflight into a 403.
func TestAgentPTY_ReauthorizesEachConnection(t *testing.T) {
	t.Run("user role binding", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		connectFixtureBroker(t, f, f.sibling)
		path := "/api/v1/agents/" + f.sibling.ID + "/pty"
		user := setupNonAdminUser(t, f.store, []string{"agent.read", "agent.attach"})

		rec := requestAsIdentity(t, f.srv, user, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		bindings, err := f.store.ListRoleBindingsForPrincipal(context.Background(), store.RoleBindingPrincipalUser, user.ID())
		require.NoError(t, err)
		require.NotEmpty(t, bindings)
		for _, b := range bindings {
			require.NoError(t, f.store.DeleteRoleBinding(context.Background(), b.ID))
		}

		rec = requestAsIdentity(t, f.srv, user, http.MethodGet, path, nil)
		assertAgentTargetDenied(t, rec, false)
	})

	t.Run("agent ancestor", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		markEdgeBackfillComplete(t, f.store)
		connectFixtureBroker(t, f, f.child)
		path := "/api/v1/agents/" + f.child.ID + "/pty"
		addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.owner.ID, f.caller.ID, f.proj.ID)

		rec := f.asAgent(t, http.MethodGet, path, nil, ScopeAgentLifecycle)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		revokeDelegateEdges(t, f.store, f.caller.ID)
		rec = f.asAgent(t, http.MethodGet, path, nil, ScopeAgentLifecycle)
		assertAgentTargetDenied(t, rec, true)
	})
}

// connectLoopbackFixtureBroker attaches target to the fixture broker and
// registers a control-channel connection backed by a loopback WebSocket, so
// an authorized PTY upgrade can open its broker stream. It returns the
// broker side of the loopback.
func connectLoopbackFixtureBroker(t *testing.T, f *bypassAgentsFixture, target *store.Agent) *websocket.Conn {
	t.Helper()
	target.RuntimeBrokerID = f.broker.ID
	require.NoError(t, f.store.UpdateAgent(context.Background(), target))
	require.NotNil(t, f.srv.controlChannel)
	hubSide, brokerSide := lifecycleWebSocketPair(t)
	f.srv.controlChannel.mu.Lock()
	f.srv.controlChannel.connections[f.broker.ID] = &BrokerConnection{
		brokerID: f.broker.ID,
		conn:     wsprotocol.NewConnection(hubSide, wsprotocol.ConnectionConfig{WriteWait: time.Second}),
		streams:  map[string]*StreamProxy{},
	}
	f.srv.controlChannel.mu.Unlock()
	t.Cleanup(func() {
		f.srv.controlChannel.mu.Lock()
		delete(f.srv.controlChannel.connections, f.broker.ID)
		f.srv.controlChannel.mu.Unlock()
	})
	return brokerSide
}

// dialAgentPTY opens a PTY WebSocket to path on hub with the calling
// agent's token and returns the connection (nil on a failed handshake) and
// the handshake response status.
func dialAgentPTY(t *testing.T, hub *httptest.Server, f *bypassAgentsFixture, path string, scopes ...AgentTokenScope) (*websocket.Conn, int) {
	t.Helper()
	header := http.Header{}
	header.Set("X-Scion-Agent-Token", f.token(t, scopes...))
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, resp, err := dialer.Dial("ws"+strings.TrimPrefix(hub.URL, "http")+path, header)
	require.NotNil(t, resp, "dial error: %v", err)
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		require.ErrorIs(t, err, websocket.ErrBadHandshake)
		return nil, resp.StatusCode
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, resp.StatusCode
}

// TestAgentPTY_AgentAncestorUpgrade pins the WebSocket upgrade for an agent
// caller: with a live delegation edge the ancestor's upgrade switches
// protocols (101) and opens the broker stream; once the edge is deactivated
// the next upgrade is refused with 403.
func TestAgentPTY_AgentAncestorUpgrade(t *testing.T) {
	f := bypassAgentsSetup(t)
	markEdgeBackfillComplete(t, f.store)
	broker := connectLoopbackFixtureBroker(t, f, f.child)
	hub := httptest.NewServer(f.srv.Handler())
	t.Cleanup(hub.Close)
	path := "/api/v1/agents/" + f.child.ID + "/pty"
	addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.owner.ID, f.caller.ID, f.proj.ID)

	conn, status := dialAgentPTY(t, hub, f, path, ScopeAgentLifecycle)
	require.Equal(t, http.StatusSwitchingProtocols, status)
	require.NoError(t, broker.SetReadDeadline(time.Now().Add(5*time.Second)))
	var open wsprotocol.StreamOpenMessage
	require.NoError(t, broker.ReadJSON(&open))
	assert.Equal(t, wsprotocol.TypeStreamOpen, open.Type)
	assert.Equal(t, f.child.Slug, open.Slug)
	require.NoError(t, conn.Close())

	revokeDelegateEdges(t, f.store, f.caller.ID)
	conn, status = dialAgentPTY(t, hub, f, path, ScopeAgentLifecycle)
	assert.Nil(t, conn)
	assert.Equal(t, http.StatusForbidden, status)
}

// --- HTTP matrix -----------------------------------------------------------

// foreignOwnedAgent creates an agent in the fixture project owned by another
// user, on which the fixture owner (the caller's delegator) holds nothing.
func foreignOwnedAgent(t *testing.T, f *bypassAgentsFixture, name string) *store.Agent {
	t.Helper()
	other := hubMemberUser(t, f.store, name+"-owner")
	a := &store.Agent{
		ID: tid(name), Slug: tid(name), Name: name, ProjectID: f.proj.ID,
		Phase: "running", CreatedBy: other.ID, OwnerID: other.ID, Ancestry: []string{other.ID},
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), a))
	return a
}

// TestAgentTargetAction_HTTPMatrix pins the lifecycle, attach and delete
// gates for an agent caller with a live delegation chain: each exact
// permission must be held by the caller and by its delegator on the target.
// The delegator owns the sibling and is an ancestor of the child, and holds
// nothing on an agent owned by another user.
func TestAgentTargetAction_HTTPMatrix(t *testing.T) {
	f := bypassAgentsSetup(t)
	markEdgeBackfillComplete(t, f.store)
	addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.owner.ID, f.caller.ID, f.proj.ID)
	foreign := foreignOwnedAgent(t, f, "matrix-foreign")
	exec := map[string]any{"command": []string{"true"}}

	type row struct {
		name    string
		method  string
		suffix  string
		body    interface{}
		target  *store.Agent
		allowed bool
	}
	rows := []row{
		{"lifecycle on descendant", http.MethodPost, "/stop", nil, f.child, true},
		{"lifecycle on sibling", http.MethodPost, "/stop", nil, f.sibling, true},
		{"lifecycle on foreign", http.MethodPost, "/stop", nil, foreign, false},
		{"project lifecycle on foreign", http.MethodPost, "", nil, foreign, false},
		{"attach (exec) on descendant", http.MethodPost, "/exec", exec, f.child, true},
		{"attach (exec) on foreign", http.MethodPost, "/exec", exec, foreign, false},
		{"attach (env) on foreign", http.MethodPost, "/env", map[string]any{}, foreign, false},
		{"reincarnate on foreign", http.MethodPost, "/reincarnate", map[string]any{}, foreign, false},
		{"delete on foreign", http.MethodDelete, "", nil, foreign, false},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			path := "/api/v1/agents/" + r.target.ID + r.suffix
			if strings.HasPrefix(r.name, "project ") {
				path = "/api/v1/projects/" + f.proj.ID + "/agents/" + r.target.ID + "/stop"
			}
			rec := f.asAgent(t, r.method, path, r.body, ScopeAgentLifecycle)
			if r.allowed {
				assert.NotEqual(t, http.StatusForbidden, rec.Code, rec.Body.String())
			} else {
				assertAgentTargetDenied(t, rec, true)
			}
		})
	}
}

// --- full message and log history ------------------------------------------

// TestAgentFullHistory_Messages pins that full message history on
// GET /agents/{id}/messages requires agent.attach on the agent: the owner and
// a UAT carrying agent:attach see every message; agent:read or
// agent:lifecycle tokens, a project admin (lifecycle without attach) and a
// read-only member see only messages they participate in. Capabilities
// report attach exactly when the full-history decision allows.
func TestAgentFullHistory_Messages(t *testing.T) {
	srv, s, alice, bob, agentID := setupMessagePrivacyTest(t)
	ctx := context.Background()
	agent, err := s.GetAgent(ctx, agentID)
	require.NoError(t, err)

	dave := &store.User{
		ID: tid("msg-dave"), Email: "dave@msg.test", DisplayName: "Dave",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, dave))
	createTestUserWithProjectRole(t, s, dave.ID, dave.Email, agent.ProjectID, store.ProjectRoleAdmin)

	aliceID := authUser(alice)
	cases := []struct {
		name     string
		identity UserIdentity
		want     int
	}{
		{"owner", aliceID, 3},
		{"UAT agent:read", NewScopedUserIdentity(aliceID, agent.ProjectID, []string{"agent:read"}), 2},
		{"UAT agent:read+lifecycle", NewScopedUserIdentity(aliceID, agent.ProjectID, []string{"agent:read", "agent:lifecycle"}), 2},
		{"UAT agent:read+attach", NewScopedUserIdentity(aliceID, agent.ProjectID, []string{"agent:read", "agent:attach"}), 3},
		{"project admin", authUser(dave), 0},
		{"read-only member", authUser(bob), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := requestAsIdentity(t, srv, tc.identity, http.MethodGet, "/api/v1/agents/"+agentID+"/messages", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var result store.ListResult[store.Message]
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
			assert.Equal(t, tc.want, result.TotalCount)

			full := srv.agentFullHistoryDecision(ctx, tc.identity, agent).Allowed
			assert.Equal(t, tc.want == 3, full)
			caps := srv.authzService.ComputeCapabilities(ctx, tc.identity, agentResource(agent))
			assert.Equal(t, full, capabilityAllows(caps, ActionAttach), "capability/decision parity")
		})
	}

	t.Run("project admin keeps lifecycle", func(t *testing.T) {
		caps := srv.authzService.ComputeCapabilities(ctx, authUser(dave), agentResource(agent))
		assert.True(t, capabilityAllows(caps, ActionLifecycle))
		assert.False(t, capabilityAllows(caps, ActionAttach))
	})
}

// syncRecorder is a flushable response writer safe for concurrent reads.
type syncRecorder struct {
	mu     sync.Mutex
	header http.Header
	body   bytes.Buffer
	code   int
}

func (r *syncRecorder) Header() http.Header { return r.header }
func (r *syncRecorder) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.code == 0 {
		r.code = code
	}
}
func (r *syncRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.body.Write(p)
}
func (r *syncRecorder) Flush() {}
func (r *syncRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.String()
}

// streamMessagesAs opens GET /agents/{id}/messages/stream as identity,
// publishes a non-participant message and a marker message addressed to
// marker until the marker arrives, and returns the stream body.
func streamMessagesAs(t *testing.T, srv *Server, events *ChannelEventPublisher, identity UserIdentity, agentID, marker string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agentID+"/messages/stream", nil)
	rctx := context.WithValue(contextWithIdentity(ctx, identity), userContextKey{}, identity)
	rec := &syncRecorder{header: http.Header{}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.mux.ServeHTTP(rec, req.WithContext(rctx))
	}()

	subject := "agent." + agentID + ".message"
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(rec.String(), "marker-message") {
		events.PublishRaw(subject, UserMessageEvent{
			ID: uuid.NewString(), SenderID: tid("stream-carol"), RecipientID: agentID, Msg: "carol-message",
		})
		events.PublishRaw(subject, UserMessageEvent{
			ID: uuid.NewString(), SenderID: agentID, RecipientID: marker, Msg: "marker-message",
		})
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	body := rec.String()
	require.Contains(t, body, "marker-message", "stream never delivered the marker")
	return body
}

// TestAgentFullHistory_MessagesStream pins the same rule on the message
// stream: agent.attach holders receive every message, other readers only
// messages they participate in.
func TestAgentFullHistory_MessagesStream(t *testing.T) {
	srv, s, alice, bob, agentID := setupMessagePrivacyTest(t)
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	srv.events = events
	agent, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err)

	t.Run("owner sees every message", func(t *testing.T) {
		body := streamMessagesAs(t, srv, events, authUser(alice), agentID, alice.ID)
		assert.Contains(t, body, "carol-message")
	})
	t.Run("UAT without agent:attach sees only its own", func(t *testing.T) {
		uat := NewScopedUserIdentity(authUser(alice), agent.ProjectID, []string{"agent:read", "agent:lifecycle"})
		body := streamMessagesAs(t, srv, events, uat, agentID, alice.ID)
		assert.NotContains(t, body, "carol-message")
	})
	t.Run("read-only member sees only its own", func(t *testing.T) {
		body := streamMessagesAs(t, srv, events, authUser(bob), agentID, bob.ID)
		assert.NotContains(t, body, "carol-message")
	})
}

// TestAgentFullHistory_MessageLogs pins the rule on the message-log path for
// agent callers, which have no user identity to filter by: an agent holding
// agent.attach on the target (which for an agent needs the lifecycle scope)
// passes to the log query, and an agent with read alone is denied.
func TestAgentFullHistory_MessageLogs(t *testing.T) {
	f := bypassAgentsSetup(t)
	f.srv.logQueryService = nil
	path := "/api/v1/agents/" + f.child.ID + "/message-logs"

	rec := f.asAgent(t, http.MethodGet, path, nil, ScopeAgentLifecycle)
	assert.Equal(t, http.StatusNotImplemented, rec.Code, "attach holder reaches the log query: %s", rec.Body.String())

	rec = f.asAgent(t, http.MethodGet, path, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "read without attach: %s", rec.Body.String())
}
