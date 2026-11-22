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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/remotefetch"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const agentTestTemplateSource = "https://github.com/acme/repo/tree/main/templates"

type agentTestFetcher struct {
	body []byte
}

func (f *agentTestFetcher) FetchBytes(ctx context.Context, rawURL string) (*remotefetch.Result, error) {
	return &remotefetch.Result{Body: f.body}, nil
}

func (f *agentTestFetcher) FetchBytesAuthorized(ctx context.Context, rawURL, _ string) (*remotefetch.Result, error) {
	return &remotefetch.Result{Body: f.body}, nil
}

func buildAgentTestTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	for name, body := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(body)),
		}))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gzw.Close())
	return buf.Bytes()
}

func installAgentTestTarballTransport(t *testing.T, files map[string]string) func() {
	t.Helper()
	archive := buildAgentTestTarGz(t, files)
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = &mockRoundTripper{
		roundTrip: func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(archive)),
			}, nil
		},
	}
	return func() { http.DefaultClient.Transport = old }
}

func doAgentRequest(t *testing.T, srv *Server, token, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func setupAgentImportProject(t *testing.T) (*Server, store.Store, *store.Project, *store.User) {
	t.Helper()
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   tid("proj-agent-import"),
		Name: "Agent Import Project",
		Slug: "agent-import-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	creator := &store.User{
		ID:          tid("user-agent-creator"),
		Email:       "creator@test.com",
		DisplayName: "Creator",
		Role:        store.UserRoleMember,
	}
	require.NoError(t, s.CreateUser(ctx, creator))
	ensureHubMembership(ctx, s, creator.ID)
	grantUserActionOnResource(t, s, creator.ID, "template", project.ID, ActionCreate)
	grantUserActionOnResource(t, s, creator.ID, "template", project.ID, ActionRead)
	return srv, s, project, creator
}

func issueAgentTokenWithEdge(t *testing.T, srv *Server, s store.Store, agentID, projectID, creatorID string, role AgentRole, extraScopes []AgentTokenScope) string {
	t.Helper()
	edge := &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser,
		DelegatorID:   creatorID,
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    agentID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       projectID,
		Role:          string(role),
		Active:        true,
	}
	edge.EffectCeiling = store.EffectCeiling{Kind: store.EffectCeilingPrincipal}
	edge.AuthorityProvenance = store.AuthorityProvenance{
		ProvenanceVersion:    store.ProvenanceVersionV1,
		SourceCredentialKind: store.SourceCredentialSession,
	}
	require.NoError(t, s.CreateDelegationEdge(context.Background(), edge))
	tok, err := srv.GenerateAgentToken(agentID, projectID, []string{creatorID}, role, extraScopes)
	require.NoError(t, err)
	return tok
}

func TestAgentTemplateImportDiscoverReimport_FullRoleOwnProject(t *testing.T) {
	srv, s, project, creator := setupAgentImportProject(t)
	ctx := context.Background()

	fullToken := issueAgentTokenWithEdge(t, srv, s, tid("agent-full"), project.ID, creator.ID, AgentRoleFull, nil)

	// 1. Discover via unified route (/api/v1/resources/discover)
	cleanup := installAgentTestTarballTransport(t, map[string]string{
		"repo-main/templates/tmpl-a/scion-agent.yaml": "schema_version: \"1\"\nharness: claude\n",
		"repo-main/templates/tmpl-b/scion-agent.yaml": "schema_version: \"1\"\nharness: claude\n",
	})
	defer cleanup()

	rec := doAgentRequest(t, srv, fullToken, http.MethodPost, "/api/v1/resources/discover", DiscoverResourcesUnifiedRequest{
		Kind:      "template",
		Scope:     "project",
		ScopeID:   project.ID,
		SourceURL: agentTestTemplateSource,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// 2. Discover via per-project route (/api/v1/projects/{id}/discover-templates)
	rec = doAgentRequest(t, srv, fullToken, http.MethodPost, "/api/v1/projects/"+project.ID+"/discover-templates", DiscoverResourcesRequest{
		SourceURL: agentTestTemplateSource,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// 3. Import tmpl-a via unified route (/api/v1/resources/import)
	rec = doAgentRequest(t, srv, fullToken, http.MethodPost, "/api/v1/resources/import", ImportResourcesRequest{
		Kind:      "template",
		Scope:     "project",
		ScopeID:   project.ID,
		SourceURL: agentTestTemplateSource,
		Names:     []string{"tmpl-a"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// 4. Import tmpl-b via per-project route (/api/v1/projects/{id}/import-templates)
	rec = doAgentRequest(t, srv, fullToken, http.MethodPost, "/api/v1/projects/"+project.ID+"/import-templates", ImportTemplatesRequest{
		SourceURL: agentTestTemplateSource,
		Names:     []string{"tmpl-b"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// 5. Reimport tmpl-a without sourceUrl override (/api/v1/templates/{id}/reimport)
	tmplA, err := s.GetTemplateBySlug(ctx, "tmpl-a", store.TemplateScopeProject, project.ID)
	require.NoError(t, err)
	srv.templateSourceFetcher = &agentTestFetcher{
		body: buildAgentTestTarGz(t, map[string]string{
			"repo-main/templates/tmpl-a/scion-agent.yaml": "schema_version: \"1\"\nharness: claude\n",
			"repo-main/templates/tmpl-a/README.md":        "refreshed content",
		}),
	}
	rec = doAgentRequest(t, srv, fullToken, http.MethodPost, "/api/v1/templates/"+tmplA.ID+"/reimport", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestAgentTemplateImportDiscoverReimport_Denials(t *testing.T) {
	srv, s, project, creator := setupAgentImportProject(t)
	ctx := context.Background()

	otherProject := &store.Project{ID: tid("proj-other"), Name: "Other Project", Slug: "other-project"}
	require.NoError(t, s.CreateProject(ctx, otherProject))

	tmpl := &store.Template{
		ID:        tid("tmpl-own"),
		Name:      "own-tmpl",
		Slug:      "own-tmpl",
		Harness:   "claude",
		Scope:     store.TemplateScopeProject,
		ScopeID:   project.ID,
		ProjectID: project.ID,
		Status:    store.TemplateStatusActive,
		SourceURL: agentTestTemplateSource + "/own-tmpl",
	}
	require.NoError(t, s.CreateTemplate(ctx, tmpl))

	otherTmpl := &store.Template{
		ID:        tid("tmpl-other"),
		Name:      "other-tmpl",
		Slug:      "other-tmpl",
		Harness:   "claude",
		Scope:     store.TemplateScopeProject,
		ScopeID:   otherProject.ID,
		ProjectID: otherProject.ID,
		Status:    store.TemplateStatusActive,
		SourceURL: agentTestTemplateSource + "/other-tmpl",
	}
	require.NoError(t, s.CreateTemplate(ctx, otherTmpl))

	routes := []struct {
		name      string
		path      string
		body      any
		projectID string
	}{
		{
			name: "resources/import",
			path: "/api/v1/resources/import",
			body: ImportResourcesRequest{Kind: "template", Scope: "project", ScopeID: project.ID, SourceURL: agentTestTemplateSource},
		},
		{
			name: "resources/discover",
			path: "/api/v1/resources/discover",
			body: DiscoverResourcesUnifiedRequest{Kind: "template", Scope: "project", ScopeID: project.ID, SourceURL: agentTestTemplateSource},
		},
		{
			name: "projects/import-templates",
			path: "/api/v1/projects/" + project.ID + "/import-templates",
			body: ImportTemplatesRequest{SourceURL: agentTestTemplateSource},
		},
		{
			name: "projects/discover-templates",
			path: "/api/v1/projects/" + project.ID + "/discover-templates",
			body: DiscoverResourcesRequest{SourceURL: agentTestTemplateSource},
		},
		{
			name: "templates/reimport",
			path: "/api/v1/templates/" + tmpl.ID + "/reimport",
			body: nil,
		},
	}

	// 1. baseline and readonly agents are denied with 403 on all 5 routes
	for _, role := range []AgentRole{AgentRoleBaseline, AgentRoleReadOnly} {
		tok := issueAgentTokenWithEdge(t, srv, s, tid("agent-"+string(role)), project.ID, creator.ID, role, nil)
		for _, rt := range routes {
			t.Run(string(role)+"/"+rt.name, func(t *testing.T) {
				rec := doAgentRequest(t, srv, tok, http.MethodPost, rt.path, rt.body)
				assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			})
		}
	}

	// 2. Agent holding project:agent:create WITHOUT project:template:write is denied (cutover from legacy ScopeAgentCreate)
	legacyScopeTok := issueAgentTokenWithEdge(t, srv, s, tid("agent-legacy-scope"), project.ID, creator.ID, AgentRoleReadOnly, []AgentTokenScope{ScopeAgentCreate})
	for _, rt := range routes {
		t.Run("agent-create-without-template-write/"+rt.name, func(t *testing.T) {
			rec := doAgentRequest(t, srv, legacyScopeTok, http.MethodPost, rt.path, rt.body)
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		})
	}

	// 3. full agent targeting another project: 403 on import/discover, 404 on reimport (read gate)
	fullTok := issueAgentTokenWithEdge(t, srv, s, tid("agent-full-cross"), project.ID, creator.ID, AgentRoleFull, nil)
	t.Run("cross-project", func(t *testing.T) {
		rec := doAgentRequest(t, srv, fullTok, http.MethodPost, "/api/v1/resources/import", ImportResourcesRequest{
			Kind: "template", Scope: "project", ScopeID: otherProject.ID, SourceURL: agentTestTemplateSource,
		})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

		rec = doAgentRequest(t, srv, fullTok, http.MethodPost, "/api/v1/resources/discover", DiscoverResourcesUnifiedRequest{
			Kind: "template", Scope: "project", ScopeID: otherProject.ID, SourceURL: agentTestTemplateSource,
		})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

		rec = doAgentRequest(t, srv, fullTok, http.MethodPost, "/api/v1/projects/"+otherProject.ID+"/import-templates", ImportTemplatesRequest{
			SourceURL: agentTestTemplateSource,
		})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

		rec = doAgentRequest(t, srv, fullTok, http.MethodPost, "/api/v1/projects/"+otherProject.ID+"/discover-templates", DiscoverResourcesRequest{
			SourceURL: agentTestTemplateSource,
		})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

		rec = doAgentRequest(t, srv, fullTok, http.MethodPost, "/api/v1/templates/"+otherTmpl.ID+"/reimport", nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	})

	// 4. full agent whose creator only has viewer / lacks template.create: 403 with delegation-ceiling reason
	viewer := &store.User{
		ID:          tid("user-viewer-creator"),
		Email:       "viewer@test.com",
		DisplayName: "Viewer",
		Role:        store.UserRoleMember,
	}
	require.NoError(t, s.CreateUser(ctx, viewer))
	ensureHubMembership(ctx, s, viewer.ID)
	grantUserActionOnResource(t, s, viewer.ID, "template", project.ID, ActionRead)
	demotedTok := issueAgentTokenWithEdge(t, srv, s, tid("agent-demoted-creator"), project.ID, viewer.ID, AgentRoleFull, nil)
	for _, rt := range routes {
		t.Run("delegation-ceiling/"+rt.name, func(t *testing.T) {
			rec := doAgentRequest(t, srv, demotedTok, http.MethodPost, rt.path, rt.body)
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "does not hold template.create", rec.Body.String())
		})
	}

	// 5. Delegated agent identity is denied
	t.Run("delegated-agent-denied", func(t *testing.T) {
		grant := &store.AgentDelegationGrant{
			AgentID:                 tid("agent-delegated"),
			AgentProjectID:          project.ID,
			IssuerUserID:            creator.ID,
			BoundaryKind:            "project",
			BoundaryProjectID:       project.ID,
			CeilingVersion:          1,
			CeilingPermissionIDs:    []string{"template.create", "template.read"},
			Name:                    "tmpl-grant",
			MaxCredentialTTLSeconds: 3600,
			ExpiresAt:               time.Now().Add(time.Hour),
			IssuanceAuditID:         uuid.New().String(),
		}
		require.NoError(t, s.CreateAgentDelegationGrant(ctx, grant))
		rawTok, hash, err := newDelegatedCredentialToken()
		require.NoError(t, err)
		cred := &store.AgentDelegatedCredential{
			GrantID:                   grant.ID,
			AgentID:                   grant.AgentID,
			KeyHash:                   hash,
			Prefix:                    delegatedCredentialPrefix,
			Audience:                  "scion-hub",
			CeilingPermissionIDs:      []string{"template.create", "template.read"},
			ExchangeAgentCredentialID: uuid.New().String(),
			ExpiresAt:                 time.Now().Add(30 * time.Minute),
		}
		require.NoError(t, s.CreateAgentDelegatedCredential(ctx, cred))

		rec := doAgentRequest(t, srv, rawTok, http.MethodPost, "/api/v1/resources/import", ImportResourcesRequest{
			Kind: "template", Scope: "project", ScopeID: project.ID, SourceURL: agentTestTemplateSource,
		})
		assert.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, rec.Code, rec.Body.String())
	})
}

func TestAgentTemplateImportDiscoverReimport_GlobalAndUserReturn401(t *testing.T) {
	srv, s, project, creator := setupAgentImportProject(t)
	ctx := context.Background()

	globalTmpl := &store.Template{
		ID: tid("tmpl-global-401"), Name: "global-t", Slug: "global-t", Harness: "claude",
		Scope: store.TemplateScopeGlobal, Status: store.TemplateStatusActive, SourceURL: agentTestTemplateSource,
	}
	require.NoError(t, s.CreateTemplate(ctx, globalTmpl))

	userTmpl := &store.Template{
		ID: tid("tmpl-user-401"), Name: "user-t", Slug: "user-t", Harness: "claude",
		Scope: store.TemplateScopeUser, ScopeID: creator.ID, OwnerID: creator.ID,
		Status: store.TemplateStatusActive, SourceURL: agentTestTemplateSource,
	}
	require.NoError(t, s.CreateTemplate(ctx, userTmpl))

	fullTok := issueAgentTokenWithEdge(t, srv, s, tid("agent-full-401"), project.ID, creator.ID, AgentRoleFull, nil)

	for _, scope := range []string{"global", "user"} {
		rec := doAgentRequest(t, srv, fullTok, http.MethodPost, "/api/v1/resources/import", ImportResourcesRequest{
			Kind: "template", Scope: scope, SourceURL: agentTestTemplateSource,
		})
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "import scope=%s: %s", scope, rec.Body.String())

		rec = doAgentRequest(t, srv, fullTok, http.MethodPost, "/api/v1/resources/discover", DiscoverResourcesUnifiedRequest{
			Kind: "template", Scope: scope, SourceURL: agentTestTemplateSource,
		})
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "discover scope=%s: %s", scope, rec.Body.String())
	}

	// Global template reimport -> 404 (read gate) or 401 (never allowed for agents)
	rec := doAgentRequest(t, srv, fullTok, http.MethodPost, "/api/v1/templates/"+globalTmpl.ID+"/reimport", nil)
	assert.Contains(t, []int{http.StatusUnauthorized, http.StatusNotFound}, rec.Code, rec.Body.String())

	// User template reimport -> 404 (not readable by agent) or 401
	rec = doAgentRequest(t, srv, fullTok, http.MethodPost, "/api/v1/templates/"+userTmpl.ID+"/reimport", nil)
	assert.Contains(t, []int{http.StatusUnauthorized, http.StatusNotFound}, rec.Code, rec.Body.String())
}

func TestAgentHarnessConfigProjectImportDiscover_LegacyScopeAgentCreateUnchanged(t *testing.T) {
	srv, _, project, creator := setupAgentImportProject(t)

	cleanup := installAgentTestTarballTransport(t, map[string]string{
		"repo-main/harness-configs/hc-a/config.yaml": "harness: claude\nimage: scion-claude:latest\n",
	})
	defer cleanup()

	// Agent with ScopeAgentCreate (even without ScopeProjectTemplateWrite) keeps legacy harness-config access.
	withCreateTok, err := srv.GenerateAgentToken(tid("agent-hc-ok"), project.ID, []string{creator.ID}, AgentRoleReadOnly, []AgentTokenScope{ScopeAgentCreate})
	require.NoError(t, err)

	rec := doAgentRequest(t, srv, withCreateTok, http.MethodPost, "/api/v1/projects/"+project.ID+"/discover-harness-configs", DiscoverResourcesRequest{
		SourceURL: "https://github.com/acme/repo/tree/main/harness-configs",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doAgentRequest(t, srv, withCreateTok, http.MethodPost, "/api/v1/projects/"+project.ID+"/import-harness-configs", ImportHarnessConfigsRequest{
		SourceURL: "https://github.com/acme/repo/tree/main/harness-configs",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Agent without ScopeAgentCreate is refused with 403.
	withoutCreateTok, err := srv.GenerateAgentToken(tid("agent-hc-deny"), project.ID, []string{creator.ID}, AgentRoleReadOnly, nil)
	require.NoError(t, err)
	rec = doAgentRequest(t, srv, withoutCreateTok, http.MethodPost, "/api/v1/projects/"+project.ID+"/discover-harness-configs", DiscoverResourcesRequest{
		SourceURL: "https://github.com/acme/repo/tree/main/harness-configs",
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

func TestAgentD6Restrictions_ReimportOverrideAndCreateOnly(t *testing.T) {
	srv, s, project, creator := setupAgentImportProject(t)
	ctx := context.Background()

	fullTok := issueAgentTokenWithEdge(t, srv, s, tid("agent-d6"), project.ID, creator.ID, AgentRoleFull, nil)

	// 1. Agent reimport with sourceUrl override -> 403 agent_source_override_forbidden; stored URL unchanged.
	t.Run("reimport sourceUrl override forbidden", func(t *testing.T) {
		origURL := agentTestTemplateSource + "/existing-tmpl"
		tmpl := &store.Template{
			ID: tid("tmpl-d6-reimport"), Name: "existing-tmpl", Slug: "existing-tmpl",
			Harness: "claude", Scope: store.TemplateScopeProject, ScopeID: project.ID,
			ProjectID: project.ID, Status: store.TemplateStatusActive, SourceURL: origURL,
			ContentHash: "sha256:original",
		}
		require.NoError(t, s.CreateTemplate(ctx, tmpl))

		rec := doAgentRequest(t, srv, fullTok, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/reimport", ReimportTemplateRequest{
			SourceURL: "https://github.com/evil/repo/tree/main/templates/existing-tmpl",
		})
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "agent_source_override_forbidden")
		assert.Contains(t, rec.Body.String(), "agents can refresh a template only from its stored source URL")

		got, err := s.GetTemplate(ctx, tmpl.ID)
		require.NoError(t, err)
		assert.Equal(t, origURL, got.SourceURL)
		assert.Equal(t, "sha256:original", got.ContentHash)

		// Human user can still override sourceUrl as before.
		srv.templateSourceFetcher = &agentTestFetcher{
			body: buildAgentTestTarGz(t, map[string]string{
				"repo-main/templates/existing-tmpl/scion-agent.yaml": "schema_version: \"1\"\nharness: claude\n",
			}),
		}
		newURL := "https://github.com/acme/other/tree/main/templates/existing-tmpl"
		rec = doRequestAsUser(t, srv, creator, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/reimport", ReimportTemplateRequest{
			SourceURL: newURL,
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		got, err = s.GetTemplate(ctx, tmpl.ID)
		require.NoError(t, err)
		assert.Equal(t, newURL, got.SourceURL)
	})

	// 2. Agent import-url create-only: existing template fails with no write, new template imports (both route families).
	t.Run("create-only on both route families", func(t *testing.T) {
		origURL := "https://github.com/acme/original/tree/main/templates/already-here"
		existing := &store.Template{
			ID: tid("tmpl-d6-existing"), Name: "already-here", Slug: "already-here",
			Harness: "claude", Scope: store.TemplateScopeProject, ScopeID: project.ID,
			ProjectID: project.ID, Status: store.TemplateStatusActive, SourceURL: origURL,
			ContentHash: "sha256:unchanged",
		}
		require.NoError(t, s.CreateTemplate(ctx, existing))

		cleanup := installAgentTestTarballTransport(t, map[string]string{
			"repo-main/templates/already-here/scion-agent.yaml": "schema_version: \"1\"\nharness: claude\n",
			"repo-main/templates/brand-new-1/scion-agent.yaml":  "schema_version: \"1\"\nharness: claude\n",
			"repo-main/templates/brand-new-2/scion-agent.yaml":  "schema_version: \"1\"\nharness: claude\n",
		})
		defer cleanup()

		// Unified route: already-here fails, brand-new-1 imports
		rec := doAgentRequest(t, srv, fullTok, http.MethodPost, "/api/v1/resources/import", ImportResourcesRequest{
			Kind:      "template",
			Scope:     "project",
			ScopeID:   project.ID,
			SourceURL: agentTestTemplateSource,
			Names:     []string{"already-here", "brand-new-1"},
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp ImportResourcesResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, []string{"brand-new-1"}, resp.Imported)
		require.Len(t, resp.Failed, 1)
		assert.Equal(t, "already-here", resp.Failed[0].Name)
		assert.Contains(t, resp.Failed[0].Reason, "template exists; agents can only create new templates")

		// Per-project route: already-here fails, brand-new-2 imports
		rec = doAgentRequest(t, srv, fullTok, http.MethodPost, "/api/v1/projects/"+project.ID+"/import-templates", ImportTemplatesRequest{
			SourceURL: agentTestTemplateSource,
			Names:     []string{"already-here", "brand-new-2"},
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var pResp ImportTemplatesResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pResp))
		assert.Equal(t, []string{"brand-new-2"}, pResp.Templates)
		require.Len(t, pResp.Failed, 1)
		assert.Equal(t, "already-here", pResp.Failed[0].Name)

		// Existing template was never modified
		got, err := s.GetTemplate(ctx, existing.ID)
		require.NoError(t, err)
		assert.Equal(t, origURL, got.SourceURL)
		assert.Equal(t, "sha256:unchanged", got.ContentHash)

		// Human user can still overwrite already-here
		rec = doRequestAsUser(t, srv, creator, http.MethodPost, "/api/v1/resources/import", ImportResourcesRequest{
			Kind:      "template",
			Scope:     "project",
			ScopeID:   project.ID,
			SourceURL: agentTestTemplateSource,
			Names:     []string{"already-here"},
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		got, err = s.GetTemplate(ctx, existing.ID)
		require.NoError(t, err)
		assert.Equal(t, agentTestTemplateSource+"/already-here", got.SourceURL)
	})

	// 3. Bundled harness-config collision refuses the whole template for agents before any write.
	t.Run("bundled harness-config collision refused whole", func(t *testing.T) {
		existingHC := &store.HarnessConfig{
			ID:          tid("hc-bundled-existing"),
			Name:        "shared-hc",
			Slug:        "shared-hc",
			Harness:     "claude",
			Scope:       store.HarnessConfigScopeProject,
			ScopeID:     project.ID,
			Status:      store.HarnessConfigStatusActive,
			ContentHash: "sha256:hc-original",
		}
		require.NoError(t, s.CreateHarnessConfig(ctx, existingHC))

		cleanup := installAgentTestTarballTransport(t, map[string]string{
			"repo-main/templates/with-existing-hc/scion-agent.yaml":                      "schema_version: \"1\"\nharness: claude\n",
			"repo-main/templates/with-existing-hc/harness-configs/shared-hc/config.yaml": "harness: claude\nimage: evil:latest\n",
			"repo-main/templates/with-new-hc/scion-agent.yaml":                           "schema_version: \"1\"\nharness: claude\n",
			"repo-main/templates/with-new-hc/harness-configs/brand-new-hc/config.yaml":   "harness: claude\nimage: good:latest\n",
		})
		defer cleanup()

		rec := doAgentRequest(t, srv, fullTok, http.MethodPost, "/api/v1/resources/import", ImportResourcesRequest{
			Kind:      "template",
			Scope:     "project",
			ScopeID:   project.ID,
			SourceURL: agentTestTemplateSource,
			Names:     []string{"with-existing-hc", "with-new-hc"},
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp ImportResourcesResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, []string{"with-new-hc"}, resp.Imported)
		require.Len(t, resp.Failed, 1)
		assert.Equal(t, "with-existing-hc", resp.Failed[0].Name)
		assert.Contains(t, resp.Failed[0].Reason, "bundled harness-config")

		// Refused template must NOT exist in DB
		_, err := s.GetTemplateBySlug(ctx, "with-existing-hc", store.TemplateScopeProject, project.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)

		// Existing harness config must NOT be modified
		gotHC, err := s.GetHarnessConfig(ctx, existingHC.ID)
		require.NoError(t, err)
		assert.Equal(t, "sha256:hc-original", gotHC.ContentHash)

		// New template AND its new bundled harness config MUST exist
		_, err = s.GetTemplateBySlug(ctx, "with-new-hc", store.TemplateScopeProject, project.ID)
		require.NoError(t, err)
		_, err = s.GetHarnessConfigBySlug(ctx, "brand-new-hc", store.HarnessConfigScopeProject, project.ID)
		require.NoError(t, err)
	})
}
