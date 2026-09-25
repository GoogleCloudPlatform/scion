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
