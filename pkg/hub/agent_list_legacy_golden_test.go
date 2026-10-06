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
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file pin the legacy (no sort parameter) agent list
// contract byte for byte: response bodies for fixed data, and the cursor
// binding input for a fixed filter and identity. The golden values were
// captured from the implementation before sorted mode existed and must
// never change as a side effect of sorted-mode work. Intentional contract
// additions update them: provisionedOnly is always sent, false included
// (ptone/scion#2929).

// legacyGoldenSetup seeds fixed users, one project and three agents with
// fixed IDs and fixed stored timestamps, and returns the server and caller.
func legacyGoldenSetup(t *testing.T) (*Server, *store.User, string) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	userID := tid("legacy-golden-admin")
	createTestUserWithRole(t, s, userID, "legacy-golden-admin@test.com", store.UserRoleMember, store.SystemRoleHubAdmin)
	user, err := s.GetUser(ctx, userID)
	require.NoError(t, err)

	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	projectID := tid("legacy-golden-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "Legacy Golden", Slug: "legacy-golden",
		OwnerID: userID, CreatedBy: userID, Created: base, Updated: base,
	}))
	createTestUserWithProjectRole(t, s, userID, user.Email, projectID, store.ProjectRoleAdmin)

	db, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "store must expose DB()")
	for i, slug := range []string{"golden-a", "golden-b", "golden-c"} {
		a := &store.Agent{
			ID: tid("legacy-golden-" + slug), Slug: slug, Name: slug,
			ProjectID: projectID, Phase: []string{"running", "stopped", "stopped"}[i],
			Labels:    map[string]string{"team": []string{"a", "b", "a"}[i]},
			CreatedBy: userID, OwnerID: userID,
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		created := base.Add(time.Duration(i) * 1500 * time.Millisecond)
		updated := base.Add(time.Duration(10-i) * time.Second)
		_, err := db.DB().ExecContext(ctx,
			"UPDATE agents SET created = ?, updated = ?, last_activity_event = NULL WHERE id = ?",
			created.String(), updated.String(), a.ID)
		require.NoError(t, err)
	}
	return srv, user, projectID
}

func TestListAgentsLegacy_ResponseBodyMatchesGolden(t *testing.T) {
	srv, user, projectID := legacyGoldenSetup(t)

	for _, tc := range []struct {
		name, path, want string
	}{
		{"global", "/api/v1/agents", legacyGoldenGlobalBody},
		{"global filtered", "/api/v1/agents?phase=stopped&label=team=a", legacyGoldenGlobalFilteredBody},
		{"project", "/api/v1/projects/" + projectID + "/agents", legacyGoldenProjectBody},
		{"global invalid label", "/api/v1/agents?label=noequals", legacyGoldenInvalidLabelBody},
		// Without sort, view values other than compact are ignored.
		{"global view=full", "/api/v1/agents?view=full", legacyGoldenGlobalBody},
		{"global view=bogus", "/api/v1/agents?view=bogus", legacyGoldenGlobalBody},
		{"global filtered view=full", "/api/v1/agents?phase=stopped&label=team=a&view=full", legacyGoldenGlobalFilteredBody},
		{"project view=full", "/api/v1/projects/" + projectID + "/agents?view=full", legacyGoldenProjectBody},
		{"project view=bogus", "/api/v1/projects/" + projectID + "/agents?view=bogus", legacyGoldenProjectBody},
		{"global invalid label view=bogus", "/api/v1/agents?label=noequals&view=bogus", legacyGoldenInvalidLabelBody},
	} {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, tc.path, nil)
		got := string(rawBodyWithoutServerTime(rec))
		assert.Equal(t, tc.want+"\n", got, "%s: legacy response body changed", tc.name)
	}
}

func TestListAgentsLegacy_CursorBindingMatchesGolden(t *testing.T) {
	identity := NewAuthenticatedUser(tid("legacy-golden-admin"), "legacy-golden-admin@test.com", "Golden", store.UserRoleMember, "api")
	filter := store.AgentFilter{
		ProjectID: tid("legacy-golden-project"),
		Phase:     "stopped",
		Labels:    map[string]string{"team": "a"},
	}
	assert.Equal(t, legacyGoldenGlobalBinding, scopedCursorBinding("agents", filter, identity))
	assert.Equal(t, legacyGoldenProjectBinding, scopedCursorBinding("project-agents:"+tid("legacy-golden-project"), filter, identity))
}

const (
	legacyGoldenGlobalBody         = `{"agents":[{"id":"cf6457ad-8d23-59cf-9931-f0bfc148b4e0","slug":"golden-c","name":"golden-c","template":"","projectId":"e6af107e-ac4d-5d7c-bf85-763c577c5e52","labels":{"team":"a"},"phase":"stopped","startedAt":"0001-01-01T00:00:00Z","detached":false,"project":"Legacy Golden","created":"2026-01-02T03:04:08Z","updated":"2026-01-02T03:04:13Z","lastSeen":"0001-01-01T00:00:00Z","lastActivityEvent":"0001-01-01T00:00:00Z","deletedAt":"0001-01-01T00:00:00Z","createdBy":"40bbe97f-7ae9-5be8-a3ac-02cb51f5c24e","ownerId":"40bbe97f-7ae9-5be8-a3ac-02cb51f5c24e","messageMode":"project","stateVersion":1,"generation":1,"provisionedOnly":false,"deletion":null,"_capabilities":{"actions":["read","update","delete","attach","lifecycle","port_access","set_message_mode","grant_hub_mode"]},"_messageability":{"canMessage":true,"canReachViewer":true}},{"id":"93c59c6a-7dd1-5b7d-aafc-da568be572bb","slug":"golden-b","name":"golden-b","template":"","projectId":"e6af107e-ac4d-5d7c-bf85-763c577c5e52","labels":{"team":"b"},"phase":"stopped","startedAt":"0001-01-01T00:00:00Z","detached":false,"project":"Legacy Golden","created":"2026-01-02T03:04:06.5Z","updated":"2026-01-02T03:04:14Z","lastSeen":"0001-01-01T00:00:00Z","lastActivityEvent":"0001-01-01T00:00:00Z","deletedAt":"0001-01-01T00:00:00Z","createdBy":"40bbe97f-7ae9-5be8-a3ac-02cb51f5c24e","ownerId":"40bbe97f-7ae9-5be8-a3ac-02cb51f5c24e","messageMode":"project","stateVersion":1,"generation":1,"provisionedOnly":false,"deletion":null,"_capabilities":{"actions":["read","update","delete","attach","lifecycle","port_access","set_message_mode","grant_hub_mode"]},"_messageability":{"canMessage":true,"canReachViewer":true}},{"id":"6b249bcb-1530-59d7-b4a7-cf789a4f5b58","slug":"golden-a","name":"golden-a","template":"","projectId":"e6af107e-ac4d-5d7c-bf85-763c577c5e52","labels":{"team":"a"},"phase":"running","startedAt":"0001-01-01T00:00:00Z","detached":false,"project":"Legacy Golden","created":"2026-01-02T03:04:05Z","updated":"2026-01-02T03:04:15Z","lastSeen":"0001-01-01T00:00:00Z","lastActivityEvent":"0001-01-01T00:00:00Z","deletedAt":"0001-01-01T00:00:00Z","createdBy":"40bbe97f-7ae9-5be8-a3ac-02cb51f5c24e","ownerId":"40bbe97f-7ae9-5be8-a3ac-02cb51f5c24e","messageMode":"project","stateVersion":1,"generation":1,"provisionedOnly":false,"deletion":null,"_capabilities":{"actions":["read","update","delete","attach","lifecycle","port_access","set_message_mode","grant_hub_mode"]},"_messageability":{"canMessage":true,"canReachViewer":true}}],"totalCount":3,"serverTime":"","_capabilities":{"actions":["create"]}}`
	legacyGoldenGlobalFilteredBody = `{"agents":[{"id":"cf6457ad-8d23-59cf-9931-f0bfc148b4e0","slug":"golden-c","name":"golden-c","template":"","projectId":"e6af107e-ac4d-5d7c-bf85-763c577c5e52","labels":{"team":"a"},"phase":"stopped","startedAt":"0001-01-01T00:00:00Z","detached":false,"project":"Legacy Golden","created":"2026-01-02T03:04:08Z","updated":"2026-01-02T03:04:13Z","lastSeen":"0001-01-01T00:00:00Z","lastActivityEvent":"0001-01-01T00:00:00Z","deletedAt":"0001-01-01T00:00:00Z","createdBy":"40bbe97f-7ae9-5be8-a3ac-02cb51f5c24e","ownerId":"40bbe97f-7ae9-5be8-a3ac-02cb51f5c24e","messageMode":"project","stateVersion":1,"generation":1,"provisionedOnly":false,"deletion":null,"_capabilities":{"actions":["read","update","delete","attach","lifecycle","port_access","set_message_mode","grant_hub_mode"]},"_messageability":{"canMessage":true,"canReachViewer":true}}],"totalCount":1,"serverTime":"","_capabilities":{"actions":["create"]}}`
	legacyGoldenProjectBody        = `{"agents":[{"id":"cf6457ad-8d23-59cf-9931-f0bfc148b4e0","slug":"golden-c","name":"golden-c","template":"","projectId":"e6af107e-ac4d-5d7c-bf85-763c577c5e52","labels":{"team":"a"},"phase":"stopped","startedAt":"0001-01-01T00:00:00Z","detached":false,"project":"Legacy Golden","created":"2026-01-02T03:04:08Z","updated":"2026-01-02T03:04:13Z","lastSeen":"0001-01-01T00:00:00Z","lastActivityEvent":"0001-01-01T00:00:00Z","deletedAt":"0001-01-01T00:00:00Z","createdBy":"40bbe97f-7ae9-5be8-a3ac-02cb51f5c24e","ownerId":"40bbe97f-7ae9-5be8-a3ac-02cb51f5c24e","messageMode":"project","stateVersion":1,"generation":1,"provisionedOnly":false,"deletion":null,"_capabilities":{"actions":["read","update","delete","attach","lifecycle","port_access","set_message_mode","grant_hub_mode"]}},{"id":"93c59c6a-7dd1-5b7d-aafc-da568be572bb","slug":"golden-b","name":"golden-b","template":"","projectId":"e6af107e-ac4d-5d7c-bf85-763c577c5e52","labels":{"team":"b"},"phase":"stopped","startedAt":"0001-01-01T00:00:00Z","detached":false,"project":"Legacy Golden","created":"2026-01-02T03:04:06.5Z","updated":"2026-01-02T03:04:14Z","lastSeen":"0001-01-01T00:00:00Z","lastActivityEvent":"0001-01-01T00:00:00Z","deletedAt":"0001-01-01T00:00:00Z","createdBy":"40bbe97f-7ae9-5be8-a3ac-02cb51f5c24e","ownerId":"40bbe97f-7ae9-5be8-a3ac-02cb51f5c24e","messageMode":"project","stateVersion":1,"generation":1,"provisionedOnly":false,"deletion":null,"_capabilities":{"actions":["read","update","delete","attach","lifecycle","port_access","set_message_mode","grant_hub_mode"]}},{"id":"6b249bcb-1530-59d7-b4a7-cf789a4f5b58","slug":"golden-a","name":"golden-a","template":"","projectId":"e6af107e-ac4d-5d7c-bf85-763c577c5e52","labels":{"team":"a"},"phase":"running","startedAt":"0001-01-01T00:00:00Z","detached":false,"project":"Legacy Golden","created":"2026-01-02T03:04:05Z","updated":"2026-01-02T03:04:15Z","lastSeen":"0001-01-01T00:00:00Z","lastActivityEvent":"0001-01-01T00:00:00Z","deletedAt":"0001-01-01T00:00:00Z","createdBy":"40bbe97f-7ae9-5be8-a3ac-02cb51f5c24e","ownerId":"40bbe97f-7ae9-5be8-a3ac-02cb51f5c24e","messageMode":"project","stateVersion":1,"generation":1,"provisionedOnly":false,"deletion":null,"_capabilities":{"actions":["read","update","delete","attach","lifecycle","port_access","set_message_mode","grant_hub_mode"]}}],"totalCount":3,"serverTime":"","_capabilities":{"actions":["create","list","stop_all","message"]}}`
	legacyGoldenInvalidLabelBody   = `{"error":{"code":"invalid_request","message":"invalid label filter \"noequals\": must be key=value"}}`
	legacyGoldenGlobalBinding      = `FxvwNElGLy4UiETPZsga7x4TH2STYUGjrbch5m9_ibE`
	legacyGoldenProjectBinding     = `5FKf1QPrR-n-XkaY--H_XhdwMFnr05mIS1r5Z1YYU84`
)
