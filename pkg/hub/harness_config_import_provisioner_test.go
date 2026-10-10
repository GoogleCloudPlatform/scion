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
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// unusableProvisionerCases are config.yaml provisioner blocks that import and
// reimport must refuse, as finalize does (ptone/scion#4181).
var unusableProvisionerCases = []struct {
	name       string
	block      string
	wantReason string
}{
	{"builtin type", "provisioner:\n  type: builtin\n", `provisioner.type "builtin"`},
	{"empty command", "provisioner:\n  type: container-script\n  interface_version: 1\n", "provisioner.command is empty"},
}

// tarGzFilesServer serves files as a .tar.gz archive at any *.tar.gz path.
func tarGzFilesServer(t *testing.T, files map[string]string) *httptest.Server {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	archive := buf.Bytes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".tar.gz") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// storageSnapshot copies the mock storage's object contents.
func storageSnapshot(stor *mockStorage) map[string][]byte {
	stor.mu.Lock()
	defer stor.mu.Unlock()
	out := make(map[string][]byte, len(stor.objects))
	for k := range stor.objects {
		out[k] = bytes.Clone(stor.content[k])
	}
	return out
}

func assertUnusableProvisionerAnswer(t *testing.T, rec *httptest.ResponseRecorder, wantReason string) {
	t.Helper()
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	assert.Equal(t, harnessConfigUnusableErrorCode, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, wantReason)
}

// Import refuses a harness-config whose provisioner block cannot provision an
// agent with the finalize answer, and persists nothing.
func TestHarnessConfigImport_RejectsUnusableProvisioner(t *testing.T) {
	for _, tc := range unusableProvisionerCases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testInstallSourceServer(t)
			stor := srv.GetStorage().(*mockStorage)
			before := storageSnapshot(stor)

			src := tarGzFilesServer(t, map[string]string{
				"config.yaml": "name: badcfg\nharness: claude\n" + tc.block,
				"README.md":   "hello",
			})
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/resources/import", ImportResourcesRequest{
				Kind: "harness-config", Scope: "global", SourceURL: src.URL + "/configs/badcfg.tar.gz",
			})
			assertUnusableProvisionerAnswer(t, rec, tc.wantReason)

			_, err := s.GetHarnessConfigBySlug(context.Background(), "badcfg", store.HarnessConfigScopeGlobal, "")
			assert.ErrorIs(t, err, store.ErrNotFound, "a refused import must not create a record")
			assert.True(t, maps.EqualFunc(before, storageSnapshot(stor), bytes.Equal),
				"a refused import must not write to storage")
		})
	}

	t.Run("usable provisioner imports", func(t *testing.T) {
		srv, s := testInstallSourceServer(t)
		src := tarGzFilesServer(t, map[string]string{
			"config.yaml": "name: goodcfg\nharness: claude\nprovisioner:\n  type: container-script\n  interface_version: 1\n  command: [python3, provision.py]\n",
		})
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/resources/import", ImportResourcesRequest{
			Kind: "harness-config", Scope: "global", SourceURL: src.URL + "/configs/goodcfg.tar.gz",
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		hc, err := s.GetHarnessConfigBySlug(context.Background(), "goodcfg", store.HarnessConfigScopeGlobal, "")
		require.NoError(t, err)
		assert.NotEmpty(t, hc.Files)
	})
}

// Reimport refuses an unusable provisioner block with the finalize answer and
// leaves the existing record and its stored files unchanged.
func TestHarnessConfigReimport_RejectsUnusableProvisioner(t *testing.T) {
	for _, tc := range unusableProvisionerCases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testInstallSourceServer(t)
			hc := installClaudeViaHub(t, srv, s, installPinnedClaudeURL)
			stor := srv.GetStorage().(*mockStorage)
			before := storageSnapshot(stor)

			src := tarGzFilesServer(t, map[string]string{
				"config.yaml": "name: claude\nharness: claude\n" + tc.block,
				"README.md":   "replacement",
			})
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/reimport",
				map[string]interface{}{"sourceUrl": src.URL + "/configs/claude.tar.gz"})
			assertUnusableProvisionerAnswer(t, rec, tc.wantReason)

			after := globalClaude(t, s)
			require.NotNil(t, after)
			assert.Equal(t, hc.ID, after.ID)
			assert.Equal(t, hc.SourceURL, after.SourceURL, "a refused reimport must not change the source URL")
			assert.Equal(t, hc.ContentHash, after.ContentHash, "a refused reimport must not change the content hash")
			assert.Equal(t, hc.Files, after.Files, "a refused reimport must not change the file list")
			assert.True(t, maps.EqualFunc(before, storageSnapshot(stor), bytes.Equal),
				"a refused reimport must not change stored files")
		})
	}
}
