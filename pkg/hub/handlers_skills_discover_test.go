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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
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
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = &mockRoundTripper{
		roundTrip: func(_ *http.Request) (*http.Response, error) {
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

// skillDiscoverAdmin creates a hub-member admin user for discover tests.
func skillDiscoverAdmin(t *testing.T, s store.Store, id string) *store.User {
	t.Helper()
	ctx := context.Background()
	admin := &store.User{ID: tid(id), Email: id + "@test.com", DisplayName: "Admin", Role: store.UserRoleAdmin}
	if err := s.CreateUser(ctx, admin); err != nil {
		t.Fatal(err)
	}
	ensureHubMembership(ctx, s, admin.ID)
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

// TestHandleSkillsDiscoverDirectory_StandardSkillsDir verifies the happy path:
// a URL pointing at a standard skills/ directory returns one entry per child
// with a gh:// shorthand URI and a non-empty name.
func TestHandleSkillsDiscoverDirectory_StandardSkillsDir(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	admin := skillDiscoverAdmin(t, s, "user-skill-discover-std")

	defer mockSkillTarball(t, map[string]string{
		"repo-main/skills/alpha-skill/SKILL.md": "---\nname: alpha-skill\n---\nAlpha",
		"repo-main/skills/beta-skill/SKILL.md":  "---\nname: beta-skill\n---\nBeta",
		"repo-main/skills/not-a-skill/README":   "no marker here",
	})()

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, skillsDiscoverPath, DiscoverSkillsRequest{
		SourceURL: "https://github.com/acme/repo/tree/main/skills",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
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
	if body := rec.Body.String(); !bytes.Contains([]byte(body), []byte("no skills found")) {
		t.Errorf("expected 'no skills found' in body, got %q", body)
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
}
