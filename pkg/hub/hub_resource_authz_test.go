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
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newOtherProjectMember creates a second project owned by owner and returns a
// member of it who has no role in any other project.
func newOtherProjectMember(t *testing.T, srv *Server, s store.Store, owner *store.User, slug string) *store.User {
	t.Helper()
	ctx := context.Background()
	other := &store.Project{ID: tid(slug), Name: slug, Slug: slug,
		OwnerID: owner.ID, CreatedBy: owner.ID, Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, other))
	srv.createProjectMembersGroup(ctx, other)
	return makeProjectMemberUser(t, s, other, tid(slug+"-member"), "Other Member", store.GroupMemberRoleMember)
}

// --- Agent status ---------------------------------------------------------

func TestAgentStatusAuthz(t *testing.T) {
	srv, s, alice, bob, victim := setupTemplateAuthzTest(t)
	ctx := context.Background()
	dave := newOtherProjectMember(t, srv, s, alice, "status-other")

	n := 0
	mkAgent := func() *store.Agent {
		n++
		a := &store.Agent{ID: tid("status-agent-" + string(rune('a'+n))), ProjectID: victim.ID,
			Name: "st" + string(rune('a'+n)), Slug: "st" + string(rune('a'+n)),
			Phase: "running", Activity: "working", OwnerID: alice.ID, CreatedBy: alice.ID}
		require.NoError(t, s.CreateAgent(ctx, a))
		return a
	}
	routes := map[string]func(*store.Agent) string{
		"agent route":   func(a *store.Agent) string { return "/api/v1/agents/" + a.ID + "/status" },
		"project route": func(a *store.Agent) string { return "/api/v1/projects/" + victim.ID + "/agents/" + a.Slug + "/status" },
	}
	body := map[string]string{"phase": "stopped", "activity": "crashed", "message": "not yours"}

	for routeName, url := range routes {
		for _, who := range []struct {
			name string
			user *store.User
		}{{"non-hub-member", bob}, {"other-project member", dave}} {
			t.Run(routeName+"/"+who.name, func(t *testing.T) {
				a := mkAgent()
				rec := doRequestAsUser(t, srv, who.user, http.MethodPost, url(a), body)
				assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
				got, err := s.GetAgent(ctx, a.ID)
				require.NoError(t, err)
				assert.Equal(t, "running", got.Phase, "status must be unchanged")
				assert.Equal(t, "working", got.Activity, "status must be unchanged")
			})
		}
		t.Run(routeName+"/owner allowed", func(t *testing.T) {
			a := mkAgent()
			rec := doRequestAsUser(t, srv, alice, http.MethodPost, url(a),
				map[string]string{"message": "owner note"})
			assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		})
	}
}

// --- Harness configs ------------------------------------------------------

const harnessConfigSecret = "HC-PRIVATE-BYTES"

// newProjectHarnessConfig stores a project-scoped harness config with one
// file in project, owned by owner, and returns it.
func newProjectHarnessConfig(t *testing.T, s store.Store, stor *contentMockStorage, project *store.Project, owner *store.User) *store.HarnessConfig {
	t.Helper()
	ctx := context.Background()
	hc := &store.HarnessConfig{
		ID: tid("hc-authz"), Name: "private-hc", Slug: "private-hc", Harness: "claude",
		Scope: store.HarnessConfigScopeProject, ScopeID: project.ID,
		OwnerID: owner.ID, CreatedBy: owner.ID,
		Status:      store.HarnessConfigStatusActive,
		StoragePath: "harness-configs/projects/" + project.ID + "/private-hc", StorageBucket: "test-bucket",
		Config:  &store.HarnessConfigData{Image: "private-image:latest"},
		Created: time.Now(), Updated: time.Now(),
	}
	content := "FROM scratch\n# " + harnessConfigSecret + "\n"
	key := hc.StoragePath + "/Dockerfile"
	stor.content[key] = []byte(content)
	stor.objects[key] = &storage.Object{Name: key, Size: int64(len(content))}
	hc.Files = []store.TemplateFile{{Path: "Dockerfile", Size: int64(len(content))}}
	require.NoError(t, s.CreateHarnessConfig(ctx, hc))
	return hc
}

func TestHarnessConfigAuthz_OutsidersDenied(t *testing.T) {
	srv, s, alice, bob, victim := setupTemplateAuthzTest(t)
	stor := newContentMockStorage("test-bucket")
	srv.SetStorage(stor)
	dave := newOtherProjectMember(t, srv, s, alice, "hc-other")
	hc := newProjectHarnessConfig(t, s, stor, victim, alice)
	base := "/api/v1/harness-configs/" + hc.ID
	key := hc.StoragePath + "/Dockerfile"
	original := string(stor.content[key])

	// Reads are listed first. Hub members may read harness configs hub-wide
	// by policy (system-scope harness_config.read), so reads are asserted
	// only for a caller outside the hub; writes are asserted for both.
	type hcCase struct {
		name, method, url string
		body              any
	}
	reads := []hcCase{
		{"get", http.MethodGet, base, nil},
		{"files list", http.MethodGet, base + "/files", nil},
		{"file read", http.MethodGet, base + "/files/Dockerfile", nil},
		{"download", http.MethodGet, base + "/download", nil},
		{"validate", http.MethodGet, base + "/validate", nil},
		{"image-status", http.MethodGet, base + "/image-status", nil},
	}
	writes := []hcCase{
		{"update", http.MethodPut, base, map[string]string{"name": "renamed"}},
		{"patch", http.MethodPatch, base, map[string]string{"name": "renamed"}},
		{"delete", http.MethodDelete, base, nil},
		{"files upload", http.MethodPost, base + "/files", nil},
		{"file write", http.MethodPut, base + "/files/Dockerfile", map[string]string{"content": "FROM other\n"}},
		{"file delete", http.MethodDelete, base + "/files/Dockerfile", nil},
		{"clone", http.MethodPost, base + "/clone", map[string]string{"name": "copy"}},
		{"reimport", http.MethodPost, base + "/reimport", map[string]string{"sourceUrl": "https://github.com/example/repo/tree/main/hc"}},
		{"upload", http.MethodPost, base + "/upload", map[string]any{"files": []any{}}},
		{"finalize", http.MethodPost, base + "/finalize", map[string]any{"manifest": map[string]any{}}},
		{"check-image", http.MethodPost, base + "/check-image", nil},
		{"local-image", http.MethodDelete, base + "/local-image", nil},
		{"pull-image", http.MethodPost, base + "/pull-image", nil},
	}
	for _, who := range []struct {
		name  string
		user  *store.User
		cases []hcCase
	}{
		{"non-hub-member", bob, append(append([]hcCase{}, reads...), writes...)},
		{"other-project member", dave, writes},
	} {
		for _, c := range who.cases {
			t.Run(who.name+"/"+c.name, func(t *testing.T) {
				rec := doRequestAsUser(t, srv, who.user, c.method, c.url, c.body)
				assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
				assert.NotContains(t, rec.Body.String(), harnessConfigSecret)
			})
		}
	}

	// The refusals must also mean nothing changed.
	got, err := s.GetHarnessConfig(context.Background(), hc.ID)
	require.NoError(t, err)
	assert.Equal(t, "private-hc", got.Name)
	assert.Len(t, got.Files, 1)
	assert.Equal(t, original, string(stor.content[key]))
}

func TestHarnessConfigAuthz_OwnerAllowed(t *testing.T) {
	srv, s, alice, _, victim := setupTemplateAuthzTest(t)
	stor := newContentMockStorage("test-bucket")
	srv.SetStorage(stor)
	hc := newProjectHarnessConfig(t, s, stor, victim, alice)
	base := "/api/v1/harness-configs/" + hc.ID

	rec := doRequestAsUser(t, srv, alice, http.MethodGet, base+"/files/Dockerfile", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), harnessConfigSecret)

	rec = doRequestAsUser(t, srv, alice, http.MethodPut, base+"/files/Dockerfile",
		map[string]string{"content": "FROM owner\n"})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "FROM owner\n", string(stor.content[hc.StoragePath+"/Dockerfile"]))
}

func TestHarnessConfigAuthz_BrokerReadOnly(t *testing.T) {
	srv, s, alice, _, victim := setupTemplateAuthzTest(t)
	stor := newContentMockStorage("test-bucket")
	srv.SetStorage(stor)
	hc := newProjectHarnessConfig(t, s, stor, victim, alice)
	base := "/api/v1/harness-configs/" + hc.ID
	key := hc.StoragePath + "/Dockerfile"
	original := string(stor.content[key])

	asBroker := func(method, url string, body any) *httptest.ResponseRecorder {
		var rdr io.Reader
		if body != nil {
			b, err := json.Marshal(body)
			require.NoError(t, err)
			rdr = bytes.NewReader(b)
		}
		req := httptest.NewRequest(method, url, rdr)
		req.Header.Set("Content-Type", "application/json")
		ident := NewBrokerIdentity("test-broker-hc-authz")
		ctx := contextWithIdentity(contextWithBrokerIdentity(req.Context(), ident), ident)
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, req.WithContext(ctx))
		return rec
	}

	// Brokers fetch the config and its download URLs during agent creation.
	rec := asBroker(http.MethodGet, base, nil)
	assert.Equal(t, http.StatusOK, rec.Code, "broker get: %s", rec.Body.String())
	rec = asBroker(http.MethodGet, base+"/download", nil)
	assert.Equal(t, http.StatusOK, rec.Code, "broker download: %s", rec.Body.String())

	// The broker exemption covers reads only.
	rec = asBroker(http.MethodPut, base+"/files/Dockerfile", map[string]string{"content": "FROM broker\n"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "broker write: %s", rec.Body.String())
	assert.Equal(t, original, string(stor.content[key]))
}

func TestHarnessConfigRouteAction(t *testing.T) {
	for _, c := range []struct {
		action, method string
		want           Action
	}{
		{"", http.MethodGet, ActionRead},
		{"", http.MethodPut, ActionUpdate},
		{"", http.MethodPatch, ActionUpdate},
		{"", http.MethodDelete, ActionRead},       // handler adds its scope-aware delete check
		{"clone", http.MethodPost, ActionRead},    // handler checks create on the destination
		{"reimport", http.MethodPost, ActionRead}, // handler checks create on the owning scope
		{"files", http.MethodGet, ActionRead},
		{"files", http.MethodPost, ActionUpdate},
		{"files/Dockerfile", http.MethodPut, ActionUpdate},
		{"files/Dockerfile", http.MethodDelete, ActionUpdate},
		{"download", http.MethodGet, ActionRead},
		{"validate", http.MethodGet, ActionRead},
		{"image-status", http.MethodGet, ActionRead},
		{"upload", http.MethodPost, ActionUpdate},
		{"finalize", http.MethodPost, ActionUpdate},
		{"check-image", http.MethodPost, ActionUpdate},
		{"pull-image", http.MethodPost, ActionUpdate},
		{"local-image", http.MethodDelete, ActionUpdate},
		{"anything-new", "WHATEVER", ActionUpdate},
	} {
		assert.Equal(t, c.want, harnessConfigRouteAction(c.action, c.method), "%s %s", c.method, c.action)
	}
}

// --- Project GitHub settings ----------------------------------------------

// newGitHubBoundProject binds victim to a GitHub installation with explicit
// token permissions and git identity, and registers a second installation an
// outsider might try to rebind to. It returns the second installation ID.
func newGitHubBoundProject(t *testing.T, s store.Store, victim *store.Project) int64 {
	t.Helper()
	ctx := context.Background()
	for _, id := range []int64{7001, 7002} {
		require.NoError(t, s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
			InstallationID: id, AccountLogin: "acct", AccountType: "Organization",
			AppID: 42, Status: store.GitHubInstallationStatusActive,
		}))
	}
	p, err := s.GetProject(ctx, victim.ID)
	require.NoError(t, err)
	inst := int64(7001)
	p.GitHubInstallationID = &inst
	p.GitHubPermissions = &store.GitHubTokenPermissions{Contents: "read", Metadata: "read"}
	p.GitIdentity = &store.GitIdentityConfig{Mode: "bot"}
	require.NoError(t, s.UpdateProject(ctx, p))
	return 7002
}

func assertGitHubSettingsUnchanged(t *testing.T, s store.Store, projectID string) {
	t.Helper()
	p, err := s.GetProject(context.Background(), projectID)
	require.NoError(t, err)
	require.NotNil(t, p.GitHubInstallationID, "installation must remain bound")
	assert.Equal(t, int64(7001), *p.GitHubInstallationID, "installation must be unchanged")
	require.NotNil(t, p.GitHubPermissions, "permissions must remain set")
	assert.Equal(t, "read", p.GitHubPermissions.Contents, "permissions must be unchanged")
	require.NotNil(t, p.GitIdentity, "git identity must remain set")
	assert.Equal(t, "bot", p.GitIdentity.Mode, "git identity must be unchanged")
}

func TestProjectGitHubSettingsAuthz_OutsidersDenied(t *testing.T) {
	srv, s, alice, bob, victim := setupTemplateAuthzTest(t)
	dave := newOtherProjectMember(t, srv, s, alice, "gh-other")
	otherInst := newGitHubBoundProject(t, s, victim)
	base := "/api/v1/projects/" + victim.ID

	writes := []struct {
		name, method, path string
		body               interface{}
	}{
		{"rebind installation", http.MethodPut, "/github-installation", map[string]int64{"installation_id": otherInst}},
		{"unbind installation", http.MethodDelete, "/github-installation", nil},
		{"check status", http.MethodPost, "/github-status", nil},
		{"raise permissions", http.MethodPut, "/github-permissions", map[string]string{"contents": "write", "actions": "write"}},
		{"reset permissions", http.MethodDelete, "/github-permissions", nil},
		{"set git identity", http.MethodPut, "/git-identity", map[string]string{"mode": "custom", "name": "x", "email": "x@example.com"}},
		{"reset git identity", http.MethodDelete, "/git-identity", nil},
	}
	reads := []string{"/github-status", "/github-permissions", "/git-identity"}

	for _, who := range []struct {
		name string
		user *store.User
	}{{"non-hub-member", bob}, {"other-project member", dave}} {
		for _, w := range writes {
			t.Run(who.name+"/"+w.name, func(t *testing.T) {
				rec := doRequestAsUser(t, srv, who.user, w.method, base+w.path, w.body)
				assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
				assertGitHubSettingsUnchanged(t, s, victim.ID)
			})
		}
	}
	for _, who := range []struct {
		name string
		user *store.User
	}{{"non-hub-member", bob}, {"other-project member", dave}} {
		for _, path := range reads {
			t.Run(who.name+"/read "+path, func(t *testing.T) {
				rec := doRequestAsUser(t, srv, who.user, http.MethodGet, base+path, nil)
				assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
			})
		}
	}
	// Also reachable via the legacy groves alias.
	t.Run("groves alias", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, bob, http.MethodPut, "/api/v1/groves/"+victim.ID+"/github-permissions",
			map[string]string{"contents": "write"})
		assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
		assertGitHubSettingsUnchanged(t, s, victim.ID)
	})
}

func TestProjectGitHubSettingsAuthz_OwnerAllowed(t *testing.T) {
	srv, s, alice, _, victim := setupTemplateAuthzTest(t)
	newGitHubBoundProject(t, s, victim)
	base := "/api/v1/projects/" + victim.ID

	for _, path := range []string{"/github-status", "/github-permissions", "/git-identity"} {
		rec := doRequestAsUser(t, srv, alice, http.MethodGet, base+path, nil)
		assert.Equal(t, http.StatusOK, rec.Code, "GET %s body: %s", path, rec.Body.String())
	}
	rec := doRequestAsUser(t, srv, alice, http.MethodPut, base+"/github-permissions",
		map[string]string{"contents": "write", "metadata": "read"})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	rec = doRequestAsUser(t, srv, alice, http.MethodPut, base+"/git-identity",
		map[string]string{"mode": "co-authored"})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	p, err := s.GetProject(context.Background(), victim.ID)
	require.NoError(t, err)
	assert.Equal(t, "write", p.GitHubPermissions.Contents)
	assert.Equal(t, "co-authored", p.GitIdentity.Mode)
}

func TestProjectGitHubRouteAction(t *testing.T) {
	assert.Equal(t, ActionRead, projectGitHubRouteAction(http.MethodGet))
	assert.Equal(t, ActionRead, projectGitHubRouteAction(http.MethodHead))
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		assert.Equal(t, ActionUpdate, projectGitHubRouteAction(m), m)
	}
}

// --- Chat search ----------------------------------------------------------

// TestChatSearchDMAuthz checks that project-wide and unscoped chat search do
// not return DM content to project members who are not party to the DM. The
// chat store shares the main database handle, as it does in production, and
// the DM is written through the production send endpoint.
func TestChatSearchDMAuthz(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	dbp, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "test store must expose DB()")
	db := dbp.DB()
	wcs := NewWebChatStore(db, "sqlite3")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)

	proj := &store.Project{ID: tid("search-dm"), Name: "search-dm", Slug: "search-dm",
		Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, proj))
	srv.createProjectMembersGroup(ctx, proj)
	carol := makeProjectMemberUser(t, s, proj, tid("search-carol"), "Carol", store.GroupMemberRoleMember)

	agent := &store.Agent{ID: tid("search-dm-agent"), ProjectID: proj.ID, Name: "Bot",
		Slug: "search-bot", Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID}
	require.NoError(t, s.CreateAgent(ctx, agent))

	dmKey := "dm:agent:" + agent.ID + ":user:" + DevUserID
	setDMConversationID(t, s, dmKey, proj.ID)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+dmKey+"/messages",
		map[string]string{"content": "PRIVATEPHRASE agent dm body"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	// A user-to-user DM row carrying a project ID, and an ordinary project
	// thread message, inserted directly with the columns search reads.
	insert := func(id, thread, msg string) {
		_, err := db.ExecContext(ctx,
			`INSERT INTO messages (id, project_id, thread_id, sender, recipient, msg, type, channel, created) VALUES (?, ?, ?, 'x', 'y', ?, 'instruction', 'web', ?)`,
			id, proj.ID, thread, msg, time.Now().UTC().Format(time.RFC3339Nano))
		require.NoError(t, err)
	}
	userDM := "dm:user:" + DevUserID + ":user:" + tid("search-erin")
	insert(tid("search-msg-udm"), userDM, "PRIVATEPHRASE user dm body")
	insert(tid("search-msg-topic"), "topic-"+proj.ID, "PRIVATEPHRASE public thread body")

	search := func(user *store.User, extra string) string {
		t.Helper()
		url := "/api/v1/chat/search?q=PRIVATEPHRASE" + extra
		var rec *httptest.ResponseRecorder
		if user == nil {
			rec = doRequest(t, srv, http.MethodGet, url, nil)
		} else {
			rec = doRequestAsUser(t, srv, user, http.MethodGet, url, nil)
		}
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		return rec.Body.String()
	}

	for name, extra := range map[string]string{"project": "&projectId=" + proj.ID, "unscoped": ""} {
		t.Run(name+"/non-participant", func(t *testing.T) {
			body := search(carol, extra)
			assert.Contains(t, body, "public thread body", "project threads stay searchable")
			assert.NotContains(t, body, "agent dm body")
			assert.NotContains(t, body, "user dm body")
		})
		t.Run(name+"/participant", func(t *testing.T) {
			body := search(nil, extra)
			assert.Contains(t, body, "public thread body")
			assert.Contains(t, body, "agent dm body", "participants still find their own DMs")
			assert.Contains(t, body, "user dm body", "participants still find their own DMs")
		})
	}
	t.Run("key-scoped non-participant refused", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/chat/search?q=PRIVATEPHRASE&key="+dmKey, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})
}

func TestFilterSearchDMs(t *testing.T) {
	a := "dm:agent:A:user:u1"
	b := "dm:user:u1:user:u2"
	c := "dm:user:u2:user:u3"
	in := func() []ChatSearchResult {
		return []ChatSearchResult{{ConversationKey: "topic"}, {ConversationKey: a}, {ConversationKey: b}, {ConversationKey: c}}
	}
	keys := func(rs []ChatSearchResult) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.ConversationKey)
		}
		return out
	}
	assert.Equal(t, []string{"topic", a, b}, keys(filterSearchDMs(in(), ChatSearchFilter{DMParticipantUserID: "u1"})))
	assert.Equal(t, []string{"topic", b, c}, keys(filterSearchDMs(in(), ChatSearchFilter{DMParticipantUserID: "u2"})))
	assert.Equal(t, []string{"topic"}, keys(filterSearchDMs(in(), ChatSearchFilter{})), "no participant means no DMs")
	assert.Len(t, filterSearchDMs(in(), ChatSearchFilter{ConversationKey: a}), 4, "key-scoped search is authorized by the caller")
}

// TestSearchChatMessages_DMParticipantFilter exercises the store-level SQL
// condition on its own, without the handler's defensive post-filter.
func TestSearchChatMessages_DMParticipantFilter(t *testing.T) {
	wcs, db := newTestWebChatStoreWithMessages(t)
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	now := time.Now().UTC()
	insertTestMessage(t, db, "t1", "p1", "topic-1", "user:a", "needle topic", now.Add(-4*time.Minute))
	insertTestMessage(t, db, "d1", "p1", "dm:agent:A:user:u1", "agent:A", "needle agent dm", now.Add(-3*time.Minute))
	insertTestMessage(t, db, "d2", "p1", "dm:user:u1:user:u2", "user:u1", "needle user dm", now.Add(-2*time.Minute))
	// A user ID that is a prefix of another must not match.
	insertTestMessage(t, db, "d3", "p1", "dm:agent:A:user:u10", "agent:A", "needle other dm", now.Add(-1*time.Minute))

	ids := func(f ChatSearchFilter) []string {
		t.Helper()
		f.Query, f.Limit = "needle", 50
		rs, _, err := wcs.SearchChatMessages(ctx, f)
		require.NoError(t, err)
		var out []string
		for _, r := range rs {
			out = append(out, r.MessageID)
		}
		return out
	}
	assert.Equal(t, []string{"d2", "d1", "t1"}, ids(ChatSearchFilter{ProjectID: "p1", DMParticipantUserID: "u1"}))
	assert.Equal(t, []string{"d2", "t1"}, ids(ChatSearchFilter{ProjectIDs: []string{"p1"}, DMParticipantUserID: "u2"}))
	assert.Equal(t, []string{"t1"}, ids(ChatSearchFilter{ProjectID: "p1", DMParticipantUserID: "%"}), "IDs are not patterns")
	assert.Equal(t, []string{"t1"}, ids(ChatSearchFilter{ProjectID: "p1"}), "no participant means no DMs")
	assert.Equal(t, []string{"d3"}, ids(ChatSearchFilter{ConversationKey: "dm:agent:A:user:u10"}), "key-scoped search is unaffected")
}
