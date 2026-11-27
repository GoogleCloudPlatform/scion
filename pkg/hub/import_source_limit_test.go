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
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestImportSource_FileCountLimit: a source that unpacks to more files than
// the import limit is refused instead of being written to the Hub's disk.
func TestImportSource_FileCountLimit(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i := 0; i <= 20000; i++ {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("repo-main/templates/f%05d", i), Mode: 0o644, Typeflag: tar.TypeReg}))
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	archive := buf.Bytes()

	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = &mockRoundTripper{roundTrip: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(archive))}, nil
	}}
	t.Cleanup(func() { http.DefaultClient.Transport = old })

	srv, s, project, _ := setupWorkspaceProject(t, "srcpol-limit")
	ctx := context.Background()
	user := &store.User{ID: tid("user-srcpol-limit"), Email: "srcpol-limit@test.com", DisplayName: "limit", Role: store.UserRoleMember}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)
	grantUserActionOnResource(t, s, user.ID, "template", project.ID, ActionCreate)

	rec := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/resources/discover", map[string]string{
		"kind": "template", "scope": "project", "scopeId": project.ID,
		"sourceUrl": "https://github.com/acme/repo/tree/main/templates",
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "exceeds the size or file count limit")
}
