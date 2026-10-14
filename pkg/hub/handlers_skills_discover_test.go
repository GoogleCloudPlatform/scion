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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const skillsDiscoverPath = "/api/v1/skills/discover-directory"

// mockSkillTarball installs a mock HTTP transport serving a gzip tarball built
// from the given path→content map, and returns a cleanup func restoring the
// previous transport. Paths are relative to the tarball root and must include
// the repo-<ref> top-level prefix that GitHub codeload tarballs carry (e.g.
// "repo-main/skills/my-skill/SKILL.md").
//
// It must not be used with t.Parallel(): it mutates http.DefaultClient.Transport
// globally.
func mockSkillTarball(t *testing.T, files map[string]string) func() {
	t.Helper()
	return mockSkillTarballWithHook(t, files, nil)
}

// mockSkillTarballWithHook is mockSkillTarball plus an inspection hook invoked
// with every outbound request before the tarball is served. Tests use it to
// assert the request URL and Authorization header that reached the wire.
func mockSkillTarballWithHook(t *testing.T, files map[string]string, hook func(*http.Request)) func() {
	t.Helper()
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = &mockRoundTripper{
		roundTrip: func(req *http.Request) (*http.Response, error) {
			if hook != nil {
				hook(req)
			}
			var buf bytes.Buffer
			gzw := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gzw)
			for name, body := range files {
				if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(body))}); err != nil {
					return nil, err
				}
				if _, err := tw.Write([]byte(body)); err != nil {
					return nil, err
				}
			}
			if err := tw.Close(); err != nil {
				return nil, err
			}
			if err := gzw.Close(); err != nil {
				return nil, err
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(buf.Bytes()))}, nil
		},
	}
	return func() { http.DefaultClient.Transport = old }
}

// skillDiscoverMember creates a hub-member non-admin user for discover tests.
// Hub members get read/list on everything and create on projects, but nothing
// that grants ActionCreate on an agent inside a project they don't belong to.
func skillDiscoverMember(t *testing.T, s store.Store, id string) *store.User {
	t.Helper()
	ctx := context.Background()
	u := &store.User{
		ID: tid(id), Email: id + "@test.com", DisplayName: "Member",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	ensureHubMembership(ctx, s, u.ID)
	return u
}

// skillDiscoverProject creates a project and, when token is non-empty, stores it
// as that project's GITHUB_TOKEN secret via the local secret backend.
func skillDiscoverProject(t *testing.T, srv *Server, s store.Store, suffix, token string) string {
	t.Helper()
	ctx := context.Background()
	projectID := tid("project-skill-discover-" + suffix)
	if err := s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "Skill Discover " + suffix, Slug: "skill-discover-" + suffix,
		Created: time.Now(), Updated: time.Now(),
	}); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	if token != "" {
		srv.SetSecretBackend(secret.NewLocalBackend(s, "", "test-secret"))
		if _, _, err := srv.GetSecretBackend().Set(ctx, &secret.SetSecretInput{
			Name: "GITHUB_TOKEN", Value: token, SecretType: secret.TypeEnvironment,
			Scope: secret.ScopeProject, ScopeID: projectID,
		}); err != nil {
			t.Fatalf("failed to store GITHUB_TOKEN: %v", err)
		}
	}
	return projectID
}

// skillDiscoverAdmin creates a hub-member admin user for discover tests.
func skillDiscoverAdmin(t *testing.T, s store.Store, id string) *store.User {
	t.Helper()
	ctx := context.Background()
	// CO1: admin needs a super-admin role binding, not just the Role field.
	createTestUserWithRole(t, s, tid(id), id+"@test.com", "admin", store.SystemRoleSuperAdmin)
	admin, err := s.GetUser(ctx, tid(id))
	if err != nil {
		t.Fatal(err)
	}
	return admin
}

// decodeDiscoverSkills parses a successful discover response body.
func decodeDiscoverSkills(t *testing.T, rec *httptest.ResponseRecorder) DiscoverSkillsResponse {
	t.Helper()
	var resp DiscoverSkillsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response %q: %v", rec.Body.String(), err)
	}
	return resp
}

// decodeDiscoverError parses an error response body and returns the APIError.
func decodeDiscoverError(t *testing.T, rec *httptest.ResponseRecorder) APIError {
	t.Helper()
	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode error response %q: %v", rec.Body.String(), err)
	}
	return resp.Error
}

// TestHandleSkillsDiscoverDirectory_StandardSkillsDir verifies the happy path:
// a URL pointing at a standard skills/ directory returns one entry per child
// with a gh:// shorthand URI and a non-empty name. It also pins the outbound
// tarball URL so a wrong-repo/wrong-ref regression cannot hide behind the
// URL-agnostic transport mock.
func TestHandleSkillsDiscoverDirectory_StandardSkillsDir(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	admin := skillDiscoverAdmin(t, s, "user-skill-discover-std")

	var requestedURL string
	defer mockSkillTarballWithHook(t, map[string]string{
		"repo-main/skills/alpha-skill/SKILL.md": "---\nname: alpha-skill\n---\nAlpha",
		"repo-main/skills/beta-skill/SKILL.md":  "---\nname: beta-skill\n---\nBeta",
		"repo-main/skills/not-a-skill/README":   "no marker here",
		// Carries a SKILL.md but its name would inject URI syntax, so the name
		// guard drops it. It must still be reported in Skipped.
		"repo-main/skills/bad=name/SKILL.md": "---\nname: bad\n---\nBad",
	}, func(req *http.Request) { requestedURL = req.URL.String() })()

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, skillsDiscoverPath, DiscoverSkillsRequest{
		SourceURL: "https://github.com/acme/repo/tree/main/skills",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	const wantURL = "https://github.com/acme/repo/archive/refs/heads/main.tar.gz"
	if requestedURL != wantURL {
		t.Errorf("outbound tarball URL = %q, want %q", requestedURL, wantURL)
	}

	resp := decodeDiscoverSkills(t, rec)
	if resp.Count != 2 || len(resp.Skills) != 2 {
		t.Fatalf("expected 2 skills, got %+v", resp)
	}
	byName := map[string]string{}
	for _, sk := range resp.Skills {
		if sk.Name == "" {
			t.Errorf("skill has empty name: %+v", sk)
		}
		byName[sk.Name] = sk.URI
	}
	if got := byName["alpha-skill"]; got != "gh://acme/repo/alpha-skill@main" {
		t.Errorf("alpha-skill URI = %q, want gh://acme/repo/alpha-skill@main", got)
	}
	if got := byName["beta-skill"]; got != "gh://acme/repo/beta-skill@main" {
		t.Errorf("beta-skill URI = %q, want gh://acme/repo/beta-skill@main", got)
	}
	if _, ok := byName["not-a-skill"]; ok {
		t.Errorf("directory without SKILL.md should not be discovered: %+v", resp.Skills)
	}
	// Both the marker-less sibling and the unsafely-named one are reported so
	// the UI can explain their absence rather than silently dropping them.
	gotSkipped := map[string]bool{}
	for _, name := range resp.Skipped {
		gotSkipped[name] = true
	}
	if len(resp.Skipped) != 2 || !gotSkipped["not-a-skill"] || !gotSkipped["bad=name"] {
		t.Errorf("Skipped = %v, want [not-a-skill bad=name] in some order", resp.Skipped)
	}
}

// TestHandleSkillsDiscoverDirectory_CustomPathDir verifies that skills outside a
// standard skills/ directory keep their full https:// URL form, since the gh://
// shorthand implies the skills/ prefix.
func TestHandleSkillsDiscoverDirectory_CustomPathDir(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	admin := skillDiscoverAdmin(t, s, "user-skill-discover-custom")

	defer mockSkillTarball(t, map[string]string{
		"repo-main/tools/helpers/one-skill/SKILL.md": "one",
	})()

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, skillsDiscoverPath, DiscoverSkillsRequest{
		SourceURL: "https://github.com/acme/repo/tree/main/tools/helpers",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	resp := decodeDiscoverSkills(t, rec)
	if resp.Count != 1 || len(resp.Skills) != 1 {
		t.Fatalf("expected 1 skill, got %+v", resp)
	}
	want := "https://github.com/acme/repo/tree/main/tools/helpers/one-skill"
	if resp.Skills[0].URI != want {
		t.Errorf("URI = %q, want %q", resp.Skills[0].URI, want)
	}
	if resp.Skills[0].Name != "one-skill" {
		t.Errorf("Name = %q, want one-skill", resp.Skills[0].Name)
	}
}

// TestHandleSkillsDiscoverDirectory_LeafSkillURL verifies that pointing at a
// single skill directory (rather than a directory of skills) yields exactly one
// entry, so the UI can still offer it for add.
func TestHandleSkillsDiscoverDirectory_LeafSkillURL(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	admin := skillDiscoverAdmin(t, s, "user-skill-discover-leaf")

	defer mockSkillTarball(t, map[string]string{
		"repo-main/skills/solo-skill/SKILL.md": "solo",
	})()

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, skillsDiscoverPath, DiscoverSkillsRequest{
		SourceURL: "https://github.com/acme/repo/tree/main/skills/solo-skill",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	resp := decodeDiscoverSkills(t, rec)
	if resp.Count != 1 || len(resp.Skills) != 1 {
		t.Fatalf("expected exactly 1 skill, got %+v", resp)
	}
	if resp.Skills[0].URI != "gh://acme/repo/solo-skill@main" {
		t.Errorf("URI = %q, want gh://acme/repo/solo-skill@main", resp.Skills[0].URI)
	}
	if resp.Skills[0].Name != "solo-skill" {
		t.Errorf("Name = %q, want solo-skill", resp.Skills[0].Name)
	}
}

// TestHandleSkillsDiscoverDirectory_NoSkillsFound verifies a directory with no
// SKILL.md-bearing children is a 400 rather than an empty 200.
func TestHandleSkillsDiscoverDirectory_NoSkillsFound(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	admin := skillDiscoverAdmin(t, s, "user-skill-discover-empty")

	defer mockSkillTarball(t, map[string]string{
		"repo-main/skills/just-docs/README.md": "nothing to see",
	})()

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, skillsDiscoverPath, DiscoverSkillsRequest{
		SourceURL: "https://github.com/acme/repo/tree/main/skills",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	apiErr := decodeDiscoverError(t, rec)
	if !strings.Contains(apiErr.Message, "no skills found") {
		t.Errorf("expected 'no skills found' in message, got %q", apiErr.Message)
	}
	if apiErr.Code != ErrCodeDiscoverFailed {
		t.Errorf("code = %q, want %q", apiErr.Code, ErrCodeDiscoverFailed)
	}
}

// TestHandleSkillsDiscoverDirectory_MissingSourceURL verifies sourceUrl is required.
func TestHandleSkillsDiscoverDirectory_MissingSourceURL(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	admin := skillDiscoverAdmin(t, s, "user-skill-discover-nourl")

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, skillsDiscoverPath, DiscoverSkillsRequest{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if code := decodeDiscoverError(t, rec).Code; code != ErrCodeInvalidRequest {
		t.Errorf("code = %q, want %q", code, ErrCodeInvalidRequest)
	}
}

// TestHandleSkillsDiscoverDirectory_Unauthenticated verifies an anonymous
// request is rejected with 401.
func TestHandleSkillsDiscoverDirectory_Unauthenticated(t *testing.T) {
	srv, _, _ := testTemplateBootstrapServer(t)

	body, _ := json.Marshal(DiscoverSkillsRequest{
		SourceURL: "https://github.com/acme/repo/tree/main/skills",
	})
	req := httptest.NewRequest(http.MethodPost, skillsDiscoverPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleSkillsDiscoverDirectory_MethodNotAllowed verifies non-POST methods
// are rejected.
func TestHandleSkillsDiscoverDirectory_MethodNotAllowed(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	admin := skillDiscoverAdmin(t, s, "user-skill-discover-method")

	rec := doRequestAsUser(t, srv, admin, http.MethodGet, skillsDiscoverPath, nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d: %s", rec.Code, rec.Body.String())
	}
}

// setupSkillDiscoverAgent creates a project plus an agent in it and returns the
// server, the project ID, and a token minted with the given scopes.
func setupSkillDiscoverAgent(t *testing.T, suffix string, scopes []AgentTokenScope) (*Server, string, string) {
	t.Helper()
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	projectID := tid("project-skill-discover-" + suffix)
	if err := s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "Skill Discover " + suffix, Slug: "skill-discover-" + suffix,
		Created: time.Now(), Updated: time.Now(),
	}); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agentID := tid("agent-skill-discover-" + suffix)
	if err := s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "skill-discover-" + suffix, Name: "Skill Discover Agent",
		ProjectID: projectID, Phase: string(state.PhaseRunning), StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	token, err := srv.agentTokenService.GenerateAgentToken(agentID, projectID, scopes, nil)
	if err != nil {
		t.Fatalf("failed to generate agent token: %v", err)
	}
	return srv, projectID, token
}

// TestHandleSkillsDiscoverDirectory_AgentWithScope verifies an agent holding
// project:agent:create may discover skills for its own project.
func TestHandleSkillsDiscoverDirectory_AgentWithScope(t *testing.T) {
	srv, projectID, token := setupSkillDiscoverAgent(t, "ok",
		[]AgentTokenScope{ScopeAgentStatusUpdate, ScopeAgentCreate})

	defer mockSkillTarball(t, map[string]string{
		"repo-main/skills/agent-skill/SKILL.md": "hi",
	})()

	rec := doRequestWithAgentToken(t, srv, http.MethodPost, skillsDiscoverPath, DiscoverSkillsRequest{
		SourceURL: "https://github.com/acme/repo/tree/main/skills",
		ProjectID: projectID,
	}, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	resp := decodeDiscoverSkills(t, rec)
	if resp.Count != 1 || resp.Skills[0].URI != "gh://acme/repo/agent-skill@main" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

// TestHandleSkillsDiscoverDirectory_AgentMissingScope verifies an agent without
// project:agent:create is rejected with 403.
func TestHandleSkillsDiscoverDirectory_AgentMissingScope(t *testing.T) {
	srv, projectID, token := setupSkillDiscoverAgent(t, "noscope",
		[]AgentTokenScope{ScopeAgentStatusUpdate})

	rec := doRequestWithAgentToken(t, srv, http.MethodPost, skillsDiscoverPath, DiscoverSkillsRequest{
		SourceURL: "https://github.com/acme/repo/tree/main/skills",
		ProjectID: projectID,
	}, token)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleSkillsDiscoverDirectory_AgentOtherProject verifies an agent may not
// discover skills on behalf of a project it does not belong to.
func TestHandleSkillsDiscoverDirectory_AgentOtherProject(t *testing.T) {
	srv, _, token := setupSkillDiscoverAgent(t, "otherproj",
		[]AgentTokenScope{ScopeAgentStatusUpdate, ScopeAgentCreate})

	rec := doRequestWithAgentToken(t, srv, http.MethodPost, skillsDiscoverPath, DiscoverSkillsRequest{
		SourceURL: "https://github.com/acme/repo/tree/main/skills",
		ProjectID: tid("project-somebody-else"),
	}, token)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSkillDiscoverKind verifies the kind's marker predicate: a directory is a
// skill only when it contains a SKILL.md.
func TestSkillDiscoverKind(t *testing.T) {
	dir := t.TempDir()
	if skillDiscoverKind.isResourceDir(dir) {
		t.Error("empty dir should not be a skill dir")
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: x\n---\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if !skillDiscoverKind.isResourceDir(dir) {
		t.Error("dir with SKILL.md should be a skill dir")
	}
	if skillDiscoverKind.marker != "SKILL.md" || skillDiscoverKind.noun != "skills" {
		t.Errorf("unexpected kind config: %+v", skillDiscoverKind)
	}
	// Design invariant: skill discovery never persists anything, so the kind
	// carries no store constructor. This is what makes the handler safe to call
	// on a hub with no object storage configured — assert it so a future edit
	// that populates newStore has to justify itself.
	if skillDiscoverKind.newStore != nil {
		t.Error("skillDiscoverKind.newStore must be nil: discovery never writes to a store")
	}
}

// TestHandleSkillsDiscoverDirectory_ProjectAuthToken verifies that supplying a
// projectId makes the fetch spend that project's GitHub credentials: the
// GITHUB_TOKEN secret must reach the wire as a Bearer header. Without this the
// whole credential-scoping path (and the authorization that guards it) is
// unverified.
func TestHandleSkillsDiscoverDirectory_ProjectAuthToken(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	admin := skillDiscoverAdmin(t, s, "user-skill-discover-token")
	projectID := skillDiscoverProject(t, srv, s, "token", "my-secret-token-12345")

	var capturedAuthHeader string
	defer mockSkillTarballWithHook(t, map[string]string{
		"repo-main/skills/private-skill/SKILL.md": "private",
	}, func(req *http.Request) {
		capturedAuthHeader = req.Header.Get("Authorization")
	})()

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, skillsDiscoverPath, DiscoverSkillsRequest{
		SourceURL: "https://github.com/acme/repo/tree/main/skills",
		ProjectID: projectID,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if capturedAuthHeader != "Bearer my-secret-token-12345" {
		t.Errorf("Authorization header = %q, want %q", capturedAuthHeader, "Bearer my-secret-token-12345")
	}
}

// TestHandleSkillsDiscoverDirectory_UserNotInProject verifies FIX-1: a
// hub-member user who is not a member of the named project may not spend that
// project's GitHub credentials.
func TestHandleSkillsDiscoverDirectory_UserNotInProject(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	outsider := skillDiscoverMember(t, s, "user-skill-discover-outsider")
	projectID := skillDiscoverProject(t, srv, s, "outsider", "should-never-be-used")

	// No transport mock: a correctly-behaving handler rejects before fetching.
	rec := doRequestAsUser(t, srv, outsider, http.MethodPost, skillsDiscoverPath, DiscoverSkillsRequest{
		SourceURL: "https://github.com/acme/repo/tree/main/skills",
		ProjectID: projectID,
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if code := decodeDiscoverError(t, rec).Code; code != ErrCodeForbidden {
		t.Errorf("code = %q, want %q", code, ErrCodeForbidden)
	}
}

// TestHandleSkillsDiscoverDirectory_UnknownProject verifies a projectId that
// does not resolve fails loudly rather than silently degrading to an
// unauthenticated fetch. Uses an admin so the authz check passes and the
// existence check is what rejects.
func TestHandleSkillsDiscoverDirectory_UnknownProject(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	admin := skillDiscoverAdmin(t, s, "user-skill-discover-noproj")

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, skillsDiscoverPath, DiscoverSkillsRequest{
		SourceURL: "https://github.com/acme/repo/tree/main/skills",
		ProjectID: tid("project-does-not-exist"),
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleSkillsDiscoverDirectory_NonAdminUser verifies discovery is not
// accidentally admin-only. Adding a hub-scope injected skill requires admin,
// but probing a public repo does not.
func TestHandleSkillsDiscoverDirectory_NonAdminUser(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	member := skillDiscoverMember(t, s, "user-skill-discover-plain")

	defer mockSkillTarball(t, map[string]string{
		"repo-main/skills/public-skill/SKILL.md": "public",
	})()

	rec := doRequestAsUser(t, srv, member, http.MethodPost, skillsDiscoverPath, DiscoverSkillsRequest{
		SourceURL: "https://github.com/acme/repo/tree/main/skills",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleSkillsDiscoverDirectory_FetchFailure verifies a failed fetch returns
// a generic message. The raw error must never be echoed: the sparse-checkout
// fallback builds an "https://x-access-token:<TOKEN>@github.com/..." remote and
// git's stderr repeats that URL verbatim.
func TestHandleSkillsDiscoverDirectory_FetchFailure(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	admin := skillDiscoverAdmin(t, s, "user-skill-discover-fetchfail")
	projectID := skillDiscoverProject(t, srv, s, "fetchfail", "leaky-token-98765")

	// Emptying PATH makes the git fallback fail on exec.LookPath, so the test
	// stays hermetic (no clone attempt against the real github.com).
	t.Setenv("PATH", "")

	old := http.DefaultClient.Transport
	defer func() { http.DefaultClient.Transport = old }()
	http.DefaultClient.Transport = &mockRoundTripper{
		roundTrip: func(_ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Body:       io.NopCloser(bytes.NewReader(nil)),
			}, nil
		},
	}

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, skillsDiscoverPath, DiscoverSkillsRequest{
		SourceURL: "https://github.com/acme/repo/tree/main/skills",
		ProjectID: projectID,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	apiErr := decodeDiscoverError(t, rec)
	if !strings.Contains(apiErr.Message, "Failed to fetch remote skills") {
		t.Errorf("message = %q, want it to contain %q", apiErr.Message, "Failed to fetch remote skills")
	}
	if apiErr.Code != ErrCodeDiscoverFailed {
		t.Errorf("code = %q, want %q", apiErr.Code, ErrCodeDiscoverFailed)
	}
	if body := rec.Body.String(); strings.Contains(body, "leaky-token-98765") ||
		strings.Contains(body, "x-access-token") {
		t.Errorf("response body leaks credentials: %s", body)
	}
}

// TestHandleSkillsDiscoverDirectory_NonRemoteURL verifies FIX-2: only
// https://github.com/ URLs are accepted. config.IsRemoteURI would have admitted
// the rclone and http:// forms, giving an attacker a filesystem-copy and an
// SSRF primitive respectively.
func TestHandleSkillsDiscoverDirectory_NonRemoteURL(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	admin := skillDiscoverAdmin(t, s, "user-skill-discover-badurl")

	// Any fetch attempt is a failure of the test's premise; make it loud.
	old := http.DefaultClient.Transport
	defer func() { http.DefaultClient.Transport = old }()
	fetched := false
	http.DefaultClient.Transport = &mockRoundTripper{
		roundTrip: func(req *http.Request) (*http.Response, error) {
			fetched = true
			return nil, fmt.Errorf("unexpected outbound request to %s", req.URL)
		},
	}

	cases := []struct {
		name      string
		sourceURL string
	}{
		{"gh shorthand", "gh://org/repo/skill@main"},
		{"rclone local", ":local:/"},
		{"rclone remote", "myremote:bucket/skills"},
		{"ftp scheme", "ftp://host/x"},
		{"plain http internal host", "http://internal/path"},
		{"http github", "http://github.com/acme/repo/tree/main/skills"},
		{"other https host", "https://gitlab.com/acme/repo/tree/main/skills"},
		{"bare name", "my-skill"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequestAsUser(t, srv, admin, http.MethodPost, skillsDiscoverPath, DiscoverSkillsRequest{
				SourceURL: tc.sourceURL,
			})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for %q, got %d: %s", tc.sourceURL, rec.Code, rec.Body.String())
			}
			if code := decodeDiscoverError(t, rec).Code; code != ErrCodeInvalidRequest {
				t.Errorf("code = %q, want %q", code, ErrCodeInvalidRequest)
			}
		})
	}
	if fetched {
		t.Error("handler attempted a fetch for a rejected sourceUrl")
	}
}

// TestHandleSkillsDiscoverDirectory_MixedCaseHost verifies the host gate and the
// downstream fetch agree on case. The gate is deliberately case-insensitive
// (hostnames are), but config.DetectRemoteType matches "github.com" exactly, so
// without canonicalization a mixed-case host passed validation and then died in
// the fetch layer with a generic 400.
func TestHandleSkillsDiscoverDirectory_MixedCaseHost(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	admin := skillDiscoverAdmin(t, s, "user-skill-discover-mixedcase")

	var requestedURL string
	defer mockSkillTarballWithHook(t, map[string]string{
		"repo-main/skills/alpha-skill/SKILL.md": "alpha",
	}, func(req *http.Request) { requestedURL = req.URL.String() })()

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, skillsDiscoverPath, DiscoverSkillsRequest{
		SourceURL: "https://GitHub.COM/acme/repo/tree/main/skills",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	const wantURL = "https://github.com/acme/repo/archive/refs/heads/main.tar.gz"
	if requestedURL != wantURL {
		t.Errorf("outbound tarball URL = %q, want %q", requestedURL, wantURL)
	}

	resp := decodeDiscoverSkills(t, rec)
	if resp.Count != 1 || resp.Skills[0].URI != "gh://acme/repo/alpha-skill@main" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

// TestHandleSkillsDiscoverDirectory_SourceURLWithFragment verifies a #fragment is
// stripped before discoverResourceDirs appends "/<child>". Left in place it would
// produce child URLs like ".../skills#notes/alpha-skill", which look plausible
// but resolve to nothing.
func TestHandleSkillsDiscoverDirectory_SourceURLWithFragment(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	admin := skillDiscoverAdmin(t, s, "user-skill-discover-fragment")

	var requestedURL string
	defer mockSkillTarballWithHook(t, map[string]string{
		"repo-main/skills/alpha-skill/SKILL.md": "alpha",
		"repo-main/skills/beta-skill/SKILL.md":  "beta",
	}, func(req *http.Request) { requestedURL = req.URL.String() })()

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, skillsDiscoverPath, DiscoverSkillsRequest{
		SourceURL: "https://github.com/acme/repo/tree/main/skills#notes",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(requestedURL, "notes") {
		t.Errorf("fragment leaked into the fetch URL: %s", requestedURL)
	}

	resp := decodeDiscoverSkills(t, rec)
	if resp.Count != 2 {
		t.Fatalf("expected 2 skills, got %+v", resp)
	}
	want := map[string]bool{
		"gh://acme/repo/alpha-skill@main": true,
		"gh://acme/repo/beta-skill@main":  true,
	}
	for _, sk := range resp.Skills {
		if !want[sk.URI] {
			t.Errorf("unexpected URI %q; want one of %v", sk.URI, want)
		}
		delete(want, sk.URI)
	}
	if len(want) != 0 {
		t.Errorf("missing URIs: %v", want)
	}
}

// TestHandleSkillsDiscoverDirectory_StripsURLCredentials verifies userinfo is
// dropped during canonicalization. "https://x-access-token:SECRET@github.com/..."
// parses with Host == "github.com", and the canonical base is both logged and
// echoed in the "no skills found at <base>" message — so a pasted credential
// would otherwise reach the hub log, the client, and GitHub.
func TestHandleSkillsDiscoverDirectory_StripsURLCredentials(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	admin := skillDiscoverAdmin(t, s, "user-skill-discover-userinfo")

	var requestedURL, authHeader string
	defer mockSkillTarballWithHook(t, map[string]string{
		"repo-main/skills/alpha-skill/SKILL.md": "alpha",
	}, func(req *http.Request) {
		requestedURL = req.URL.String()
		authHeader = req.Header.Get("Authorization")
	})()

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, skillsDiscoverPath, DiscoverSkillsRequest{
		SourceURL: "https://x-access-token:pasted-secret-4242@github.com/acme/repo/tree/main/skills",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	const wantURL = "https://github.com/acme/repo/archive/refs/heads/main.tar.gz"
	if requestedURL != wantURL {
		t.Errorf("outbound tarball URL = %q, want %q", requestedURL, wantURL)
	}
	if strings.Contains(authHeader, "pasted-secret-4242") {
		t.Errorf("pasted credential reached the wire as auth: %q", authHeader)
	}
	if body := rec.Body.String(); strings.Contains(body, "pasted-secret-4242") ||
		strings.Contains(body, "x-access-token") {
		t.Errorf("response body echoes the pasted credential: %s", body)
	}
}

// TestHandleSkillsDiscoverDirectory_SourceURLWithoutTreeRef verifies a GitHub URL
// with no /tree/<ref>/ segment is rejected up front. Such a URL would otherwise
// pass the host check, spend a full tarball fetch, and then fail normalization
// for every child — surfacing as a misleading "no skills found".
func TestHandleSkillsDiscoverDirectory_SourceURLWithoutTreeRef(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	admin := skillDiscoverAdmin(t, s, "user-skill-discover-notree")

	// Any fetch attempt is a failure of the test's premise; make it loud.
	old := http.DefaultClient.Transport
	defer func() { http.DefaultClient.Transport = old }()
	fetched := false
	http.DefaultClient.Transport = &mockRoundTripper{
		roundTrip: func(req *http.Request) (*http.Response, error) {
			fetched = true
			return nil, fmt.Errorf("unexpected outbound request to %s", req.URL)
		},
	}

	cases := []struct {
		name      string
		sourceURL string
	}{
		{"bare repo", "https://github.com/acme/repo"},
		{"repo with trailing slash", "https://github.com/acme/repo/"},
		{"path without tree", "https://github.com/acme/repo/skills"},
		{"org only", "https://github.com/acme"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequestAsUser(t, srv, admin, http.MethodPost, skillsDiscoverPath, DiscoverSkillsRequest{
				SourceURL: tc.sourceURL,
			})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for %q, got %d: %s", tc.sourceURL, rec.Code, rec.Body.String())
			}
			apiErr := decodeDiscoverError(t, rec)
			if apiErr.Code != ErrCodeInvalidRequest {
				t.Errorf("code = %q, want %q", apiErr.Code, ErrCodeInvalidRequest)
			}
			if !strings.Contains(apiErr.Message, "tree") {
				t.Errorf("message = %q, want an actionable message mentioning 'tree'", apiErr.Message)
			}
		})
	}
	if fetched {
		t.Error("handler attempted a fetch for a sourceUrl with no /tree/<ref>/ segment")
	}
}

// TestHandleSkillsDiscoverDirectory_CacheCleanup is a canary for the handler's
// `defer os.RemoveAll(cachePath)`: discovery persists nothing, so the fetched
// tarball must not outlive the response. HOME is redirected so the assertion
// covers a private copy of ~/.scion/cache/remote-templates.
func TestHandleSkillsDiscoverDirectory_CacheCleanup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cacheRoot := filepath.Join(home, ".scion", "cache", "remote-templates")

	srv, s, _ := testTemplateBootstrapServer(t)
	admin := skillDiscoverAdmin(t, s, "user-skill-discover-cleanup")

	defer mockSkillTarball(t, map[string]string{
		"repo-main/skills/alpha-skill/SKILL.md": "alpha",
	})()

	countCacheEntries := func() int {
		entries, err := os.ReadDir(cacheRoot)
		if err != nil {
			if os.IsNotExist(err) {
				return 0
			}
			t.Fatalf("failed to read cache root %s: %v", cacheRoot, err)
		}
		return len(entries)
	}

	before := countCacheEntries()

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, skillsDiscoverPath, DiscoverSkillsRequest{
		SourceURL: "https://github.com/acme/repo/tree/main/skills",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if after := countCacheEntries(); after != before {
		t.Errorf("cache entries under %s = %d after discovery, want %d — the fetched "+
			"tree was not cleaned up", cacheRoot, after, before)
	}
}

// TestHandleSkillsDiscoverDirectory_MalformedBody verifies a body that is not
// JSON is a 400 rather than a panic or a 500.
func TestHandleSkillsDiscoverDirectory_MalformedBody(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	admin := skillDiscoverAdmin(t, s, "user-skill-discover-badbody")

	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		admin.ID, admin.Email, admin.DisplayName, admin.Role, ClientTypeWeb,
	)
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, skillsDiscoverPath, bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleSkillsDiscoverDirectory_TokenSuffix verifies FIX-5: a
// ?token=SECRET_NAME suffix survives discovery. discoverResourceDirs builds
// child URLs by plain concatenation, so without the split/re-attach every child
// URL would read ".../skills?token=X/child" and fail normalization, producing a
// spurious "no skills found".
func TestHandleSkillsDiscoverDirectory_TokenSuffix(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	admin := skillDiscoverAdmin(t, s, "user-skill-discover-tokensuffix")

	var requestedURL string
	defer mockSkillTarballWithHook(t, map[string]string{
		"repo-main/skills/alpha-skill/SKILL.md": "alpha",
		"repo-main/skills/beta-skill/SKILL.md":  "beta",
	}, func(req *http.Request) { requestedURL = req.URL.String() })()

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, skillsDiscoverPath, DiscoverSkillsRequest{
		SourceURL: "https://github.com/acme/repo/tree/main/skills?token=SKILLS_TOKEN",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// The suffix names a secret; it must not travel to GitHub.
	if strings.Contains(requestedURL, "SKILLS_TOKEN") {
		t.Errorf("token suffix leaked into the fetch URL: %s", requestedURL)
	}

	resp := decodeDiscoverSkills(t, rec)
	if resp.Count != 2 {
		t.Fatalf("expected 2 skills, got %+v", resp)
	}
	want := map[string]bool{
		"gh://acme/repo/alpha-skill@main?token=SKILLS_TOKEN": true,
		"gh://acme/repo/beta-skill@main?token=SKILLS_TOKEN":  true,
	}
	for _, sk := range resp.Skills {
		if !want[sk.URI] {
			t.Errorf("unexpected URI %q; want one of %v", sk.URI, want)
		}
		delete(want, sk.URI)
	}
	if len(want) != 0 {
		t.Errorf("missing URIs: %v", want)
	}
}

// TestHandleSkillsDiscoverDirectory_SkipsUnnormalizableChild verifies graceful
// degradation: a child that cannot yield a valid skill URI is dropped and the
// rest are still returned.
//
// With the FIX-6 name guard in place, the reachable form of "unnormalizable
// child" is a directory whose name carries URI syntax — the guard rejects it
// before NormalizeSkillURI would, and for the same reason: a directory named
// "helper?token=PROD_SECRET" would otherwise smuggle a secret reference into a
// URI the client is invited to add.
func TestHandleSkillsDiscoverDirectory_SkipsUnnormalizableChild(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	admin := skillDiscoverAdmin(t, s, "user-skill-discover-skipchild")

	defer mockSkillTarball(t, map[string]string{
		"repo-main/skills/alpha-skill/SKILL.md":              "alpha",
		"repo-main/skills/beta-skill/SKILL.md":               "beta",
		"repo-main/skills/helper?token=PROD_SECRET/SKILL.md": "sneaky",
	})()

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, skillsDiscoverPath, DiscoverSkillsRequest{
		SourceURL: "https://github.com/acme/repo/tree/main/skills",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	resp := decodeDiscoverSkills(t, rec)
	if resp.Count != 2 || len(resp.Skills) != 2 {
		t.Fatalf("expected exactly the 2 valid skills, got %+v", resp)
	}
	for _, sk := range resp.Skills {
		if strings.Contains(sk.URI, "PROD_SECRET") || strings.Contains(sk.Name, "?") {
			t.Errorf("unsafe directory name leaked into the response: %+v", sk)
		}
	}
}

// TestHandleSkillsDiscoverDirectory_AllChildrenUnnormalizable is the companion
// to the case above: when nothing survives, the result is a 400 rather than an
// empty 200.
func TestHandleSkillsDiscoverDirectory_AllChildrenUnnormalizable(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	admin := skillDiscoverAdmin(t, s, "user-skill-discover-allbad")

	defer mockSkillTarball(t, map[string]string{
		"repo-main/skills/one?token=A/SKILL.md": "one",
		"repo-main/skills/two=two/SKILL.md":     "two",
	})()

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, skillsDiscoverPath, DiscoverSkillsRequest{
		SourceURL: "https://github.com/acme/repo/tree/main/skills",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if msg := decodeDiscoverError(t, rec).Message; !strings.Contains(msg, "no skills found") {
		t.Errorf("message = %q, want it to contain 'no skills found'", msg)
	}
}

// TestHandleSkillsDiscoverDirectory_AgentOmitsProjectID verifies the handler
// force-scopes an agent's request onto its own project. Without that, an agent
// that left projectId off would silently get an unauthenticated fetch and fail
// on private repos.
func TestHandleSkillsDiscoverDirectory_AgentOmitsProjectID(t *testing.T) {
	srv, projectID, token := setupSkillDiscoverAgent(t, "noprojid",
		[]AgentTokenScope{ScopeAgentStatusUpdate, ScopeAgentCreate})

	srv.SetSecretBackend(secret.NewLocalBackend(srv.store, "", "test-secret"))
	if _, _, err := srv.GetSecretBackend().Set(context.Background(), &secret.SetSecretInput{
		Name: "GITHUB_TOKEN", Value: "agent-project-token", SecretType: secret.TypeEnvironment,
		Scope: secret.ScopeProject, ScopeID: projectID,
	}); err != nil {
		t.Fatalf("failed to store GITHUB_TOKEN: %v", err)
	}

	var capturedAuthHeader string
	defer mockSkillTarballWithHook(t, map[string]string{
		"repo-main/skills/agent-skill/SKILL.md": "hi",
	}, func(req *http.Request) {
		capturedAuthHeader = req.Header.Get("Authorization")
	})()

	rec := doRequestWithAgentToken(t, srv, http.MethodPost, skillsDiscoverPath, DiscoverSkillsRequest{
		SourceURL: "https://github.com/acme/repo/tree/main/skills",
	}, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if capturedAuthHeader != "Bearer agent-project-token" {
		t.Errorf("Authorization header = %q, want %q — agent request was not scoped to its own project",
			capturedAuthHeader, "Bearer agent-project-token")
	}
}
