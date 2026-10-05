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
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestPaginatedLists_MalformedCursorReturns400 sends a malformed cursor
// (?cursor, or ?pageToken where the endpoint uses that name) to every hub
// list endpoint whose cursor reaches an entadapter cursor decoder
// (decodeListCursor, decodeCursor, decodeConstraintCursor, or a UUID ID
// cursor) and asserts 400, never 500 (ptone/scion#1957). Some endpoints
// validate or unseal the cursor before the store sees it; others (runtime
// brokers, skills, messages, schedules, admin invites, access constraints)
// hand it straight to the store, which is where the decoders'
// store.ErrInvalidInput wrapping matters. Not listed: endpoints backed by
// ListUsers (users, admin allow-list), whose numeric offset cursor ignores
// unparseable input by design, and the access-constraint principals and
// audit-history pageTokens, which the hub decodes itself and already rejects
// with 400.
func TestPaginatedLists_MalformedCursorReturns400(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := uuid.NewString()
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "Cursor Project", Slug: "cursor-project",
	}))

	agentID := uuid.NewString()
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "cursor-agent", Name: "Cursor Agent", ProjectID: projectID,
		Phase: string(state.PhaseStopped), CreatedBy: DevUserID, OwnerID: DevUserID,
	}))

	// DM reads authorize on the canonical DM key, which names a real user
	// principal (the dev identity's kind is "dev", never a DM participant).
	alice := &store.User{
		ID: uuid.NewString(), Email: "cursor-alice@test.com", DisplayName: "Alice",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, alice))
	dmKey, err := messages.DMConversationKey("user", alice.ID, "user", uuid.NewString())
	require.NoError(t, err)
	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind: "direct", Surface: "native", ExternalRef: dmKey, DriftState: "active",
	})
	require.NoError(t, err)

	enc := func(raw string) string { return base64.URLEncoding.EncodeToString([]byte(raw)) }
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	cursors := map[string]string{
		"not base64":    "not-base64-!!!",
		"padding only":  "====",
		"wrong shape":   enc("some-garbage"),
		"bad timestamp": enc("not-a-timestamp," + uuid.NewString()),
		"bad id":        enc(ts + ",not-a-uuid"),
	}

	type endpoint struct {
		path   string
		param  string      // cursor query parameter; "" means "cursor"
		caller *store.User // nil: dev (admin) identity
	}
	endpoints := map[string]endpoint{
		"agents":          {path: "/api/v1/agents"},
		"projects":        {path: "/api/v1/projects"},
		"project agents":  {path: "/api/v1/projects/" + projectID + "/agents"},
		"templates":       {path: "/api/v1/templates"},
		"harness configs": {path: "/api/v1/harness-configs"},
		"groups":          {path: "/api/v1/groups"},
		"skills":          {path: "/api/v1/skills"},
		"runtime brokers": {path: "/api/v1/runtime-brokers"},
		// decodeCursor (unbound) callers: messages and schedules.
		"messages":              {path: "/api/v1/messages"},
		"agent messages":        {path: "/api/v1/agents/" + agentID + "/messages"},
		"conversation messages": {path: "/api/v1/conversations/" + conv.ID + "/messages", caller: alice},
		"chat history":          {path: "/api/v1/chat/conversations/" + dmKey + "/messages", caller: alice},
		"schedules":             {path: "/api/v1/projects/" + projectID + "/schedules"},
		// UUID cursor.
		"scheduled events": {path: "/api/v1/projects/" + projectID + "/scheduled-events"},
		"admin invites":    {path: "/api/v1/admin/invites"},
		// decodeConstraintCursor, read from ?pageToken.
		"access constraints": {path: "/api/v1/admin/access-constraints", param: "pageToken"},
	}

	for epName, ep := range endpoints {
		for cName, cursor := range cursors {
			t.Run(epName+"/"+cName, func(t *testing.T) {
				param := ep.param
				if param == "" {
					param = "cursor"
				}
				path := ep.path + "?" + url.Values{param: {cursor}}.Encode()
				var rec *httptest.ResponseRecorder
				if ep.caller != nil {
					rec = doRequestAsUser(t, srv, ep.caller, http.MethodGet, path, nil)
				} else {
					rec = doRequest(t, srv, http.MethodGet, path, nil)
				}
				assert.Equal(t, http.StatusBadRequest, rec.Code,
					"malformed cursor must be a 400, not a %d; body: %s", rec.Code, rec.Body.String())
			})
		}
	}
}

// TestAdminInvitesList_UnknownCursorReturns400: a well-formed UUID cursor that
// names no invite (deleted since the page was issued, or never issued) is a
// bad cursor, so 400, not a 404 for the list itself or a 500.
// ptone/scion#1957.
func TestAdminInvitesList_UnknownCursorReturns400(t *testing.T) {
	srv, _ := testServer(t)
	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/invites?"+url.Values{"cursor": {uuid.NewString()}}.Encode(), nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
}
