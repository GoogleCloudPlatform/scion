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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// leakTemplateTarGz is a .tar.gz with one template, "leaked".
func leakTemplateTarGz(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{
		"root/templates/leaked/scion-agent.yaml": "schema_version: \"1\"\nharness: claude\n",
		"root/templates/leaked/secret.txt":       "hub-only",
	} {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// TestImportSource_ProjectMemberCannotReadHubSources: a project member
// with import permission cannot make a hosted Hub discover or import from
// local paths (:local:), Cloud Storage buckets, or plain-http addresses.
// Each is refused with a message naming the operator setting, and nothing
// from the source is returned.
func TestImportSource_ProjectMemberCannotReadHubSources(t *testing.T) {
	hubDir := t.TempDir()
	tmplDir := filepath.Join(hubDir, "templates", "leaked")
	require.NoError(t, os.MkdirAll(tmplDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmplDir, "scion-agent.yaml"), []byte("schema_version: \"1\"\nharness: claude\n"), 0o644))

	archive := leakTemplateTarGz(t)
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	}))
	t.Cleanup(internal.Close)

	sources := map[string]string{
		"hub filesystem":     ":local:" + hubDir + "/templates",
		"cloud storage":      ":gcs:some-bucket/templates",
		"gs url":             "gs://some-bucket/templates",
		"loopback http":      internal.URL + "/templates.tar.gz",
		"rclone with option": ":gcs,endpoint=" + internal.URL + ":some-bucket/templates",
	}
	for _, operation := range []string{"discover", "import"} {
		for name, source := range sources {
			t.Run(operation+" "+name, func(t *testing.T) {
				slug := "srcpol-" + operation
				srv, s, project, _ := setupWorkspaceProject(t, slug)
				ctx := context.Background()
				user := &store.User{ID: tid("user-" + slug), Email: slug + "@test.com", DisplayName: slug, Role: store.UserRoleMember}
				require.NoError(t, s.CreateUser(ctx, user))
				ensureHubMembership(ctx, s, user.ID)
				grantUserActionOnResource(t, s, user.ID, "template", project.ID, ActionCreate)

				rec := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/resources/"+operation, map[string]string{
					"kind": "template", "scope": "project", "scopeId": project.ID, "sourceUrl": source,
				})
				assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), config.AllowedImportSourcesKey)
				assert.NotContains(t, rec.Body.String(), "leaked")

				templates, err := s.ListTemplates(ctx, store.TemplateFilter{Scope: store.ScopeProject, ScopeID: project.ID}, store.ListOptions{})
				require.NoError(t, err)
				for _, tmpl := range templates.Items {
					assert.NotEqual(t, "leaked", tmpl.Name)
				}
			})
		}
	}
}
