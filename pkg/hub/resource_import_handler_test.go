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

package hub

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
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
