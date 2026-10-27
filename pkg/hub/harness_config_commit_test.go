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
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the harness-config single commit path (ptone/scion#4219, TS-4a).

const deriveHCName = "derive-hc"

// deriveFullYAML sets every derived key; deriveBareYAML sets only the
// harness, so committing it after deriveFullYAML must clear every other
// derived field.
const deriveFullYAML = `name: derive-hc
harness: claude
image: example/claude:1
user: scion
model: opus
args: ["--verbose"]
env:
  TZ: Europe/Paris
auth_selected_type: api-key
secrets:
  - key: EXAMPLE_API_KEY
model_aliases:
  large: big-model
no_auth:
  behavior: drop-to-shell
auth:
  default_type: api-key
  types:
    api-key:
      required_env:
        - any_of: ["EXAMPLE_API_KEY"]
`

const deriveBareYAML = "name: derive-hc\nharness: claude\n"

// unusableHCYAML declares a provisioner that could never provision an agent.
const unusableHCYAML = "name: derive-hc\nharness: claude\nprovisioner:\n  type: builtin\n"

// copyingMockStorage is mockStorage with a working Copy, for the clone paths.
type copyingMockStorage struct {
	*mockStorage
}

func (m *copyingMockStorage) Copy(ctx context.Context, src, dst string) (*storage.Object, error) {
	r, _, err := m.Download(ctx, src)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return m.Upload(ctx, dst, r, storage.UploadOptions{})
}

func newHCCommitTestServer(t *testing.T) (*Server, store.Store, *mockStorage) {
	t.Helper()
	srv, s := testServer(t)
	stor := newMockStorage("test-bucket")
	srv.SetStorage(&copyingMockStorage{mockStorage: stor})
	return srv, s, stor
}

func globalHC(t *testing.T, s store.Store, slug string) *store.HarnessConfig {
	t.Helper()
	hc, err := s.GetHarnessConfigBySlug(context.Background(), slug, store.HarnessConfigScopeGlobal, "")
	if err == store.ErrNotFound {
		return nil
	}
	require.NoError(t, err)
	return hc
}

// ensureDeriveHC returns the global derive-hc row, creating it (no files)
// through the create API when it does not exist.
func ensureDeriveHC(t *testing.T, srv *Server, s store.Store) *store.HarnessConfig {
	t.Helper()
	if hc := globalHC(t, s, deriveHCName); hc != nil {
		return hc
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs", map[string]interface{}{
		"name": deriveHCName, "harness": "claude", "scope": "global",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	hc := globalHC(t, s, deriveHCName)
	require.NotNil(t, hc)
	return hc
}

// finalizeHCManifest stages files under a fresh upload ID, as the upload
// URLs do, and finalizes that upload with them as the full manifest.
func finalizeHCManifest(t *testing.T, srv *Server, hc *store.HarnessConfig, files map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	uploadID, manifest := stageHCUpload(t, srv, hc, files)
	return doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/finalize", map[string]interface{}{
		"manifest": map[string]interface{}{"version": "1.0", "files": manifest},
		"uploadId": uploadID,
	})
}

func putHCFile(t *testing.T, srv *Server, id, filePath, content string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/harness-configs/"+id+"/files/"+filePath, strings.NewReader(content))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// commitPath commits config.yaml with the given content into the global
// derive-hc row through one writer, and returns the stored row.
type commitPath struct {
	name  string
	apply func(t *testing.T, srv *Server, s store.Store, yaml string) *store.HarnessConfig
}

var harnessConfigCommitPaths = []commitPath{
	{"finalize", func(t *testing.T, srv *Server, s store.Store, yaml string) *store.HarnessConfig {
		hc := ensureDeriveHC(t, srv, s)
		rec := finalizeHCManifest(t, srv, hc, map[string]string{"config.yaml": yaml})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		return globalHC(t, s, deriveHCName)
	}},
	{"file write", func(t *testing.T, srv *Server, s store.Store, yaml string) *store.HarnessConfig {
		hc := ensureDeriveHC(t, srv, s)
		rec := putHCFile(t, srv, hc.ID, "config.yaml", yaml)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		return globalHC(t, s, deriveHCName)
	}},
	{"multipart upload", func(t *testing.T, srv *Server, s store.Store, yaml string) *store.HarnessConfig {
		hc := ensureDeriveHC(t, srv, s)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, harnessConfigMultipartRequest(t, hc.ID, map[string][]byte{"config.yaml": []byte(yaml)}))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		return globalHC(t, s, deriveHCName)
	}},
	{"import", func(t *testing.T, srv *Server, s store.Store, yaml string) *store.HarnessConfig {
		src := tarGzFilesServer(t, map[string]string{deriveHCName + "/config.yaml": yaml})
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/resources/import", ImportResourcesRequest{
			Kind: "harness-config", Scope: "global", SourceURL: src.URL + "/configs/derive.tar.gz",
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		return globalHC(t, s, deriveHCName)
	}},
	{"bootstrap", func(t *testing.T, srv *Server, s store.Store, yaml string) *store.HarnessConfig {
		dir := makeHarnessConfigDir(t, deriveHCName, map[string]string{"config.yaml": yaml})
		require.NoError(t, srv.BootstrapHarnessConfigsFromDir(context.Background(), dir))
		return globalHC(t, s, deriveHCName)
	}},
	{"repair", func(t *testing.T, srv *Server, s store.Store, yaml string) *store.HarnessConfig {
		// Repair from storage only rewrites legacy rows (a blob row's
		// content cannot drift). Put the row in the legacy layout with a
		// stored config.yaml that differs from its manifest, as an
		// interrupted upload or a manual storage edit would, then repair.
		hc := ensureDeriveHC(t, srv, s)
		hc = resetHCToLegacy(t, srv, s, hc,
			map[string]string{"config.yaml": "name: derive-hc\nharness: claude\nmodel: seed\n"},
			map[string]string{"config.yaml": yaml})
		require.NoError(t, srv.syncHarnessConfigFromStorage(context.Background(), HarnessConfigRepairRef{ID: hc.ID}))
		return globalHC(t, s, deriveHCName)
	}},
}

// assertDerivedFrom checks that hc's derived fields equal
// deriveHarnessConfigIndex(yaml).
func assertDerivedFrom(t *testing.T, hc *store.HarnessConfig, yaml string) {
	t.Helper()
	require.NotNil(t, hc)
	want := deriveHarnessConfigIndex([]byte(yaml), true, hc.Name)
	assert.Equal(t, want.Harness, hc.Harness, "Harness")
	a, _ := json.Marshal(hc.Config)
	b, _ := json.Marshal(want.Config)
	assert.JSONEq(t, string(b), string(a), "Config must equal what config.yaml derives")
}

// Acceptance 1 (and the env/secrets/user/args/auth_selected_type row): on
// every routed path the derived fields after the commit equal
// deriveHarnessConfigIndex(config.yaml), and removing a key clears its field.
func TestHarnessConfigCommit_DerivedFieldsFollowConfigYAMLOnEveryPath(t *testing.T) {
	for _, path := range harnessConfigCommitPaths {
		t.Run(path.name, func(t *testing.T) {
			srv, s, _ := newHCCommitTestServer(t)

			full := path.apply(t, srv, s, deriveFullYAML)
			assertDerivedFrom(t, full, deriveFullYAML)
			require.NotNil(t, full.Config)
			assert.Equal(t, "example/claude:1", full.Config.Image)
			assert.Equal(t, "opus", full.Config.Model)
			assert.Equal(t, map[string]string{"large": "big-model"}, full.Config.ModelAliases)
			assert.Equal(t, "drop-to-shell", full.Config.NoAuthBehavior)
			require.NotNil(t, full.Config.AuthMeta)
			assert.Contains(t, full.Config.AuthMeta.Types, "api-key")
			assert.Equal(t, map[string]string{"TZ": "Europe/Paris"}, full.Config.Env)
			assert.Equal(t, []api.RequiredSecret{{Key: "EXAMPLE_API_KEY"}}, full.Config.Secrets)
			assert.Equal(t, "scion", full.Config.User)
			assert.Equal(t, []string{"--verbose"}, full.Config.Args)
			assert.Equal(t, "api-key", full.Config.AuthSelectedType)

			bare := path.apply(t, srv, s, deriveBareYAML)
			assert.Equal(t, full.ID, bare.ID, "the same row is committed")
			assertDerivedFrom(t, bare, deriveBareYAML)
			assert.Equal(t, &store.HarnessConfigData{Harness: "claude"}, bare.Config,
				"every key absent from config.yaml clears its derived field")
		})
	}
}

// deriveHarnessConfigIndex: Harness falls back to the name; unparsable and
// missing config.yaml derive as empty.
func TestDeriveHarnessConfigIndex(t *testing.T) {
	idx := deriveHarnessConfigIndex([]byte("image: x\n"), true, "my-claude")
	assert.Equal(t, "claude", idx.Harness, "harness inferred from the name")
	assert.Equal(t, &store.HarnessConfigData{Image: "x"}, idx.Config)

	idx = deriveHarnessConfigIndex([]byte(":\tnot yaml: ["), true, "my-claude")
	assert.Nil(t, idx.Config)
	assert.Equal(t, "claude", idx.Harness)

	idx = deriveHarnessConfigIndex(nil, false, "custom")
	assert.Nil(t, idx.Config)
	assert.Empty(t, idx.Harness)

	idx = deriveHarnessConfigIndex([]byte("auth:\n  default_type: x\n"), true, "custom")
	assert.Nil(t, idx.Config, "auth metadata without types is not derived")
}

// When config.yaml sets no harness and the name infers none, the commit keeps
// the row's stored Harness (the column is non-empty).
func TestHarnessConfigCommit_KeepsHarnessWhenNoneDerivable(t *testing.T) {
	srv, s, _ := newHCCommitTestServer(t)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs", map[string]interface{}{
		"name": "custom-thing", "harness": "generic", "scope": "global",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	hc := globalHC(t, s, "custom-thing")
	rec = finalizeHCManifest(t, srv, hc, map[string]string{"config.yaml": "image: x\n"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := globalHC(t, s, "custom-thing")
	assert.Equal(t, "generic", got.Harness)
	assert.Equal(t, "x", got.Config.Image)
}

// Acceptance 2: an unusable provisioner is refused on the API paths with 422
// and leaves the row unchanged.
func TestHarnessConfigCommit_RefusesUnusableProvisionerOnAPIPaths(t *testing.T) {
	for _, path := range harnessConfigCommitPaths[:4] { // finalize, file write, multipart, import
		t.Run(path.name, func(t *testing.T) {
			srv, s, _ := newHCCommitTestServer(t)
			before := path.apply(t, srv, s, deriveFullYAML)

			var rec *httptest.ResponseRecorder
			switch path.name {
			case "finalize":
				rec = finalizeHCManifest(t, srv, before, map[string]string{"config.yaml": unusableHCYAML})
			case "file write":
				rec = putHCFile(t, srv, before.ID, "config.yaml", unusableHCYAML)
			case "multipart upload":
				rec = httptest.NewRecorder()
				srv.Handler().ServeHTTP(rec, harnessConfigMultipartRequest(t, before.ID, map[string][]byte{"config.yaml": []byte(unusableHCYAML)}))
			case "import":
				src := tarGzFilesServer(t, map[string]string{deriveHCName + "/config.yaml": unusableHCYAML})
				rec = doRequest(t, srv, http.MethodPost, "/api/v1/resources/import", ImportResourcesRequest{
					Kind: "harness-config", Scope: "global", SourceURL: src.URL + "/configs/derive.tar.gz",
				})
			}
			require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), harnessConfigUnusableErrorCode)

			after := globalHC(t, s, deriveHCName)
			assert.Equal(t, before.ContentHash, after.ContentHash)
			assert.Equal(t, before.Files, after.Files)
			assertDerivedFrom(t, after, deriveFullYAML)
		})
	}
}

// Acceptance 2: bootstrap refuses an unusable provisioner before any upload
// (CheckDir): warn and skip, no row and no storage change.
func TestHarnessConfigBootstrap_UnusableProvisionerWarnsAndSkips(t *testing.T) {
	t.Run("new config", func(t *testing.T) {
		srv, s, stor := newHCCommitTestServer(t)
		dir := makeHarnessConfigDir(t, deriveHCName, map[string]string{"config.yaml": unusableHCYAML, "extra.txt": "x"})
		require.NoError(t, srv.BootstrapHarnessConfigsFromDir(context.Background(), dir), "startup does not fail")
		assert.Nil(t, globalHC(t, s, deriveHCName), "no row is created")
		assert.Empty(t, storageSnapshot(stor), "nothing is uploaded")
	})

	t.Run("existing config", func(t *testing.T) {
		srv, s, stor := newHCCommitTestServer(t)
		dir := makeHarnessConfigDir(t, deriveHCName, map[string]string{"config.yaml": deriveFullYAML})
		require.NoError(t, srv.BootstrapHarnessConfigsFromDir(context.Background(), dir))
		before := globalHC(t, s, deriveHCName)
		snap := storageSnapshot(stor)

		require.NoError(t, os.WriteFile(filepath.Join(dir, deriveHCName, "config.yaml"), []byte(unusableHCYAML), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, deriveHCName, "extra.txt"), []byte("new"), 0o644))
		require.NoError(t, srv.BootstrapHarnessConfigsFromDir(context.Background(), dir), "startup does not fail")

		after := globalHC(t, s, deriveHCName)
		assert.Equal(t, before.ContentHash, after.ContentHash)
		assertDerivedFrom(t, after, deriveFullYAML)
		assert.True(t, maps.EqualFunc(snap, storageSnapshot(stor), bytes.Equal), "storage must not change")
	})
}

// Acceptance 2: repair refuses a stored config.yaml with an unusable
// provisioner: the row and storage are left as they are.
func TestHarnessConfigRepair_UnusableProvisionerLeavesRowAndStorage(t *testing.T) {
	srv, s, stor := newHCCommitTestServer(t)
	hc := harnessConfigCommitPaths[0].apply(t, srv, s, deriveFullYAML)
	// A legacy row whose stored config.yaml was replaced by an unusable one.
	hc = resetHCToLegacy(t, srv, s, hc,
		map[string]string{"config.yaml": deriveFullYAML},
		map[string]string{"config.yaml": unusableHCYAML})
	snap := storageSnapshot(stor)

	err := srv.syncHarnessConfigFromStorage(context.Background(), HarnessConfigRepairRef{ID: hc.ID})
	require.Error(t, err)

	after := globalHC(t, s, deriveHCName)
	assert.Equal(t, hc.ContentHash, after.ContentHash)
	assert.Equal(t, "", after.Layout, "the refused repair does not migrate the row")
	assertDerivedFrom(t, after, deriveFullYAML)
	assert.True(t, maps.EqualFunc(snap, storageSnapshot(stor), bytes.Equal), "repair must not change storage")
}

// Acceptance 3: the build executor's BuiltImage survives a later commit, and
// the image handlers use it.
func TestHarnessConfigBuiltImage_SurvivesCommitAndIsTheCheckedImage(t *testing.T) {
	srv, s, _ := newHCCommitTestServer(t)
	hc := harnessConfigCommitPaths[0].apply(t, srv, s, deriveFullYAML)

	require.NoError(t, syncBuiltImage(context.Background(), s, io.Discard, hc, "registry.example/built:7"))
	got := globalHC(t, s, deriveHCName)
	assert.Equal(t, "registry.example/built:7", got.BuiltImage)
	assert.Equal(t, "example/claude:1", got.Config.Image, "the build executor does not write Config.Image")

	// A later commit re-derives Config but never touches BuiltImage.
	rec := putHCFile(t, srv, hc.ID, "config.yaml", strings.Replace(deriveFullYAML, "example/claude:1", "example/claude:2", 1))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got = globalHC(t, s, deriveHCName)
	assert.Equal(t, "registry.example/built:7", got.BuiltImage, "BuiltImage survives a commit")
	assert.Equal(t, "example/claude:2", got.Config.Image)
	assert.Equal(t, "registry.example/built:7", srv.harnessConfigImage(got), "the image check uses BuiltImage")
	assert.Equal(t, "example/claude", deriveImageBaseName(got), "the build base name keeps using config.yaml's image")

	got.BuiltImage = ""
	assert.Equal(t, "example/claude:2", srv.harnessConfigImage(got), "without a built image the check uses Config.Image")
}

// Acceptance 4: reimport persists into the row with the requested ID even
// when its slug changes before the import is persisted (here: while the
// source is being fetched), and never creates a row under the source slug.
func TestHarnessConfigReimport_UpdatesRowByIDAfterSlugRename(t *testing.T) {
	srv, s, _ := newHCCommitTestServer(t)
	ctx := context.Background()
	hc := harnessConfigCommitPaths[0].apply(t, srv, s, deriveBareYAML)

	archive := tarGzFilesServer(t, map[string]string{deriveHCName + "/config.yaml": deriveFullYAML})
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Rename the row's slug while the source is fetched: after the
		// slug filter selected the entry, before it is persisted.
		cur, err := s.GetHarnessConfig(ctx, hc.ID)
		if err == nil && cur.Slug == deriveHCName {
			cur.Slug = "derive-hc-renamed"
			_ = s.UpdateHarnessConfig(ctx, cur)
		}
		resp, err := http.Get(archive.URL + r.URL.Path)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(src.Close)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/reimport",
		map[string]interface{}{"sourceUrl": src.URL + "/configs/derive.tar.gz"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	after, err := s.GetHarnessConfig(ctx, hc.ID)
	require.NoError(t, err)
	assert.Equal(t, "derive-hc-renamed", after.Slug)
	assert.NotEqual(t, hc.ContentHash, after.ContentHash, "the row with the requested ID is updated")
	assertDerivedFrom(t, after, deriveFullYAML)
	assert.Nil(t, globalHC(t, s, deriveHCName), "no row is created under the source slug")
}

// Acceptance 5: two finalizes that read the same row give exactly one 409.
func TestHarnessConfigFinalize_ConcurrentCommitsGiveOneConflict(t *testing.T) {
	srv, s, _ := newHCCommitTestServer(t)
	hc := harnessConfigCommitPaths[0].apply(t, srv, s, deriveBareYAML)
	stale := *hc

	finalize := func(content string) *httptest.ResponseRecorder {
		read := stale // both requests read the row before either wrote
		uploadID, manifest := stageHCUpload(t, srv, hc, map[string]string{"config.yaml": content})
		body, _ := json.Marshal(map[string]interface{}{"manifest": map[string]interface{}{
			"version": "1.0",
			"files":   manifest,
		}, "uploadId": uploadID})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/finalize", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.handleHarnessConfigFinalize(rec, req, &read)
		return rec
	}

	first := finalize(deriveFullYAML)
	second := finalize(strings.Replace(deriveFullYAML, "opus", "sonnet", 1))
	codes := []int{first.Code, second.Code}
	assert.ElementsMatch(t, []int{http.StatusOK, http.StatusConflict}, codes, "first=%s second=%s", first.Body, second.Body)
	assert.Contains(t, second.Body.String(), ErrCodeHarnessConfigConflict)

	got := globalHC(t, s, deriveHCName)
	assert.Equal(t, "opus", got.Config.Model, "the winning commit stays")
}

// Acceptance 5: a metadata PUT from a stale read does not revert a commit.
func TestHarnessConfigPut_StaleReadDoesNotRevertCommit(t *testing.T) {
	srv, s, _ := newHCCommitTestServer(t)
	stale := *harnessConfigCommitPaths[0].apply(t, srv, s, deriveBareYAML)
	committed := harnessConfigCommitPaths[0].apply(t, srv, s, deriveFullYAML)

	body, _ := json.Marshal(map[string]interface{}{"name": deriveHCName, "slug": deriveHCName, "description": "edited"})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/harness-configs/"+stale.ID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(),
		NewAuthenticatedUser(tid("hc-put-user"), "put@example.com", "Put", "admin", "cli")))
	rec := httptest.NewRecorder()
	srv.updateHarnessConfig(rec, req, &stale)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got := globalHC(t, s, deriveHCName)
	assert.Equal(t, "edited", got.Description)
	assert.Equal(t, committed.ContentHash, got.ContentHash)
	assert.Equal(t, committed.Files, got.Files)
	assertDerivedFrom(t, got, deriveFullYAML)
}

// Acceptance 6: create with config is refused; a provided slug is
// normalized.
func TestHarnessConfigCreate_ConfigRefusedAndSlugNormalized(t *testing.T) {
	srv, s, _ := newHCCommitTestServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs", map[string]interface{}{
		"name": "with-config", "harness": "claude", "scope": "global",
		"config": map[string]interface{}{"image": "x"},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeHarnessConfigConfigInFile)
	assert.Nil(t, globalHC(t, s, "with-config"))

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs", map[string]interface{}{
		"name": "null-config", "harness": "claude", "scope": "global", "config": nil,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs", map[string]interface{}{
		"name": "slugged", "slug": "My Custom Slug", "harness": "claude", "scope": "global",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.NotNil(t, globalHC(t, s, "my-custom-slug"), "the provided slug is passed through Slugify")
}

// Acceptance 6: deleting config.yaml through the file API is refused.
func TestHarnessConfigFileDelete_ConfigYAMLRefused(t *testing.T) {
	srv, s, _ := newHCCommitTestServer(t)
	hc := ensureDeriveHC(t, srv, s)
	rec := finalizeHCManifest(t, srv, hc, map[string]string{"config.yaml": deriveFullYAML, "extra.txt": "x"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	before := globalHC(t, s, deriveHCName)

	// Through the router: the exact name.
	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/harness-configs/"+hc.ID+"/files/config.yaml", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeHarnessConfigYAMLRequired)

	// Spellings that clean to config.yaml reach the handler directly: the
	// router's ServeMux redirects a URL with "./" or ".." segments (301)
	// before any handler runs.
	for _, p := range []string{"./config.yaml", "sub/../config.yaml"} {
		row := globalHC(t, s, deriveHCName)
		req := httptest.NewRequest(http.MethodDelete, "/", nil)
		rec := httptest.NewRecorder()
		srv.handleHarnessConfigFileDelete(rec, req, row, p)
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", p, rec.Body.String())
		assert.Contains(t, rec.Body.String(), ErrCodeHarnessConfigYAMLRequired)
	}
	assert.Equal(t, before.ContentHash, globalHC(t, s, deriveHCName).ContentHash)

	// Another file still deletes through the commit. Its blob stays for
	// in-flight hydrations until the blob garbage collector removes it
	// (ptone/scion#4219 part 2).
	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/harness-configs/"+hc.ID+"/files/extra.txt", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	after := globalHC(t, s, deriveHCName)
	require.Len(t, after.Files, 1)
	assertDerivedFrom(t, after, deriveFullYAML)
	exists, err := srv.GetStorage().Exists(context.Background(), hcBlobPath(after, "x"))
	require.NoError(t, err)
	assert.True(t, exists, "a commit never deletes a blob")
}

// Acceptance 7: a clone re-derives Harness and Config from the copied files
// instead of copying the source's.
func TestHarnessConfigClone_ReDerivesFromCopiedFiles(t *testing.T) {
	srv, s, _ := newHCCommitTestServer(t)
	src := harnessConfigCommitPaths[0].apply(t, srv, s, deriveFullYAML)
	// Stage a stale derived Config on the source row.
	src.Config = &store.HarnessConfigData{Image: "stale/image:0", Model: "stale"}
	require.NoError(t, setHarnessConfigContentForTest(context.Background(), s, src))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+src.ID+"/clone", map[string]interface{}{
		"name": "derive-clone", "scope": "global",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	clone := globalHC(t, s, "derive-clone")
	require.NotNil(t, clone)
	assert.Equal(t, store.HarnessConfigStatusActive, clone.Status)
	assert.Equal(t, src.ContentHash, clone.ContentHash)
	assertDerivedFrom(t, clone, deriveFullYAML)
}

// Acceptance 7: project clone re-derives each harness-config. A source
// without stored content gets an empty pending row: files and their hash are
// written only by the commit path (ts4a-rev #1).
func TestProjectCloneHarnessConfigs_ReDerivesFromCopiedFiles(t *testing.T) {
	for _, tc := range []struct {
		name        string
		storagePath bool
	}{
		{"stored content", true},
		{"no storage path", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, stor := newHCCommitTestServer(t)
			ctx := context.Background()
			srcProject := api.NewUUID()
			srcHC := &store.HarnessConfig{
				ID: api.NewUUID(), Name: deriveHCName, Slug: deriveHCName, Harness: "claude",
				Scope: store.HarnessConfigScopeProject, ScopeID: srcProject, Status: store.HarnessConfigStatusActive,
				Config: &store.HarnessConfigData{Image: "stale/image:0"},
			}
			if tc.storagePath {
				srcHC.StoragePath = storage.HarnessConfigStoragePath(srv.HubID(), store.HarnessConfigScopeProject, srcProject, deriveHCName)
				_, err := stor.Upload(ctx, srcHC.StoragePath+"/config.yaml", strings.NewReader(deriveFullYAML), storage.UploadOptions{})
				require.NoError(t, err)
			}
			srcHC.Files = []store.TemplateFile{{Path: "config.yaml", Size: int64(len(deriveFullYAML)), Hash: commitFileHash(deriveFullYAML)}}
			srcHC.ContentHash = defaultContentHash(srcHC.Files)
			require.NoError(t, s.CreateHarnessConfig(ctx, srcHC))

			clone := &store.Project{ID: api.NewUUID()}
			var rollback []func()
			require.NoError(t, srv.cloneProjectHarnessConfigs(ctx, srcProject, clone, &rollback))

			got, err := s.GetHarnessConfigBySlug(ctx, deriveHCName, store.HarnessConfigScopeProject, clone.ID)
			require.NoError(t, err)
			assert.NotEqual(t, srcHC.ID, got.ID)
			if tc.storagePath {
				assert.Equal(t, srcHC.ContentHash, got.ContentHash)
				assertDerivedFrom(t, got, deriveFullYAML)
				return
			}
			assert.Empty(t, got.Files, "no manifest is copied without its objects")
			assert.Empty(t, got.ContentHash)
			assert.Nil(t, got.Config, "Config is never copied")
			assert.Equal(t, store.HarnessConfigStatusPending, got.Status)
		})
	}
}

// The hub's TZ read follows config.yaml: an agent created against a
// harness-config reads the TZ its config.yaml sets, and none once the key is
// removed (template-settings-arch, ptone/scion#4219).
func TestCaptureCreateTimezone_ReadsHarnessConfigTZFromConfigYAML(t *testing.T) {
	srv, s, _ := newHCCommitTestServer(t)
	ctx := context.Background()

	capture := func(hc *store.HarnessConfig) string {
		agent := &store.Agent{ID: api.NewUUID(), AppliedConfig: &store.AgentAppliedConfig{HarnessConfigID: hc.ID}}
		srv.captureCreateTimezone(ctx, agent, nil)
		return agent.AppliedConfig.ExplicitTimezone
	}

	withTZ := harnessConfigCommitPaths[4].apply(t, srv, s, deriveFullYAML) // bootstrap
	assert.Equal(t, "Europe/Paris", capture(withTZ))

	withoutTZ := harnessConfigCommitPaths[4].apply(t, srv, s, deriveBareYAML)
	assert.Equal(t, "", capture(withoutTZ), "a TZ removed from config.yaml no longer reaches the hub")
}

// A bootstrap commit that loses the compare-and-swap is retried once against
// the re-read row.
func TestHarnessConfigBootstrap_RetriesOnceOnConflict(t *testing.T) {
	srv, s, _ := newHCCommitTestServer(t)
	ctx := context.Background()
	dir := makeHarnessConfigDir(t, deriveHCName, map[string]string{"config.yaml": deriveBareYAML})
	require.NoError(t, srv.BootstrapHarnessConfigsFromDir(ctx, dir))
	row := globalHC(t, s, deriveHCName)

	// The persistence read the row; another commit lands before its write.
	p := &harnessConfigPersistence{s: srv}
	rec, err := p.GetBySlug(ctx, deriveHCName, store.HarnessConfigScopeGlobal, "")
	require.NoError(t, err)
	other := *row
	other.ContentHash = "sha256:other-commit"
	require.NoError(t, setHarnessConfigContentForTest(ctx, s, &other))

	// The retry writes the new blob from the directory.
	require.NoError(t, os.WriteFile(filepath.Join(dir, deriveHCName, "config.yaml"), []byte(deriveFullYAML), 0o644))
	rec.Files = []store.TemplateFile{{Path: "config.yaml", Size: int64(len(deriveFullYAML)), Hash: commitFileHash(deriveFullYAML)}}
	rec.Status = store.HarnessConfigStatusActive
	require.NoError(t, p.Update(ctx, rec, collectTestDir(t, filepath.Join(dir, deriveHCName))))

	got := globalHC(t, s, deriveHCName)
	assert.Equal(t, defaultContentHash(rec.Files), got.ContentHash)
	assertDerivedFrom(t, got, deriveFullYAML)
}

// hcConflictStore fails UpdateHarnessConfigContent with a conflict while its
// switch is armed, counting the attempts. onConflict, when set, runs on the
// first armed attempt through the inner store (another commit landing).
type hcConflictStore struct {
	store.Store
	fault      *storeFaultSwitch
	mu         sync.Mutex
	calls      int
	onConflict func(ctx context.Context, inner store.Store, hc *store.HarnessConfig)
}

func (w *hcConflictStore) UpdateHarnessConfigContent(ctx context.Context, hc *store.HarnessConfig, expected store.HarnessConfigContentPrecondition) error {
	if !w.fault.Active() {
		return w.Store.UpdateHarnessConfigContent(ctx, hc, expected)
	}
	w.mu.Lock()
	w.calls++
	first := w.calls == 1
	w.mu.Unlock()
	if first && w.onConflict != nil {
		w.onConflict(ctx, w.Store, hc)
	}
	return store.ErrHarnessConfigConflict
}

func (w *hcConflictStore) attempts() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

func newHCCommitTestServerWithConflicts(t *testing.T) (*Server, store.Store, *hcConflictStore, *storeFaultSwitch) {
	t.Helper()
	srv, s := testServer(t)
	w, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *hcConflictStore {
		return &hcConflictStore{Store: inner, fault: f}
	})
	srv.SetStorage(&copyingMockStorage{mockStorage: newMockStorage("test-bucket")})
	return srv, s, w, fault
}

// The hub-side retry bound (ts4a-rev #4): bootstrap retries a lost
// compare-and-swap once; a second conflict is an error, the config is
// skipped (startup does not fail), and there is no third attempt.
func TestHarnessConfigBootstrap_SecondConflictSkipsWithoutThirdAttempt(t *testing.T) {
	srv, s, w, fault := newHCCommitTestServerWithConflicts(t)
	ctx := context.Background()
	dir := makeHarnessConfigDir(t, deriveHCName, map[string]string{"config.yaml": deriveBareYAML, "extra.txt": "x"})
	require.NoError(t, srv.BootstrapHarnessConfigsFromDir(ctx, dir))
	before := globalHC(t, s, deriveHCName)
	require.Len(t, before.Files, 2)

	// The new directory drops extra.txt. A bootstrap that loses the
	// compare-and-swap leaves the row with the other commit's manifest,
	// which still lists extra.txt, and its blob in place.
	require.NoError(t, os.WriteFile(filepath.Join(dir, deriveHCName, "config.yaml"), []byte(deriveFullYAML), 0o644))
	require.NoError(t, os.Remove(filepath.Join(dir, deriveHCName, "extra.txt")))
	fault.Arm()
	require.NoError(t, srv.BootstrapHarnessConfigsFromDir(ctx, dir), "startup does not fail")
	assert.Equal(t, 2, w.attempts(), "one retry, no third attempt")

	after := globalHC(t, s, deriveHCName)
	assert.Equal(t, before.ContentHash, after.ContentHash)
	assert.Len(t, after.Files, 2)
	assertDerivedFrom(t, after, deriveBareYAML)
	exists, err := srv.GetStorage().Exists(ctx, hcBlobPath(after, "x"))
	require.NoError(t, err)
	assert.True(t, exists, "a lost commit must not remove a file the row still lists")
}

// A bootstrap whose commit succeeds deletes nothing (ptone/scion#3170, the
// prefix reconcile is gone): a file the new directory drops keeps its blob
// until the blob garbage collector's grace period has passed, and an object
// the bootstrap never wrote under the row's paths is left alone.
func TestHarnessConfigBootstrap_SuccessfulCommitDeletesNothing(t *testing.T) {
	srv, s, _ := newHCCommitTestServer(t)
	ctx := context.Background()
	dir := makeHarnessConfigDir(t, deriveHCName, map[string]string{"config.yaml": deriveBareYAML, "extra.txt": "x"})
	require.NoError(t, srv.BootstrapHarnessConfigsFromDir(ctx, dir))
	row := globalHC(t, s, deriveHCName)
	stray := templateStagedObjectPath(row.StoragePath, "other-upload", "stray.txt")
	_, err := srv.GetStorage().Upload(ctx, stray, strings.NewReader("stray"), storage.UploadOptions{})
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(dir, deriveHCName, "config.yaml"), []byte(deriveFullYAML), 0o644))
	require.NoError(t, os.Remove(filepath.Join(dir, deriveHCName, "extra.txt")))
	require.NoError(t, srv.BootstrapHarnessConfigsFromDir(ctx, dir))
	after := globalHC(t, s, deriveHCName)
	assertDerivedFrom(t, after, deriveFullYAML)
	require.Len(t, after.Files, 1)
	for _, obj := range []string{hcBlobPath(after, "x"), hcBlobPath(after, deriveBareYAML), stray} {
		exists, err := srv.GetStorage().Exists(ctx, obj)
		require.NoError(t, err)
		assert.True(t, exists, "%s: a commit deletes nothing; the blob garbage collector does", obj)
	}
}

// OnHashMatch re-derive: when the re-read row no longer has the directory's
// content hash, another writer replaced the content and the re-derive is
// skipped without a second attempt (ts4a-rev #4).
func TestHarnessConfigOnHashMatch_SkipsWhenContentReplaced(t *testing.T) {
	srv, s, w, fault := newHCCommitTestServerWithConflicts(t)
	ctx := context.Background()
	dir := makeHarnessConfigDir(t, deriveHCName, map[string]string{"config.yaml": deriveFullYAML})
	require.NoError(t, srv.BootstrapHarnessConfigsFromDir(ctx, dir))
	row := globalHC(t, s, deriveHCName)
	// Stale derived fields with unchanged content: the next bootstrap
	// takes the OnHashMatch re-derive.
	row.Config = &store.HarnessConfigData{Image: "stale/image:0"}
	require.NoError(t, setHarnessConfigContentForTest(ctx, s, row))

	w.onConflict = func(ctx context.Context, inner store.Store, hc *store.HarnessConfig) {
		other, err := inner.GetHarnessConfig(ctx, hc.ID)
		require.NoError(t, err)
		expected := harnessConfigCommitPrecondition(other)
		other.ContentHash = "sha256:replaced"
		require.NoError(t, inner.UpdateHarnessConfigContent(ctx, other, expected))
	}
	fault.Arm()
	require.NoError(t, srv.BootstrapHarnessConfigsFromDir(ctx, dir))
	assert.Equal(t, 1, w.attempts(), "replaced content: no retry")
	assert.Equal(t, "sha256:replaced", globalHC(t, s, deriveHCName).ContentHash, "the other writer's commit stays")
}

// Repair retries a lost compare-and-swap once; a second conflict is logged
// and skipped (nil), with no third attempt and the row unchanged
// (ts4a-rev #4).
func TestHarnessConfigRepair_SecondConflictSkips(t *testing.T) {
	srv, s, w, fault := newHCCommitTestServerWithConflicts(t)
	ctx := context.Background()
	hc := harnessConfigCommitPaths[0].apply(t, srv, s, deriveFullYAML)
	hc = resetHCToLegacy(t, srv, s, hc,
		map[string]string{"config.yaml": deriveFullYAML},
		map[string]string{"config.yaml": deriveBareYAML})

	fault.Arm()
	require.NoError(t, srv.syncHarnessConfigFromStorage(ctx, HarnessConfigRepairRef{ID: hc.ID}))
	assert.Equal(t, 2, w.attempts(), "one retry, no third attempt")

	after := globalHC(t, s, deriveHCName)
	assert.Equal(t, hc.ContentHash, after.ContentHash)
	assertDerivedFrom(t, after, deriveFullYAML)
}

// BootstrapSource (the bundled/built-in startup path): an update or a
// storage repair whose commit loses the compare-and-swap twice deletes
// nothing, so the row's manifest never lists objects that are gone
// (ts4a-rev R2; with the blob layout no path reconciles storage at all).
func TestHarnessConfigBootstrapSource_LostCommitDoesNotReconcile(t *testing.T) {
	t.Run("update", func(t *testing.T) {
		srv, s, w, fault := newHCCommitTestServerWithConflicts(t)
		ctx := context.Background()
		rs := srv.harnessConfigStore("claude")
		v1 := testBundledResource(storage.ResourceKindHarnessConfig, deriveHCName, map[string]string{
			"config.yaml": deriveBareYAML, "extra.txt": "x",
		})
		_, err := rs.BootstrapSource(ctx, NewFSResourceSource(v1), BootstrapOptions{OverwritePolicy: OverwriteBuiltinManaged})
		require.NoError(t, err)
		before := globalHC(t, s, deriveHCName)
		require.Len(t, before.Files, 2)

		v2 := testBundledResource(storage.ResourceKindHarnessConfig, deriveHCName, map[string]string{
			"config.yaml": deriveFullYAML,
		})
		fault.Arm()
		_, err = srv.harnessConfigStore("claude").BootstrapSource(ctx, NewFSResourceSource(v2), BootstrapOptions{OverwritePolicy: OverwriteBuiltinManaged})
		require.ErrorIs(t, err, store.ErrHarnessConfigConflict)
		assert.Equal(t, 2, w.attempts(), "one retry, no third attempt")

		after := globalHC(t, s, deriveHCName)
		assert.Equal(t, before.ContentHash, after.ContentHash)
		assert.Len(t, after.Files, 2)
		exists, err := srv.GetStorage().Exists(ctx, hcBlobPath(after, "x"))
		require.NoError(t, err)
		assert.True(t, exists, "a lost commit must not remove a file the row still lists")
	})

	t.Run("storage repair", func(t *testing.T) {
		srv, s, w, fault := newHCCommitTestServerWithConflicts(t)
		ctx := context.Background()
		v1 := testBundledResource(storage.ResourceKindHarnessConfig, deriveHCName, map[string]string{
			"config.yaml": deriveBareYAML, "extra.txt": "x",
		})
		_, err := srv.harnessConfigStore("claude").BootstrapSource(ctx, NewFSResourceSource(v1), BootstrapOptions{OverwritePolicy: OverwriteBuiltinManaged})
		require.NoError(t, err)
		row := globalHC(t, s, deriveHCName)
		// A missing listed blob triggers the repair; an unreferenced blob
		// shows whether anything was swept.
		require.NoError(t, srv.GetStorage().Delete(ctx, hcBlobPath(row, "x")))
		stray := hcBlobPath(row, "stray")
		_, err = srv.GetStorage().Upload(ctx, stray, strings.NewReader("stray"), storage.UploadOptions{})
		require.NoError(t, err)

		fault.Arm()
		_, err = srv.harnessConfigStore("claude").BootstrapSource(ctx, NewFSResourceSource(v1), BootstrapOptions{
			OverwritePolicy: OverwriteBuiltinManaged, RepairStorage: true,
		})
		require.ErrorIs(t, err, store.ErrHarnessConfigConflict)
		assert.Equal(t, 2, w.attempts(), "one retry, no third attempt")
		exists, err := srv.GetStorage().Exists(ctx, stray)
		require.NoError(t, err)
		assert.True(t, exists, "a lost repair commit must not sweep storage")
	})
}

// existsErrorStorage fails every Exists and Download call while failing is
// set, like a transient storage or permission error. With the blob layout
// (ptone/scion#4219 part 2) a commit reads staged uploads and blobs with
// Download rather than checking paths with Exists, so both fail.
type existsErrorStorage struct {
	*mockStorage
	failing bool
}

func (m *existsErrorStorage) Exists(ctx context.Context, objectPath string) (bool, error) {
	if m.failing {
		return false, errors.New("injected storage error")
	}
	return m.mockStorage.Exists(ctx, objectPath)
}

func (m *existsErrorStorage) Download(ctx context.Context, objectPath string) (io.ReadCloser, *storage.Object, error) {
	if m.failing {
		return nil, nil, errors.New("injected storage error")
	}
	return m.mockStorage.Download(ctx, objectPath)
}

// A storage error while the commit reads the objects it introduces (staged
// uploads, blobs) is a 500, not the 400 "file not found" (GCP#3669 review,
// finding 1), and the row is left unchanged.
func TestHarnessConfigCommit_StorageErrorIsNotFileNotFound(t *testing.T) {
	srv, s := testServer(t)
	stor := &existsErrorStorage{mockStorage: newMockStorage("test-bucket")}
	srv.SetStorage(stor)
	hc := harnessConfigCommitPaths[0].apply(t, srv, s, deriveBareYAML)

	stor.failing = true
	rec := finalizeHCManifest(t, srv, hc, map[string]string{"config.yaml": deriveFullYAML})
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "file not found")

	after := globalHC(t, s, deriveHCName)
	assert.Equal(t, hc.ContentHash, after.ContentHash, "a failed commit leaves the row unchanged")
	assertDerivedFrom(t, after, deriveBareYAML)
}
