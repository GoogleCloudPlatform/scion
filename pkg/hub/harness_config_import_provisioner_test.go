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
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
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

// assertUnusableProvisionerAnswer checks the finalize 422 answer and returns
// its message.
func assertUnusableProvisionerAnswer(t *testing.T, rec *httptest.ResponseRecorder, wantReason string) string {
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
	return resp.Error.Message
}

// Import refuses a harness-config whose provisioner block cannot provision an
// agent with the finalize answer, and persists nothing.
func TestHarnessConfigImport_RejectsUnusableProvisioner(t *testing.T) {
	for _, tc := range unusableProvisionerCases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testInstallSourceServer(t)
			stor := srv.GetStorage().(*mockStorage)
			before := storageSnapshot(stor)

			cfgYAML := "name: badcfg\nharness: claude\n" + tc.block
			src := tarGzFilesServer(t, map[string]string{
				"config.yaml": cfgYAML,
				"README.md":   "hello",
			})
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/resources/import", ImportResourcesRequest{
				Kind: "harness-config", Scope: "global", SourceURL: src.URL + "/configs/badcfg.tar.gz",
			})
			msg := assertUnusableProvisionerAnswer(t, rec, tc.wantReason)
			entry, err := config.ParseHarnessConfigYAML([]byte(cfgYAML))
			require.NoError(t, err)
			want := harness.CheckProvisionerUsable("badcfg", nil, entry)
			require.NotNil(t, want)
			assert.Equal(t, want.PublicMessage(), msg, "import must answer byte-for-byte as finalize does")

			_, err = s.GetHarnessConfigBySlug(context.Background(), "badcfg", store.HarnessConfigScopeGlobal, "")
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

// multiConfigSource is a source holding one usable harness-config and two
// unusable ones. An import of it must refuse all three, name both unusable
// configs in name order, and persist nothing. zzz-bad lives in directory
// "000", which sorts before the others, so directory order differs from name
// order and the name ordering of the refusals is checked on its own.
var multiConfigSource = map[string]string{
	"000/config.yaml":      "name: zzz-bad\nharness: claude\n" + unusableProvisionerCases[1].block,
	"aaa-good/config.yaml": "name: aaa-good\nharness: claude\n",
	"mmm-bad/config.yaml":  "name: mmm-bad\nharness: claude\n" + unusableProvisionerCases[0].block,
}

// assertNamesBothUnusable checks that msg names both unusable configs, in name
// order, with their reasons, and does not name the usable one.
func assertNamesBothUnusable(t *testing.T, msg string) {
	t.Helper()
	mmm := strings.Index(msg, `harness-config "mmm-bad"`)
	zzz := strings.Index(msg, `harness-config "zzz-bad"`)
	require.GreaterOrEqual(t, mmm, 0, "message must name mmm-bad: %s", msg)
	require.GreaterOrEqual(t, zzz, 0, "message must name zzz-bad: %s", msg)
	assert.Less(t, mmm, zzz, "refusals must be reported in name order: %s", msg)
	assert.Contains(t, msg, unusableProvisionerCases[0].wantReason)
	assert.Contains(t, msg, unusableProvisionerCases[1].wantReason)
	assert.NotContains(t, msg, "aaa-good")
}

func writeWorkspaceFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	}
}

func projectHarnessConfigCount(t *testing.T, s store.Store, projectID string) int {
	t.Helper()
	result, err := s.ListHarnessConfigs(context.Background(), store.HarnessConfigFilter{
		Scope:     store.HarnessConfigScopeProject,
		ProjectID: projectID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	return result.TotalCount
}

// A workspace import holding usable and unusable harness-configs is refused
// as a whole: every unusable config is named and nothing is persisted, not
// even the usable config.
func TestImportHarnessConfigsFromWorkspace_RejectsUnusableProvisionerAllOrNothing(t *testing.T) {
	srv, s, project, wsRoot := setupWorkspaceProject(t, "hc-import-unusable")
	writeWorkspaceFiles(t, filepath.Join(wsRoot, ".scion", "harness-configs"), multiConfigSource)
	stor := srv.GetStorage().(*mockStorage)
	before := storageSnapshot(stor)

	imported, err := srv.importHarnessConfigsFromWorkspace(context.Background(), project, "/.scion/harness-configs")
	require.Error(t, err)
	assert.Empty(t, imported)
	var ierr *unusableProvisionerImportError
	require.True(t, errors.As(err, &ierr), "want *unusableProvisionerImportError, got %T: %v", err, err)
	assertNamesBothUnusable(t, err.Error())

	assert.Equal(t, 0, projectHarnessConfigCount(t, s, project.ID), "a refused import must not create any record")
	assert.True(t, maps.EqualFunc(before, storageSnapshot(stor), bytes.Equal),
		"a refused import must not write to storage")
}

// The per-project import endpoint answers a refused workspace import with the
// finalize 422 and persists nothing.
func TestHandleProjectImportHarnessConfigs_RejectsUnusableProvisioner(t *testing.T) {
	srv, s, project, wsRoot := setupWorkspaceProject(t, "hc-project-import-unusable")
	ctx := context.Background()
	writeWorkspaceFiles(t, filepath.Join(wsRoot, ".scion", "harness-configs"), multiConfigSource)
	stor := srv.GetStorage().(*mockStorage)
	before := storageSnapshot(stor)

	admin := &store.User{
		ID:          tid("user-admin-hc-project-import-unusable"),
		Email:       "hc-project-import-unusable@test.com",
		DisplayName: "Admin",
		Role:        store.UserRoleAdmin,
	}
	require.NoError(t, s.CreateUser(ctx, admin))
	ensureHubMembership(ctx, s, admin.ID)
	ensureAdminRoleBinding(t, s, admin.ID)

	rec := doRequestAsUser(t, srv, admin, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/import-harness-configs",
		ImportHarnessConfigsRequest{WorkspacePath: "/.scion/harness-configs"})
	assertUnusableProvisionerAnswer(t, rec, `harness-config "mmm-bad"`)
	var resp struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assertNamesBothUnusable(t, resp.Error.Message)

	assert.Equal(t, 0, projectHarnessConfigCount(t, s, project.ID), "a refused import must not create any record")
	assert.True(t, maps.EqualFunc(before, storageSnapshot(stor), bytes.Equal),
		"a refused import must not write to storage")
}

// A remote import holding usable and unusable harness-configs is refused as a
// whole through POST /api/v1/resources/import.
func TestHarnessConfigImport_RejectsUnusableProvisionerAllOrNothing(t *testing.T) {
	srv, s := testInstallSourceServer(t)
	stor := srv.GetStorage().(*mockStorage)
	before := storageSnapshot(stor)

	src := tarGzFilesServer(t, multiConfigSource)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/resources/import", ImportResourcesRequest{
		Kind: "harness-config", Scope: "global", SourceURL: src.URL + "/configs/multi.tar.gz",
	})
	assertUnusableProvisionerAnswer(t, rec, `harness-config "zzz-bad"`)
	var resp struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assertNamesBothUnusable(t, resp.Error.Message)

	for _, slug := range []string{"aaa-good", "mmm-bad", "zzz-bad"} {
		_, err := s.GetHarnessConfigBySlug(context.Background(), slug, store.HarnessConfigScopeGlobal, "")
		assert.ErrorIs(t, err, store.ErrNotFound, "a refused import must not create %s", slug)
	}
	assert.True(t, maps.EqualFunc(before, storageSnapshot(stor), bytes.Equal),
		"a refused import must not write to storage")
}

// reimportSiblingSource is a source holding the reimport target "claude" and
// an unusable sibling "zzz-bad".
func reimportSiblingSource(claudeBlock string) map[string]string {
	return map[string]string{
		"claude/config.yaml":  "name: claude\nharness: claude\n" + claudeBlock,
		"claude/README.md":    "replacement",
		"zzz-bad/config.yaml": "name: zzz-bad\nharness: claude\n" + unusableProvisionerCases[0].block,
	}
}

// Reimport validates and imports only the target config, so an unusable
// sibling in the same source neither blocks it nor gets imported
// (ptone/scion#4213).
func TestHarnessConfigReimport_IgnoresUnusableSibling(t *testing.T) {
	srv, s := testInstallSourceServer(t)
	hc := installClaudeViaHub(t, srv, s, installPinnedClaudeURL)

	src := tarGzFilesServer(t, reimportSiblingSource(""))
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/reimport",
		map[string]interface{}{"sourceUrl": src.URL + "/configs/multi.tar.gz"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ImportHarnessConfigsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, []string{"claude"}, resp.HarnessConfigs)

	after := globalClaude(t, s)
	require.NotNil(t, after)
	assert.Equal(t, hc.ID, after.ID, "reimport must update the existing config")
	assert.NotEqual(t, hc.ContentHash, after.ContentHash, "reimport must replace the files")

	_, err := s.GetHarnessConfigBySlug(context.Background(), "zzz-bad", store.HarnessConfigScopeGlobal, "")
	assert.ErrorIs(t, err, store.ErrNotFound, "reimport must not import a sibling config")
}

// Reimport still refuses an unusable target when the source also holds
// other configs, and changes nothing.
func TestHarnessConfigReimport_RejectsUnusableTargetWithSiblings(t *testing.T) {
	srv, s := testInstallSourceServer(t)
	hc := installClaudeViaHub(t, srv, s, installPinnedClaudeURL)
	stor := srv.GetStorage().(*mockStorage)
	before := storageSnapshot(stor)

	files := reimportSiblingSource(unusableProvisionerCases[1].block)
	files["aaa-good/config.yaml"] = "name: aaa-good\nharness: claude\n"
	src := tarGzFilesServer(t, files)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/reimport",
		map[string]interface{}{"sourceUrl": src.URL + "/configs/multi.tar.gz"})
	msg := assertUnusableProvisionerAnswer(t, rec, unusableProvisionerCases[1].wantReason)
	assert.Contains(t, msg, `harness-config "claude"`)
	assert.NotContains(t, msg, "zzz-bad", "only the target is validated")

	after := globalClaude(t, s)
	require.NotNil(t, after)
	assert.Equal(t, hc.ContentHash, after.ContentHash, "a refused reimport must not change the content hash")
	assert.True(t, maps.EqualFunc(before, storageSnapshot(stor), bytes.Equal),
		"a refused reimport must not change stored files")
	for _, slug := range []string{"aaa-good", "zzz-bad"} {
		_, err := s.GetHarnessConfigBySlug(context.Background(), slug, store.HarnessConfigScopeGlobal, "")
		assert.ErrorIs(t, err, store.ErrNotFound, "reimport must not import %s", slug)
	}
}

// doStreamRequest issues an authenticated request that opts into the NDJSON
// import stream.
func doStreamRequest(t *testing.T, srv *Server, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	bodyBytes, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// A refused config in the import stream yields an error event carrying the
// harness_config_unusable code and the same public message as the
// non-streaming 422 answer (ptone/scion#4214).
func TestHarnessConfigImportStream_RefusalCarriesCode(t *testing.T) {
	srv, s := testInstallSourceServer(t)
	src := tarGzFilesServer(t, multiConfigSource)
	importReq := ImportResourcesRequest{
		Kind: "harness-config", Scope: "global", SourceURL: src.URL + "/configs/multi.tar.gz",
	}

	plain := doRequest(t, srv, http.MethodPost, "/api/v1/resources/import", importReq)
	wantMsg := assertUnusableProvisionerAnswer(t, plain, `harness-config "zzz-bad"`)

	hc := installClaudeViaHub(t, srv, s, installPinnedClaudeURL)
	badClaude := tarGzFilesServer(t, map[string]string{
		"config.yaml": "name: claude\nharness: claude\n" + unusableProvisionerCases[0].block,
	})

	for _, tc := range []struct {
		name, path string
		body       interface{}
		wantMsg    string
	}{
		{"import", "/api/v1/resources/import", importReq, wantMsg},
		{"reimport", "/api/v1/harness-configs/" + hc.ID + "/reimport",
			map[string]interface{}{"sourceUrl": badClaude.URL + "/configs/claude.tar.gz"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doStreamRequest(t, srv, http.MethodPost, tc.path, tc.body)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			events := parseImportEvents(t, rec.Body.String())
			require.NotEmpty(t, events)
			last := events[len(events)-1]
			require.Equal(t, ImportEventError, last.Type, rec.Body.String())
			assert.Equal(t, harnessConfigUnusableErrorCode, last.Code)
			if tc.wantMsg != "" {
				assert.Equal(t, tc.wantMsg, last.Reason, "the stream must carry the non-streaming public message")
			} else {
				assert.Contains(t, last.Reason, unusableProvisionerCases[0].wantReason)
			}
		})
	}
}
