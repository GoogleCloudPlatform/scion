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
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/resources"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The install source URL recorded as metadata at finalize (ptone/scion#3853).
// 'scion harness-config install --global <url>' uploads the fetched files
// and sends the URL in the finalize body; the Hub stores it as the config's
// SourceURL. These tests drive the same calls the CLI makes (create, upload,
// finalize) against the real handlers, then run the hosted startup bootstrap.

const (
	installCanonicalClaudeURL = "https://github.com/GoogleCloudPlatform/scion/harnesses/claude"
	installPinnedClaudeURL    = "https://github.com/GoogleCloudPlatform/scion/tree/v0.9.0/harnesses/claude"
)

type installFile struct {
	path string
	data []byte
}

// embeddedClaudeFiles returns the files of the bundled claude harness config,
// standing in for what the CLI fetched from the URL.
func embeddedClaudeFiles(t *testing.T) []installFile {
	t.Helper()
	for _, r := range resources.BuiltinHarnessConfigs() {
		if r.Name != "claude" {
			continue
		}
		var files []installFile
		require.NoError(t, fs.WalkDir(r.FS, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, err := fs.ReadFile(r.FS, p)
			if err != nil {
				return err
			}
			files = append(files, installFile{p, data})
			return nil
		}))
		require.NotEmpty(t, files)
		return files
	}
	t.Fatal("claude not in the bundled catalog")
	return nil
}

// globalClaude returns the global claude row, or nil.
func globalClaude(t *testing.T, s store.Store) *store.HarnessConfig {
	t.Helper()
	hc, err := s.GetHarnessConfigBySlug(context.Background(), "claude", store.HarnessConfigScopeGlobal, "")
	if err == store.ErrNotFound {
		return nil
	}
	require.NoError(t, err)
	require.NotNil(t, hc)
	return hc
}

// installClaudeViaHub mirrors syncHarnessConfigToHub for a global install:
// create the row if it does not exist, upload the files to its storage path,
// then finalize with the manifest and sourceURL ("" sends no sourceUrl).
func installClaudeViaHub(t *testing.T, srv *Server, s store.Store, sourceURL string) *store.HarnessConfig {
	t.Helper()
	files := embeddedClaudeFiles(t)

	hc := globalClaude(t, s)
	if hc == nil {
		reqFiles := make([]map[string]interface{}, 0, len(files))
		for _, f := range files {
			reqFiles = append(reqFiles, map[string]interface{}{"path": f.path, "size": len(f.data)})
		}
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs", map[string]interface{}{
			"name": "claude", "harness": "claude", "scope": "global", "files": reqFiles,
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		hc = globalClaude(t, s)
		require.NotNil(t, hc)
	}

	stor := srv.GetStorage()
	manifestFiles := make([]map[string]interface{}, 0, len(files))
	for _, f := range files {
		_, err := stor.Upload(context.Background(), hc.StoragePath+"/"+f.path, bytes.NewReader(f.data), storage.UploadOptions{})
		require.NoError(t, err)
		sum := sha256.Sum256(f.data)
		manifestFiles = append(manifestFiles, map[string]interface{}{
			"path": f.path, "size": len(f.data), "hash": "sha256:" + hex.EncodeToString(sum[:]),
		})
	}
	body := map[string]interface{}{"manifest": map[string]interface{}{"version": "1.0", "harness": "claude", "files": manifestFiles}}
	if sourceURL != "" {
		body["sourceUrl"] = sourceURL
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/finalize", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	after := globalClaude(t, s)
	require.NotNil(t, after)
	return after
}

// testInstallSourceServer is a dev-auth hub with storage after one hosted
// bootstrap, with the bundled claude row deleted (the ledger lists it, so
// startup does not re-create it).
func testInstallSourceServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	srv, s := testServer(t)
	srv.SetStorage(newMockStorage("test-bucket"))
	hostedInstallBootstrap(t, srv)
	hc := globalClaude(t, s)
	require.NotNil(t, hc)
	require.NoError(t, s.DeleteHarnessConfig(context.Background(), hc.ID))
	return srv, s
}

// hostedInstallBootstrap runs the hosted startup bootstrap with the options
// cmd/server_foreground.go uses.
func hostedInstallBootstrap(t *testing.T, srv *Server) {
	t.Helper()
	require.NoError(t, srv.BootstrapBundledResources(context.Background(), BootstrapOptions{
		RepairStorage:   true,
		OverwritePolicy: OverwriteBuiltinManaged,
	}))
}

// markClaudeStale gives the row a content hash that differs from the bundled
// content, so the next bootstrap has an update to apply.
func markClaudeStale(t *testing.T, s store.Store, id string) {
	t.Helper()
	hc, err := s.GetHarnessConfig(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, hc)
	hc.ContentHash = "stale"
	require.NoError(t, s.UpdateHarnessConfig(context.Background(), hc))
}

func TestInstallSourceURL_CanonicalURLIsBuiltinManagedAndUpdated(t *testing.T) {
	srv, s := testInstallSourceServer(t)

	hc := installClaudeViaHub(t, srv, s, installCanonicalClaudeURL)
	assert.Equal(t, installCanonicalClaudeURL, hc.SourceURL, "finalize must record the install source URL")
	assert.True(t, IsBuiltinManaged(hc.SourceURL))

	markClaudeStale(t, s, hc.ID)
	hostedInstallBootstrap(t, srv)
	after := globalClaude(t, s)
	require.NotNil(t, after, "bootstrap must keep the installed row")
	assert.Equal(t, hc.ID, after.ID)
	assert.NotEqual(t, "stale", after.ContentHash, "bootstrap must update a built-in-managed row")
}

func TestInstallSourceURL_NonCanonicalURLIsStoredButUserManaged(t *testing.T) {
	srv, s := testInstallSourceServer(t)

	hc := installClaudeViaHub(t, srv, s, installPinnedClaudeURL)
	assert.Equal(t, installPinnedClaudeURL, hc.SourceURL, "finalize must record the install source URL")
	assert.False(t, IsBuiltinManaged(hc.SourceURL))

	markClaudeStale(t, s, hc.ID)
	hostedInstallBootstrap(t, srv)
	after := globalClaude(t, s)
	require.NotNil(t, after, "bootstrap must keep the installed row")
	assert.Equal(t, hc.ID, after.ID)
	assert.Equal(t, "stale", after.ContentHash, "bootstrap must leave a user-managed row alone")
	assert.Equal(t, installPinnedClaudeURL, after.SourceURL)
}

// TestInstallSourceURL_UpdateExistingRowRecordsNewSource covers the
// update path: a second install (install --force) over an existing row
// finalizes the same row and records the new source URL.
func TestInstallSourceURL_UpdateExistingRowRecordsNewSource(t *testing.T) {
	srv, s := testInstallSourceServer(t)

	pinned := installClaudeViaHub(t, srv, s, installPinnedClaudeURL)
	require.False(t, IsBuiltinManaged(pinned.SourceURL))

	hc := installClaudeViaHub(t, srv, s, installCanonicalClaudeURL)
	assert.Equal(t, pinned.ID, hc.ID, "the update path must finalize the existing row")
	assert.Equal(t, installCanonicalClaudeURL, hc.SourceURL)
	assert.True(t, IsBuiltinManaged(hc.SourceURL))

	markClaudeStale(t, s, hc.ID)
	hostedInstallBootstrap(t, srv)
	after := globalClaude(t, s)
	require.NotNil(t, after)
	assert.Equal(t, hc.ID, after.ID)
	assert.NotEqual(t, "stale", after.ContentHash, "bootstrap must update the row after the canonical install")
}

// TestInstallSourceURL_OmittedSourceURLPreservesExisting: a finalize with no
// sourceUrl (web upload, 'harness-config sync') keeps the recorded source.
func TestInstallSourceURL_OmittedSourceURLPreservesExisting(t *testing.T) {
	srv, s := testInstallSourceServer(t)

	hc := installClaudeViaHub(t, srv, s, installCanonicalClaudeURL)
	require.Equal(t, installCanonicalClaudeURL, hc.SourceURL)

	again := installClaudeViaHub(t, srv, s, "")
	assert.Equal(t, hc.ID, again.ID)
	assert.Equal(t, installCanonicalClaudeURL, again.SourceURL, "an omitted sourceUrl must leave the stored source URL unchanged")
}

func TestInstallSourceURL_NonRemoteSourceURLRejected(t *testing.T) {
	srv, s := testInstallSourceServer(t)
	hc := installClaudeViaHub(t, srv, s, installCanonicalClaudeURL)

	for _, bad := range []string{
		"/home/me/harness-configs/claude",
		"file:///home/me/claude",
		"claude",
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"https://github.com/GoogleCloudPlatform/scion/harnesses/claude\n\x1b[31mred",
		"https://github.com/GoogleCloudPlatform/scion/harnesses/claude\rinjected",
		"https://github.com/GoogleCloudPlatform/scion/harnesses/\x00claude",
		"https://github.com/GoogleCloudPlatform/scion/harnesses/\tclaude",
		"https://github.com/GoogleCloudPlatform/scion/harnesses/\u202eclaude",
		"https://github.com/GoogleCloudPlatform/scion/harnesses/\u2028claude",
		"https://github.com/GoogleCloudPlatform/scion/harnesses/\u2029claude",
		"https://github.com/GoogleCloudPlatform/scion/harnesses/\u200bclaude",
		"https://github.com/GoogleCloudPlatform/scion/harnesses/\u2066claude\u2069",
	} {
		body := map[string]interface{}{
			"manifest":  map[string]interface{}{"files": []map[string]interface{}{{"path": "config.yaml", "size": 1, "hash": "sha256:x"}}},
			"sourceUrl": bad,
		}
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/finalize", body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%q: %s", bad, rec.Body.String())
	}
	after := globalClaude(t, s)
	require.NotNil(t, after)
	assert.Equal(t, installCanonicalClaudeURL, after.SourceURL, "a rejected finalize must not change the source URL")
	assert.Equal(t, hc.ContentHash, after.ContentHash, "a rejected finalize must not change the files")
}

// TestInstallSourceURL_FinalizeAuthzUnchanged: recording a source URL needs
// the same permission finalize always needed (update on the config).
func TestInstallSourceURL_FinalizeAuthzUnchanged(t *testing.T) {
	srv, s := testInstallSourceServer(t)
	hc := installClaudeViaHub(t, srv, s, installPinnedClaudeURL)
	ctx := context.Background()

	viewer := &store.User{
		ID:          tid("user-install-source-viewer"),
		Email:       "install-source-viewer@test.com",
		DisplayName: "Install Source Viewer",
		Role:        store.UserRoleViewer,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, viewer))
	ensureHubMembership(ctx, s, viewer.ID)
	viewerRole, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubViewer, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: viewerRole.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      viewer.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "system",
	})
	require.NoError(t, err)

	var manifest []map[string]interface{}
	require.NoError(t, json.Unmarshal(mustJSON(t, hc.Files), &manifest))
	rec := doRequestAsUser(t, srv, viewer, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/finalize",
		map[string]interface{}{"manifest": map[string]interface{}{"files": manifest}, "sourceUrl": installCanonicalClaudeURL})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	after := globalClaude(t, s)
	require.NotNil(t, after)
	assert.Equal(t, installPinnedClaudeURL, after.SourceURL, "a denied finalize must not change the source URL")
}
