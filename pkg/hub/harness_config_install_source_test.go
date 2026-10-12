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
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The install source URL recorded as metadata at finalize (ptone/scion#3853).
// 'scion harness-config install --global <url>' uploads the fetched files
// and sends the URL in the finalize body; the Hub stores it as the config's
// SourceURL. These tests drive the same calls the CLI makes (create, upload,
// finalize) against the real handlers, then run the hosted startup bootstrap.

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
		"javascript:void(0)",
		"data:text/plain,hello",
		"https://github.com/GoogleCloudPlatform/scion/harnesses/claude\n\x1b[31mred",
		"https://github.com/GoogleCloudPlatform/scion/harnesses/claude\rextra",
		"https://github.com/GoogleCloudPlatform/scion/harnesses/\x00claude",
		"https://github.com/GoogleCloudPlatform/scion/harnesses/\tclaude",
		"https://github.com/GoogleCloudPlatform/scion/harnesses/\u202eclaude",
		"https://github.com/GoogleCloudPlatform/scion/harnesses/\u2028claude",
		"https://github.com/GoogleCloudPlatform/scion/harnesses/\u2029claude",
		"https://github.com/GoogleCloudPlatform/scion/harnesses/\u200bclaude",
		"https://github.com/GoogleCloudPlatform/scion/harnesses/\u2066claude\u2069",
		sourceURLOfLength(t, maxRecordedSourceURLBytes+1),
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

// sourceURLOfLength returns an https URL exactly n bytes long.
func sourceURLOfLength(t *testing.T, n int) string {
	t.Helper()
	const prefix = "https://example.com/configs/"
	require.Greater(t, n, len(prefix))
	u := prefix + strings.Repeat("a", n-len(prefix))
	require.Len(t, u, n)
	return u
}

// TestInstallSourceURL_MaxLengthSourceURLAccepted: a source URL of exactly
// maxRecordedSourceURLBytes is recorded (one byte more is rejected, see
// TestInstallSourceURL_NonRemoteSourceURLRejected).
func TestInstallSourceURL_MaxLengthSourceURLAccepted(t *testing.T) {
	srv, s := testInstallSourceServer(t)
	longest := sourceURLOfLength(t, maxRecordedSourceURLBytes)
	hc := installClaudeViaHub(t, srv, s, longest)
	assert.Equal(t, longest, hc.SourceURL)
}
