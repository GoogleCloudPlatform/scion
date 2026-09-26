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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Regression tests for SSE subject authorization's default-deny posture ---
//
// authorizeSSESubjects denies any subject whose first token is not on an
// explicit allowlist: project, user and agent each get a per-resource check;
// broker and notification are a deliberate pass-through; system and admin
// get the checks described at their cases in web.go. Every other first
// token — including the legacy grove.<projectId>.* duplicate subjects
// (never published; see events.go) and any future namespace — is denied.
// These tests pin down that allowlist.

func TestAuthorizeSSESubjects_UnknownNamespaceDeniedByDefault(t *testing.T) {
	ws := &WebServer{authzService: NewAuthzService(&mockAuthzStore{}, nil)}
	req := httptest.NewRequest("GET", "/events", nil)
	user := &webSessionUser{UserID: "user-1", Email: "a@b.com", Role: "user"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))

	// Concrete (non-wildcard) subjects so expandSSEWildcards passes them
	// through unchanged and authorizeSSESubjects is the thing under test.
	subjects := []string{
		"grove.proj-1.user.message", // legacy duplicate subject; never published
		"grove.proj-1.created",
		"foo.bar",
	}
	denied := ws.authorizeSSESubjects(req, subjects)
	assert.ElementsMatch(t, subjects, denied,
		"unknown namespaces, including the legacy grove.* duplicate subjects, must be denied by default")
}

func TestAuthorizeSSESubjects_SystemImages(t *testing.T) {
	ws := &WebServer{authzService: NewAuthzService(&mockAuthzStore{}, nil)}
	req := httptest.NewRequest("GET", "/events", nil)
	user := &webSessionUser{UserID: "user-1", Email: "a@b.com", Role: "user"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))

	// A concrete job ID is allowed (the web client's image-pull progress view).
	assert.Empty(t, ws.authorizeSSESubjects(req, []string{"system.images.job-123"}),
		"system.images.<jobId> with a concrete job ID must be allowed")

	for _, sub := range []string{
		"system.images.>",           // wildcard job ID
		"system.images.*",           // wildcard job ID
		"system.images.job-123.>",   // trailing tokens past the job ID
		"system.images.job-123.foo", // trailing tokens past the job ID
		"system.foo",                // unknown system subcategory
		"system",                    // no subcategory at all
	} {
		t.Run(sub, func(t *testing.T) {
			assert.Equal(t, []string{sub}, ws.authorizeSSESubjects(req, []string{sub}),
				"subject %q must be denied", sub)
		})
	}
}

func TestAuthorizeSSESubjects_AdminNamespaceRequiresAdminRole(t *testing.T) {
	ws := &WebServer{authzService: NewAuthzService(&mockAuthzStore{}, nil)}
	subject := "admin.settings.updated"

	userReq := httptest.NewRequest("GET", "/events", nil)
	user := &webSessionUser{UserID: "user-1", Email: "a@b.com", Role: "user"}
	userReq = userReq.WithContext(context.WithValue(userReq.Context(), webUserContextKey{}, user))
	assert.Equal(t, []string{subject}, ws.authorizeSSESubjects(userReq, []string{subject}),
		"admin.* must be denied for a non-admin session")

	adminReq := httptest.NewRequest("GET", "/events", nil)
	admin := &webSessionUser{UserID: "admin-1", Email: "admin@b.com", Role: "admin"}
	adminReq = adminReq.WithContext(context.WithValue(adminReq.Context(), webUserContextKey{}, admin))
	assert.Empty(t, ws.authorizeSSESubjects(adminReq, []string{subject}),
		"admin.* must be allowed for an admin session")
}

// TestSSEHandler_LegacyAndUnknownSubjectsDenied is an end-to-end regression
// test: a non-member gets 403 for grove.>, grove.<otherId>.> and an unknown
// prefix such as foo.>.
func TestSSEHandler_LegacyAndUnknownSubjectsDenied(t *testing.T) {
	const userID = "user-1"
	victimProject := tid("legacy-victim-project")

	s := &mockAuthzStore{
		projects: []store.Project{{ID: victimProject, OwnerID: "other-user"}},
	}

	subjects := []string{
		"grove.>",
		"grove." + victimProject + ".>",
		"foo.>",
		"project." + victimProject + ".>", // control: still denied for a non-member
	}

	for _, sub := range subjects {
		t.Run(sub, func(t *testing.T) {
			pub := NewChannelEventPublisher()
			defer pub.Close()
			ws := &WebServer{store: s, events: pub, authzService: NewAuthzService(s, nil)}
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // bounds the streaming loop; only the initial authz decision matters here
			w := httptest.NewRecorder()
			ws.handleSSE(w, sseAgentRequest(ctx, userID, "user", sub))
			assert.Equal(t, http.StatusForbidden, w.Code, "subject %q must be denied", sub)
		})
	}
}

// TestSSEHandler_MixedUnknownAndAllowedSubjectDenied pins down the invariant:
// an unknown subject alongside an allowed one must deny the whole request
// (403) and name only the unknown subject; it must not be silently dropped
// from the subscription. This holds for both a wildcard unknown subject
// (e.g. grove.>) and a concrete one.
func TestSSEHandler_MixedUnknownAndAllowedSubjectDenied(t *testing.T) {
	const userID = "user-1"
	ownProject := tid("mixed-own-project")

	s := &mockAuthzStore{
		projects: []store.Project{{ID: ownProject, OwnerID: userID}},
		projectMemberships: map[string]*store.ProjectMembership{
			ownProject + ":" + userID: {ProjectID: ownProject, UserID: userID, Role: store.ProjectRoleOwner},
		},
	}

	for _, tc := range []struct {
		name    string
		unknown string
	}{
		{"wildcard unknown subject", "grove.>"},
		{"concrete unknown subject", "grove." + ownProject + ".created"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pub := NewChannelEventPublisher()
			defer pub.Close()
			ws := &WebServer{store: s, events: pub, authzService: NewAuthzService(s, nil)}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			w := httptest.NewRecorder()
			ws.handleSSE(w, sseAgentRequest(ctx, userID, "user", tc.unknown, "project."+ownProject+".>"))
			assert.Equal(t, http.StatusForbidden, w.Code,
				"an unknown subject alongside an allowed one must deny the whole request")

			var body struct {
				Denied []string `json:"denied_subjects"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			assert.Equal(t, []string{tc.unknown}, body.Denied,
				"only the unknown subject should be named as denied; the allowed one is not silently dropped")
		})
	}
}

// TestSSEHandler_AdminWildcardRequiresAdminRole covers the flip side of the
// admin-role gate: admin.> is not expanded (admin has no resource ID), so it
// reaches the admin-role gate unchanged: allowed for an admin session, denied
// for everyone else.
func TestSSEHandler_AdminWildcardRequiresAdminRole(t *testing.T) {
	const userID = "user-1"

	for _, tc := range []struct {
		role   string
		denied bool
	}{
		{role: "admin", denied: false},
		{role: "user", denied: true},
	} {
		t.Run(tc.role, func(t *testing.T) {
			s := &mockAuthzStore{}
			pub := NewChannelEventPublisher()
			defer pub.Close()
			ws := &WebServer{store: s, events: pub, authzService: NewAuthzService(s, nil)}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			w := httptest.NewRecorder()
			ws.handleSSE(w, sseAgentRequest(ctx, userID, tc.role, "admin.>"))
			if tc.denied {
				assert.Equal(t, http.StatusForbidden, w.Code, "admin.> must be denied for a non-admin session")
			} else {
				assert.Equal(t, http.StatusOK, w.Code, "admin.> must be allowed for an admin session")
			}
		})
	}
}

// TestSSEHandler_InventorySubjectsAllowed asserts that every subject the real
// web client subscribes to (per the grep-based inventory of web/src) is
// allowed by the default-deny allowlist: project feeds, per-user
// notification/chat subjects, per-agent streams, the notification and broker
// pass-throughs, and image-build progress.
func TestSSEHandler_InventorySubjectsAllowed(t *testing.T) {
	const userID = "user-1"
	ownProject := tid("inventory-own-project")
	agentID := tid("inventory-agent")
	jobID := tid("inventory-job")

	s := &sseAgentStore{
		mockAuthzStore: &mockAuthzStore{
			projects: []store.Project{{ID: ownProject, OwnerID: userID}},
			projectMemberships: map[string]*store.ProjectMembership{
				ownProject + ":" + userID: {ProjectID: ownProject, UserID: userID, Role: store.ProjectRoleOwner},
			},
		},
		agents: map[string]*store.Agent{
			agentID: {ID: agentID, ProjectID: ownProject, OwnerID: userID},
		},
	}

	subjects := []string{
		"project.>",                          // dashboard: all accessible projects, expanded then authorized
		"project." + ownProject + ".>",       // per-project feed (scope=project / space)
		"project." + ownProject + ".chat.>",  // per-project chat feed
		"project." + ownProject + ".agent.>", // per-project agent feed (chat members sidebar)
		"user." + userID + ".notification",
		"user." + userID + ".chat.>",
		"agent." + agentID + ".>", // per-agent stream (scope=agent)
		"notification.>",          // notification tray
		"notification.created",
		"broker.>",               // broker pages
		"system.images." + jobID, // image pull progress
	}

	for _, sub := range subjects {
		t.Run(sub, func(t *testing.T) {
			pub := NewChannelEventPublisher()
			defer pub.Close()
			ws := &WebServer{store: s, events: pub, authzService: NewAuthzService(s, nil)}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			w := httptest.NewRecorder()
			ws.handleSSE(w, sseAgentRequest(ctx, userID, "user", sub))
			assert.Equal(t, http.StatusOK, w.Code, "legitimate subject %q must remain allowed", sub)
		})
	}
}
