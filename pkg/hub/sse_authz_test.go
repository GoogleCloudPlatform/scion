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

func TestAuthorizeSSESubjects_NoAuthzService_DeniesAll(t *testing.T) {
	ws := &WebServer{} // no authzService
	req := httptest.NewRequest("GET", "/events", nil)
	user := &webSessionUser{UserID: "user-1", Email: "a@b.com", Role: "user"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))

	subjects := []string{
		"project.proj-1.chat.message",
		"user.other-user.chat.dm",
	}
	denied := ws.authorizeSSESubjects(req, subjects)
	assert.Equal(t, subjects, denied, "nil authzService must deny all (fail closed)")
}

func TestAuthorizeSSESubjects_WildcardRootDenied(t *testing.T) {
	ws := &WebServer{
		authzService: NewAuthzService(&mockAuthzStore{}, nil),
	}
	tests := []struct {
		name    string
		subject string
	}{
		{"bare >", ">"},
		{"bare *", "*"},
		{"*.>", "*.>"},
		{"*.*.chat.>", "*.*.chat.>"},
		{"* as root with children", "*.project.something"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/events", nil)
			// Admin user — would pass project/user checks, but wildcard
			// root must be rejected regardless.
			user := &webSessionUser{UserID: "admin-1", Email: "a@b.com", Role: "admin"}
			req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))

			subjects := []string{tt.subject}
			denied := ws.authorizeSSESubjects(req, subjects)
			assert.Equal(t, subjects, denied,
				"wildcard root %q must be denied even for admin", tt.subject)
		})
	}
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
	// CO1: admin role alone is insufficient — needs super-admin role binding.
	ws := &WebServer{
		authzService: NewAuthzService(mockSuperAdminStore("admin-1"), nil),
	}
	req := httptest.NewRequest("GET", "/events", nil)
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

// --- expandSSEWildcards: NATS wildcard expansion tests (F2) ---

func TestExpandSSEWildcards_ProjectWildcard_SingleProject(t *testing.T) {
	// User owns one project → project.> expands to project.<uuid>.>
	mockStore := &mockAuthzStore{
		projects: []store.Project{
			{ID: "proj-1", OwnerID: "user-1"},
		},
		projectMemberships: map[string]*store.ProjectMembership{
			"proj-1:user-1": {ProjectID: "proj-1", UserID: "user-1", Role: store.ProjectRoleOwner},
		},
	}
	ws := &WebServer{
		store:        mockStore,
		authzService: NewAuthzService(mockStore, nil),
	}
	req := httptest.NewRequest("GET", "/events", nil)
	user := &webSessionUser{UserID: "user-1", Email: "a@b.com", Role: "user"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))

	expanded := ws.expandSSEWildcards(req, []string{"project.>", "notification.>"})

	assert.Contains(t, expanded, "project.proj-1.>",
		"project.> should expand to project.proj-1.>")
	assert.Contains(t, expanded, "notification.>",
		"notification.> should pass through unchanged")
	assert.NotContains(t, expanded, "project.>",
		"bare project.> should not remain after expansion")

	// Verify authz passes for the expanded subjects.
	denied := ws.authorizeSSESubjects(req, expanded)
	assert.Empty(t, denied, "expanded subjects should be authorized")
}

func TestExpandSSEWildcards_ProjectWildcard_ZeroProjects(t *testing.T) {
	// User with no projects → project.> expands to nothing (fail-closed).
	mockStore := &mockAuthzStore{
		projects: []store.Project{}, // no projects at all
	}
	ws := &WebServer{
		store:        mockStore,
		authzService: NewAuthzService(mockStore, nil),
	}
	req := httptest.NewRequest("GET", "/events", nil)
	user := &webSessionUser{UserID: "user-1", Email: "a@b.com", Role: "user"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))

	expanded := ws.expandSSEWildcards(req, []string{"project.>", "notification.>"})

	// project.> should have been dropped (no accessible projects).
	for _, sub := range expanded {
		assert.False(t, len(sub) >= 8 && sub[:8] == "project.",
			"no project subjects should remain when user has 0 projects")
	}
	assert.Contains(t, expanded, "notification.>",
		"notification.> should pass through even when no projects")
}

func TestExpandSSEWildcards_NilListProjectsResult(t *testing.T) {
	// Regression: if ListProjects returns (nil, nil) — a store contract
	// violation — expandProjectWildcard must fail-closed (no panic).
	mockStore := &mockAuthzStore{
		listProjectsReturnNil: true,
	}
	ws := &WebServer{
		store:        mockStore,
		authzService: NewAuthzService(mockStore, nil),
	}
	req := httptest.NewRequest("GET", "/events", nil)
	user := &webSessionUser{UserID: "user-1", Email: "a@b.com", Role: "user"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))

	expanded := ws.expandSSEWildcards(req, []string{"project.>", "notification.>"})

	// project.> should be dropped (fail-closed), notification.> passes through.
	for _, sub := range expanded {
		assert.False(t, len(sub) >= 8 && sub[:8] == "project.",
			"no project subjects should remain when ListProjects returns nil")
	}
	assert.Contains(t, expanded, "notification.>",
		"notification.> should pass through even when ListProjects returns nil")
}

func TestExpandSSEWildcards_ProjectWildcard_MultipleProjects(t *testing.T) {
	// User owns proj-1 and proj-2, but NOT proj-3 → only accessible ones expand.
	mockStore := &mockAuthzStore{
		projects: []store.Project{
			{ID: "proj-1", OwnerID: "user-1"},
			{ID: "proj-2", OwnerID: "user-1"},
			{ID: "proj-3", OwnerID: "other-user"},
		},
		projectMemberships: map[string]*store.ProjectMembership{
			"proj-1:user-1": {ProjectID: "proj-1", UserID: "user-1", Role: store.ProjectRoleOwner},
			"proj-2:user-1": {ProjectID: "proj-2", UserID: "user-1", Role: store.ProjectRoleOwner},
			// user-1 has no membership in proj-3
		},
	}
	ws := &WebServer{
		store:        mockStore,
		authzService: NewAuthzService(mockStore, nil),
	}
	req := httptest.NewRequest("GET", "/events", nil)
	user := &webSessionUser{UserID: "user-1", Email: "a@b.com", Role: "user"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))

	expanded := ws.expandSSEWildcards(req, []string{"project.>"})

	assert.Contains(t, expanded, "project.proj-1.>")
	assert.Contains(t, expanded, "project.proj-2.>")
	assert.NotContains(t, expanded, "project.proj-3.>",
		"user should not get subjects for projects they can't access")
	assert.NotContains(t, expanded, "project.>",
		"bare wildcard should not remain")
}

func TestExpandSSEWildcards_SpecificProjectID_Unchanged(t *testing.T) {
	// Specific project ID (project.<uuid>.>) passes through without expansion.
	mockStore := &mockAuthzStore{}
	ws := &WebServer{
		store:        mockStore,
		authzService: NewAuthzService(mockStore, nil),
	}
	req := httptest.NewRequest("GET", "/events", nil)
	user := &webSessionUser{UserID: "user-1", Email: "a@b.com", Role: "user"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))

	subjects := []string{"project.proj-1.>", "notification.>"}
	expanded := ws.expandSSEWildcards(req, subjects)

	assert.Equal(t, subjects, expanded,
		"subjects without wildcards in resource-ID position should pass through unchanged")
}

func TestExpandSSEWildcards_NotificationBroker_Passthrough(t *testing.T) {
	// notification.> and broker.> pass through without modification.
	mockStore := &mockAuthzStore{}
	ws := &WebServer{
		store:        mockStore,
		authzService: NewAuthzService(mockStore, nil),
	}
	req := httptest.NewRequest("GET", "/events", nil)
	user := &webSessionUser{UserID: "user-1", Email: "a@b.com", Role: "user"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))

	expanded := ws.expandSSEWildcards(req, []string{"notification.>", "broker.>"})

	assert.Equal(t, []string{"notification.>", "broker.>"}, expanded,
		"notification and broker wildcards should pass through unchanged")
}

func TestExpandSSEWildcards_MixedSubjects(t *testing.T) {
	// Mixed: project.> + project.<uuid>.> + notification.> + user.> — each handled correctly.
	mockStore := &mockAuthzStore{
		projects: []store.Project{
			{ID: "proj-1", OwnerID: "user-1"},
		},
		projectMemberships: map[string]*store.ProjectMembership{
			"proj-1:user-1": {ProjectID: "proj-1", UserID: "user-1", Role: store.ProjectRoleOwner},
		},
	}
	ws := &WebServer{
		store:        mockStore,
		authzService: NewAuthzService(mockStore, nil),
	}
	req := httptest.NewRequest("GET", "/events", nil)
	user := &webSessionUser{UserID: "user-1", Email: "a@b.com", Role: "user"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))

	expanded := ws.expandSSEWildcards(req, []string{
		"project.>",         // wildcard — should expand
		"project.proj-99.>", // specific — should pass through
		"notification.>",    // passthrough
		"broker.>",          // passthrough
	})

	assert.Contains(t, expanded, "project.proj-1.>",
		"project.> should expand to accessible project")
	assert.Contains(t, expanded, "project.proj-99.>",
		"specific project subject should pass through")
	assert.Contains(t, expanded, "notification.>")
	assert.Contains(t, expanded, "broker.>")
	assert.NotContains(t, expanded, "project.>")
}

// --- expandSSEWildcards: subject deduplication tests ---

func TestExpandSSEWildcards_DuplicateWildcardInput(t *testing.T) {
	// Duplicate wildcard input: ["project.>", "project.>", "notification.>"]
	// Each project.> independently expands to the same project subjects.
	// After dedup, each project.<uuid>.> should appear exactly once.
	mockStore := &mockAuthzStore{
		projects: []store.Project{
			{ID: "proj-1", OwnerID: "user-1"},
			{ID: "proj-2", OwnerID: "user-1"},
		},
		projectMemberships: map[string]*store.ProjectMembership{
			"proj-1:user-1": {ProjectID: "proj-1", UserID: "user-1", Role: store.ProjectRoleOwner},
			"proj-2:user-1": {ProjectID: "proj-2", UserID: "user-1", Role: store.ProjectRoleOwner},
		},
	}
	ws := &WebServer{
		store:        mockStore,
		authzService: NewAuthzService(mockStore, nil),
	}
	req := httptest.NewRequest("GET", "/events", nil)
	user := &webSessionUser{UserID: "user-1", Email: "a@b.com", Role: "user"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))

	expanded := ws.expandSSEWildcards(req, []string{"project.>", "project.>", "notification.>"})

	// Count occurrences of each subject.
	counts := make(map[string]int)
	for _, s := range expanded {
		counts[s]++
	}
	assert.Equal(t, 1, counts["project.proj-1.>"],
		"project.proj-1.> should appear exactly once after dedup")
	assert.Equal(t, 1, counts["project.proj-2.>"],
		"project.proj-2.> should appear exactly once after dedup")
	assert.Equal(t, 1, counts["notification.>"],
		"notification.> should appear exactly once")

	// Total: 2 projects + 1 notification = 3 (not 2*2 + 1 = 5)
	assert.Equal(t, 3, len(expanded),
		"deduped length should be len(projects) + 1, not 2*len(projects) + 1")
}

func TestExpandSSEWildcards_WildcardExplicitOverlap(t *testing.T) {
	// Wildcard + explicit overlap: ["project.>", "project.proj-1.>"]
	// Expansion produces project.proj-1.> from the wildcard, which overlaps
	// with the explicit project.proj-1.> already in the list.
	// After dedup, project.proj-1.> should appear exactly once.
	mockStore := &mockAuthzStore{
		projects: []store.Project{
			{ID: "proj-1", OwnerID: "user-1"},
			{ID: "proj-2", OwnerID: "user-1"},
		},
		projectMemberships: map[string]*store.ProjectMembership{
			"proj-1:user-1": {ProjectID: "proj-1", UserID: "user-1", Role: store.ProjectRoleOwner},
			"proj-2:user-1": {ProjectID: "proj-2", UserID: "user-1", Role: store.ProjectRoleOwner},
		},
	}
	ws := &WebServer{
		store:        mockStore,
		authzService: NewAuthzService(mockStore, nil),
	}
	req := httptest.NewRequest("GET", "/events", nil)
	user := &webSessionUser{UserID: "user-1", Email: "a@b.com", Role: "user"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))

	expanded := ws.expandSSEWildcards(req, []string{"project.>", "project.proj-1.>"})

	// Count occurrences.
	counts := make(map[string]int)
	for _, s := range expanded {
		counts[s]++
	}
	assert.Equal(t, 1, counts["project.proj-1.>"],
		"project.proj-1.> should appear exactly once (not twice from wildcard + explicit)")
	assert.Equal(t, 1, counts["project.proj-2.>"],
		"project.proj-2.> should appear exactly once from wildcard expansion")

	// Total: 2 unique project subjects (proj-1 deduped, proj-2 from wildcard)
	assert.Equal(t, 2, len(expanded),
		"deduped length should be 2, not 3")

	// The explicit subject should still be present (independently authorized).
	assert.Contains(t, expanded, "project.proj-1.>")
}

func TestExpandSSEWildcards_MixedNotificationDedup(t *testing.T) {
	// Mixed with notification dedup: ["project.>", "notification.>", "notification.>"]
	// notification.> appears twice in input — after dedup, only once.
	mockStore := &mockAuthzStore{
		projects: []store.Project{
			{ID: "proj-1", OwnerID: "user-1"},
		},
		projectMemberships: map[string]*store.ProjectMembership{
			"proj-1:user-1": {ProjectID: "proj-1", UserID: "user-1", Role: store.ProjectRoleOwner},
		},
	}
	ws := &WebServer{
		store:        mockStore,
		authzService: NewAuthzService(mockStore, nil),
	}
	req := httptest.NewRequest("GET", "/events", nil)
	user := &webSessionUser{UserID: "user-1", Email: "a@b.com", Role: "user"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))

	expanded := ws.expandSSEWildcards(req, []string{"project.>", "notification.>", "notification.>"})

	counts := make(map[string]int)
	for _, s := range expanded {
		counts[s]++
	}
	assert.Equal(t, 1, counts["notification.>"],
		"notification.> should appear exactly once after dedup")
	assert.Equal(t, 1, counts["project.proj-1.>"],
		"project.proj-1.> should appear exactly once")

	// Total: 1 project + 1 notification = 2 (not 1 + 2 = 3)
	assert.Equal(t, 2, len(expanded),
		"deduped length should be 2")
}

func TestAuthorizeSSESubjects_WildcardInResourceID_Denied(t *testing.T) {
	// Belt-and-suspenders: if a wildcard somehow reaches authorizeSSESubjects
	// in the resource-ID position, it must be denied.
	mockStore := mockSuperAdminStore("admin-1")
	ws := &WebServer{
		authzService: NewAuthzService(mockStore, nil),
	}
	req := httptest.NewRequest("GET", "/events", nil)
	user := &webSessionUser{UserID: "admin-1", Email: "admin@b.com", Role: "admin"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))

	denied := ws.authorizeSSESubjects(req, []string{"project.>", "user.>"})
	assert.Contains(t, denied, "project.>",
		"project.> must be denied in authorizeSSESubjects")
	assert.Contains(t, denied, "user.>",
		"user.> must be denied in authorizeSSESubjects")
}

func TestExpandSSEWildcards_UserWildcard_ExpandsToCallerID(t *testing.T) {
	// user.> should expand to user.<callerID>.>
	mockStore := &mockAuthzStore{}
	ws := &WebServer{
		store:        mockStore,
		authzService: NewAuthzService(mockStore, nil),
	}
	req := httptest.NewRequest("GET", "/events", nil)
	user := &webSessionUser{UserID: "user-1", Email: "a@b.com", Role: "user"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))

	expanded := ws.expandSSEWildcards(req, []string{"user.>"})

	assert.Equal(t, []string{"user.user-1.>"}, expanded,
		"user.> should expand to user.<callerID>.>")
}

func TestExpandSSEWildcards_NoSessionUser(t *testing.T) {
	// No session user → subjects pass through unchanged (authz will deny).
	mockStore := &mockAuthzStore{}
	ws := &WebServer{
		store:        mockStore,
		authzService: NewAuthzService(mockStore, nil),
	}
	req := httptest.NewRequest("GET", "/events", nil)
	// No user in context.

	subjects := []string{"project.>", "notification.>"}
	expanded := ws.expandSSEWildcards(req, subjects)

	assert.Equal(t, subjects, expanded,
		"without session user, subjects should pass through unchanged")
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

// --- validateSSESubjects: wildcard root rejection (belt-and-suspenders for F1) ---

func TestValidateSSESubjects_WildcardRootRejected(t *testing.T) {
	tests := []struct {
		name    string
		subject string
	}{
		{"bare >", ">"},
		{"bare *", "*"},
		{"*.>", "*.>"},
		{"*.*.chat.>", "*.*.chat.>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errMsg := validateSSESubjects([]string{tt.subject})
			assert.NotEmpty(t, errMsg, "wildcard root %q must fail syntax validation", tt.subject)
			assert.Contains(t, errMsg, "first token must not be a wildcard")
		})
	}
}

// mockSuperAdminStore returns a mockAuthzStore pre-configured with a
// super-admin role binding for the given user ID. This allows the AK1
// kernel to grant all permissions without a real database.
func mockSuperAdminStore(userID string) *mockAuthzStore {
	rdID := "mock-rd-super-admin"
	return &mockAuthzStore{
		roleBindings: []*store.RoleBinding{{
			ID:               "mock-rb-super-admin",
			RoleDefinitionID: rdID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      userID,
			ScopeType:        store.RoleScopeSystem,
			CreatedBy:        store.SystemReconcileCreatedBy,
		}},
		roleDefinitions: map[string]*store.RoleDefinition{
			rdID: {
				ID:          rdID,
				Name:        store.SystemRoleSuperAdmin,
				ScopeType:   store.RoleScopeSystem,
				Permissions: allPermissionIDs(),
			},
		},
	}
}

// --- mock store for authz tests ---

// mockAuthzStore satisfies store.Store for NewAuthzService. Only the methods
// called by ComputeCapabilitiesBatch's dependency chain are implemented;
// the embedded interface satisfies everything else at the signature level.
//
// Optional fields allow tests to inject role bindings and role definitions
// so the CO1 kernel can resolve permissions without a real database.
type mockAuthzStore struct {
	store.Store // embed to satisfy interface

	roleBindings          []*store.RoleBinding
	roleDefinitions       map[string]*store.RoleDefinition
	projects              []store.Project                     // injectable project list for wildcard expansion tests
	projectMemberships    map[string]*store.ProjectMembership // key: "projectID:userID"
	listProjectsReturnNil bool                                // when true, ListProjects returns (nil, nil)
}

func (m *mockAuthzStore) GetEffectiveGroups(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (m *mockAuthzStore) GetEffectiveGroupsForAgent(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (m *mockAuthzStore) ListGroups(_ context.Context, _ store.GroupFilter, _ store.ListOptions) (*store.ListResult[store.Group], error) {
	return &store.ListResult[store.Group]{}, nil
}

func (m *mockAuthzStore) GetProject(_ context.Context, _ string) (*store.Project, error) {
	return nil, store.ErrNotFound
}

func (m *mockAuthzStore) GetGroupBySlug(_ context.Context, _ string) (*store.Group, error) {
	return nil, store.ErrNotFound
}

func (m *mockAuthzStore) GetGroupMembership(_ context.Context, _, _ string, _ string) (*store.GroupMember, error) {
	return nil, store.ErrNotFound
}

func (m *mockAuthzStore) GetProjectMembership(_ context.Context, projectID, userID string) (*store.ProjectMembership, error) {
	if m.projectMemberships != nil {
		key := projectID + ":" + userID
		if pm, ok := m.projectMemberships[key]; ok {
			return pm, nil
		}
	}
	return nil, store.ErrNotFound
}

func (m *mockAuthzStore) ListRoleBindingsForPrincipal(_ context.Context, _, _ string) ([]*store.RoleBinding, error) {
	return nil, nil
}

func (m *mockAuthzStore) GetRoleDefinition(_ context.Context, id string) (*store.RoleDefinition, error) {
	if m.roleDefinitions != nil {
		if rd, ok := m.roleDefinitions[id]; ok {
			return rd, nil
		}
	}
	return nil, store.ErrNotFound
}

func (m *mockAuthzStore) GetRoleDefinitionsByIDs(_ context.Context, ids []string) (map[string]*store.RoleDefinition, error) {
	result := make(map[string]*store.RoleDefinition, len(ids))
	for _, id := range ids {
		if m.roleDefinitions != nil {
			if rd, ok := m.roleDefinitions[id]; ok {
				result[id] = rd
			}
		}
	}
	return result, nil
}

func (m *mockAuthzStore) ListRoleBindingsForPrincipals(_ context.Context, _ []store.PrincipalRef, _ []string, _ []string) ([]*store.RoleBinding, error) {
	return m.roleBindings, nil
}

func (m *mockAuthzStore) ListAccessConstraints(_ context.Context, _, _ int) ([]*store.AccessConstraint, error) {
	return nil, nil
}

func (m *mockAuthzStore) ListProjects(_ context.Context, _ store.ProjectFilter, _ store.ListOptions) (*store.ListResult[store.Project], error) {
	if m.listProjectsReturnNil {
		return nil, nil
	}
	items := m.projects
	if items == nil {
		items = []store.Project{}
	}
	return &store.ListResult[store.Project]{Items: items, TotalCount: len(items)}, nil
}
