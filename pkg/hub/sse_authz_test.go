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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- authorizeSSESubjects unit tests ---

func TestAuthorizeSSESubjects_NoAuthzService(t *testing.T) {
	ws := &WebServer{} // no authzService
	req := httptest.NewRequest("GET", "/events", nil)
	// Set a web session user in context.
	user := &webSessionUser{UserID: "user-1", Email: "a@b.com", Role: "user"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))

	denied := ws.authorizeSSESubjects(req, []string{
		"project.proj-1.chat.message",
		"user.other-user.chat.dm",
	})
	assert.Nil(t, denied, "nil authzService should allow all subjects")
}

func TestAuthorizeSSESubjects_NoSessionUser(t *testing.T) {
	ws := &WebServer{
		authzService: NewAuthzService(&mockAuthzStore{}, nil),
	}
	req := httptest.NewRequest("GET", "/events", nil)
	// No web session user in context.

	subjects := []string{"project.proj-1.agent.status"}
	denied := ws.authorizeSSESubjects(req, subjects)
	assert.Equal(t, subjects, denied, "no session user should deny all subjects")
}

func TestAuthorizeSSESubjects_UserSubject_OwnID(t *testing.T) {
	ws := &WebServer{
		authzService: NewAuthzService(&mockAuthzStore{}, nil),
	}
	req := httptest.NewRequest("GET", "/events", nil)
	user := &webSessionUser{UserID: "user-1", Email: "a@b.com", Role: "user"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))

	denied := ws.authorizeSSESubjects(req, []string{
		"user.user-1.chat.dm",
		"user.user-1.message",
	})
	assert.Nil(t, denied, "user should be allowed to subscribe to own user subjects")
}

func TestAuthorizeSSESubjects_UserSubject_OtherID(t *testing.T) {
	ws := &WebServer{
		authzService: NewAuthzService(&mockAuthzStore{}, nil),
	}
	req := httptest.NewRequest("GET", "/events", nil)
	user := &webSessionUser{UserID: "user-1", Email: "a@b.com", Role: "user"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))

	denied := ws.authorizeSSESubjects(req, []string{
		"user.other-user.chat.dm",
	})
	assert.Equal(t, []string{"user.other-user.chat.dm"}, denied,
		"user should NOT be allowed to subscribe to another user's subjects")
}

func TestAuthorizeSSESubjects_MixedAllowedDenied_AllFail(t *testing.T) {
	ws := &WebServer{
		authzService: NewAuthzService(&mockAuthzStore{}, nil),
	}
	req := httptest.NewRequest("GET", "/events", nil)
	user := &webSessionUser{UserID: "user-1", Email: "a@b.com", Role: "user"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))

	subjects := []string{
		"user.user-1.chat.dm",       // allowed (own ID)
		"user.other-user.chat.dm",   // denied (other user)
		"notification.user-1.inbox", // pass-through (no project/user prefix check)
	}
	denied := ws.authorizeSSESubjects(req, subjects)
	assert.Equal(t, []string{"user.other-user.chat.dm"}, denied,
		"only the denied subject should be returned")
}

func TestAuthorizeSSESubjects_ProjectSubject_AdminAllowed(t *testing.T) {
	ws := &WebServer{
		authzService: NewAuthzService(&mockAuthzStore{}, nil),
	}
	req := httptest.NewRequest("GET", "/events", nil)
	// Admin role gets short-circuited in ComputeCapabilitiesBatch.
	user := &webSessionUser{UserID: "admin-1", Email: "admin@b.com", Role: "admin"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))

	denied := ws.authorizeSSESubjects(req, []string{
		"project.proj-1.chat.message",
		"project.proj-1.agent.status",
		"project.proj-2.chat.topic",
	})
	assert.Nil(t, denied, "admin should be allowed to subscribe to all project subjects")
}

func TestAuthorizeSSESubjects_ProjectSubject_NonMemberDenied(t *testing.T) {
	ws := &WebServer{
		authzService: NewAuthzService(&mockAuthzStore{}, nil),
	}
	req := httptest.NewRequest("GET", "/events", nil)
	// Non-admin user with no policies — ComputeCapabilitiesBatch returns no actions.
	user := &webSessionUser{UserID: "user-1", Email: "a@b.com", Role: "user"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))

	denied := ws.authorizeSSESubjects(req, []string{
		"project.proj-1.chat.message",
	})
	assert.Equal(t, []string{"project.proj-1.chat.message"}, denied,
		"non-member should be denied project subjects")
}

func TestAuthorizeSSESubjects_PassthroughSubjects(t *testing.T) {
	ws := &WebServer{
		authzService: NewAuthzService(&mockAuthzStore{}, nil),
	}
	req := httptest.NewRequest("GET", "/events", nil)
	user := &webSessionUser{UserID: "user-1", Email: "a@b.com", Role: "user"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))

	// Notification and broker subjects pass through without authorization.
	denied := ws.authorizeSSESubjects(req, []string{
		"notification.user-1.inbox",
		"broker.status",
	})
	assert.Nil(t, denied, "notification/broker subjects should pass through")
}

// --- SSE Handler integration test for authz ---

func TestSSEHandler_SubjectAuthzDenied(t *testing.T) {
	ws := newDevAuthWebServer(t)
	pub := NewChannelEventPublisher()
	ws.SetEventPublisher(pub)
	ws.SetAuthzService(NewAuthzService(&mockAuthzStore{}, nil))
	t.Cleanup(pub.Close)

	// DevUserID is admin, so project subjects are allowed. But subscribing
	// to another user's subject should be denied.
	req := httptest.NewRequest("GET", "/events?sub=user.other-user.chat.dm", nil)
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestSSEHandler_SubjectAuthzAllowed(t *testing.T) {
	ws := newDevAuthWebServer(t)
	pub := NewChannelEventPublisher()
	ws.SetEventPublisher(pub)
	ws.SetAuthzService(NewAuthzService(&mockAuthzStore{}, nil))
	t.Cleanup(pub.Close)

	// DevUserID is admin; subscribing to own user subject should succeed.
	ts := httptest.NewServer(ws.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/events?sub=user." + DevUserID + ".chat.dm")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// --- validateSSESubjects: chat.* patterns pass syntax validation ---

func TestValidateSSESubjects_ChatPatterns(t *testing.T) {
	tests := []struct {
		name    string
		subject string
	}{
		{"chat message", "project.abc123.chat.message"},
		{"chat topic", "project.abc123.chat.topic"},
		{"chat typing", "project.abc123.chat.typing"},
		{"chat presence", "project.abc123.chat.presence"},
		{"chat wildcard", "project.abc123.chat.>"},
		{"chat star", "project.abc123.chat.*"},
		{"user chat dm", "user.uid123.chat.dm"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errMsg := validateSSESubjects([]string{tt.subject})
			assert.Empty(t, errMsg, "subject %q should pass syntax validation", tt.subject)
		})
	}
}

// --- mock store for authz tests ---

// mockAuthzStore satisfies store.Store for NewAuthzService. Only the methods
// called by ComputeCapabilitiesBatch's dependency chain are implemented;
// the embedded interface satisfies everything else at the signature level.
type mockAuthzStore struct {
	store.Store // embed to satisfy interface
}

func (m *mockAuthzStore) GetEffectiveGroups(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (m *mockAuthzStore) GetEffectiveGroupsForAgent(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (m *mockAuthzStore) GetPoliciesForPrincipals(_ context.Context, _ []store.PrincipalRef) ([]store.Policy, error) {
	return nil, nil
}

func (m *mockAuthzStore) ListGroups(_ context.Context, _ store.GroupFilter, _ store.ListOptions) (*store.ListResult[store.Group], error) {
	return &store.ListResult[store.Group]{}, nil
}

func (m *mockAuthzStore) GetGroupMembership(_ context.Context, _, _ string, _ string) (*store.GroupMember, error) {
	return nil, store.ErrNotFound
}
