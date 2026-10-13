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
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
)

func artifactTestAgent(agentID, projectID string, scopes ...AgentTokenScope) *agentIdentityWrapper {
	return &agentIdentityWrapper{AgentTokenClaims: &AgentTokenClaims{
		Claims:      jwt.Claims{Subject: agentID, ID: "jti-" + agentID},
		ProjectID:   projectID,
		Scopes:      scopes,
		ScopeSchema: CurrentAgentScopeSchema,
	}}
}

// addRecordedArtifactEdge records an active, principal-bounded delegation
// edge with recorded provenance from user delegatorID to agentID in project.
func addRecordedArtifactEdge(t *testing.T, s store.Store, delegatorID, agentID, project string) {
	t.Helper()
	edge := &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser,
		DelegatorID:   delegatorID,
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    agentID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       project,
		Role:          string(AgentRoleFull),
		Active:        true,
	}
	edge.EffectCeiling = store.EffectCeiling{Kind: store.EffectCeilingPrincipal}
	edge.AuthorityProvenance = store.AuthorityProvenance{ProvenanceVersion: store.ProvenanceVersionV1, SourceCredentialKind: store.SourceCredentialSession}
	require.NoError(t, s.CreateDelegationEdge(context.Background(), edge))
}

// enableArtifactsForTest turns the artifact service on for srv: the
// hub.artifacts experiment (through a registry where it defaults on, so no
// operational settings are needed), an artifact store on its own SQLite
// file, and local blob storage. It returns the store and the storage.
func enableArtifactsForTest(t *testing.T, srv *Server) (artifacts.Store, *storage.LocalStorage) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		if e.Name == experiments.Artifacts {
			e.Default = true
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
	require.True(t, srv.experimentEnabled(experiments.Artifacts))

	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "artifacts.db")+"?_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	st := artifacts.NewStore(db, "sqlite")
	require.NoError(t, st.Init(context.Background()))
	srv.SetArtifactStore(st)

	blobs, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "hub", LocalPath: t.TempDir()})
	require.NoError(t, err)
	srv.SetStorage(blobs)
	return st, blobs
}

// ensureEdgeBackfillComplete marks the delegation edge backfill complete,
// as on a migrated hub, unless it already is.
func ensureEdgeBackfillComplete(t *testing.T, s store.Store) {
	t.Helper()
	if _, err := s.GetHubSetting(context.Background(), "migration_delegation_edge_backfill_v1"); err == nil {
		return
	}
	markEdgeBackfillComplete(t, s)
}

// artifactAgent creates an agent row in project, created by a project owner
// with a recorded delegation edge, and mints it a token with the scopes of
// role, as the hub does at dispatch.
func artifactAgent(t *testing.T, srv *Server, s store.Store, projectID, slug string, role AgentRole) (*store.Agent, string) {
	t.Helper()
	a := &store.Agent{ID: tid("art-" + slug), Slug: slug, Name: slug, ProjectID: projectID, Phase: "running"}
	require.NoError(t, s.CreateAgent(context.Background(), a))
	// Created by a project owner, with a recorded delegation edge, as the
	// agent-create handler records it.
	delegator := tid("art-delegator-" + projectID)
	createTestUserWithProjectRole(t, s, delegator, "delegator-"+slug+"@test.com", projectID, store.ProjectRoleOwner)
	addRecordedArtifactEdge(t, s, delegator, a.ID, projectID)
	ensureEdgeBackfillComplete(t, s)
	tok, err := srv.GetAgentTokenService().GenerateAgentToken(a.ID, projectID, ScopesForRole(role), nil)
	require.NoError(t, err)
	return a, tok
}

func artifactProject(t *testing.T, s store.Store, slug string) *store.Project {
	t.Helper()
	p := &store.Project{ID: tid("art-" + slug), Name: slug, Slug: slug, Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(context.Background(), p))
	return p
}

// userArtifactRequest sends an artifact request as user with a raw body.
func userArtifactRequest(t *testing.T, srv *Server, user *store.User, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	token, _, _, err := srv.userTokenService.GenerateTokenPair(user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb)
	require.NoError(t, err)
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeArtifactID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var resp artifacts.ArtifactResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Artifact.ID)
	return resp.Artifact.ID
}

// identityArtifactRequest serves an artifact request with identity injected
// into the context, through the hub's mux (route guards and handlers run).
func identityArtifactRequest(t *testing.T, srv *Server, identity Identity, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req = req.WithContext(contextWithIdentity(req.Context(), identity))
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	return rec
}

// doRawAgentRequest sends a raw-body request with an agent token through the
// full hub handler (authentication included).
func doRawAgentRequest(t *testing.T, srv *Server, method, path string, body []byte, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("X-Scion-Agent-Token", token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// anonymous serves an unauthenticated request through the full hub
// handler (authentication included).
func anonymous(srv *Server, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

// artifactTestUAT builds a current-version (V1) user access token bounded
// to project, whose ceiling is exactly the registry permissions of scopes.
func artifactTestUAT(t *testing.T, user UserIdentity, project string, scopes ...string) *ScopedUserIdentity {
	t.Helper()
	return NewScopedUserIdentityWithCeiling(user, project, scopes, "uat-"+project,
		permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: uatCeilingFromSelectors(t, scopes...).PermissionIDs})
}

// preArtifactReadCeiling is a bounded ceiling frozen before artifacts
// existed: exactly project:read's coverage at that time, plus agent.create,
// as a UAT minted with the read selectors and agent:create carries.
func preArtifactReadCeiling() store.EffectCeiling {
	return boundedCeiling(
		"harness_config.list", "harness_config.read", "project.read",
		"skill.list", "skill.read", "template.list", "template.read",
		"agent.create",
	)
}

func artifactsOps(t *testing.T, raw string) *OperationalSettings {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	if raw != "" {
		fakeStore.seed("artifacts", json.RawMessage(raw))
	}
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	return ops
}
