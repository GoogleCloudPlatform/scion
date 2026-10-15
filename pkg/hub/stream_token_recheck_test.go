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
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// openTailLogQuerier records the options of every query and tail, and
// serves tails that stay open until the request ends.
type openTailLogQuerier struct {
	mu    sync.Mutex
	query []LogQueryOptions
	tail  []LogQueryOptions
}

func (q *openTailLogQuerier) Query(_ context.Context, opts LogQueryOptions) (*LogQueryResult, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.query = append(q.query, opts)
	return &LogQueryResult{}, nil
}

func (q *openTailLogQuerier) Tail(_ context.Context, opts LogQueryOptions) (<-chan CloudLogEntry, func(), error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.tail = append(q.tail, opts)
	return make(chan CloudLogEntry), func() {}, nil
}

func (q *openTailLogQuerier) GCPProjectID() string { return "gcp-proj" }
func (q *openTailLogQuerier) Close() error         { return nil }

func (q *openTailLogQuerier) tails() []LogQueryOptions {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]LogQueryOptions(nil), q.tail...)
}

func (q *openTailLogQuerier) queries() []LogQueryOptions {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]LogQueryOptions(nil), q.query...)
}

// tokenStreamFixture is a test server with an open log tail and an event
// publisher, one project with one agent, and the stream routes of both.
type tokenStreamFixture struct {
	srv     *Server
	store   store.Store
	logs    *openTailLogQuerier
	agent   *store.Agent
	streams []tokenStream
}

// tokenStream is one stream route and the selector a token needs on it.
type tokenStream struct {
	name     string
	path     string
	selector string
}

func newTokenStreamFixture(t *testing.T) *tokenStreamFixture {
	t.Helper()
	srv, s := testServer(t)
	logs := &openTailLogQuerier{}
	srv.logQueryService = logs
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	srv.events = events
	agent := createTestAgent(t, s)
	byID := "/api/v1/agents/" + agent.ID
	inProject := "/api/v1/projects/" + agent.ProjectID + "/agents/" + agent.ID
	return &tokenStreamFixture{
		srv: srv, store: s, logs: logs, agent: agent,
		streams: []tokenStream{
			{"agent cloud logs", byID + "/cloud-logs/stream", "agent:read"},
			{"project agent cloud logs", inProject + "/cloud-logs/stream", "agent:read"},
			{"agent message logs", byID + "/message-logs/stream", "agent:read"},
			{"project agent message logs", inProject + "/message-logs/stream", "agent:read"},
			{"agent messages", byID + "/messages/stream", "agent:read"},
			{"project message logs", "/api/v1/projects/" + agent.ProjectID + "/message-logs/stream", "project:read"},
		},
	}
}

// superAdmin creates an active super-admin hub member.
func (f *tokenStreamFixture) superAdmin(t *testing.T, name string) string {
	t.Helper()
	id := tid(name)
	createTestUserWithRole(t, f.store, id, id+"@test.com", "member", store.SystemRoleSuperAdmin)
	ensureHubMembership(context.Background(), f.store, id)
	return id
}

// openedRecorder is an httptest.ResponseRecorder that closes opened on
// its first Flush: a stream handler flushes its headers once the request
// is authorized and the stream is set up.
type openedRecorder struct {
	*httptest.ResponseRecorder
	once   sync.Once
	opened chan struct{}
}

func (r *openedRecorder) Flush() {
	r.ResponseRecorder.Flush()
	r.once.Do(func() { close(r.opened) })
}

// openStream starts the stream at path with the given Authorization value
// and waits until the handler has flushed its headers (the stream is open)
// or returned. It returns a channel closed when the handler returns, the
// recorder (read it only after the channel is closed), and a cancel for
// the request.
func openStream(t *testing.T, srv *Server, path, auth string) (chan struct{}, *httptest.ResponseRecorder, context.CancelFunc) {
	t.Helper()
	reqCtx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(reqCtx)
	req.Header.Set("Authorization", auth)
	rec := &openedRecorder{ResponseRecorder: httptest.NewRecorder(), opened: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		srv.Handler().ServeHTTP(rec, req)
		close(done)
	}()
	select {
	case <-rec.opened:
	case <-done:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("stream did not open")
	}
	return done, rec.ResponseRecorder, cancel
}

// shortenStreamRecheck sets the stream re-check interval to 20ms for the
// test.
func shortenStreamRecheck(t *testing.T) {
	t.Helper()
	prev := streamCredentialRecheckInterval
	streamCredentialRecheckInterval = 20 * time.Millisecond
	t.Cleanup(func() { streamCredentialRecheckInterval = prev })
}

// requireStreamStaysOpen requires the stream to keep running for ten
// re-check intervals.
func requireStreamStaysOpen(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
		t.Fatal("stream ended while the credential still validates")
	case <-time.After(10 * streamCredentialRecheckInterval):
	}
}

func requireStreamEnds(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stream did not end")
	}
}

// runTokenStreamEnds opens every fixture stream with a fresh super-admin's
// hub token carrying the stream's selector, requires it to stay open, applies
// change, and requires the stream to end with streamCredentialEndedEvent.
// With expiry set the token expires on its own after that long, and the
// stream is not first required to stay open.
func runTokenStreamEnds(t *testing.T, cause string, expiry time.Duration, change func(t *testing.T, f *tokenStreamFixture, userID, tokenID string)) {
	shortenStreamRecheck(t)
	f := newTokenStreamFixture(t)
	for i, st := range f.streams {
		t.Run(st.name, func(t *testing.T) {
			userID := f.superAdmin(t, "ts-"+cause+"-"+string(rune('a'+i)))
			params := CreateTokenParams{
				UserID: userID, Name: "ts-" + tid(cause), Boundary: hubBoundary(), Scopes: []string{st.selector},
			}
			if expiry > 0 {
				exp := time.Now().Add(expiry)
				params.ExpiresAt = &exp
			}
			key, token, err := f.srv.uatService.CreateTokenWithParams(rs4MintContext(userID), params)
			require.NoError(t, err)

			done, rec, cancel := openStream(t, f.srv, st.path, "Bearer "+key)
			defer cancel()
			if expiry == 0 {
				requireStreamStaysOpen(t, done)
			}
			change(t, f, userID, token.ID)
			requireStreamEnds(t, done)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			body := rec.Body.String()
			require.Contains(t, body, streamCredentialEndedEvent)
			assert.Equal(t, streamCredentialEndedEvent, body[len(body)-len(streamCredentialEndedEvent):],
				"the event is the last thing the stream writes")
		})
	}
}

// TestTokenStream_EndsAfterRevocation requires every agent and project
// stream opened by a user access token to end with
// streamCredentialEndedEvent once the token is revoked.
func TestTokenStream_EndsAfterRevocation(t *testing.T) {
	runTokenStreamEnds(t, "revoked", 0, func(t *testing.T, f *tokenStreamFixture, userID, tokenID string) {
		require.NoError(t, f.srv.uatService.RevokeToken(rs4MintContext(userID), userID, tokenID))
	})
}

// TestTokenStream_EndsAfterExpiry requires every agent and project stream
// opened by a user access token to end with streamCredentialEndedEvent once
// the token expires.
func TestTokenStream_EndsAfterExpiry(t *testing.T) {
	runTokenStreamEnds(t, "expired", 3*time.Second, func(*testing.T, *tokenStreamFixture, string, string) {})
}

// TestTokenStream_EndsAfterUserSuspended requires every agent and project
// stream opened by a user access token to end once the token's user is
// suspended.
func TestTokenStream_EndsAfterUserSuspended(t *testing.T) {
	runTokenStreamEnds(t, "suspended", 0, func(t *testing.T, f *tokenStreamFixture, userID, _ string) {
		setUserStatus(t, f.store, userID, store.UserStatusSuspended)
	})
}

// TestTokenStream_EndsAfterPermissionRemoved requires every agent and
// project stream opened by a user access token to end once the token's
// user loses the role that granted the permission the stream was admitted
// with.
func TestTokenStream_EndsAfterPermissionRemoved(t *testing.T) {
	runTokenStreamEnds(t, "denied", 0, func(t *testing.T, f *tokenStreamFixture, userID, _ string) {
		ctx := context.Background()
		rd, err := f.store.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
		require.NoError(t, err)
		bindings, err := f.store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
		require.NoError(t, err)
		for _, b := range bindings {
			if b.RoleDefinitionID == rd.ID {
				require.NoError(t, f.store.DeleteRoleBinding(ctx, b.ID))
			}
		}
	})
}

// TestTokenStream_ValidCredentialKeepsStream requires every agent and
// project stream to keep running past many re-check intervals for a user
// access token that still validates, for a session and for a dev
// credential; a session is not re-checked, so it keeps its stream even
// after its user loses the role.
func TestTokenStream_ValidCredentialKeepsStream(t *testing.T) {
	shortenStreamRecheck(t)
	f := newTokenStreamFixture(t)
	for i, st := range f.streams {
		t.Run(st.name, func(t *testing.T) {
			userID := f.superAdmin(t, "ts-valid-"+string(rune('a'+i)))
			key := mintHubConfigToken(t, f.srv, userID, hubBoundary(), st.selector)
			u, err := f.store.GetUser(context.Background(), userID)
			require.NoError(t, err)
			session, _, _, err := f.srv.userTokenService.GenerateTokenPair(u.ID, u.Email, u.DisplayName, u.Role, ClientTypeWeb)
			require.NoError(t, err)

			for _, c := range []struct{ name, auth string }{
				{"token", "Bearer " + key},
				{"session", "Bearer " + session},
				{"dev", "Bearer " + testDevToken},
			} {
				t.Run(c.name, func(t *testing.T) {
					done, rec, cancel := openStream(t, f.srv, st.path, c.auth)
					requireStreamStaysOpen(t, done)
					cancel()
					requireStreamEnds(t, done)
					require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
					assert.NotContains(t, rec.Body.String(), streamCredentialEndedEvent)
				})
			}

			t.Run("session after the role is removed", func(t *testing.T) {
				done, rec, cancel := openStream(t, f.srv, st.path, "Bearer "+session)
				defer cancel()
				ctx := context.Background()
				rd, err := f.store.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
				require.NoError(t, err)
				bindings, err := f.store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
				require.NoError(t, err)
				for _, b := range bindings {
					if b.RoleDefinitionID == rd.ID {
						require.NoError(t, f.store.DeleteRoleBinding(ctx, b.ID))
					}
				}
				requireStreamStaysOpen(t, done)
				cancel()
				requireStreamEnds(t, done)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				assert.NotContains(t, rec.Body.String(), streamCredentialEndedEvent)
			})
		})
	}
}

// TestBearerStillValid_OnlyTokensAreChecked requires bearerStillValid to
// pass a request whose credential is not a user access token and to fail a
// token credential without a token ID or whose stored token is revoked.
func TestBearerStillValid_OnlyTokensAreChecked(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	userID := tid("bsv-user")
	createTestUserWithRole(t, s, userID, userID+"@test.com", "member", store.SystemRoleSuperAdmin)
	ensureHubMembership(ctx, s, userID)
	user := NewAuthenticatedUser(userID, userID+"@test.com", "BSV", "member", "web")

	session := contextWithCredentialContext(contextWithIdentity(ctx, user), CredentialContext{Kind: CredentialKindInteractive})
	assert.True(t, srv.bearerStillValid(session), "a session passes")

	noID := contextWithCredentialContext(contextWithIdentity(ctx, user), CredentialContext{Kind: CredentialKindUAT})
	assert.False(t, srv.bearerStillValid(noID), "a token credential without a token ID fails")

	_, token, err := srv.uatService.CreateTokenWithParams(rs4MintContext(userID), CreateTokenParams{
		UserID: userID, Name: "bsv-" + tid("tok"), Boundary: hubBoundary(), Scopes: []string{"agent:read"},
	})
	require.NoError(t, err)
	recorded := contextWithCredentialContext(contextWithIdentity(ctx, user), CredentialContext{Kind: CredentialKindUAT, ID: token.ID})
	assert.True(t, srv.bearerStillValid(recorded), "a stored token that validates passes")
	require.NoError(t, srv.uatService.RevokeToken(rs4MintContext(userID), userID, token.ID))
	assert.False(t, srv.bearerStillValid(recorded), "a revoked token fails")

	other := NewAuthenticatedUser(tid("bsv-other"), "other@test.com", "Other", "member", "web")
	_, token2, err := srv.uatService.CreateTokenWithParams(rs4MintContext(userID), CreateTokenParams{
		UserID: userID, Name: "bsv-" + tid("tok2"), Boundary: hubBoundary(), Scopes: []string{"agent:read"},
	})
	require.NoError(t, err)
	foreign := contextWithCredentialContext(contextWithIdentity(ctx, other), CredentialContext{Kind: CredentialKindUAT, ID: token2.ID})
	assert.False(t, srv.bearerStillValid(foreign), "a token of another user fails")
}

// TestMessageLogStream_UsesFullHistoryRule requires the agent message-log
// stream to apply the message-log list's rule for every caller: agent.attach
// gives the full history (no participant filter), agent.read alone gives the
// messages the caller sent or received, a caller the list refuses gets 403
// on the stream too, and a non-user caller follows the same rule: an agent
// with agent.attach gets the full history on both, and a read-only agent
// gets 403 on both.
func TestMessageLogStream_UsesFullHistoryRule(t *testing.T) {
	srv, s, alice, bob, agentID := setupMessagePrivacyTest(t)
	logs := &openTailLogQuerier{}
	srv.logQueryService = logs
	ctx := context.Background()
	agent, err := s.GetAgent(ctx, agentID)
	require.NoError(t, err)
	byID := "/api/v1/agents/" + agentID + "/message-logs"
	inProject := "/api/v1/projects/" + agent.ProjectID + "/agents/" + agentID + "/message-logs"

	sessionOf := func(u *store.User) string {
		tok, _, _, err := srv.userTokenService.GenerateTokenPair(u.ID, u.Email, u.DisplayName, u.Role, ClientTypeWeb)
		require.NoError(t, err)
		return "Bearer " + tok
	}
	tokenOf := func(u *store.User, scopes ...string) string {
		key, _, err := srv.uatService.CreateToken(rs4MintContext(u.ID), u.ID, "mls-"+tid("tok"), agent.ProjectID, scopes, nil)
		require.NoError(t, err)
		return "Bearer " + key
	}
	eve := &store.User{ID: tid("mls-eve"), Email: "eve@msg.test", DisplayName: "Eve", Role: store.UserRoleMember, Status: "active", Created: time.Now()}
	require.NoError(t, s.CreateUser(ctx, eve))

	caller := &store.Agent{ID: tid("mls-caller"), Slug: "mls-caller", Name: "Caller", ProjectID: agent.ProjectID, Phase: "running", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateAgent(ctx, caller))
	agentTok, err := srv.GenerateAgentToken(caller.ID, caller.ProjectID, nil, AgentRoleFull, nil)
	require.NoError(t, err)

	// list returns the list status and the participant filter it queried
	// with.
	list := func(t *testing.T, path string, setAuth func(*http.Request)) (int, string) {
		t.Helper()
		before := len(logs.queries())
		req := httptest.NewRequest(http.MethodGet, path, nil)
		setAuth(req)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		q := logs.queries()
		if len(q) > before {
			return rec.Code, q[len(q)-1].ParticipantID
		}
		return rec.Code, "<none>"
	}
	// stream opens the stream for a short while and returns its status and
	// the participant filter it tailed with.
	stream := func(t *testing.T, path string, setAuth func(*http.Request)) (int, string) {
		t.Helper()
		before := len(logs.tails())
		reqCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		req := httptest.NewRequest(http.MethodGet, path+"/stream", nil).WithContext(reqCtx)
		setAuth(req)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		tl := logs.tails()
		if len(tl) > before {
			return rec.Code, tl[len(tl)-1].ParticipantID
		}
		return rec.Code, "<none>"
	}
	bearer := func(v string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Authorization", v) }
	}

	cases := []struct {
		name        string
		setAuth     func(*http.Request)
		wantStatus  int
		participant string // "" = full history; "<none>" = no query
	}{
		{"owner session: full history", bearer(sessionOf(alice)), http.StatusOK, ""},
		{"member session: own messages only", bearer(sessionOf(bob)), http.StatusOK, bob.ID},
		{"owner token with agent:read and agent:attach: full history", bearer(tokenOf(alice, "agent:read", "agent:attach")), http.StatusOK, ""},
		{"owner token with agent:read only: own messages only", bearer(tokenOf(alice, "agent:read")), http.StatusOK, alice.ID},
		{"member token with agent:read: own messages only", bearer(tokenOf(bob, "agent:read")), http.StatusOK, bob.ID},
		{"user without access: refused", bearer(sessionOf(eve)), http.StatusForbidden, "<none>"},
	}
	for _, path := range []string{byID, inProject} {
		for _, tc := range cases {
			t.Run(path+" "+tc.name, func(t *testing.T) {
				listStatus, listParticipant := list(t, path, tc.setAuth)
				require.Equal(t, tc.wantStatus, listStatus, "list")
				require.Equal(t, tc.participant, listParticipant, "list participant filter")
				streamStatus, streamParticipant := stream(t, path, tc.setAuth)
				assert.Equal(t, tc.wantStatus, streamStatus, "stream")
				assert.Equal(t, tc.participant, streamParticipant, "stream participant filter")
			})
		}
	}

	readOnlyCaller := &store.Agent{ID: tid("mls-ro-caller"), Slug: "mls-ro-caller", Name: "RO Caller", ProjectID: agent.ProjectID, Phase: "running", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateAgent(ctx, readOnlyCaller))
	// The token carries exactly the read-only role's scopes, signed
	// through the token service.
	require.NotContains(t, ScopesForRole(AgentRoleReadOnly), ScopeAgentLifecycle)
	readOnlyTok, err := srv.GetAgentTokenService().GenerateAgentToken(readOnlyCaller.ID, readOnlyCaller.ProjectID, ScopesForRole(AgentRoleReadOnly), nil)
	require.NoError(t, err)

	for _, path := range []string{byID, inProject} {
		t.Run(path+" agent caller with agent.attach: full history on list and stream", func(t *testing.T) {
			setAuth := func(r *http.Request) { r.Header.Set("X-Scion-Agent-Token", agentTok) }
			listStatus, listParticipant := list(t, path, setAuth)
			require.Equal(t, http.StatusOK, listStatus, "list")
			require.Equal(t, "", listParticipant, "list: full history")
			streamStatus, streamParticipant := stream(t, path, setAuth)
			assert.Equal(t, http.StatusOK, streamStatus, "stream")
			assert.Equal(t, "", streamParticipant, "stream: full history")
		})
		t.Run(path+" read-only agent caller: refused on list and stream", func(t *testing.T) {
			setAuth := func(r *http.Request) { r.Header.Set("X-Scion-Agent-Token", readOnlyTok) }
			listStatus, listParticipant := list(t, path, setAuth)
			require.Equal(t, http.StatusForbidden, listStatus, "list")
			require.Equal(t, "<none>", listParticipant, "list: no query")
			streamStatus, streamParticipant := stream(t, path, setAuth)
			assert.Equal(t, http.StatusForbidden, streamStatus, "stream")
			assert.Equal(t, "<none>", streamParticipant, "stream: no tail")
		})
	}
}

// revokeSuperAdmin removes every super-admin binding of userID.
func revokeSuperAdmin(t *testing.T, s store.Store, userID string) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	removed := 0
	for _, b := range bindings {
		if b.RoleDefinitionID == rd.ID {
			require.NoError(t, s.DeleteRoleBinding(ctx, b.ID))
			removed++
		}
	}
	require.NotZero(t, removed, "the user held super-admin")
}

// TestTokenStream_EndsAfterAttachRemoved requires a message-log or message
// stream opened by a user access token with the full history (agent.attach)
// to end once the token's user loses agent.attach while keeping agent.read,
// and a stream the same user opened with agent.read alone (participant
// filtered) to keep running across the same change. The user is a project
// member (agent.read, no agent.attach on another member's agent) who also
// holds super-admin (agent.attach) until the change removes it.
func TestTokenStream_EndsAfterAttachRemoved(t *testing.T) {
	shortenStreamRecheck(t)
	f := newTokenStreamFixture(t)
	byID := "/api/v1/agents/" + f.agent.ID
	inProject := "/api/v1/projects/" + f.agent.ProjectID + "/agents/" + f.agent.ID
	streams := []struct {
		name, path string
		logs       bool // a log tail records the participant filter
	}{
		{"agent message logs", byID + "/message-logs/stream", true},
		{"project agent message logs", inProject + "/message-logs/stream", true},
		{"agent messages", byID + "/messages/stream", false},
	}
	for i, st := range streams {
		t.Run(st.name, func(t *testing.T) {
			userID := tid("ts-attach-" + string(rune('a'+i)))
			createTestUserWithProjectRole(t, f.store, userID, userID+"@test.com", f.agent.ProjectID, store.ProjectRoleMember)
			ensureHubMembership(context.Background(), f.store, userID)
			grantSuperAdmin(t, f.store, userID)
			full := mintHubConfigToken(t, f.srv, userID, hubBoundary(), "agent:read", "agent:attach")
			readOnly := mintHubConfigToken(t, f.srv, userID, hubBoundary(), "agent:read")

			before := len(f.logs.tails())
			fullDone, fullRec, fullCancel := openStream(t, f.srv, st.path, "Bearer "+full)
			defer fullCancel()
			if st.logs {
				tails := f.logs.tails()
				require.Greater(t, len(tails), before)
				require.Equal(t, "", tails[len(tails)-1].ParticipantID, "the attach token opens the full history")
			}
			before = len(f.logs.tails())
			readDone, readRec, readCancel := openStream(t, f.srv, st.path, "Bearer "+readOnly)
			defer readCancel()
			if st.logs {
				tails := f.logs.tails()
				require.Greater(t, len(tails), before)
				require.Equal(t, userID, tails[len(tails)-1].ParticipantID, "the read token opens the participant view")
			}
			requireStreamStaysOpen(t, fullDone)

			revokeSuperAdmin(t, f.store, userID)

			requireStreamEnds(t, fullDone)
			require.Equal(t, http.StatusOK, fullRec.Code, fullRec.Body.String())
			body := fullRec.Body.String()
			require.Contains(t, body, streamCredentialEndedEvent)
			assert.Equal(t, streamCredentialEndedEvent, body[len(body)-len(streamCredentialEndedEvent):])

			requireStreamStaysOpen(t, readDone)
			readCancel()
			requireStreamEnds(t, readDone)
			require.Equal(t, http.StatusOK, readRec.Code, readRec.Body.String())
			assert.NotContains(t, readRec.Body.String(), streamCredentialEndedEvent,
				"a stream admitted with agent.read keeps running while agent.read holds")
		})
	}
}

// streamStatus opens the stream at path with auth for a short while and
// returns its status: 200 for a stream that opened, the refusal status
// otherwise.
func streamStatus(t *testing.T, srv *Server, path, auth string) int {
	t.Helper()
	reqCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(reqCtx)
	req.Header.Set("Authorization", auth)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec.Code
}

// TestMessagesStreamToken_AdmissionByBoundary requires the agent message
// stream to admit a user access token with agent:read on a boundary that
// contains the agent's project (a project token for the project, a hub
// token), to refuse a token without agent:read and a token bounded to
// another project, and to admit a session. The user is a member of the
// agent's project and of one other project.
func TestMessagesStreamToken_AdmissionByBoundary(t *testing.T) {
	f := newTokenStreamFixture(t)
	ctx := context.Background()
	path := "/api/v1/agents/" + f.agent.ID + "/messages/stream"
	userID := tid("ms-member")
	createTestUserWithProjectRole(t, f.store, userID, userID+"@test.com", f.agent.ProjectID, store.ProjectRoleMember)
	ensureHubMembership(ctx, f.store, userID)
	other := &store.Project{ID: tid("ms-other"), Name: "ms-other", Slug: "ms-other"}
	require.NoError(t, f.store.CreateProject(ctx, other))
	createTestUserWithProjectRole(t, f.store, userID, userID+"@test.com", other.ID, store.ProjectRoleMember)

	mint := func(boundary TokenBoundary, scopes ...string) string {
		key, _, err := f.srv.uatService.CreateTokenWithParams(rs4MintContext(userID), CreateTokenParams{
			UserID: userID, Name: "ms-" + tid("tok"), Boundary: boundary, Scopes: scopes,
		})
		require.NoError(t, err)
		return "Bearer " + key
	}
	u, err := f.store.GetUser(ctx, userID)
	require.NoError(t, err)
	session, _, _, err := f.srv.userTokenService.GenerateTokenPair(u.ID, u.Email, u.DisplayName, u.Role, ClientTypeWeb)
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, streamStatus(t, f.srv, path, mint(projectBoundary(f.agent.ProjectID), "agent:read")),
		"project token with agent:read")
	assert.Equal(t, http.StatusOK, streamStatus(t, f.srv, path, mint(hubBoundary(), "agent:read")),
		"hub token with agent:read")
	assert.Equal(t, http.StatusForbidden, streamStatus(t, f.srv, path, mint(projectBoundary(f.agent.ProjectID), "project:read")),
		"token without agent:read")
	other403or404 := streamStatus(t, f.srv, path, mint(projectBoundary(other.ID), "agent:read"))
	assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, other403or404,
		"token bounded to another project")
	assert.Equal(t, http.StatusOK, streamStatus(t, f.srv, path, "Bearer "+session), "session")
}
