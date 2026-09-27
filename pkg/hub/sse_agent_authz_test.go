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

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sseAgentStore struct {
	*mockAuthzStore
	agents    map[string]*store.Agent
	lookupErr error
	authzErr  error
}

func (s *sseAgentStore) GetAgent(_ context.Context, id string) (*store.Agent, error) {
	if s.lookupErr != nil {
		return nil, s.lookupErr
	}
	if a, ok := s.agents[id]; ok {
		return a, nil
	}
	return nil, store.ErrNotFound
}

func (s *sseAgentStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopes, ids []string) ([]*store.RoleBinding, error) {
	if s.authzErr != nil {
		return nil, s.authzErr
	}
	return s.mockAuthzStore.ListRoleBindingsForPrincipals(ctx, principals, scopes, ids)
}

type sseCountingPublisher struct {
	*ChannelEventPublisher
	subscriptions int
}

func (p *sseCountingPublisher) Subscribe(patterns ...string) (<-chan Event, func()) {
	p.subscriptions++
	return p.ChannelEventPublisher.Subscribe(patterns...)
}

func sseAgentRequest(ctx context.Context, userID, role string, subjects ...string) *http.Request {
	query := url.Values{"sub": subjects}
	req := httptest.NewRequest(http.MethodGet, "/events?"+query.Encode(), nil)
	user := &webSessionUser{UserID: userID, Role: role}
	return req.WithContext(context.WithValue(ctx, webUserContextKey{}, user))
}

func TestSSEHandler_AgentUnauthorized(t *testing.T) {
	id := tid("sse-unauthorized-agent")
	s := &sseAgentStore{mockAuthzStore: &mockAuthzStore{}, agents: map[string]*store.Agent{
		id: {ID: id, ProjectID: tid("other-project"), OwnerID: "other-user"},
	}}
	pub := &sseCountingPublisher{ChannelEventPublisher: NewChannelEventPublisher()}
	defer pub.Close()
	ws := &WebServer{store: s, events: pub, authzService: NewAuthzService(s, slog.Default())}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Bounds the old, incorrectly accepted streaming path without sleeps.
	subject := "agent." + id + ".>"
	w := httptest.NewRecorder()
	ws.handleSSE(w, sseAgentRequest(ctx, "user-1", "user", subject))
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.False(t, w.Flushed)
	assert.Zero(t, pub.subscriptions, "denied requests must never subscribe")
	var body struct {
		Denied []string `json:"denied_subjects"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, []string{subject}, body.Denied)
}

func sseAgentReadBinding(userID, scopeType, scopeID string) *mockAuthzStore {
	return &mockAuthzStore{
		roleDefinitions: map[string]*store.RoleDefinition{"read-role": {
			ID: "read-role", Name: "sse-agent-reader", ScopeType: scopeType, Permissions: []string{"agent.read"},
		}},
		roleBindings: []*store.RoleBinding{{ID: "read-binding", RoleDefinitionID: "read-role", PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: userID, ScopeType: scopeType, ScopeID: scopeID}},
	}
}

func TestAuthorizeSSESubjects_AgentPolicy(t *testing.T) {
	id, projectID := tid("sse-policy-agent"), tid("sse-policy-project")
	for _, tc := range []struct {
		name     string
		role     string
		owner    string
		ancestry []string
		bindings *mockAuthzStore
		want     bool
	}{
		{name: "unrelated user", bindings: &mockAuthzStore{}},
		{name: "resource owner", owner: "user-1", bindings: &mockAuthzStore{}, want: true},
		{name: "ancestor", ancestry: []string{"user-1"}, bindings: &mockAuthzStore{}, want: true},
		{name: "project inherited read", bindings: sseAgentReadBinding("user-1", store.RoleScopeProject, projectID), want: true},
		{name: "cross project", bindings: sseAgentReadBinding("user-1", store.RoleScopeProject, tid("other-project"))},
		{name: "hub read grant", bindings: sseAgentReadBinding("user-1", store.RoleScopeSystem, ""), want: true},
		{name: "super admin", role: "admin", bindings: mockSuperAdminStore("user-1"), want: true},
		{name: "role string alone is not admin grant", role: "admin", bindings: &mockAuthzStore{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &store.Agent{ID: id, ProjectID: projectID, OwnerID: tc.owner, Ancestry: tc.ancestry}
			s := &sseAgentStore{mockAuthzStore: tc.bindings, agents: map[string]*store.Agent{id: agent}}
			ws := &WebServer{store: s, authzService: NewAuthzService(s, slog.Default())}
			req := sseAgentRequest(context.Background(), "user-1", tc.role)
			// The metadata endpoint uses this exact resource/action policy, not PTY attach.
			identity := NewAuthenticatedUser("user-1", "", "", tc.role, "web")
			require.Equal(t, tc.want, ws.authzService.CheckAccess(req.Context(), identity, agentResource(agent), ActionRead).Allowed)
			for _, suffix := range []string{">", "status", "created", "deleted", "ports", "message", "*"} {
				subject := "agent." + id + "." + suffix
				denied := ws.authorizeSSESubjects(req, []string{subject})
				if tc.want {
					assert.Empty(t, denied)
				} else {
					assert.Equal(t, []string{subject}, denied)
				}
			}
		})
	}
}

func TestAuthorizeSSESubjects_AgentSelectorsFailClosed(t *testing.T) {
	id := tid("sse-selector-agent")
	for _, subject := range []string{"agent", "agent.>", "agent.*.>", "agent.>.status", "agent.not-a-uuid.>", "agent." + id, "agent." + strings.ReplaceAll(id, "-", "") + ".>", "agent." + strings.ToUpper(id) + ".>", "agent." + tid("missing-agent") + ".>"} {
		t.Run(subject, func(t *testing.T) {
			s := &sseAgentStore{mockAuthzStore: mockSuperAdminStore("user-1"), agents: map[string]*store.Agent{id: {ID: id}}}
			ws := &WebServer{store: s, authzService: NewAuthzService(s, slog.Default())}
			req := sseAgentRequest(context.Background(), "user-1", "admin", subject)
			assert.Equal(t, []string{subject}, ws.authorizeSSESubjects(req, []string{subject}))
		})
	}
	for _, tc := range []struct {
		name                string
		missingStore        bool
		lookupErr, authzErr error
		agent               *store.Agent
	}{
		{name: "missing store", missingStore: true},
		{name: "lookup error", lookupErr: errors.New("lookup unavailable")},
		{name: "authorization error", authzErr: errors.New("policy unavailable"), agent: &store.Agent{ID: id}},
		{name: "nil resource"},
		{name: "mismatched resource", agent: &store.Agent{ID: tid("different-agent")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &sseAgentStore{mockAuthzStore: &mockAuthzStore{}, agents: map[string]*store.Agent{id: tc.agent}, lookupErr: tc.lookupErr, authzErr: tc.authzErr}
			ws := &WebServer{store: s, authzService: NewAuthzService(s, slog.Default())}
			if tc.missingStore {
				ws.store = nil
			}
			subject := "agent." + id + ".>"
			req := sseAgentRequest(context.Background(), "user-1", "user", subject)
			assert.Equal(t, []string{subject}, ws.authorizeSSESubjects(req, []string{subject}))
		})
	}
}

func TestSSEHandler_AgentMixedBatch(t *testing.T) {
	allowedID, deniedID := tid("sse-allowed-agent"), tid("sse-denied-agent")
	for _, deny := range []bool{false, true} {
		t.Run(map[bool]string{false: "authorized batch", true: "mixed batch"}[deny], func(t *testing.T) {
			s := &sseAgentStore{mockAuthzStore: &mockAuthzStore{}, agents: map[string]*store.Agent{
				allowedID: {ID: allowedID, OwnerID: "user-1"}, deniedID: {ID: deniedID, OwnerID: "other-user"},
			}}
			pub := &sseCountingPublisher{ChannelEventPublisher: NewChannelEventPublisher()}
			defer pub.Close()
			ws := &WebServer{store: s, events: pub, authzService: NewAuthzService(s, slog.Default())}
			subjects := []string{"agent." + allowedID + ".>", "user.user-1.message", "agent." + allowedID + ".ports"}
			deniedSubject := "agent." + deniedID + ".>"
			if deny {
				subjects = append(subjects, deniedSubject)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			w := httptest.NewRecorder()
			ws.handleSSE(w, sseAgentRequest(ctx, "user-1", "user", subjects...))
			if deny {
				assert.Equal(t, http.StatusForbidden, w.Code)
				assert.False(t, w.Flushed)
				assert.Zero(t, pub.subscriptions)
				var body struct {
					Denied []string `json:"denied_subjects"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
				assert.Equal(t, []string{deniedSubject}, body.Denied)
			} else {
				assert.Equal(t, http.StatusOK, w.Code)
				assert.True(t, w.Flushed)
				assert.Equal(t, 1, pub.subscriptions)
			}
			assertNoSSESubscribers(t, pub.ChannelEventPublisher)
		})
	}
}
