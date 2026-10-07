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
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func agentClient(t *testing.T, baseURL, token string) hubclient.Client {
	t.Helper()
	c, err := hubclient.New(baseURL, hubclient.WithAgentToken(token))
	require.NoError(t, err)
	return c
}

func sha256OfReader(t *testing.T, rc io.ReadCloser) string {
	t.Helper()
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	_, err := io.Copy(h, rc)
	require.NoError(t, err)
	return hex.EncodeToString(h.Sum(nil))
}

func requireHubStatus(t *testing.T, err error, status int) {
	t.Helper()
	var apiErr *apiclient.APIError
	require.True(t, errors.As(err, &apiErr), "want an API error with status %d, got %v", status, err)
	require.Equal(t, status, apiErr.StatusCode, apiErr.Error())
}

// TestArtifactsTwoAgentsSameProject is the cross-broker publish/get
// acceptance criterion of P1 (ptone/scion#3208), in process: one hub,
// two distinct agent identities in the same project (as if on two
// different runtime brokers, sharing nothing but the hub). Agent A
// publishes through the hub client; agent B fetches by reference and gets
// byte-identical content. An agent in another project gets 404 on every
// read route, the same answer as for an artifact that does not exist.
func TestArtifactsTwoAgentsSameProject(t *testing.T) {
	srv, s := testServer(t)
	enableArtifactsForTest(t, srv)
	hub := httptest.NewServer(srv.Handler())
	t.Cleanup(hub.Close)

	p1 := artifactProject(t, s, "p1")
	p2 := artifactProject(t, s, "p2")
	agentA, tokA := artifactAgent(t, srv, s, p1.ID, "agent-a", AgentRoleBaseline)
	_, tokB := artifactAgent(t, srv, s, p1.ID, "agent-b", AgentRoleBaseline)
	_, tokX := artifactAgent(t, srv, s, p2.ID, "agent-x", AgentRoleBaseline)
	a, b, x := agentClient(t, hub.URL, tokA), agentClient(t, hub.URL, tokB), agentClient(t, hub.URL, tokX)
	ctx := context.Background()

	content := []byte("# Artifact system design\n\nPublished by agent A, read by agent B.\n")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])

	// Agent A publishes; the artifact is owned by A's stable id and homed in p1.
	pub, err := a.Artifacts().Publish(ctx, &hubclient.PublishArtifactRequest{
		Name: "design.md", Title: "Artifact system design",
		Content: bytes.NewReader(content), Size: int64(len(content)), SHA256: digest,
	})
	require.NoError(t, err)
	assert.Equal(t, "scion://artifact/"+pub.Artifact.ID, pub.Artifact.Ref)
	assert.Equal(t, artifacts.PrincipalKindAgent, pub.Artifact.OwnerKind)
	assert.Equal(t, agentA.ID, pub.Artifact.OwnerRef)
	assert.Equal(t, p1.ID, pub.Artifact.ScopeRef)
	require.NotNil(t, pub.Version)
	assert.Equal(t, 1, pub.Version.Seq)

	// Agent B resolves the reference and fetches byte-identical content.
	id, seq, err := artifacts.ParseRef(pub.Artifact.Ref)
	require.NoError(t, err)
	meta, err := b.Artifacts().Get(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, meta.Version)
	assert.Equal(t, digest, meta.Version.Files[0].SHA256)
	for _, s := range []int{seq, 1} {
		rc, err := b.Artifacts().OpenFile(ctx, id, s, meta.Version.EntryPath)
		require.NoError(t, err)
		assert.Equal(t, digest, sha256OfReader(t, rc), "seq %d: agent B's bytes differ from what agent A published", s)
	}

	// An agent in another project: 404 everywhere, like a missing artifact.
	_, err = x.Artifacts().Get(ctx, id)
	requireHubStatus(t, err, http.StatusNotFound)
	_, err = x.Artifacts().OpenFile(ctx, id, 0, "design.md")
	requireHubStatus(t, err, http.StatusNotFound)
	_, err = x.Artifacts().OpenFile(ctx, id, 1, "design.md")
	requireHubStatus(t, err, http.StatusNotFound)
	_, err = b.Artifacts().Get(ctx, "00000000-0000-4000-8000-000000000000")
	requireHubStatus(t, err, http.StatusNotFound)
}

// TestArtifactsAgentScopes pins the least-privilege agent scope wiring: a
// read-only agent can read its project's artifacts but cannot publish
// (project:artifact:write is minted for baseline and full only), and an
// agent cannot publish into another project.
func TestArtifactsAgentScopes(t *testing.T) {
	assert.Contains(t, ScopesForRole(AgentRoleBaseline), ScopeProjectArtifactWrite)
	assert.Contains(t, ScopesForRole(AgentRoleFull), ScopeProjectArtifactWrite)
	assert.NotContains(t, ScopesForRole(AgentRoleReadOnly), ScopeProjectArtifactWrite)

	srv, s := testServer(t)
	enableArtifactsForTest(t, srv)
	hub := httptest.NewServer(srv.Handler())
	t.Cleanup(hub.Close)
	p1 := artifactProject(t, s, "scopes-p1")
	p2 := artifactProject(t, s, "scopes-p2")
	_, tokW := artifactAgent(t, srv, s, p1.ID, "writer", AgentRoleBaseline)
	_, tokR := artifactAgent(t, srv, s, p1.ID, "reader", AgentRoleReadOnly)
	w, r := agentClient(t, hub.URL, tokW), agentClient(t, hub.URL, tokR)
	ctx := context.Background()

	publish := func(c hubclient.Client, scope string) (*hubclient.ArtifactResponse, error) {
		return c.Artifacts().Publish(ctx, &hubclient.PublishArtifactRequest{
			Name: "notes.txt", Scope: scope, Content: bytes.NewReader([]byte("notes")), Size: 5,
		})
	}
	pub, err := publish(w, "")
	require.NoError(t, err)

	_, err = publish(r, "")
	requireHubStatus(t, err, http.StatusForbidden)
	_, err = publish(w, p2.ID)
	requireHubStatus(t, err, http.StatusForbidden)

	// The read-only agent still reads through project:artifact:read.
	rc, err := r.Artifacts().OpenFile(ctx, pub.Artifact.ID, 0, "notes.txt")
	require.NoError(t, err)
	_ = rc.Close()
}

// TestArtifactsExperimentOffAnswers404 checks that every artifact route is
// 404 while hub.artifacts is off (P1 acceptance), with the service fully
// configured: turning the experiment off hides every route, including for a
// caller who could otherwise read.
func TestArtifactsExperimentOffAnswers404(t *testing.T) {
	srv, s := testServer(t)
	enableArtifactsForTest(t, srv)
	p1 := artifactProject(t, s, "off-p1")
	_, tok := artifactAgent(t, srv, s, p1.ID, "off-agent", AgentRoleBaseline)

	rec := doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/artifacts/00000000-0000-4000-8000-000000000000", nil, tok)
	require.Equal(t, http.StatusNotFound, rec.Code)

	hub := httptest.NewServer(srv.Handler())
	t.Cleanup(hub.Close)
	pub, err := agentClient(t, hub.URL, tok).Artifacts().Publish(context.Background(), &hubclient.PublishArtifactRequest{
		Name: "a.txt", Content: bytes.NewReader([]byte("a")), Size: 1,
	})
	require.NoError(t, err)

	srv.experiments = experiments.Default() // hub.artifacts defaults off
	for _, rq := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/artifacts/" + pub.Artifact.ID},
		{http.MethodGet, "/api/v1/artifacts/" + pub.Artifact.ID + "/files/a.txt"},
		{http.MethodGet, "/api/v1/artifacts/" + pub.Artifact.ID + "/versions/1/files/a.txt"},
		{http.MethodPost, "/api/v1/artifacts?name=b.txt"},
		{http.MethodGet, "/api/v1/artifacts/shared/token"},
	} {
		rec := doRequestWithAgentToken(t, srv, rq.method, rq.path, nil, tok)
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s %s", rq.method, rq.path)
	}
}

// TestArtifactWriteScopeDelegation: an agent whose token predates the
// artifact scopes can still delegate the full role, and an explicitly
// requested artifact write scope still requires the delegator to hold it.
func TestArtifactWriteScopeDelegation(t *testing.T) {
	authz, _ := authzTestSetup(t)
	var withoutArtifact []AgentTokenScope
	for _, s := range ScopesForRole(AgentRoleFull) {
		if s != ScopeProjectArtifactWrite {
			withoutArtifact = append(withoutArtifact, s)
		}
	}
	actor := &agentIdentityWrapper{&AgentTokenClaims{Scopes: withoutArtifact}}
	grant := GrantDescriptor{Type: GrantTypeAgentDelegation, AgentRole: string(AgentRoleFull), ProjectID: tid("deleg-artifact")}
	decision := authz.CanDelegate(context.Background(), actor, grant)
	assert.True(t, decision.Allowed, decision.Reason)

	grant.AgentScopes = []AgentTokenScope{ScopeProjectArtifactWrite}
	decision = authz.CanDelegate(context.Background(), actor, grant)
	assert.False(t, decision.Allowed, "an explicitly requested scope the actor lacks must be denied")
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

// TestArtifactsProjectMembers covers the user side (a user opens the
// artifact; P1 acceptance, ptone/scion#3208): project
// roles carry artifact.read and artifact.create, so a project member can
// publish into the project and other members and the project's agents can
// read it; a user without a role in the project cannot publish there and
// gets 404 on every read route.
func TestArtifactsProjectMembers(t *testing.T) {
	srv, s := testServer(t)
	enableArtifactsForTest(t, srv)
	ctx := context.Background()
	p1 := artifactProject(t, s, "members-p1")
	createTestUserWithProjectRole(t, s, tid("art-member"), "art-member@test.com", p1.ID, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, s, tid("art-member2"), "art-member2@test.com", p1.ID, store.ProjectRoleMember)
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: tid("art-outsider"), Email: "art-outsider@test.com", DisplayName: "Outsider", Role: "member", Status: "active"}))
	member, err := s.GetUser(ctx, tid("art-member"))
	require.NoError(t, err)
	member2, err := s.GetUser(ctx, tid("art-member2"))
	require.NoError(t, err)
	outsider, err := s.GetUser(ctx, tid("art-outsider"))
	require.NoError(t, err)
	_, agentTok := artifactAgent(t, srv, s, p1.ID, "members-agent", AgentRoleBaseline)

	// A member publishes into the project (users name the scope).
	rec := userArtifactRequest(t, srv, member, http.MethodPost, "/api/v1/artifacts?name=report.md&scope="+p1.ID, []byte("# Report\n"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	id := decodeArtifactID(t, rec)

	paths := []string{
		"/api/v1/artifacts/" + id,
		"/api/v1/artifacts/" + id + "/files/report.md",
		"/api/v1/artifacts/" + id + "/versions/1/files/report.md",
	}
	for _, p := range paths {
		assert.Equal(t, http.StatusOK, userArtifactRequest(t, srv, member, http.MethodGet, p, nil).Code, "owner %s", p)
		assert.Equal(t, http.StatusOK, userArtifactRequest(t, srv, member2, http.MethodGet, p, nil).Code, "member %s", p)
		assert.Equal(t, http.StatusOK, doRequestWithAgentToken(t, srv, http.MethodGet, p, nil, agentTok).Code, "project agent %s", p)
		assert.Equal(t, http.StatusNotFound, userArtifactRequest(t, srv, outsider, http.MethodGet, p, nil).Code, "outsider %s", p)
	}

	rec = userArtifactRequest(t, srv, outsider, http.MethodPost, "/api/v1/artifacts?name=x.md&scope="+p1.ID, []byte("x"))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

func decodeArtifactID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var resp artifacts.ArtifactResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Artifact.ID)
	return resp.Artifact.ID
}

// TestArtifactGrantsStayWithinAgentReadScope pins the invariant on the real
// read routes: a principal grant decides which artifacts an agent may
// read, the project:artifact:read scope decides whether it may use the
// artifact API at all. An agent in another project holding an explicit
// grant reads the artifact; the same agent with a token that lacks the
// scope gets 404 on every read route, like any unreadable artifact.
func TestArtifactGrantsStayWithinAgentReadScope(t *testing.T) {
	srv, s := testServer(t)
	st, blobs := enableArtifactsForTest(t, srv)
	ctx := context.Background()
	p1 := artifactProject(t, s, "grant-p1")
	p2 := artifactProject(t, s, "grant-p2")
	owner, _ := artifactAgent(t, srv, s, p1.ID, "grant-owner", AgentRoleBaseline)
	grantee, withScope := artifactAgent(t, srv, s, p2.ID, "grant-grantee", AgentRoleBaseline)

	var noRead []AgentTokenScope
	for _, sc := range ScopesForRole(AgentRoleBaseline) {
		if sc != ScopeProjectArtifactRead {
			noRead = append(noRead, sc)
		}
	}
	withoutScope, err := srv.GetAgentTokenService().GenerateAgentToken(grantee.ID, p2.ID, noRead, nil)
	require.NoError(t, err)

	// Publish as the owner, with a principal read grant for the grantee.
	content := []byte("granted")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	_, err = blobs.Upload(ctx, artifacts.BlobPath(srv.HubID(), digest), bytes.NewReader(content), storage.UploadOptions{})
	require.NoError(t, err)
	now := time.Now()
	a := &artifacts.Artifact{ID: tid("grant-artifact"), ScopeKind: artifacts.ScopeKindProject, ScopeRef: p1.ID,
		OwnerKind: artifacts.PrincipalKindAgent, OwnerRef: owner.ID, Title: "granted", CreatedAt: now, UpdatedAt: now}
	v := &artifacts.Version{ID: tid("grant-version"), ArtifactID: a.ID, Seq: 1, Kind: artifacts.VersionKindPublish,
		EntryPath: "g.txt", TotalBytes: int64(len(content)), FileCount: 1, CreatedAt: now, State: artifacts.VersionStateReady}
	files := []artifacts.File{{VersionID: v.ID, Path: "g.txt", Size: int64(len(content)), SHA256: digest, MediaType: "text/plain"}}
	grants := []artifacts.Grant{{ID: tid("grant-row"), ArtifactID: a.ID, SubjectKind: artifacts.SubjectPrincipal,
		SubjectRef: artifacts.PrincipalRef(artifacts.PrincipalKindAgent, grantee.ID), Permission: artifacts.GrantRead, CreatedAt: now}}
	require.NoError(t, st.CreatePublished(ctx, a, v, files, grants))

	for _, p := range []string{
		"/api/v1/artifacts/" + a.ID,
		"/api/v1/artifacts/" + a.ID + "/files/g.txt",
		"/api/v1/artifacts/" + a.ID + "/versions/1/files/g.txt",
	} {
		assert.Equal(t, http.StatusOK, doRequestWithAgentToken(t, srv, http.MethodGet, p, nil, withScope).Code, "granted, with scope: %s", p)
		assert.Equal(t, http.StatusNotFound, doRequestWithAgentToken(t, srv, http.MethodGet, p, nil, withoutScope).Code, "granted, without scope: %s", p)
	}
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

// TestArtifactsUserAccessTokensAreBounded: a user access token's ceiling
// and project boundary bound every artifact read and publish. Ownership and
// grants never reach past them: a token without artifact read, or bounded
// to another project, gets 404 on every read route even for an artifact its
// user owns or holds a grant on.
func TestArtifactsUserAccessTokensAreBounded(t *testing.T) {
	srv, s := testServer(t)
	st, blobs := enableArtifactsForTest(t, srv)
	ctx := context.Background()
	p1 := artifactProject(t, s, "uat-p1")
	p2 := artifactProject(t, s, "uat-p2")
	p3 := artifactProject(t, s, "uat-p3")
	createTestUserWithProjectRole(t, s, tid("uat-user"), "uat-user@test.com", p1.ID, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, s, tid("uat-user"), "uat-user@test.com", p2.ID, store.ProjectRoleMember)
	user, err := s.GetUser(ctx, tid("uat-user"))
	require.NoError(t, err)
	session := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")

	// An artifact the user owns, homed in p1.
	rec := identityArtifactRequest(t, srv, session, http.MethodPost, "/api/v1/artifacts?name=own.md&scope="+p1.ID, []byte("# own\n"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	owned := decodeArtifactID(t, rec)

	// An artifact homed in p3 (no role there) shared with the user by a
	// principal grant.
	content := []byte("granted")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	_, err = blobs.Upload(ctx, artifacts.BlobPath(srv.HubID(), digest), bytes.NewReader(content), storage.UploadOptions{})
	require.NoError(t, err)
	now := time.Now()
	a := &artifacts.Artifact{ID: tid("uat-granted"), ScopeKind: artifacts.ScopeKindProject, ScopeRef: p3.ID,
		OwnerKind: artifacts.PrincipalKindAgent, OwnerRef: tid("uat-other-agent"), Title: "g", CreatedAt: now, UpdatedAt: now}
	v := &artifacts.Version{ID: tid("uat-granted-v1"), ArtifactID: a.ID, Seq: 1, Kind: artifacts.VersionKindPublish,
		EntryPath: "g.txt", TotalBytes: int64(len(content)), FileCount: 1, CreatedAt: now, State: artifacts.VersionStateReady}
	require.NoError(t, st.CreatePublished(ctx, a, v,
		[]artifacts.File{{VersionID: v.ID, Path: "g.txt", Size: int64(len(content)), SHA256: digest, MediaType: "text/plain"}},
		[]artifacts.Grant{{ID: tid("uat-grant"), ArtifactID: a.ID, SubjectKind: artifacts.SubjectPrincipal,
			SubjectRef: artifacts.PrincipalRef(artifacts.PrincipalKindUser, user.ID), Permission: artifacts.GrantRead, CreatedAt: now}}))

	routes := func(id, file string) []string {
		return []string{
			"/api/v1/artifacts/" + id,
			"/api/v1/artifacts/" + id + "/files/" + file,
			"/api/v1/artifacts/" + id + "/versions/1/files/" + file,
		}
	}
	check := func(name string, identity Identity, id, file string, want int) {
		t.Helper()
		for _, p := range routes(id, file) {
			assert.Equal(t, want, identityArtifactRequest(t, srv, identity, http.MethodGet, p, nil).Code, "%s: %s", name, p)
		}
	}

	// The session user reads both (owner; principal grant).
	check("session, owned", session, owned, "own.md", http.StatusOK)
	check("session, granted", session, a.ID, "g.txt", http.StatusOK)

	// Owned artifact (home p1).
	check("UAT without artifact:read, owned", artifactTestUAT(t, session, p1.ID, "agent:read"), owned, "own.md", http.StatusNotFound)
	check("UAT bounded to p2, owned", artifactTestUAT(t, session, p2.ID, "artifact:read"), owned, "own.md", http.StatusNotFound)
	check("UAT with artifact:read in p1, owned", artifactTestUAT(t, session, p1.ID, "artifact:read"), owned, "own.md", http.StatusOK)

	// Principal-granted artifact (home p3).
	check("UAT without artifact:read, granted", artifactTestUAT(t, session, p3.ID, "agent:read"), a.ID, "g.txt", http.StatusNotFound)
	check("UAT bounded to p1, granted", artifactTestUAT(t, session, p1.ID, "artifact:read"), a.ID, "g.txt", http.StatusNotFound)
	check("UAT with artifact:read in p3, granted", artifactTestUAT(t, session, p3.ID, "artifact:read"), a.ID, "g.txt", http.StatusOK)

	// Publish needs artifact:create in the token, in its boundary.
	rec = identityArtifactRequest(t, srv, artifactTestUAT(t, session, p1.ID, "artifact:read"), http.MethodPost, "/api/v1/artifacts?name=x.md&scope="+p1.ID, []byte("x"))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = identityArtifactRequest(t, srv, artifactTestUAT(t, session, p2.ID, "artifact:create"), http.MethodPost, "/api/v1/artifacts?name=x.md&scope="+p1.ID, []byte("x"))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = identityArtifactRequest(t, srv, artifactTestUAT(t, session, p1.ID, "artifact:create"), http.MethodPost, "/api/v1/artifacts?name=x.md&scope="+p1.ID, []byte("x"))
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// TestArtifactsPreArtifactCeilingTokenOnRoutes: a token minted under a
// ceiling recorded before artifacts existed reads nothing and publishes
// nothing through the real routes.
func TestArtifactsPreArtifactCeilingTokenOnRoutes(t *testing.T) {
	srv, s := testServer(t)
	enableArtifactsForTest(t, srv)
	p1 := artifactProject(t, s, "preart-p1")
	_, ownerTok := artifactAgent(t, srv, s, p1.ID, "preart-owner", AgentRoleBaseline)
	child := &store.Agent{ID: tid("art-preart-child"), Slug: "preart-child", Name: "preart-child", ProjectID: p1.ID, Phase: "running"}
	require.NoError(t, s.CreateAgent(context.Background(), child))
	issued := filterScopes(ScopesForRole(AgentRoleBaseline), preArtifactReadCeiling(), ScopeCeilings{})
	childTok, err := srv.GetAgentTokenService().GenerateAgentToken(child.ID, p1.ID, issued, nil)
	require.NoError(t, err)

	rec := doRawAgentRequest(t, srv, http.MethodPost, "/api/v1/artifacts?name=a.txt", []byte("a"), ownerTok)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	id := decodeArtifactID(t, rec)

	for _, p := range []string{"/api/v1/artifacts/" + id, "/api/v1/artifacts/" + id + "/files/a.txt", "/api/v1/artifacts/" + id + "/versions/1/files/a.txt"} {
		assert.Equal(t, http.StatusNotFound, doRawAgentRequest(t, srv, http.MethodGet, p, nil, childTok).Code, p)
	}
	rec = doRawAgentRequest(t, srv, http.MethodPost, "/api/v1/artifacts?name=b.txt", []byte("b"), childTok)
	assert.Equal(t, http.StatusForbidden, rec.Code, "an agent without the artifact scopes is told which one it lacks: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"code":"`+artifacts.CodeMissingScope+`"`)
	assert.Contains(t, rec.Body.String(), string(ScopeProjectArtifactRead))
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

// TestArtifactServiceReachableOnlyThroughHTTPAuth pins that the artifact
// service has no in-process callers: the hub builds it in exactly one place
// (artifactsHandler), mounts it only on its own mux (server.go, pinned by
// TestArtifactRoutesMatchService), and that mux is served only behind
// applyMiddleware, whose UnifiedAuthMiddleware derives the identity from the
// request's credentials. User identities the hub constructs in process for
// its own decisions therefore never reach artifacts.Host.
func TestArtifactServiceReachableOnlyThroughHTTPAuth(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	builds := map[string]int{}
	handlerCalls := map[string]int{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		require.NoError(t, err)
		text := string(src)
		builds[name] += strings.Count(text, "artifacts.NewService(") + strings.Count(text, "newArtifactHost(s)")
		handlerCalls[name] += strings.Count(text, "s.artifactsHandler()")
		if strings.Contains(text, "mux.ServeHTTP(") {
			t.Errorf("%s serves the hub mux in process; artifact requests must pass UnifiedAuthMiddleware", name)
		}
	}
	for name, n := range builds {
		if n > 0 && name != "artifacts_store.go" {
			t.Errorf("%s builds the artifact service or its host; only artifacts_store.go may", name)
		}
	}
	for name, n := range handlerCalls {
		if n > 0 && name != "server.go" {
			t.Errorf("%s calls artifactsHandler; only route registration in server.go may", name)
		}
	}
	assert.Equal(t, 1, handlerCalls["server.go"], "artifactsHandler is built once, for route registration")

	// Behaviourally: an identity placed in the context by in-process code,
	// with no credentials on the request, does not survive the hub's
	// middleware.
	srv, s := testServer(t)
	enableArtifactsForTest(t, srv)
	p1 := artifactProject(t, s, "inproc-p1")
	createTestUserWithProjectRole(t, s, tid("inproc-user"), "inproc-user@test.com", p1.ID, store.ProjectRoleMember)
	user := NewAuthenticatedUser(tid("inproc-user"), "inproc-user@test.com", "U", "member", "web")
	rec := identityArtifactRequest(t, srv, user, http.MethodPost, "/api/v1/artifacts?name=x.md&scope="+p1.ID, []byte("x"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	id := decodeArtifactID(t, rec)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/"+id, nil)
	req = req.WithContext(contextWithIdentity(req.Context(), user))
	out := httptest.NewRecorder()
	srv.Handler().ServeHTTP(out, req)
	assert.Equal(t, http.StatusUnauthorized, out.Code, "a context identity without credentials must not read through the hub handler: %s", out.Body.String())
}

// bearerArtifactRequest sends a request with a bearer credential through
// the full hub handler.
func bearerArtifactRequest(t *testing.T, srv *Server, method, path, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestArtifactsAccessTokenAfterMembershipLoss pins the designed behaviour of
// the credential check with real access tokens: it applies the token's
// project boundary and permissions, not the holder's current project
// membership, so a token keeps reading what its user owns after the user
// loses the project role (owner access survives loss of the home scope),
// while artifacts the user neither owns nor was granted stay unreadable.
// Revoking the token cuts it off at once.
func TestArtifactsAccessTokenAfterMembershipLoss(t *testing.T) {
	srv, s := testServer(t)
	st, blobs := enableArtifactsForTest(t, srv)
	ctx := context.Background()
	p1 := artifactProject(t, s, "loss-p1")
	userID := tid("loss-user")
	createTestUserWithProjectRole(t, s, userID, "loss-user@test.com", p1.ID, store.ProjectRoleMember)
	user, err := s.GetUser(ctx, userID)
	require.NoError(t, err)

	token, row, err := srv.uatService.CreateToken(rs4MintContext(userID), userID, "artifacts", p1.ID, []string{"artifact:read", "artifact:create"}, nil)
	require.NoError(t, err)

	rec := bearerArtifactRequest(t, srv, http.MethodPost, "/api/v1/artifacts?name=own.md&scope="+p1.ID, token, []byte("# own\n"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	owned := decodeArtifactID(t, rec)

	// Another artifact in p1 that the user neither owns nor was granted.
	content := []byte("other")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	_, err = blobs.Upload(ctx, artifacts.BlobPath(srv.HubID(), digest), bytes.NewReader(content), storage.UploadOptions{})
	require.NoError(t, err)
	now := time.Now()
	other := &artifacts.Artifact{ID: tid("loss-other"), ScopeKind: artifacts.ScopeKindProject, ScopeRef: p1.ID,
		OwnerKind: artifacts.PrincipalKindAgent, OwnerRef: tid("loss-agent"), Title: "o", CreatedAt: now, UpdatedAt: now}
	ov := &artifacts.Version{ID: tid("loss-other-v1"), ArtifactID: other.ID, Seq: 1, Kind: artifacts.VersionKindPublish,
		EntryPath: "o.txt", TotalBytes: int64(len(content)), FileCount: 1, CreatedAt: now, State: artifacts.VersionStateReady}
	require.NoError(t, st.CreatePublished(ctx, other, ov,
		[]artifacts.File{{VersionID: ov.ID, Path: "o.txt", Size: int64(len(content)), SHA256: digest, MediaType: "text/plain"}},
		[]artifacts.Grant{{ID: tid("loss-other-home"), ArtifactID: other.ID, SubjectKind: artifacts.SubjectScope,
			SubjectRef: p1.ID, Permission: artifacts.GrantRead, CreatedAt: now}}))
	require.Equal(t, http.StatusOK, bearerArtifactRequest(t, srv, http.MethodGet, "/api/v1/artifacts/"+other.ID, token, nil).Code,
		"precondition: a member reads the project's artifacts")

	// The user loses every role.
	_, err = s.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, user.ID)
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, bearerArtifactRequest(t, srv, http.MethodGet, "/api/v1/artifacts/"+owned+"/files/own.md", token, nil).Code,
		"the owner's token still reads the owned artifact")
	for _, p := range []string{"/api/v1/artifacts/" + other.ID, "/api/v1/artifacts/" + other.ID + "/files/o.txt"} {
		assert.Equal(t, http.StatusNotFound, bearerArtifactRequest(t, srv, http.MethodGet, p, token, nil).Code,
			"neither owned nor granted after the role is gone: %s", p)
	}

	// Revoking the token is the immediate cut-off.
	require.NoError(t, srv.uatService.RevokeToken(rs4MintContext(userID), userID, row.ID))
	rec = bearerArtifactRequest(t, srv, http.MethodGet, "/api/v1/artifacts/"+owned+"/files/own.md", token, nil)
	assert.Contains(t, []int{http.StatusUnauthorized, http.StatusNotFound}, rec.Code, rec.Body.String())
	assert.NotEqual(t, http.StatusOK, rec.Code)
}

// TestArtifactsAgentChainCheckedAtUse: an agent's delegation chain is
// checked at use on every read path, ownership and grants included. While
// its token still carries project:artifact:read, an agent whose edge is gone
// stops reading what it owns, and an agent whose delegator no longer holds
// artifact read stops reading what it was granted. A grant to an agent in
// another project works while that agent's own chain allows reads.
func TestArtifactsAgentChainCheckedAtUse(t *testing.T) {
	srv, s := testServer(t)
	st, blobs := enableArtifactsForTest(t, srv)
	ctx := context.Background()
	p1 := artifactProject(t, s, "chain-p1")
	p2 := artifactProject(t, s, "chain-p2")
	owner, ownerTok := artifactAgent(t, srv, s, p1.ID, "chain-owner", AgentRoleBaseline)
	reader, readerTok := artifactAgent(t, srv, s, p2.ID, "chain-reader", AgentRoleBaseline)

	// An artifact owned by the p1 agent, with a principal grant to the p2
	// agent (grants have no API yet), written the way publish writes it.
	content := []byte("chain")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	_, err := blobs.Upload(ctx, artifacts.BlobPath(srv.HubID(), digest), bytes.NewReader(content), storage.UploadOptions{})
	require.NoError(t, err)
	now := time.Now()
	id := tid("chain-artifact")
	a := &artifacts.Artifact{ID: id, ScopeKind: artifacts.ScopeKindProject, ScopeRef: p1.ID,
		OwnerKind: artifacts.PrincipalKindAgent, OwnerRef: owner.ID, Title: "c", CreatedAt: now, UpdatedAt: now}
	v := &artifacts.Version{ID: tid("chain-artifact-v1"), ArtifactID: id, Seq: 1, Kind: artifacts.VersionKindPublish,
		EntryPath: "c.txt", TotalBytes: int64(len(content)), FileCount: 1, CreatedAt: now, State: artifacts.VersionStateReady}
	require.NoError(t, st.CreatePublished(ctx, a, v,
		[]artifacts.File{{VersionID: v.ID, Path: "c.txt", Size: int64(len(content)), SHA256: digest, MediaType: "text/plain"}},
		[]artifacts.Grant{
			{ID: tid("chain-home"), ArtifactID: id, SubjectKind: artifacts.SubjectScope, SubjectRef: p1.ID, Permission: artifacts.GrantRead, CreatedAt: now},
			{ID: tid("chain-grant"), ArtifactID: id, SubjectKind: artifacts.SubjectPrincipal,
				SubjectRef: artifacts.PrincipalRef(artifacts.PrincipalKindAgent, reader.ID), Permission: artifacts.GrantRead, CreatedAt: now},
		}))

	routes := []string{"/api/v1/artifacts/" + id, "/api/v1/artifacts/" + id + "/files/c.txt", "/api/v1/artifacts/" + id + "/versions/1/files/c.txt"}
	expect := func(name, token string, want int) {
		t.Helper()
		for _, p := range routes {
			assert.Equal(t, want, doRawAgentRequest(t, srv, http.MethodGet, p, nil, token).Code, "%s: %s", name, p)
		}
	}
	expect("owner", ownerTok, http.StatusOK)
	expect("cross-project grantee", readerTok, http.StatusOK)

	// The grantee's delegator loses its role in p2: the grant no longer
	// reaches past the chain.
	_, err = s.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, tid("art-delegator-"+p2.ID))
	require.NoError(t, err)
	expect("grantee with a narrowed chain", readerTok, http.StatusNotFound)

	// The owner loses its edge: ownership no longer reaches past the chain.
	_, err = s.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, owner.ID,
		store.Deactivation{Cause: store.EdgeDeactivationAgentSoftDelete, OpID: "chain-test"})
	require.NoError(t, err)
	expect("owner without an edge", ownerTok, http.StatusNotFound)
}

// TestArtifactsPublishRefusedWithBothScopesIsPlainForbidden: an agent whose
// token carries both artifact scopes but is refused for another reason (a
// project it does not belong to, or a delegation chain that no longer
// allows publishing) gets the plain 403 forbidden, never missing_scope.
func TestArtifactsPublishRefusedWithBothScopesIsPlainForbidden(t *testing.T) {
	srv, s := testServer(t)
	enableArtifactsForTest(t, srv)
	ctx := context.Background()
	p1 := artifactProject(t, s, "plain403-p1")
	p2 := artifactProject(t, s, "plain403-p2")
	_, tok := artifactAgent(t, srv, s, p1.ID, "plain403-agent", AgentRoleBaseline)
	claims, err := srv.GetAgentTokenService().ValidateAgentToken(tok)
	require.NoError(t, err)
	require.Contains(t, claims.Scopes, ScopeProjectArtifactRead)
	require.Contains(t, claims.Scopes, ScopeProjectArtifactWrite)

	assertPlainForbidden := func(name, target string) {
		t.Helper()
		rec := doRawAgentRequest(t, srv, http.MethodPost, target, []byte("x"), tok)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", name, rec.Body.String())
		assert.Contains(t, rec.Body.String(), `"code":"forbidden"`, name)
		assert.NotContains(t, rec.Body.String(), artifacts.CodeMissingScope, name)
	}
	assertPlainForbidden("another project", "/api/v1/artifacts?name=a.txt&scope="+p2.ID)

	// Its own project works until the delegator loses its role there.
	rec := doRawAgentRequest(t, srv, http.MethodPost, "/api/v1/artifacts?name=a.txt", []byte("x"), tok)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	_, err = s.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, tid("art-delegator-"+p1.ID))
	require.NoError(t, err)
	assertPlainForbidden("narrowed delegation chain", "/api/v1/artifacts?name=b.txt")
}

// TestArtifactsTwoStepBundleOnRoutes publishes a bundle through the real
// routes with the two-step API, appends a version by key, and reads it
// back as another agent of the project; an agent of another project sees
// nothing. The bundle limits come from the artifacts settings.
func TestArtifactsTwoStepBundleOnRoutes(t *testing.T) {
	srv, s := testServer(t)
	enableArtifactsForTest(t, srv)
	srv.SetOperationalSettings(artifactsOps(t, `{"max_files":2}`))
	p1 := artifactProject(t, s, "twostep-p1")
	p2 := artifactProject(t, s, "twostep-p2")
	_, ownerTok := artifactAgent(t, srv, s, p1.ID, "twostep-owner", AgentRoleBaseline)
	_, peerTok := artifactAgent(t, srv, s, p1.ID, "twostep-peer", AgentRoleBaseline)
	_, otherTok := artifactAgent(t, srv, s, p2.ID, "twostep-other", AgentRoleBaseline)

	files := map[string][]byte{"index.html": []byte("<p>hi</p>"), "img/a.png": []byte("png")}
	manifest := func(key string, fs map[string][]byte) []byte {
		req := artifacts.CreateVersionRequest{Key: key, Entry: "index.html"}
		for p, b := range fs {
			sum := sha256.Sum256(b)
			req.Files = append(req.Files, artifacts.ManifestFile{Path: p, Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:])})
		}
		body, err := json.Marshal(req)
		require.NoError(t, err)
		return body
	}
	publish := func(fs map[string][]byte) artifacts.ArtifactResponse {
		t.Helper()
		rec := doRawAgentRequest(t, srv, http.MethodPost, "/api/v1/artifacts", manifest("site", fs), ownerTok)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var pend artifacts.PendingVersionResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pend))
		base := fmt.Sprintf("/api/v1/artifacts/%s/versions/%d", pend.Artifact.ID, pend.Version.Seq)
		for _, p := range pend.Upload.Required {
			rec := doRawAgentRequest(t, srv, http.MethodPut, base+"/files/"+p, fs[p], ownerTok)
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		}
		rec = doRawAgentRequest(t, srv, http.MethodPost, base+"/finalize", nil, ownerTok)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var out artifacts.ArtifactResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out
	}
	v1 := publish(files)
	assert.Equal(t, 1, v1.Artifact.CurrentSeq)
	v2 := publish(map[string][]byte{"index.html": []byte("<p>v2</p>"), "img/a.png": []byte("png")})
	assert.Equal(t, v1.Artifact.ID, v2.Artifact.ID, "the same key appends")
	assert.Equal(t, 2, v2.Artifact.CurrentSeq)
	id := v1.Artifact.ID

	rec := doRawAgentRequest(t, srv, http.MethodGet, "/api/v1/artifacts/"+id+"/versions/1/files/img/a.png", nil, peerTok)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "png", rec.Body.String())
	rec = doRawAgentRequest(t, srv, http.MethodGet, "/api/v1/artifacts/"+id+"/versions", nil, peerTok)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"seq":2`)
	for _, p := range []string{"/versions", "/versions/1", "/versions/1/files/index.html"} {
		assert.Equal(t, http.StatusNotFound, doRawAgentRequest(t, srv, http.MethodGet, "/api/v1/artifacts/"+id+p, nil, otherTok).Code, p)
	}
	// A project peer may read but not append.
	rec = doRawAgentRequest(t, srv, http.MethodPost, "/api/v1/artifacts/"+id+"/versions", manifest("", files), peerTok)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	// The settings' file count applies.
	rec = doRawAgentRequest(t, srv, http.MethodPost, "/api/v1/artifacts", manifest("", map[string][]byte{
		"index.html": []byte("a"), "b": []byte("b"), "c": []byte("c")}), ownerTok)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
}

// TestArtifactsReaperRetiresAbandonedVersions: the hub's reap pass fails a
// pending version older than the TTL.
func TestArtifactsReaperRetiresAbandonedVersions(t *testing.T) {
	srv, _ := testServer(t)
	st, _ := enableArtifactsForTest(t, srv)
	ctx := context.Background()
	old := time.Now().Add(-artifacts.PendingVersionTTL - time.Hour)
	a := &artifacts.Artifact{ID: tid("reap-artifact"), ScopeKind: artifacts.ScopeKindProject, ScopeRef: "p",
		OwnerKind: artifacts.PrincipalKindUser, OwnerRef: "u", Title: "t", CreatedAt: old, UpdatedAt: old}
	v := &artifacts.Version{ID: tid("reap-v1"), ArtifactID: a.ID, Seq: 1, Kind: artifacts.VersionKindPublish,
		EntryPath: "a.txt", CreatedAt: old, State: artifacts.VersionStatePending}
	require.NoError(t, st.CreatePending(ctx, a, v, nil, nil))
	srv.reapArtifactVersions(ctx)
	got, err := st.GetVersion(ctx, a.ID, 1)
	require.NoError(t, err)
	assert.Equal(t, artifacts.VersionStateFailed, got.State)
}

// TestArtifactsRemoteImageRefusedOnRoutes: through the real hub, with the
// production fetcher and default settings (remote images on), a markdown
// artifact whose image points at the metadata address publishes; the image
// is a failed manifest row with a generic warning, and reading it answers
// 404 with the remote status header.
func TestArtifactsRemoteImageRefusedOnRoutes(t *testing.T) {
	srv, s := testServer(t)
	enableArtifactsForTest(t, srv)
	p1 := artifactProject(t, s, "remote-p1")
	_, tok := artifactAgent(t, srv, s, p1.ID, "remote-agent", AgentRoleBaseline)
	const metadata = "http://169.254.169.254/latest/meta-data/"
	rec := doRawAgentRequest(t, srv, http.MethodPost, "/api/v1/artifacts?name=doc.md", []byte("# Doc\n\n![m]("+metadata+")\n"), tok)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp artifacts.ArtifactResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Version.Files, 2)
	var row artifacts.FileInfo
	for _, f := range resp.Version.Files {
		if f.Path == artifacts.RemotePath(metadata) {
			row = f
		}
	}
	assert.Equal(t, artifacts.FetchStatusFailed, row.FetchStatus)
	assert.Equal(t, metadata, row.SourceURL)
	assert.Equal(t, []string{"image could not be fetched: " + metadata}, resp.Warnings)
	get := doRawAgentRequest(t, srv, http.MethodGet, "/api/v1/artifacts/"+resp.Artifact.ID+"/versions/1/files/"+row.Path, nil, tok)
	assert.Equal(t, http.StatusNotFound, get.Code)
	assert.Equal(t, artifacts.FetchStatusFailed, get.Header().Get(artifacts.HeaderRemoteStatus))
}

// TestArtifactsHTMLViewOnRoutes: through the real hub, an agent mints a
// view of an HTML bundle; the view URL serves the bundle's files with no
// credentials at all and the view CSP, a tampered capability gets 404, and
// a credential-less request elsewhere under /api/v1/artifacts/ is still
// refused.
func TestArtifactsHTMLViewOnRoutes(t *testing.T) {
	srv, s := testServer(t)
	enableArtifactsForTest(t, srv)
	require.NotEmpty(t, srv.artifactViewKey, "the hub initializes the view key")
	p1 := artifactProject(t, s, "view-p1")
	p2 := artifactProject(t, s, "view-p2")
	_, tok := artifactAgent(t, srv, s, p1.ID, "view-agent", AgentRoleBaseline)
	_, otherTok := artifactAgent(t, srv, s, p2.ID, "view-other", AgentRoleBaseline)

	page := []byte(`<img src="img/a.png">`)
	rec := doRawAgentRequest(t, srv, http.MethodPost, "/api/v1/artifacts?name=index.html", page, tok)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	id := decodeArtifactID(t, rec)

	mint := "/api/v1/artifacts/" + id + "/versions/1/view"
	assert.Equal(t, http.StatusNotFound, doRawAgentRequest(t, srv, http.MethodPost, mint, nil, otherTok).Code)
	rec = doRawAgentRequest(t, srv, http.MethodPost, mint, nil, tok)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var view artifacts.ViewResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &view))
	require.True(t, strings.HasPrefix(view.URL, artifacts.RouteView), view.URL)

	anon := httptest.NewRecorder()
	srv.Handler().ServeHTTP(anon, httptest.NewRequest(http.MethodGet, view.URL, nil))
	require.Equal(t, http.StatusOK, anon.Code, anon.Body.String())
	assert.Equal(t, string(page), anon.Body.String())
	assert.Contains(t, anon.Header().Get("Content-Security-Policy"), "sandbox allow-scripts;")
	assert.NotContains(t, anon.Header().Get("Content-Security-Policy"), "allow-same-origin")
	assert.Contains(t, anon.Header().Get("Content-Security-Policy"), "img-src example.com"+strings.TrimSuffix(view.URL, "index.html")+" data:")

	badCred := httptest.NewRequest(http.MethodGet, view.URL, nil)
	badCred.Header.Set("Authorization", "Bearer not-a-valid-token")
	rejected := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rejected, badCred)
	assert.Equal(t, http.StatusUnauthorized, rejected.Code, "an invalid credential is rejected before the view pass")

	tampered := httptest.NewRecorder()
	srv.Handler().ServeHTTP(tampered, httptest.NewRequest(http.MethodGet, strings.Replace(view.URL, ".1.", ".2.", 1), nil))
	assert.Equal(t, http.StatusNotFound, tampered.Code)

	for _, p := range []string{"/api/v1/artifacts/" + id, "/api/v1/artifacts/" + id + "/files/index.html"} {
		out := httptest.NewRecorder()
		srv.Handler().ServeHTTP(out, httptest.NewRequest(http.MethodGet, p, nil))
		assert.Equal(t, http.StatusUnauthorized, out.Code, p)
	}
	post := httptest.NewRecorder()
	srv.Handler().ServeHTTP(post, httptest.NewRequest(http.MethodPost, view.URL, nil))
	assert.Equal(t, http.StatusUnauthorized, post.Code, "only reads pass without credentials")

	// The view route answers 404 when the artifacts settings section is
	// disabled, and when the experiment is off.
	srv.SetOperationalSettings(artifactsOps(t, `{"enabled":false}`))
	off := httptest.NewRecorder()
	srv.Handler().ServeHTTP(off, httptest.NewRequest(http.MethodGet, view.URL, nil))
	assert.Equal(t, http.StatusNotFound, off.Code, "settings off")
	srv.SetOperationalSettings(artifactsOps(t, ""))
	reg, err := experiments.NewRegistry(experiments.Default().All(), nil)
	require.NoError(t, err)
	srv.experiments = reg
	require.False(t, srv.experimentEnabled(experiments.Artifacts))
	off = httptest.NewRecorder()
	srv.Handler().ServeHTTP(off, httptest.NewRequest(http.MethodGet, view.URL, nil))
	assert.Equal(t, http.StatusNotFound, off.Code, "experiment off")
}
