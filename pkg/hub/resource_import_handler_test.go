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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// mockTemplateTarball installs a mock HTTP transport that serves a gzip tarball
// containing a single template under templates/my-template, and returns a
// cleanup func that restores the previous transport. It must not be used with
// t.Parallel() (it mutates http.DefaultClient.Transport globally).
func mockTemplateTarball(t *testing.T) func() {
	t.Helper()
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = &mockRoundTripper{
		roundTrip: func(req *http.Request) (*http.Response, error) {
			var buf bytes.Buffer
			gzw := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gzw)
			files := map[string]string{
				"repo-main/templates/my-template/scion-agent.yaml": "schema_version: \"1\"\nharness: claude\n",
				"repo-main/templates/my-template/README.md":        "hello",
			}
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

// TestHandleResourcesImport_GlobalAsAdmin verifies an admin can import a
// global-scoped template via the unified endpoint and that it lands in the
// store with global scope.
func TestHandleResourcesImport_GlobalAsAdmin(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	admin := &store.User{ID: "user-admin", Email: "admin@test.com", DisplayName: "Admin", Role: store.UserRoleAdmin}
	if err := s.CreateUser(ctx, admin); err != nil {
		t.Fatal(err)
	}
	ensureHubMembership(ctx, s, admin.ID)

	defer mockTemplateTarball(t)()

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/resources/import", ImportResourcesRequest{
		Kind:      "template",
		Scope:     "global",
		SourceURL: "https://github.com/acme/repo/tree/main/templates",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ImportResourcesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Count != 1 || len(resp.Imported) != 1 || resp.Imported[0] != "my-template" {
		t.Fatalf("expected [my-template], got %+v", resp)
	}

	result, err := s.ListTemplates(ctx, store.TemplateFilter{Scope: store.TemplateScopeGlobal}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 1 {
		t.Fatalf("expected 1 global template, got %d", result.TotalCount)
	}
	if result.Items[0].Scope != store.TemplateScopeGlobal {
		t.Errorf("expected global scope, got %q", result.Items[0].Scope)
	}
}

// doStreamRequestAsUser issues a request as user with Accept:
// application/x-ndjson so the import endpoint streams progress events, and
// returns the recorder for parsing.
func doStreamRequestAsUser(t *testing.T, srv *Server, user *store.User, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()

	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb,
	)
	if err != nil {
		t.Fatal(err)
	}

	var bodyBytes []byte
	if body != nil {
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// parseImportEvents splits an NDJSON body into ResourceImportEvents.
func parseImportEvents(t *testing.T, body string) []ResourceImportEvent {
	t.Helper()
	var events []ResourceImportEvent
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev ResourceImportEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("failed to parse event line %q: %v", line, err)
		}
		events = append(events, ev)
	}
	return events
}

// TestHandleResourcesImport_StreamsProgress verifies the unified endpoint emits
// NDJSON progress events (discovered → started → completed → done) when the
// client opts in via Accept: application/x-ndjson.
func TestHandleResourcesImport_StreamsProgress(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	admin := &store.User{ID: "user-admin", Email: "admin@test.com", DisplayName: "Admin", Role: store.UserRoleAdmin}
	if err := s.CreateUser(ctx, admin); err != nil {
		t.Fatal(err)
	}
	ensureHubMembership(ctx, s, admin.ID)

	defer mockTemplateTarball(t)()

	rec := doStreamRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/resources/import", ImportResourcesRequest{
		Kind:      "template",
		Scope:     "global",
		SourceURL: "https://github.com/acme/repo/tree/main/templates",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/x-ndjson") {
		t.Fatalf("expected NDJSON content type, got %q", ct)
	}

	events := parseImportEvents(t, rec.Body.String())
	if len(events) == 0 {
		t.Fatal("expected progress events, got none")
	}

	var discovered, done *ResourceImportEvent
	var completedNames []string
	for i := range events {
		switch events[i].Type {
		case ImportEventDiscovered:
			discovered = &events[i]
		case ImportEventCompleted:
			completedNames = append(completedNames, events[i].Name)
		case ImportEventError:
			t.Fatalf("unexpected error event: %s", events[i].Reason)
		case ImportEventDone:
			done = &events[i]
		}
	}

	if discovered == nil {
		t.Fatal("missing discovered event")
	}
	if discovered.Total != 1 || len(discovered.Names) != 1 || discovered.Names[0] != "my-template" {
		t.Fatalf("unexpected discovered event: %+v", discovered)
	}
	if len(completedNames) != 1 || completedNames[0] != "my-template" {
		t.Fatalf("expected my-template completed, got %v", completedNames)
	}
	if done == nil {
		t.Fatal("missing done event")
	}
	if len(done.Imported) != 1 || done.Imported[0] != "my-template" {
		t.Fatalf("unexpected done summary: %+v", done)
	}

	// The import still lands in the store.
	result, err := s.ListTemplates(ctx, store.TemplateFilter{Scope: store.TemplateScopeGlobal}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 1 {
		t.Fatalf("expected 1 global template, got %d", result.TotalCount)
	}
}

// TestHandleResourcesImport_StreamErrorEvent verifies that a failure reached
// after the stream has started (here: nothing found at the URL) is reported as
// an in-band error event rather than an HTTP error status.
func TestHandleResourcesImport_StreamErrorEvent(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	admin := &store.User{ID: "user-admin", Email: "admin@test.com", DisplayName: "Admin", Role: store.UserRoleAdmin}
	if err := s.CreateUser(ctx, admin); err != nil {
		t.Fatal(err)
	}
	ensureHubMembership(ctx, s, admin.ID)

	// Serve an empty tarball: no resource dirs are discovered.
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = &mockRoundTripper{
		roundTrip: func(req *http.Request) (*http.Response, error) {
			var buf bytes.Buffer
			gzw := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gzw)
			_ = tw.WriteHeader(&tar.Header{Name: "repo-main/", Mode: 0700, Typeflag: tar.TypeDir})
			_ = tw.Close()
			_ = gzw.Close()
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(buf.Bytes()))}, nil
		},
	}
	defer func() { http.DefaultClient.Transport = old }()

	rec := doStreamRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/resources/import", ImportResourcesRequest{
		Kind:      "template",
		Scope:     "global",
		SourceURL: "https://github.com/acme/repo/tree/main/templates",
	})
	// Stream already committed a 200; the failure surfaces as an error event.
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (stream started), got %d: %s", rec.Code, rec.Body.String())
	}

	events := parseImportEvents(t, rec.Body.String())
	var sawError bool
	for _, ev := range events {
		if ev.Type == ImportEventError {
			sawError = true
		}
	}
	if !sawError {
		t.Fatalf("expected an error event, got events: %+v", events)
	}
}

// TestHandleResourcesImport_GlobalForbiddenForMember verifies a non-admin user
// cannot import global resources.
func TestHandleResourcesImport_GlobalForbiddenForMember(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	member := &store.User{ID: "user-member", Email: "member@test.com", DisplayName: "Member", Role: store.UserRoleMember}
	if err := s.CreateUser(ctx, member); err != nil {
		t.Fatal(err)
	}
	ensureHubMembership(ctx, s, member.ID)

	rec := doRequestAsUser(t, srv, member, http.MethodPost, "/api/v1/resources/import", ImportResourcesRequest{
		Kind:      "template",
		Scope:     "global",
		SourceURL: "https://github.com/acme/repo/tree/main/templates",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	result, err := s.ListTemplates(ctx, store.TemplateFilter{Scope: store.TemplateScopeGlobal}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 0 {
		t.Fatalf("expected no templates imported, got %d", result.TotalCount)
	}
}

// TestHandleResourcesImport_InvalidKind verifies an unknown kind is rejected.
func TestHandleResourcesImport_InvalidKind(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	admin := &store.User{ID: "user-admin", Email: "admin@test.com", DisplayName: "Admin", Role: store.UserRoleAdmin}
	if err := s.CreateUser(ctx, admin); err != nil {
		t.Fatal(err)
	}
	ensureHubMembership(ctx, s, admin.ID)

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/resources/import", ImportResourcesRequest{
		Kind:      "not-a-kind",
		Scope:     "global",
		SourceURL: "https://github.com/acme/repo/tree/main/templates",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// mockHarnessConfigTarball installs a mock HTTP transport that serves a gzip
// tarball containing a single harness-config directory, and returns a cleanup
// func. It must not be used with t.Parallel().
func mockHarnessConfigTarball(t *testing.T) func() {
	t.Helper()
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = &mockRoundTripper{
		roundTrip: func(req *http.Request) (*http.Response, error) {
			var buf bytes.Buffer
			gzw := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gzw)
			files := map[string]string{
				"repo-main/harness-configs/my-config/config.yaml": "harness: claude\n",
				"repo-main/harness-configs/my-config/README.md":   "hello",
			}
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

// mockSingleHarnessConfigTarball serves a tarball where the pointed-to path IS
// the harness-config (leaf), not a parent of configs.
func mockSingleHarnessConfigTarball(t *testing.T) func() {
	t.Helper()
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = &mockRoundTripper{
		roundTrip: func(req *http.Request) (*http.Response, error) {
			var buf bytes.Buffer
			gzw := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gzw)
			files := map[string]string{
				"repo-main/harnesses/antigravity/config.yaml": "harness: claude\n",
				"repo-main/harnesses/antigravity/README.md":   "hello",
			}
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

// TestHandleResourcesImport_HarnessConfigGlobal verifies importing harness-configs
// via the unified endpoint with global scope.
func TestHandleResourcesImport_HarnessConfigGlobal(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	admin := &store.User{ID: tid("user-admin-hc"), Email: "admin-hc@test.com", DisplayName: "Admin", Role: store.UserRoleAdmin}
	if err := s.CreateUser(ctx, admin); err != nil {
		t.Fatal(err)
	}
	ensureHubMembership(ctx, s, admin.ID)

	defer mockHarnessConfigTarball(t)()

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/resources/import", ImportResourcesRequest{
		Kind:      "harness-config",
		Scope:     "global",
		SourceURL: "https://github.com/acme/repo/tree/main/harness-configs",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ImportResourcesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Count != 1 || len(resp.Imported) != 1 || resp.Imported[0] != "my-config" {
		t.Fatalf("expected [my-config], got %+v", resp)
	}

	result, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{Scope: store.HarnessConfigScopeGlobal}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 1 {
		t.Fatalf("expected 1 global harness-config, got %d", result.TotalCount)
	}
	if result.Items[0].Scope != store.HarnessConfigScopeGlobal {
		t.Errorf("expected global scope, got %q", result.Items[0].Scope)
	}
}

// TestHandleResourcesImport_SingleHarnessConfig verifies importing a single
// harness-config directory (not a directory-of-directories) works correctly.
func TestHandleResourcesImport_SingleHarnessConfig(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	admin := &store.User{ID: tid("user-admin-single-hc"), Email: "admin-single-hc@test.com", DisplayName: "Admin", Role: store.UserRoleAdmin}
	if err := s.CreateUser(ctx, admin); err != nil {
		t.Fatal(err)
	}
	ensureHubMembership(ctx, s, admin.ID)

	defer mockSingleHarnessConfigTarball(t)()

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/resources/import", ImportResourcesRequest{
		Kind:      "harness-config",
		Scope:     "global",
		SourceURL: "https://github.com/acme/repo/tree/main/harnesses/antigravity",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ImportResourcesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Count != 1 || len(resp.Imported) != 1 || resp.Imported[0] != "antigravity" {
		t.Fatalf("expected [antigravity], got %+v", resp)
	}

	result, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{Scope: store.HarnessConfigScopeGlobal}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 1 {
		t.Fatalf("expected 1 global harness-config, got %d", result.TotalCount)
	}
}

// TestHandleProjectImportHarnessConfigs verifies the per-project endpoint
// POST /api/v1/projects/{id}/import-harness-configs works for remote URLs.
func TestHandleProjectImportHarnessConfigs(t *testing.T) {
	srv, s, project, _ := setupWorkspaceProject(t, "hc-proj-import")
	ctx := context.Background()

	admin := &store.User{ID: tid("user-admin-proj-hc"), Email: "admin-proj-hc@test.com", DisplayName: "Admin", Role: store.UserRoleAdmin}
	if err := s.CreateUser(ctx, admin); err != nil {
		t.Fatal(err)
	}
	ensureHubMembership(ctx, s, admin.ID)

	defer mockHarnessConfigTarball(t)()

	rec := doRequestAsUser(t, srv, admin, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/import-harness-configs",
		ImportHarnessConfigsRequest{
			SourceURL: "https://github.com/acme/repo/tree/main/harness-configs",
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ImportHarnessConfigsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Count != 1 || len(resp.HarnessConfigs) != 1 || resp.HarnessConfigs[0] != "my-config" {
		t.Fatalf("expected [my-config], got %+v", resp)
	}

	result, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{
		Scope:     store.HarnessConfigScopeProject,
		ProjectID: project.ID,
	}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 1 {
		t.Fatalf("expected 1 project-scoped harness-config, got %d", result.TotalCount)
	}
}

// TestHandleResourcesImport_MissingSourceURL verifies sourceUrl is required.
func TestHandleResourcesImport_MissingSourceURL(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	admin := &store.User{ID: "user-admin", Email: "admin@test.com", DisplayName: "Admin", Role: store.UserRoleAdmin}
	if err := s.CreateUser(ctx, admin); err != nil {
		t.Fatal(err)
	}
	ensureHubMembership(ctx, s, admin.ID)

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/resources/import", ImportResourcesRequest{
		Kind:  "template",
		Scope: "global",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}
