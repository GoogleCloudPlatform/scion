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
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/stretchr/testify/require"
)

// Tests for the content-addressed harness-config storage layout
// (ptone/scion#4219 part 2, TS-4b; closes ptone/scion#3170).

const (
	hcCfgA = "harness: claude\nimage: example/a:1\n"
	hcCfgB = "harness: claude\nimage: example/b:1\n"
)

// hcBlobPath is the object path of content's blob under a blob-layout
// harness-config's storage path.
func hcBlobPath(hc *store.HarnessConfig, content string) string {
	hex, _ := templateBlobHex(commitFileHash(content))
	return templateBlobPath(hc.StoragePath, hex)
}

// stageHCUpload stages files under a fresh upload ID, the way a client's
// upload URLs do, and returns the ID and the matching manifest entries.
func stageHCUpload(t *testing.T, srv *Server, hc *store.HarnessConfig, files map[string]string) (string, []map[string]interface{}) {
	t.Helper()
	uploadID := api.NewUUID()
	manifest := make([]map[string]interface{}, 0, len(files))
	for p, c := range files {
		_, err := srv.GetStorage().Upload(context.Background(), srv.harnessConfigStagedObjectPath(hc, uploadID, p), strings.NewReader(c), storage.UploadOptions{})
		require.NoError(t, err)
		manifest = append(manifest, map[string]interface{}{"path": p, "size": len(c), "hash": commitFileHash(c)})
	}
	return uploadID, manifest
}

// resetHCToLegacy puts hc into the legacy layout at its slug path, with a
// manifest of the manifest files and the stored files written at that path,
// as a row committed before the blob layout (possibly with drifted storage)
// would be. It returns the re-read row.
func resetHCToLegacy(t *testing.T, srv *Server, s store.Store, hc *store.HarnessConfig, manifest, stored map[string]string) *store.HarnessConfig {
	t.Helper()
	ctx := context.Background()
	legacy := *hc
	legacy.Layout = ""
	legacy.StoragePath = storage.HarnessConfigStoragePath(srv.HubID(), hc.Scope, hc.ScopeID, hc.Slug)
	legacy.StorageURI = ""
	legacy.Files = commitManifest(manifest)
	legacy.ContentHash = computeContentHash(legacy.Files)
	putObjects(t, srv.GetStorage(), legacy.StoragePath, stored)
	require.NoError(t, setHarnessConfigContentForTest(ctx, s, &legacy))
	got, err := s.GetHarnessConfig(ctx, hc.ID)
	require.NoError(t, err)
	return got
}

// seedLegacyHC creates a legacy-layout harness-config row with its files at
// its slug path, bypassing the commit path.
func seedLegacyHC(t *testing.T, srv *Server, s store.Store, name string, files map[string]string) *store.HarnessConfig {
	t.Helper()
	slug := api.Slugify(name)
	hc := &store.HarnessConfig{
		ID: api.NewUUID(), Name: name, Slug: slug, Harness: "claude",
		Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
		StoragePath: storage.HarnessConfigStoragePath(srv.HubID(), store.HarnessConfigScopeGlobal, "", slug),
		Files:       commitManifest(files),
	}
	hc.ContentHash = computeContentHash(hc.Files)
	putObjects(t, srv.GetStorage(), hc.StoragePath, files)
	require.NoError(t, s.CreateHarnessConfig(context.Background(), hc))
	return hc
}

// putHCBlobs stores files as blobs under hc's content base, as the file APIs
// do before they commit.
func putHCBlobs(t *testing.T, srv *Server, hc *store.HarnessConfig, files map[string]string) {
	t.Helper()
	base := srv.harnessConfigContentBase(hc)
	for p, c := range files {
		hex, _ := templateBlobHex(commitFileHash(c))
		_, err := srv.GetStorage().Upload(context.Background(), templateBlobPath(base, hex), strings.NewReader(c), storage.UploadOptions{})
		require.NoError(t, err, "put blob %s", p)
	}
}

// seedBlobHC creates a blob-layout harness-config with files through the
// commit path.
func seedBlobHC(t *testing.T, srv *Server, name string, files map[string]string) *store.HarnessConfig {
	t.Helper()
	slug := api.Slugify(name)
	hc := &store.HarnessConfig{
		ID: api.NewUUID(), Name: name, Slug: slug, Harness: "claude",
		Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
		Layout: store.HarnessConfigLayoutBlobs,
	}
	hc.StoragePath = srv.harnessConfigBlobStoragePath(hc)
	putHCBlobs(t, srv, hc, files)
	require.NoError(t, srv.commitHarnessConfigFiles(context.Background(), hc, commitManifest(files), harnessConfigCommitOpts{create: true}))
	return hc
}

func getHC(t *testing.T, s store.Store, id string) *store.HarnessConfig {
	t.Helper()
	got, err := s.GetHarnessConfig(context.Background(), id)
	require.NoError(t, err)
	return got
}

func downloadHC(t *testing.T, srv *Server, id string) DownloadResponse {
	t.Helper()
	w := doTemplateRequest(t, srv, http.MethodGet, "/api/v1/harness-configs/"+id+"/download", "", nil)
	mustStatus(t, w, http.StatusOK)
	var resp DownloadResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp
}

// writeHCFile writes one file through the file API (one commit).
func writeHCFile(t *testing.T, srv *Server, id, path, content string) {
	t.Helper()
	mustStatus(t, doTemplateRequest(t, srv, http.MethodPut, "/api/v1/harness-configs/"+id+"/files/"+path, "application/octet-stream", []byte(content)), http.StatusOK)
}

func finalizeHC(t *testing.T, srv *Server, id string, files []store.TemplateFile, uploadID string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{
		"manifest": map[string]interface{}{"version": "1.0", "files": files},
		"uploadId": uploadID,
	})
	require.NoError(t, err)
	return doTemplateRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+id+"/finalize", "application/json", body)
}

// Acceptance: a hydration that runs while a push lands gets one complete
// version (here the old one) on a GCS-style backend and on local storage,
// and the download response names that version's content hash (the
// broker's cache key). Mutant "download signs path URLs for blob rows"
// fails both subtests.
func TestHarnessConfigBlob_HydrationDuringPushSeesOneVersion(t *testing.T) {
	v1 := map[string]string{"config.yaml": hcCfgA, "a.txt": "a, version one", "b.txt": "b, version one"}
	v2 := map[string]string{"config.yaml": hcCfgB, "a.txt": "a, version two", "b.txt": "b, version two"}

	check := func(t *testing.T, resp DownloadResponse, hc *store.HarnessConfig, got map[string][]byte) {
		t.Helper()
		if resp.ContentHash != hc.ContentHash {
			t.Errorf("download contentHash = %q, want the signed version's %q", resp.ContentHash, hc.ContentHash)
		}
		for _, f := range resp.Files {
			if transfer.HashBytes(got[f.Path]) != f.Hash {
				t.Errorf("%s: content does not match its manifest hash (mixed versions)", f.Path)
			}
			if string(got[f.Path]) != v1[f.Path] {
				t.Errorf("%s = %q, want the version the hydration started with %q", f.Path, got[f.Path], v1[f.Path])
			}
		}
	}

	t.Run("gcs", func(t *testing.T) {
		gcs := newMockStorage("gcs-bucket")
		gcs.provider = storage.ProviderGCS
		srv, s := newCommitTestServer(t, gcs)
		hc := seedBlobHC(t, srv, "hydrate-gcs", v1)

		resp := downloadHC(t, srv, hc.ID)
		if len(resp.Files) != len(v1) {
			t.Fatalf("download listed %d files, want %d", len(resp.Files), len(v1))
		}
		got := make(map[string][]byte)
		first := resp.Files[0]
		data, err := fetchMockSignedURL(t, gcs, "gcs-bucket", first.URL)
		if err != nil {
			t.Fatalf("fetch %s: %v", first.Path, err)
		}
		got[first.Path] = data

		// The push lands mid-hydration.
		uploadID, _ := stageHCUpload(t, srv, hc, v2)
		mustStatus(t, finalizeHC(t, srv, hc.ID, commitManifest(v2), uploadID), http.StatusOK)
		if pushed := getHC(t, s, hc.ID); pushed.ContentHash == hc.ContentHash {
			t.Fatal("precondition: the push did not commit")
		}

		for _, f := range resp.Files[1:] {
			data, err := fetchMockSignedURL(t, gcs, "gcs-bucket", f.URL)
			if err != nil {
				t.Fatalf("fetch %s after the push: %v", f.Path, err)
			}
			got[f.Path] = data
		}
		check(t, resp, hc, got)
	})

	t.Run("local", func(t *testing.T) {
		stor := newCommitTestStorage(t)
		srv, s := newCommitTestServer(t, stor)
		hc := seedBlobHC(t, srv, "hydrate-local", v1)

		resp := downloadHC(t, srv, hc.ID)
		got := make(map[string][]byte)
		first := resp.Files[0]
		got[first.Path] = fetchHubURL(t, srv, first.URL)

		// The push lands mid-hydration, one commit per file, as the file
		// APIs do.
		for p, c := range v2 {
			writeHCFile(t, srv, hc.ID, p, c)
		}
		if pushed := getHC(t, s, hc.ID); pushed.ContentHash == hc.ContentHash {
			t.Fatal("precondition: the push did not commit")
		}

		for _, f := range resp.Files[1:] {
			got[f.Path] = fetchHubURL(t, srv, f.URL)
		}
		check(t, resp, hc, got)

		// A hash that is not this harness-config's blob is not found; a
		// malformed one is refused.
		w := doTemplateRequest(t, srv, http.MethodGet, "/api/v1/harness-configs/"+hc.ID+"/files/a.txt?raw=1&hash="+url.QueryEscape(commitFileHash("never stored")), "", nil)
		mustStatus(t, w, http.StatusNotFound)
		w = doTemplateRequest(t, srv, http.MethodGet, "/api/v1/harness-configs/"+hc.ID+"/files/a.txt?raw=1&hash=sha256:..%2F..%2Fx", "", nil)
		mustStatus(t, w, http.StatusBadRequest)
	})
}

// stageHCViaUploadURLs requests upload URLs for files and PUTs each file to
// its (local-storage) URL, as the CLI does. It returns the upload response.
func stageHCViaUploadURLs(t *testing.T, srv *Server, id string, files map[string]string) UploadResponse {
	t.Helper()
	var reqs []FileUploadRequest
	for p, c := range files {
		reqs = append(reqs, FileUploadRequest{Path: p, Size: int64(len(c))})
	}
	body, _ := json.Marshal(UploadRequest{Files: reqs})
	w := doTemplateRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+id+"/upload", "application/json", body)
	mustStatus(t, w, http.StatusOK)
	var resp UploadResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	if resp.UploadID == "" || len(resp.UploadURLs) != len(files) {
		t.Fatalf("upload response = %+v", resp)
	}
	putToUploadURLs(t, srv, resp.UploadURLs, resp.UploadID, files)
	return resp
}

func putToUploadURLs(t *testing.T, srv *Server, urls []UploadURLInfo, uploadID string, files map[string]string) {
	t.Helper()
	for _, u := range urls {
		if !strings.Contains(u.URL, "uploadId="+uploadID) {
			t.Fatalf("local upload URL %q does not stage under the upload ID", u.URL)
		}
		parsed, err := url.Parse(u.URL)
		require.NoError(t, err)
		mustStatus(t, doTemplateRequest(t, srv, u.Method, parsed.RequestURI(), "application/octet-stream", []byte(files[u.Path])), http.StatusOK)
	}
}

// Acceptance: old-CLI compatibility and the staging flow. Upload URLs (and
// the create response's URLs) stage without committing, and a finalize with
// or without the uploadId hashes and moves the staged files into blobs. A
// file that was neither staged nor stored keeps the "file not found" text
// older CLIs retry on.
func TestHarnessConfigBlob_UploadStagesAndFinalizeCommits(t *testing.T) {
	ctx := context.Background()
	stor := newCommitTestStorage(t)
	srv, s := newCommitTestServer(t, stor)

	// Create with files: the response carries staging URLs and an upload ID.
	createBody, _ := json.Marshal(map[string]interface{}{
		"name": "cli-hc", "harness": "claude", "scope": "global",
		"files": []FileUploadRequest{{Path: "config.yaml", Size: int64(len(hcCfgA))}},
	})
	w := doTemplateRequest(t, srv, http.MethodPost, "/api/v1/harness-configs", "application/json", createBody)
	mustStatus(t, w, http.StatusCreated)
	var created CreateHarnessConfigResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	hc := created.HarnessConfig
	if hc.Layout != store.HarnessConfigLayoutBlobs || hc.StoragePath != srv.harnessConfigBlobStoragePath(hc) {
		t.Fatalf("new harness-config layout %q path %q, want the blob layout at its row-unique path", hc.Layout, hc.StoragePath)
	}
	if created.UploadID == "" || len(created.UploadURLs) != 1 {
		t.Fatalf("create response = %+v", created)
	}
	putToUploadURLs(t, srv, created.UploadURLs, created.UploadID, map[string]string{"config.yaml": hcCfgA})
	if got := getHC(t, s, hc.ID); got.Status != store.HarnessConfigStatusPending || len(got.Files) != 0 {
		t.Fatal("staging an upload changed the row before finalize")
	}
	first := map[string]string{"config.yaml": hcCfgA}
	mustStatus(t, finalizeHC(t, srv, hc.ID, commitManifest(first), created.UploadID), http.StatusOK)
	got := getHC(t, s, hc.ID)
	if got.Status != store.HarnessConfigStatusActive || got.Config == nil || got.Config.Image != "example/a:1" {
		t.Fatalf("after the first finalize: status %q config %+v", got.Status, got.Config)
	}

	// Old CLI: uploads, then finalizes without an uploadId.
	push := map[string]string{"config.yaml": hcCfgA, "new.txt": "from an old cli", "dir/nested.txt": "nested"}
	stageHCViaUploadURLs(t, srv, hc.ID, map[string]string{"new.txt": push["new.txt"], "dir/nested.txt": push["dir/nested.txt"]})
	if again := getHC(t, s, hc.ID); again.ContentHash != got.ContentHash {
		t.Fatal("staging an upload changed the row before finalize")
	}
	mustStatus(t, finalizeHC(t, srv, hc.ID, commitManifest(push), ""), http.StatusOK)
	got = getHC(t, s, hc.ID)
	if !reflect.DeepEqual(got.Files, commitManifest(push)) {
		t.Errorf("files = %+v, want %+v", got.Files, commitManifest(push))
	}
	for p, c := range push {
		if string(readStored(t, stor, hcBlobPath(got, c))) != c {
			t.Errorf("blob of %s missing or wrong after finalize", p)
		}
	}
	if listed, err := stor.List(ctx, storage.ListOptions{Prefix: templateStagingPrefix(got.StoragePath)}); err != nil || len(listed.Objects) != 0 {
		t.Errorf("staged uploads left after finalize: %+v %v", listed, err)
	}

	// New client: sends the uploadId back.
	push2 := map[string]string{"config.yaml": hcCfgB, "new.txt": "from a new cli", "dir/nested.txt": "nested"}
	up := stageHCViaUploadURLs(t, srv, hc.ID, map[string]string{"config.yaml": push2["config.yaml"], "new.txt": push2["new.txt"]})
	mustStatus(t, finalizeHC(t, srv, hc.ID, commitManifest(push2), up.UploadID), http.StatusOK)
	got = getHC(t, s, hc.ID)
	if string(readStored(t, stor, hcBlobPath(got, push2["new.txt"]))) != push2["new.txt"] {
		t.Error("blob of the new client's upload missing after finalize")
	}
	if got.Config == nil || got.Config.Image != "example/b:1" {
		t.Errorf("config not derived from the staged config.yaml: %+v", got.Config)
	}

	// A file neither staged nor stored keeps the old CLI's retry text.
	missing := map[string]string{"config.yaml": hcCfgB, "ghost.txt": "never uploaded"}
	w = finalizeHC(t, srv, hc.ID, commitManifest(missing), "")
	mustStatus(t, w, http.StatusBadRequest)
	if !strings.Contains(w.Body.String(), "file not found") {
		t.Errorf("missing file: body %q lacks \"file not found\"", w.Body.String())
	}

	// An upload ID that cannot name a staging directory is refused.
	w = doTemplateRequest(t, srv, http.MethodPut, "/api/v1/harness-configs/"+hc.ID+"/files/x.txt?uploadId=..%2Fescape", "application/octet-stream", []byte("x"))
	mustStatus(t, w, http.StatusBadRequest)
	mustStatus(t, finalizeHC(t, srv, hc.ID, commitManifest(push2), "../escape"), http.StatusBadRequest)
}

// Acceptance: staged content that does not match the manifest hash is
// refused with 400, with or without an uploadId, and the row and blobs are
// unchanged.
func TestHarnessConfigBlob_StagedHashMismatchRefused(t *testing.T) {
	stor := newCommitTestStorage(t)
	srv, s := newCommitTestServer(t, stor)
	hc := seedBlobHC(t, srv, "mismatch", map[string]string{"config.yaml": hcCfgA})
	staged := srv.harnessConfigStagedObjectPath(hc, "u1", "x.txt")
	_, err := stor.Upload(context.Background(), staged, strings.NewReader("tampered"), storage.UploadOptions{})
	require.NoError(t, err)
	manifest := commitManifest(map[string]string{"config.yaml": hcCfgA, "x.txt": "expected"})

	for _, uploadID := range []string{"u1", ""} {
		w := finalizeHC(t, srv, hc.ID, manifest, uploadID)
		mustStatus(t, w, http.StatusBadRequest)
		got := getHC(t, s, hc.ID)
		if got.ContentHash != hc.ContentHash || !reflect.DeepEqual(got.Files, hc.Files) {
			t.Errorf("uploadId %q: refused finalize changed the row", uploadID)
		}
		if objectExists(t, stor, hcBlobPath(hc, "tampered")) || objectExists(t, stor, hcBlobPath(hc, "expected")) {
			t.Errorf("uploadId %q: refused finalize wrote a blob", uploadID)
		}
	}
}

// Acceptance: the collector deletes only unreferenced harness-config blobs
// older than the grace period and stale staged uploads; referenced blobs and
// young blobs are kept, and the maintenance operation covers harness-configs
// in the same pass as templates. Mutant "GC ignores grace" fails it.
func TestHarnessConfigBlobGC_DeletesOnlyAgedUnreferencedBlobs(t *testing.T) {
	ctx := context.Background()
	stor := newCommitTestStorage(t)
	srv, s := newCommitTestServer(t, stor)
	hc := seedBlobHC(t, srv, "gc", map[string]string{"config.yaml": hcCfgA, "a.txt": "a1"})
	writeHCFile(t, srv, hc.ID, "a.txt", "a2")
	got := getHC(t, s, hc.ID)
	putHCBlobs(t, srv, got, map[string]string{"young.txt": "young and unreferenced"})

	oldUnref := hcBlobPath(got, "a1")
	young := hcBlobPath(got, "young and unreferenced")
	referenced := hcBlobPath(got, "a2")
	cfg := hcBlobPath(got, hcCfgA)
	for _, p := range []string{oldUnref, referenced, cfg} {
		ageObject(t, stor, p, 48*time.Hour)
	}
	staleStaged := templateStagedObjectPath(got.StoragePath, "old-upload", "x.txt")
	freshStaged := templateStagedObjectPath(got.StoragePath, "new-upload", "x.txt")
	for _, p := range []string{staleStaged, freshStaged} {
		_, err := stor.Upload(ctx, p, strings.NewReader("staged"), storage.UploadOptions{})
		require.NoError(t, err)
	}
	ageObject(t, stor, staleStaged, 48*time.Hour)

	report, err := srv.collectBlobGarbage(ctx, 24*time.Hour, time.Now())
	require.NoError(t, err)
	if objectExists(t, stor, oldUnref) {
		t.Error("an unreferenced blob older than the grace period was kept")
	}
	if !objectExists(t, stor, young) {
		t.Error("an unreferenced blob younger than the grace period was deleted")
	}
	if !objectExists(t, stor, referenced) || !objectExists(t, stor, cfg) {
		t.Error("a referenced blob was deleted")
	}
	if objectExists(t, stor, staleStaged) {
		t.Error("a staged upload older than the grace period was kept")
	}
	if !objectExists(t, stor, freshStaged) {
		t.Error("a staged upload younger than the grace period was deleted")
	}
	if report.HarnessConfigs < 1 || report.DeletedBlobs != 1 || report.DeletedStaged != 1 {
		t.Errorf("report = %+v, want harness-configs scanned, 1 blob and 1 staged upload deleted", report)
	}

	// The row still hydrates completely.
	for _, f := range downloadHC(t, srv, hc.ID).Files {
		if transfer.HashBytes(fetchHubURL(t, srv, f.URL)) != f.Hash {
			t.Errorf("%s does not hydrate after collection", f.Path)
		}
	}

	// The maintenance operation runs the same pass for harness-configs.
	putHCBlobs(t, srv, got, map[string]string{"old.txt": "another old unreferenced"})
	another := hcBlobPath(got, "another old unreferenced")
	ageObject(t, stor, another, 48*time.Hour)
	exec := &TemplateBlobGCExecutor{srv: srv}
	var log bytes.Buffer
	require.NoError(t, exec.Run(ctx, &log, map[string]string{"grace": "1h"}))
	if objectExists(t, stor, another) {
		t.Error("the maintenance operation did not collect a harness-config blob")
	}
	if !strings.Contains(log.String(), "harness configs: ") || strings.Contains(log.String(), "harness configs: 0,") {
		t.Errorf("executor log does not report harness-configs: %q", log.String())
	}
}

// F5b for harness-configs: a commit that re-references a blob that was
// present but unreferenced rewrites it, so a collection pass that listed it
// as old cannot delete it.
func TestHarnessConfigBlobGC_ReReferencedBlobSurvives(t *testing.T) {
	ctx := context.Background()
	stor := newCommitTestStorage(t)
	srv, s := newCommitTestServer(t, stor)
	hc := seedBlobHC(t, srv, "revert", map[string]string{"config.yaml": hcCfgA, "a.txt": "a1"})
	writeHCFile(t, srv, hc.ID, "a.txt", "a2")
	revived := hcBlobPath(hc, "a1")
	ageObject(t, stor, revived, 48*time.Hour)
	listed := storage.Object{Name: revived, Updated: time.Now().Add(-48 * time.Hour)}

	// The revert re-references a1 without uploading it (it is stored).
	revert := map[string]string{"config.yaml": hcCfgA, "a.txt": "a1"}
	mustStatus(t, finalizeHC(t, srv, hc.ID, commitManifest(revert), ""), http.StatusOK)

	if srv.deleteAgedTemplateObject(ctx, stor, listed, 24*time.Hour, time.Now(), &templateBlobGCReport{}) {
		t.Fatal("the collector deleted a blob a commit had just refreshed")
	}
	_, err := srv.collectHarnessConfigBlobGarbage(ctx, 24*time.Hour, time.Now())
	require.NoError(t, err)
	if !objectExists(t, stor, revived) {
		t.Fatal("re-referenced blob was deleted")
	}
	if got := getHC(t, s, hc.ID); !reflect.DeepEqual(got.Files, commitManifest(revert)) {
		t.Errorf("files = %+v, want the reverted manifest", got.Files)
	}
}

// The grace period's 1h floor applies to harness-config collection too.
func TestHarnessConfigBlobGC_GraceHasFloor(t *testing.T) {
	ctx := context.Background()
	stor := newCommitTestStorage(t)
	srv, s := newCommitTestServer(t, stor)
	hc := seedBlobHC(t, srv, "floor", map[string]string{"config.yaml": hcCfgA})
	got := getHC(t, s, hc.ID)
	putHCBlobs(t, srv, got, map[string]string{"pending.txt": "written for a commit that has not landed"})
	pending := hcBlobPath(got, "written for a commit that has not landed")
	ageObject(t, stor, pending, 10*time.Minute)

	_, err := srv.collectHarnessConfigBlobGarbage(ctx, 0, time.Now())
	require.NoError(t, err)
	if !objectExists(t, stor, pending) {
		t.Fatal("a pass with a zero grace deleted a 10-minute-old blob; the 1h floor was not applied")
	}
}

// Acceptance: on a blob row a single-file write or delete makes the same
// small number of storage calls whether the harness-config has 2 files or
// 200.
func TestHarnessConfigBlob_SingleFileCommitCostIsConstant(t *testing.T) {
	measure := func(t *testing.T, n int) (write, del int64) {
		t.Helper()
		cs := &countingStorage{Storage: newCommitTestStorage(t)}
		srv, _ := newCommitTestServer(t, cs)
		files := map[string]string{"config.yaml": hcCfgA}
		for i := 0; i < n; i++ {
			files[fmt.Sprintf("f%03d.txt", i)] = fmt.Sprintf("file %d", i)
		}
		hc := seedBlobHC(t, srv, fmt.Sprintf("cost-%d", n), files)
		cs.calls.Store(0)
		writeHCFile(t, srv, hc.ID, "README.md", "# readme")
		write = cs.calls.Load()
		cs.calls.Store(0)
		mustStatus(t, doTemplateRequest(t, srv, http.MethodDelete, "/api/v1/harness-configs/"+hc.ID+"/files/f000.txt", "", nil), http.StatusOK)
		del = cs.calls.Load()
		return write, del
	}
	w2, d2 := measure(t, 2)
	w200, d200 := measure(t, 200)
	if w200 != w2 || d200 != d2 {
		t.Errorf("storage calls grow with the manifest: write %d -> %d, delete %d -> %d (2 -> 200 files)", w2, w200, d2, d200)
	}
	if w200 > 3 || d200 > 2 {
		t.Errorf("storage calls: write %d (want <= 3), delete %d (want <= 2)", w200, d200)
	}
}

// Acceptance: a legacy row hydrates, repairs and clones unchanged until its
// first commit migrates it; the migration removes the legacy tree and leaves
// nothing at the new storage path itself, so a co-located broker's direct
// read misses and the broker hydrates.
func TestHarnessConfigBlob_LegacyRowHydratesClonesAndMigrates(t *testing.T) {
	ctx := context.Background()
	stor := newCommitTestStorage(t)
	srv, s := newCommitTestServer(t, stor)
	files := map[string]string{"config.yaml": hcCfgA, "a.txt": "legacy a"}
	legacy := seedLegacyHC(t, srv, s, "legacy-hc", files)

	resp := downloadHC(t, srv, legacy.ID)
	for _, f := range resp.Files {
		if strings.Contains(f.URL, "hash=") {
			t.Errorf("legacy download URL %q names a blob", f.URL)
		}
		if got := fetchHubURL(t, srv, f.URL); string(got) != files[f.Path] {
			t.Errorf("legacy hydration of %s = %q", f.Path, got)
		}
	}

	require.NoError(t, srv.syncHarnessConfigFromStorage(ctx, HarnessConfigRepairRef{ID: legacy.ID}))
	if got := getHC(t, s, legacy.ID); got.Layout != "" || got.StoragePath != legacy.StoragePath {
		t.Fatalf("repair without drift changed the legacy row: layout %q path %q", got.Layout, got.StoragePath)
	}

	body, _ := json.Marshal(CloneTemplateRequest{Name: "legacy-hc-clone", Scope: store.HarnessConfigScopeGlobal})
	w := doTemplateRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+legacy.ID+"/clone", "application/json", body)
	mustStatus(t, w, http.StatusCreated)
	var clone store.HarnessConfig
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &clone))
	if clone.Layout != store.HarnessConfigLayoutBlobs {
		t.Errorf("clone layout = %q, want blobs", clone.Layout)
	}
	for p, c := range files {
		if string(readStored(t, stor, hcBlobPath(&clone, c))) != c {
			t.Errorf("clone blob of %s missing", p)
		}
	}
	if got := getHC(t, s, legacy.ID); got.Layout != "" {
		t.Fatal("cloning migrated the source")
	}

	// The first commit migrates the row.
	writeHCFile(t, srv, legacy.ID, "b.txt", "new b")
	got := getHC(t, s, legacy.ID)
	if got.Layout != store.HarnessConfigLayoutBlobs || got.StoragePath != srv.harnessConfigBlobStoragePath(got) {
		t.Fatalf("after the first commit: layout %q path %q", got.Layout, got.StoragePath)
	}
	for _, f := range got.Files {
		if !objectExists(t, stor, srv.harnessConfigObjectPath(got, f)) {
			t.Errorf("blob of %s missing after migration", f.Path)
		}
	}
	if !notExistOnDisk(t, stor, legacy.StoragePath) {
		t.Error("the unshared legacy tree was not removed")
	}
	if !notExistOnDisk(t, stor, got.StoragePath) {
		t.Error("something was written at the blob row's storage path itself; a co-located broker would read it")
	}
	resp = downloadHC(t, srv, legacy.ID)
	if resp.ContentHash != got.ContentHash {
		t.Errorf("download contentHash = %q, want %q", resp.ContentHash, got.ContentHash)
	}
	for _, f := range resp.Files {
		if !strings.Contains(f.URL, "hash=") {
			t.Errorf("blob download URL %q does not name its blob", f.URL)
		}
		if transfer.HashBytes(fetchHubURL(t, srv, f.URL)) != f.Hash {
			t.Errorf("%s does not hydrate after the migration", f.Path)
		}
	}

	// The cloned blob row survives the source's deletion with its files.
	mustStatus(t, doTemplateRequest(t, srv, http.MethodDelete, "/api/v1/harness-configs/"+legacy.ID+"?deleteFiles=true", "", nil), http.StatusNoContent)
	for _, f := range downloadHC(t, srv, clone.ID).Files {
		if transfer.HashBytes(fetchHubURL(t, srv, f.URL)) != f.Hash {
			t.Errorf("clone's %s no longer hydrates after the source was deleted", f.Path)
		}
	}
}

// Acceptance: two rows that share a legacy path (a rename, then a new
// harness-config with the old name) never read, delete or collect each
// other's objects after either one migrates.
func TestHarnessConfigBlob_SharedLegacyPathIsolation(t *testing.T) {
	ctx := context.Background()
	stor := newCommitTestStorage(t)
	srv, s := newCommitTestServer(t, stor)

	a := seedLegacyHC(t, srv, s, "foo", map[string]string{"config.yaml": hcCfgA, "x.txt": "from A"})
	shared := a.StoragePath
	a.Name, a.Slug = "bar", "bar"
	require.NoError(t, s.UpdateHarnessConfig(ctx, a))
	b := seedLegacyHC(t, srv, s, "foo", map[string]string{"config.yaml": hcCfgB, "y.txt": "from B"})
	if b.StoragePath != shared {
		t.Fatalf("precondition: rows do not share the legacy path (%q, %q)", b.StoragePath, shared)
	}
	// The shared tree now holds B's config.yaml; A's entry for it no
	// longer matches, as on a real shared path.

	// A migrates; B still reads the shared tree.
	writeHCFile(t, srv, a.ID, "z.txt", "A again")
	gotA := getHC(t, s, a.ID)
	if gotA.Layout != store.HarnessConfigLayoutBlobs || gotA.StoragePath == shared {
		t.Fatalf("A did not migrate to its own path: %q", gotA.StoragePath)
	}
	if !objectExists(t, stor, shared+"/y.txt") {
		t.Fatal("A's migration removed the legacy tree B still uses")
	}
	for _, f := range downloadHC(t, srv, b.ID).Files {
		want := map[string]string{"config.yaml": hcCfgB, "y.txt": "from B"}[f.Path]
		if string(fetchHubURL(t, srv, f.URL)) != want {
			t.Errorf("B reads %s wrongly after A migrated", f.Path)
		}
	}

	// Collection two days later, at the minimum grace, touches neither B's
	// tree nor A's blobs.
	_, err := srv.collectHarnessConfigBlobGarbage(ctx, 0, time.Now().Add(48*time.Hour))
	require.NoError(t, err)
	if !objectExists(t, stor, shared+"/y.txt") {
		t.Error("collection deleted B's legacy object")
	}
	for _, f := range gotA.Files {
		if !objectExists(t, stor, srv.harnessConfigObjectPath(gotA, f)) {
			t.Errorf("collection deleted A's blob of %s", f.Path)
		}
	}

	// B migrates; now nothing shares the legacy path, so it is removed.
	writeHCFile(t, srv, b.ID, "w.txt", "B again")
	gotB := getHC(t, s, b.ID)
	if gotB.Layout != store.HarnessConfigLayoutBlobs || gotB.StoragePath == gotA.StoragePath {
		t.Fatalf("B did not migrate to its own path: %q", gotB.StoragePath)
	}
	if !notExistOnDisk(t, stor, shared) {
		t.Error("the legacy tree was kept after the last row on it migrated")
	}
	for _, f := range downloadHC(t, srv, a.ID).Files {
		if transfer.HashBytes(fetchHubURL(t, srv, f.URL)) != f.Hash {
			t.Errorf("A's %s no longer hydrates after B migrated", f.Path)
		}
	}
	if _, ok := templateFileByPath(gotB.Files, "x.txt"); ok {
		t.Error("B's manifest picked up A's file")
	}
}

// ptone/scion#3170 (1): a legacy clone made before the blob layout lives at
// <slug path>/<clone id>, under its source's legacy path. When the source
// (renamed or not) migrates, its legacy tree is kept, because removing it
// would remove the clone's files too; nothing reconciles by prefix.
func TestHarnessConfigBlob_NestedLegacyCloneSurvivesParentMigration(t *testing.T) {
	ctx := context.Background()
	stor := newCommitTestStorage(t)
	srv, s := newCommitTestServer(t, stor)

	parent := seedLegacyHC(t, srv, s, "foo", map[string]string{"config.yaml": hcCfgA})
	parent.Name, parent.Slug = "bar", "bar"
	require.NoError(t, s.UpdateHarnessConfig(ctx, parent))
	cloneFiles := map[string]string{"config.yaml": hcCfgB}
	clone := &store.HarnessConfig{
		ID: api.NewUUID(), Name: "foo-clone", Slug: "foo-clone", Harness: "claude",
		Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
		Files: commitManifest(cloneFiles),
	}
	clone.StoragePath = parent.StoragePath + "/" + clone.ID
	clone.ContentHash = computeContentHash(clone.Files)
	putObjects(t, stor, clone.StoragePath, cloneFiles)
	require.NoError(t, s.CreateHarnessConfig(ctx, clone))

	writeHCFile(t, srv, parent.ID, "z.txt", "parent again")
	if got := getHC(t, s, parent.ID); got.Layout != store.HarnessConfigLayoutBlobs {
		t.Fatalf("parent did not migrate: layout %q", got.Layout)
	}
	if !objectExists(t, stor, clone.StoragePath+"/config.yaml") {
		t.Fatal("the parent's migration removed a nested legacy clone's files")
	}
	for _, f := range downloadHC(t, srv, clone.ID).Files {
		if string(fetchHubURL(t, srv, f.URL)) != cloneFiles[f.Path] {
			t.Errorf("nested clone's %s no longer hydrates", f.Path)
		}
	}
}

// Acceptance: repair never drops entries from a blob row, even when a blob
// is missing, on the targeted and the startup repair paths, and
// ValidateStorage reports the missing blob.
func TestHarnessConfigBlob_RepairNeverDropsEntries(t *testing.T) {
	ctx := context.Background()
	stor := newCommitTestStorage(t)
	srv, s := newCommitTestServer(t, stor)
	hc := seedBlobHC(t, srv, "keep", map[string]string{"config.yaml": hcCfgA, "a.txt": "a", "b.txt": "b"})
	require.NoError(t, stor.Delete(ctx, hcBlobPath(hc, "b")))
	before := getHC(t, s, hc.ID)

	require.NoError(t, srv.syncHarnessConfigFromStorage(ctx, HarnessConfigRepairRef{ID: hc.ID}))
	srv.SyncAllHarnessConfigsFromStorage(ctx)

	after := getHC(t, s, hc.ID)
	if !reflect.DeepEqual(after.Files, before.Files) || after.ContentHash != before.ContentHash || after.Layout != store.HarnessConfigLayoutBlobs {
		t.Fatalf("repair changed a blob row: files %+v hash %q layout %q", after.Files, after.ContentHash, after.Layout)
	}
	report, err := srv.harnessConfigStore(after.Harness).ValidateStorage(ctx, harnessConfigToRecord(after))
	require.NoError(t, err)
	if len(report.Issues) != 1 || report.Issues[0].Kind != ValidationIssueMissingObject || report.Issues[0].File != "b.txt" {
		t.Errorf("validation issues = %+v, want only b.txt's missing blob", report.Issues)
	}
}

// Acceptance: a commit that read a legacy row loses against a concurrent
// migration that kept the content hash (the ABA case across layouts).
func TestCommitHarnessConfigFiles_LayoutABAConflicts(t *testing.T) {
	ctx := context.Background()
	stor := newCommitTestStorage(t)
	srv, s := newCommitTestServer(t, stor)
	legacy := seedLegacyHC(t, srv, s, "aba", map[string]string{"config.yaml": hcCfgA, "a.txt": "a"})
	stale := getHC(t, s, legacy.ID)
	migrator := getHC(t, s, legacy.ID)

	require.NoError(t, srv.commitHarnessConfigFiles(ctx, migrator, migrator.Files, harnessConfigCommitOpts{}))
	if migrator.ContentHash != stale.ContentHash || migrator.Layout != store.HarnessConfigLayoutBlobs {
		t.Fatalf("precondition: migration must keep the hash and move to blobs (hash %q -> %q, layout %q)", stale.ContentHash, migrator.ContentHash, migrator.Layout)
	}

	putHCBlobs(t, srv, stale, map[string]string{"b.txt": "b"})
	err := srv.commitHarnessConfigFiles(ctx, stale, upsertTemplateFile(stale.Files, commitManifest(map[string]string{"b.txt": "b"})[0]), harnessConfigCommitOpts{})
	if !errors.Is(err, store.ErrHarnessConfigConflict) {
		t.Fatalf("commit from the legacy read: err = %v, want store.ErrHarnessConfigConflict", err)
	}
	got := getHC(t, s, legacy.ID)
	if got.Layout != store.HarnessConfigLayoutBlobs || !reflect.DeepEqual(got.Files, migrator.Files) || got.StoragePath != migrator.StoragePath {
		t.Errorf("row = layout %q path %q files %+v, want the migration's", got.Layout, got.StoragePath, got.Files)
	}
}

// The hub-namespacing storage migration moves a legacy harness-config's
// path through the compare-and-swap and skips blob-layout rows. A writer
// that read the row before the migration (and its legacy cleanup) cannot
// write the old storage path back: the storage path is in the predicate
// (ptone/scion#4219 review).
func TestStorageMigration_HarnessConfigLayouts(t *testing.T) {
	ctx := context.Background()
	stor := newCommitTestStorage(t)
	srv, s := newCommitTestServer(t, stor)

	newRow := func(name, path, layout string, files map[string]string) *store.HarnessConfig {
		t.Helper()
		row := &store.HarnessConfig{
			ID: api.NewUUID(), Name: name, Slug: name, Harness: "claude",
			Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
			StoragePath: path, Layout: layout, Files: commitManifest(files),
		}
		row.ContentHash = computeContentHash(row.Files)
		if layout == store.HarnessConfigLayoutBlobs {
			putHCBlobs(t, srv, row, files)
		} else {
			putObjects(t, stor, path, files)
		}
		require.NoError(t, s.CreateHarnessConfig(ctx, row))
		return row
	}
	legacy := newRow("mig-legacy", "harness-configs/global/mig-legacy", "", map[string]string{"config.yaml": hcCfgA})
	blob := newRow("mig-blob", "harness-configs/global/mig-blob.x", store.HarnessConfigLayoutBlobs, map[string]string{"config.yaml": hcCfgB})
	stale := getHC(t, s, legacy.ID)

	report := srv.migrateResourceKind(ctx, storage.ResourceKindHarnessConfig, "mig-hub", false, true)
	if report.Migrated != 1 || report.Skipped != 1 || report.Failed != 0 {
		t.Errorf("report = %+v, want 1 migrated, 1 skipped, 0 failed", report)
	}
	wantPath := storage.ResourceStoragePath("mig-hub", storage.ResourceKindHarnessConfig, store.HarnessConfigScopeGlobal, "", "mig-legacy")
	got := getHC(t, s, legacy.ID)
	if got.StoragePath != wantPath || got.Layout != "" || got.ContentHash != legacy.ContentHash {
		t.Errorf("legacy row = path %q layout %q hash %q, want path %q with its content", got.StoragePath, got.Layout, got.ContentHash, wantPath)
	}
	if !objectExists(t, stor, wantPath+"/config.yaml") || objectExists(t, stor, legacy.StoragePath+"/config.yaml") {
		t.Error("legacy objects were not moved to the namespaced path")
	}
	if gotBlob := getHC(t, s, blob.ID); gotBlob.StoragePath != blob.StoragePath || gotBlob.Layout != store.HarnessConfigLayoutBlobs {
		t.Errorf("blob row was migrated: path %q layout %q", gotBlob.StoragePath, gotBlob.Layout)
	}

	// A narrow writer (archive-obsolete) that read the row before the
	// migration: same content hash, same layout, old storage path.
	staleWrite := *stale
	staleWrite.Status = store.HarnessConfigStatusArchived
	err := s.UpdateHarnessConfigContent(ctx, &staleWrite, harnessConfigCommitPrecondition(stale))
	if !errors.Is(err, store.ErrHarnessConfigConflict) {
		t.Fatalf("stale write after the storage migration: err = %v, want store.ErrHarnessConfigConflict", err)
	}
	if after := getHC(t, s, legacy.ID); after.StoragePath != wantPath || after.Status != store.HarnessConfigStatusActive {
		t.Errorf("stale write changed the row: path %q status %q", after.StoragePath, after.Status)
	}

	// A commit from the same stale read does not write the old path back
	// either.
	staleCommit := *stale
	if err := srv.commitHarnessConfigFiles(ctx, &staleCommit, stale.Files, harnessConfigCommitOpts{}); err == nil {
		t.Fatal("a commit from the pre-migration read succeeded")
	}
	if after := getHC(t, s, legacy.ID); after.StoragePath != wantPath || after.Layout != "" {
		t.Errorf("stale commit changed the row: path %q layout %q", after.StoragePath, after.Layout)
	}
}

// Deleting a harness-config with deleteFiles removes its blobs and staged
// uploads (and, for a legacy row, its legacy tree).
func TestHarnessConfigBlob_DeleteRemovesBlobsAndStaging(t *testing.T) {
	stor := newCommitTestStorage(t)
	srv, s := newCommitTestServer(t, stor)

	hc := seedBlobHC(t, srv, "doomed", map[string]string{"config.yaml": hcCfgA})
	stageHCUpload(t, srv, hc, map[string]string{"x.txt": "staged"})
	mustStatus(t, doTemplateRequest(t, srv, http.MethodDelete, "/api/v1/harness-configs/"+hc.ID+"?deleteFiles=true", "", nil), http.StatusNoContent)
	if !notExistOnDisk(t, stor, hc.StoragePath+templateBlobsSuffix) || !notExistOnDisk(t, stor, hc.StoragePath+templateStagingSuffix) {
		t.Error("deleting a blob row left its .blobs or .staging directory")
	}

	legacy := seedLegacyHC(t, srv, s, "doomed-legacy", map[string]string{"config.yaml": hcCfgA})
	// Blobs a failed migration commit already wrote.
	putHCBlobs(t, srv, legacy, map[string]string{"config.yaml": hcCfgA})
	base := srv.harnessConfigContentBase(legacy)
	mustStatus(t, doTemplateRequest(t, srv, http.MethodDelete, "/api/v1/harness-configs/"+legacy.ID+"?deleteFiles=true", "", nil), http.StatusNoContent)
	if !notExistOnDisk(t, stor, legacy.StoragePath) || !notExistOnDisk(t, stor, base+templateBlobsSuffix) {
		t.Error("deleting a legacy row left its legacy tree or its migration blobs")
	}
}

// Deleting a legacy harness-config with deleteFiles keeps a legacy tree
// another harness-config still uses: a nested legacy clone at
// <slug path>/<clone id>, or a newer row that shares the renamed parent's
// path. The same guard decides as on migration
// (legacyHarnessConfigTreeInUse). An unshared legacy tree is removed.
func TestHarnessConfigBlob_DeleteKeepsSharedOrNestedLegacyTree(t *testing.T) {
	ctx := context.Background()
	stor := newCommitTestStorage(t)
	srv, s := newCommitTestServer(t, stor)

	parent := seedLegacyHC(t, srv, s, "foo", map[string]string{"config.yaml": hcCfgA})
	parent.Name, parent.Slug = "bar", "bar"
	require.NoError(t, s.UpdateHarnessConfig(ctx, parent))
	cloneFiles := map[string]string{"config.yaml": hcCfgB}
	clone := &store.HarnessConfig{
		ID: api.NewUUID(), Name: "foo-clone", Slug: "foo-clone", Harness: "claude",
		Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
		Files: commitManifest(cloneFiles),
	}
	clone.StoragePath = parent.StoragePath + "/" + clone.ID
	clone.ContentHash = computeContentHash(clone.Files)
	putObjects(t, stor, clone.StoragePath, cloneFiles)
	require.NoError(t, s.CreateHarnessConfig(ctx, clone))

	// The parent's tree holds a nested legacy clone: deleting the parent
	// keeps the tree.
	mustStatus(t, doTemplateRequest(t, srv, http.MethodDelete, "/api/v1/harness-configs/"+parent.ID+"?deleteFiles=true", "", nil), http.StatusNoContent)
	if !objectExists(t, stor, clone.StoragePath+"/config.yaml") {
		t.Fatal("deleting a legacy parent removed a nested legacy clone's files")
	}
	for _, f := range downloadHC(t, srv, clone.ID).Files {
		if string(fetchHubURL(t, srv, f.URL)) != cloneFiles[f.Path] {
			t.Errorf("nested clone's %s no longer hydrates after its parent was deleted", f.Path)
		}
	}

	// Two rows on one legacy path (a rename, then a new row with the old
	// name): deleting one keeps the tree the other reads.
	a := seedLegacyHC(t, srv, s, "shared", map[string]string{"a.txt": "from A", "config.yaml": hcCfgA})
	a.Name, a.Slug = "shared-renamed", "shared-renamed"
	require.NoError(t, s.UpdateHarnessConfig(ctx, a))
	b := seedLegacyHC(t, srv, s, "shared", map[string]string{"b.txt": "from B", "config.yaml": hcCfgB})
	require.Equal(t, a.StoragePath, b.StoragePath, "precondition: the rows share the legacy path")
	mustStatus(t, doTemplateRequest(t, srv, http.MethodDelete, "/api/v1/harness-configs/"+a.ID+"?deleteFiles=true", "", nil), http.StatusNoContent)
	if !objectExists(t, stor, b.StoragePath+"/b.txt") {
		t.Fatal("deleting one row removed the legacy tree another row shares")
	}

	// Now nothing else uses the path: deleting the last row removes it.
	mustStatus(t, doTemplateRequest(t, srv, http.MethodDelete, "/api/v1/harness-configs/"+b.ID+"?deleteFiles=true", "", nil), http.StatusNoContent)
	if !notExistOnDisk(t, stor, b.StoragePath) {
		t.Error("deleting the last row on a legacy path kept its tree")
	}
}
