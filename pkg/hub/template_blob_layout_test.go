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
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// Tests for the content-addressed template storage layout
// (ptone/scion#4221 part 2).

func downloadTemplate(t *testing.T, srv *Server, id string) DownloadResponse {
	t.Helper()
	w := doTemplateRequest(t, srv, http.MethodGet, "/api/v1/templates/"+id+"/download", "", nil)
	mustStatus(t, w, http.StatusOK)
	var resp DownloadResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// fetchHubURL GETs a hub URL (a local-storage download URL) through the
// server's handler.
func fetchHubURL(t *testing.T, srv *Server, rawURL string) []byte {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	w := doTemplateRequest(t, srv, http.MethodGet, u.RequestURI(), "", nil)
	mustStatus(t, w, http.StatusOK)
	return w.Body.Bytes()
}

// fetchMockSignedURL reads the object a mockStorage signed URL names.
func fetchMockSignedURL(t *testing.T, stor storage.Storage, bucket, rawURL string) ([]byte, error) {
	t.Helper()
	obj := strings.TrimPrefix(rawURL, "https://storage.example.com/"+bucket+"/")
	obj = strings.SplitN(obj, "?", 2)[0]
	rc, _, err := stor.Download(context.Background(), obj)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

func readStored(t *testing.T, stor storage.Storage, objectPath string) []byte {
	t.Helper()
	data, err := readTemplateObject(context.Background(), stor, objectPath, maxUploadFileSize)
	if err != nil {
		t.Fatalf("read %s: %v", objectPath, err)
	}
	return data
}

// ageObject sets a local-storage object's modification time to age ago.
func ageObject(t *testing.T, stor storage.Storage, objectPath string, age time.Duration) {
	t.Helper()
	local, ok := stor.(*storage.LocalStorage)
	if !ok {
		t.Fatalf("ageObject needs local storage, got %T", stor)
	}
	ts := time.Now().Add(-age)
	if err := os.Chtimes(local.ObjectFSPath(objectPath), ts, ts); err != nil {
		t.Fatal(err)
	}
}

func writeTemplateFileJSON(t *testing.T, srv *Server, id, path, content string) {
	t.Helper()
	body, _ := json.Marshal(TemplateFileWriteRequest{Content: content})
	mustStatus(t, doTemplateRequest(t, srv, http.MethodPut, "/api/v1/templates/"+id+"/files/"+path, "application/json", body), http.StatusOK)
}

func getTemplate(t *testing.T, s store.Store, id string) *store.Template {
	t.Helper()
	got, err := s.GetTemplate(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func notExistOnDisk(t *testing.T, stor storage.Storage, objectPath string) bool {
	t.Helper()
	_, err := os.Stat(stor.(*storage.LocalStorage).ObjectFSPath(objectPath))
	return os.IsNotExist(err)
}

// Acceptance 1: a hydration that runs while a push lands gets one complete
// version (here the old one) on a GCS-style backend and on local storage,
// and the download response names that version's content hash, which the
// broker uses as its cache key. Mutant (c), signing path URLs for blob rows,
// fails both subtests.
func TestTemplateBlob_HydrationDuringPushSeesOneVersion(t *testing.T) {
	ctx := context.Background()
	v1 := map[string]string{"scion-agent.yaml": commitCfgOld, "a.md": "a, version one", "b.md": "b, version one"}
	v2 := map[string]string{"scion-agent.yaml": commitCfgBoth, "a.md": "a, version two", "b.md": "b, version two"}

	check := func(t *testing.T, resp DownloadResponse, tmpl *store.Template, got map[string][]byte) {
		t.Helper()
		if resp.ContentHash != tmpl.ContentHash {
			t.Errorf("download contentHash = %q, want the signed version's %q", resp.ContentHash, tmpl.ContentHash)
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
		tmpl := seedCommittedTemplate(t, srv, "hydrate-gcs", store.TemplateScopeGlobal, "", v1)

		resp := downloadTemplate(t, srv, tmpl.ID)
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
		stageObjects(t, srv, tmpl, v2)
		mustStatus(t, finalizeTemplate(t, srv, tmpl.ID, commitManifest(v2)), http.StatusOK)
		if pushed := getTemplate(t, s, tmpl.ID); pushed.ContentHash == tmpl.ContentHash {
			t.Fatal("precondition: the push did not commit")
		}

		for _, f := range resp.Files[1:] {
			data, err := fetchMockSignedURL(t, gcs, "gcs-bucket", f.URL)
			if err != nil {
				t.Fatalf("fetch %s after the push: %v", f.Path, err)
			}
			got[f.Path] = data
		}
		check(t, resp, tmpl, got)
	})

	t.Run("local", func(t *testing.T) {
		stor := newCommitTestStorage(t)
		srv, s := newCommitTestServer(t, stor)
		tmpl := seedCommittedTemplate(t, srv, "hydrate-local", store.TemplateScopeGlobal, "", v1)

		resp := downloadTemplate(t, srv, tmpl.ID)
		got := make(map[string][]byte)
		first := resp.Files[0]
		got[first.Path] = fetchHubURL(t, srv, first.URL)

		// The push lands mid-hydration, one commit per file, as the file
		// APIs do.
		for p, c := range v2 {
			mustStatus(t, doTemplateRequest(t, srv, http.MethodPut, "/api/v1/templates/"+tmpl.ID+"/files/"+p, "application/octet-stream", []byte(c)), http.StatusOK)
		}
		if pushed := getTemplate(t, s, tmpl.ID); pushed.ContentHash == tmpl.ContentHash {
			t.Fatal("precondition: the push did not commit")
		}

		for _, f := range resp.Files[1:] {
			got[f.Path] = fetchHubURL(t, srv, f.URL)
		}
		check(t, resp, tmpl, got)

		// A hash that is not this template's blob is not found; a malformed
		// one is refused.
		w := doTemplateRequest(t, srv, http.MethodGet, "/api/v1/templates/"+tmpl.ID+"/files/a.md?raw=1&hash="+url.QueryEscape(commitHash("never stored")), "", nil)
		mustStatus(t, w, http.StatusNotFound)
		w = doTemplateRequest(t, srv, http.MethodGet, "/api/v1/templates/"+tmpl.ID+"/files/a.md?raw=1&hash=sha256:..%2F..%2Fx", "", nil)
		mustStatus(t, w, http.StatusBadRequest)
		_ = ctx
	})
}

// Acceptance 2 (old CLI) and the staging flow: upload URLs stage without
// committing, and a finalize with or without the uploadId hashes and moves
// the staged files into blobs. "Staging move skipped" fails this test.
func TestTemplateBlob_UploadStagesAndFinalizeCommits(t *testing.T) {
	ctx := context.Background()
	stor := newCommitTestStorage(t)
	srv, s := newCommitTestServer(t, stor)
	tmpl := seedCommittedTemplate(t, srv, "cli", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": commitCfgOld})

	stage := func(t *testing.T, files map[string]string) UploadResponse {
		t.Helper()
		var reqs []FileUploadRequest
		for p, c := range files {
			reqs = append(reqs, FileUploadRequest{Path: p, Size: int64(len(c))})
		}
		body, _ := json.Marshal(UploadRequest{Files: reqs})
		w := doTemplateRequest(t, srv, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/upload", "application/json", body)
		mustStatus(t, w, http.StatusOK)
		var resp UploadResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.UploadID == "" || len(resp.UploadURLs) != len(files) {
			t.Fatalf("upload response = %+v", resp)
		}
		for _, u := range resp.UploadURLs {
			if !strings.Contains(u.URL, "uploadId="+resp.UploadID) {
				t.Fatalf("local upload URL %q does not stage under the upload ID", u.URL)
			}
			parsed, err := url.Parse(u.URL)
			if err != nil {
				t.Fatal(err)
			}
			mustStatus(t, doTemplateRequest(t, srv, u.Method, parsed.RequestURI(), "application/octet-stream", []byte(files[u.Path])), http.StatusOK)
		}
		return resp
	}

	// Old CLI: uploads, then finalizes without an uploadId.
	push := map[string]string{"scion-agent.yaml": commitCfgOld, "new.md": "from an old cli", "dir/nested.md": "nested"}
	stage(t, map[string]string{"new.md": push["new.md"], "dir/nested.md": push["dir/nested.md"]})
	if got := getTemplate(t, s, tmpl.ID); got.ContentHash != tmpl.ContentHash {
		t.Fatal("staging an upload changed the row before finalize")
	}
	mustStatus(t, finalizeTemplate(t, srv, tmpl.ID, commitManifest(push)), http.StatusOK)
	got := getTemplate(t, s, tmpl.ID)
	if !reflect.DeepEqual(got.Files, commitManifest(push)) {
		t.Errorf("files = %+v, want %+v", got.Files, commitManifest(push))
	}
	for p, c := range push {
		if string(readStored(t, stor, blobObjectPath(got, c))) != c {
			t.Errorf("blob of %s missing or wrong after finalize", p)
		}
	}
	if listed, err := stor.List(ctx, storage.ListOptions{Prefix: templateStagingPrefix(got.StoragePath)}); err != nil || len(listed.Objects) != 0 {
		t.Errorf("staged uploads left after finalize: %+v %v", listed, err)
	}

	// New client: sends the uploadId back.
	push2 := map[string]string{"scion-agent.yaml": commitCfgOld, "new.md": "from a new cli", "dir/nested.md": "nested"}
	up := stage(t, map[string]string{"new.md": push2["new.md"]})
	body, _ := json.Marshal(FinalizeRequest{Manifest: &TemplateManifest{Version: "1.0", Files: commitManifest(push2)}, UploadID: up.UploadID})
	mustStatus(t, doTemplateRequest(t, srv, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/finalize", "application/json", body), http.StatusOK)
	got = getTemplate(t, s, tmpl.ID)
	if string(readStored(t, stor, blobObjectPath(got, push2["new.md"]))) != push2["new.md"] {
		t.Error("blob of the new client's upload missing after finalize")
	}

	// A file neither staged nor stored keeps the old CLI's retry text.
	missing := map[string]string{"scion-agent.yaml": commitCfgOld, "ghost.md": "never uploaded"}
	w := finalizeTemplate(t, srv, tmpl.ID, commitManifest(missing))
	mustStatus(t, w, http.StatusBadRequest)
	if !strings.Contains(w.Body.String(), "file not found") {
		t.Errorf("missing file: body %q lacks \"file not found\"", w.Body.String())
	}

	// An upload ID that cannot name a staging directory is refused.
	w = doTemplateRequest(t, srv, http.MethodPut, "/api/v1/templates/"+tmpl.ID+"/files/x.md?uploadId=..%2Fescape", "application/octet-stream", []byte("x"))
	mustStatus(t, w, http.StatusBadRequest)
}

// Acceptance 4: staged content that does not match the manifest hash is
// refused with 400, with or without an uploadId, and the row and blobs are
// unchanged.
func TestTemplateBlob_StagedHashMismatchRefused(t *testing.T) {
	stor := newCommitTestStorage(t)
	srv, s := newCommitTestServer(t, stor)
	tmpl := seedCommittedTemplate(t, srv, "mismatch", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": commitCfgOld})
	staged := templateStagedObjectPath(srv.templateContentBase(tmpl), "u1", "x.md")
	if _, err := stor.Upload(context.Background(), staged, strings.NewReader("tampered"), storage.UploadOptions{}); err != nil {
		t.Fatal(err)
	}
	manifest := commitManifest(map[string]string{"scion-agent.yaml": commitCfgOld, "x.md": "expected"})

	for _, uploadID := range []string{"u1", ""} {
		body, _ := json.Marshal(FinalizeRequest{Manifest: &TemplateManifest{Version: "1.0", Files: manifest}, UploadID: uploadID})
		w := doTemplateRequest(t, srv, http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/finalize", "application/json", body)
		mustStatus(t, w, http.StatusBadRequest)
		got := getTemplate(t, s, tmpl.ID)
		if got.ContentHash != tmpl.ContentHash || !reflect.DeepEqual(got.Files, tmpl.Files) {
			t.Errorf("uploadId %q: refused finalize changed the row", uploadID)
		}
		if objectExists(t, stor, blobObjectPath(tmpl, "tampered")) || objectExists(t, stor, blobObjectPath(tmpl, "expected")) {
			t.Errorf("uploadId %q: refused finalize wrote a blob", uploadID)
		}
	}
}

// Acceptance 5: the collector deletes only unreferenced blobs older than the
// grace period and stale staged uploads; referenced blobs and young blobs
// are kept. Mutant (b), ignoring the grace period, fails it.
func TestTemplateBlobGC_DeletesOnlyAgedUnreferencedBlobs(t *testing.T) {
	ctx := context.Background()
	stor := newCommitTestStorage(t)
	srv, s := newCommitTestServer(t, stor)
	tmpl := seedCommittedTemplate(t, srv, "gc", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": commitCfgOld, "a.md": "a1"})
	writeTemplateFileJSON(t, srv, tmpl.ID, "a.md", "a2")
	got := getTemplate(t, s, tmpl.ID)
	putBlobs(t, srv, got, map[string]string{"young.md": "young and unreferenced"})

	oldUnref := blobObjectPath(got, "a1")
	young := blobObjectPath(got, "young and unreferenced")
	referenced := blobObjectPath(got, "a2")
	cfg := blobObjectPath(got, commitCfgOld)
	for _, p := range []string{oldUnref, referenced, cfg} {
		ageObject(t, stor, p, 48*time.Hour)
	}
	staleStaged := templateStagedObjectPath(got.StoragePath, "old-upload", "x.md")
	freshStaged := templateStagedObjectPath(got.StoragePath, "new-upload", "x.md")
	for _, p := range []string{staleStaged, freshStaged} {
		if _, err := stor.Upload(ctx, p, strings.NewReader("staged"), storage.UploadOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	ageObject(t, stor, staleStaged, 48*time.Hour)

	report, err := srv.collectTemplateBlobGarbage(ctx, 24*time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
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
	if report.DeletedBlobs != 1 || report.DeletedStaged != 1 {
		t.Errorf("report = %+v, want 1 blob and 1 staged upload deleted", report)
	}

	// The row still hydrates completely.
	resp := downloadTemplate(t, srv, tmpl.ID)
	for _, f := range resp.Files {
		if transfer.HashBytes(fetchHubURL(t, srv, f.URL)) != f.Hash {
			t.Errorf("%s does not hydrate after collection", f.Path)
		}
	}

	// The maintenance operation runs the same pass and checks its grace.
	exec := &TemplateBlobGCExecutor{srv: srv}
	var log bytes.Buffer
	if err := exec.Run(ctx, &log, map[string]string{"grace": "1h"}); err != nil {
		t.Fatalf("executor: %v", err)
	}
	if err := exec.Run(ctx, &log, map[string]string{"grace": "soon"}); err == nil {
		t.Error("executor accepted an invalid grace")
	}
}

// Acceptance 5, F5b: a commit that re-references a blob that was present but
// unreferenced rewrites it, so a collection pass that listed it as old
// cannot delete it.
func TestTemplateBlobGC_ReReferencedBlobSurvives(t *testing.T) {
	ctx := context.Background()
	stor := newCommitTestStorage(t)
	srv, s := newCommitTestServer(t, stor)
	tmpl := seedCommittedTemplate(t, srv, "revert", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": commitCfgOld, "a.md": "a1"})
	writeTemplateFileJSON(t, srv, tmpl.ID, "a.md", "a2")
	revived := blobObjectPath(tmpl, "a1")
	ageObject(t, stor, revived, 48*time.Hour)
	// What a collection pass listed before the revert committed.
	listed := storage.Object{Name: revived, Updated: time.Now().Add(-48 * time.Hour)}

	// The revert re-references a1 without uploading it (it is stored).
	revert := map[string]string{"scion-agent.yaml": commitCfgOld, "a.md": "a1"}
	mustStatus(t, finalizeTemplate(t, srv, tmpl.ID, commitManifest(revert)), http.StatusOK)

	if srv.deleteAgedTemplateObject(ctx, stor, listed, 24*time.Hour, time.Now(), &templateBlobGCReport{}) {
		t.Fatal("the collector deleted a blob a commit had just refreshed")
	}
	if _, err := srv.collectTemplateBlobGarbage(ctx, 24*time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !objectExists(t, stor, revived) {
		t.Fatal("re-referenced blob was deleted")
	}
	got := getTemplate(t, s, tmpl.ID)
	if !reflect.DeepEqual(got.Files, commitManifest(revert)) {
		t.Errorf("files = %+v, want the reverted manifest", got.Files)
	}
}

// Acceptance 6: a file-API edit on a blob row checks only the blob it
// introduces, so an unrelated missing blob does not block it.
func TestTemplateBlob_EditNotBlockedByUnrelatedMissingBlob(t *testing.T) {
	stor := newCommitTestStorage(t)
	srv, s := newCommitTestServer(t, stor)
	tmpl := seedCommittedTemplate(t, srv, "drifted", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": commitCfgOld, "a.md": "a", "b.md": "b"})
	if err := stor.Delete(context.Background(), blobObjectPath(tmpl, "b")); err != nil {
		t.Fatal(err)
	}
	writeTemplateFileJSON(t, srv, tmpl.ID, "a.md", "a2")
	mustStatus(t, doTemplateRequest(t, srv, http.MethodDelete, "/api/v1/templates/"+tmpl.ID+"/files/scion-agent.yaml", "", nil), http.StatusNoContent)
	got := getTemplate(t, s, tmpl.ID)
	want := commitManifest(map[string]string{"a.md": "a2", "b.md": "b"})
	if len(got.Files) != 2 {
		t.Fatalf("files = %+v, want a.md and b.md", got.Files)
	}
	for i := range want {
		f, ok := templateFileByPath(got.Files, want[i].Path)
		if !ok || f.Hash != want[i].Hash {
			t.Errorf("%s: entry %+v, want hash %s", want[i].Path, f, want[i].Hash)
		}
	}
}

// countingStorage counts every storage call.
type countingStorage struct {
	storage.Storage
	calls atomic.Int64
}

func (c *countingStorage) GenerateSignedURL(ctx context.Context, p string, o storage.SignedURLOptions) (*storage.SignedURL, error) {
	c.calls.Add(1)
	return c.Storage.GenerateSignedURL(ctx, p, o)
}
func (c *countingStorage) Upload(ctx context.Context, p string, r io.Reader, o storage.UploadOptions) (*storage.Object, error) {
	c.calls.Add(1)
	return c.Storage.Upload(ctx, p, r, o)
}
func (c *countingStorage) Download(ctx context.Context, p string) (io.ReadCloser, *storage.Object, error) {
	c.calls.Add(1)
	return c.Storage.Download(ctx, p)
}
func (c *countingStorage) Delete(ctx context.Context, p string) error {
	c.calls.Add(1)
	return c.Storage.Delete(ctx, p)
}
func (c *countingStorage) DeletePrefix(ctx context.Context, p string) error {
	c.calls.Add(1)
	return c.Storage.DeletePrefix(ctx, p)
}
func (c *countingStorage) List(ctx context.Context, o storage.ListOptions) (*storage.ListResult, error) {
	c.calls.Add(1)
	return c.Storage.List(ctx, o)
}
func (c *countingStorage) Exists(ctx context.Context, p string) (bool, error) {
	c.calls.Add(1)
	return c.Storage.Exists(ctx, p)
}
func (c *countingStorage) GetObject(ctx context.Context, p string) (*storage.Object, error) {
	c.calls.Add(1)
	return c.Storage.GetObject(ctx, p)
}
func (c *countingStorage) Copy(ctx context.Context, src, dst string) (*storage.Object, error) {
	c.calls.Add(1)
	return c.Storage.Copy(ctx, src, dst)
}

// Acceptance 7: on a blob row a single-file write or delete makes the same
// small number of storage calls whether the template has 2 files or 200.
func TestTemplateBlob_SingleFileCommitCostIsConstant(t *testing.T) {
	measure := func(t *testing.T, n int) (write, del int64) {
		t.Helper()
		cs := &countingStorage{Storage: newCommitTestStorage(t)}
		srv, _ := newCommitTestServer(t, cs)
		files := map[string]string{"scion-agent.yaml": commitCfgBoth}
		for i := 0; i < n; i++ {
			files[fmt.Sprintf("f%03d.md", i)] = fmt.Sprintf("file %d", i)
		}
		tmpl := seedCommittedTemplate(t, srv, fmt.Sprintf("cost-%d", n), store.TemplateScopeGlobal, "", files)
		cs.calls.Store(0)
		writeTemplateFileJSON(t, srv, tmpl.ID, "README.md", "# readme")
		write = cs.calls.Load()
		cs.calls.Store(0)
		mustStatus(t, doTemplateRequest(t, srv, http.MethodDelete, "/api/v1/templates/"+tmpl.ID+"/files/f000.md", "", nil), http.StatusNoContent)
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

// Acceptance 8 and 3: a legacy row hydrates, repairs and clones unchanged
// until its first commit migrates it; the migration removes the legacy tree
// and leaves nothing at the new storage path itself, so a co-located broker's
// direct read misses.
func TestTemplateBlob_LegacyRowHydratesClonesAndMigrates(t *testing.T) {
	ctx := context.Background()
	stor := newCommitTestStorage(t)
	srv, s := newCommitTestServer(t, stor)
	files := map[string]string{"scion-agent.yaml": commitCfgOld, "a.md": "legacy a"}
	legacy := seedUncommittedTemplate(t, srv, s, "", "legacy", store.TemplateScopeGlobal, "", files)

	resp := downloadTemplate(t, srv, legacy.ID)
	for _, f := range resp.Files {
		if strings.Contains(f.URL, "hash=") {
			t.Errorf("legacy download URL %q names a blob", f.URL)
		}
		if got := fetchHubURL(t, srv, f.URL); string(got) != files[f.Path] {
			t.Errorf("legacy hydration of %s = %q", f.Path, got)
		}
	}

	if err := srv.syncTemplateFromStorage(ctx, TemplateRepairRef{ID: legacy.ID}); err != nil {
		t.Fatal(err)
	}
	if got := getTemplate(t, s, legacy.ID); got.Layout != "" || got.StoragePath != legacy.StoragePath {
		t.Fatalf("repair without drift changed the legacy row: layout %q path %q", got.Layout, got.StoragePath)
	}

	body, _ := json.Marshal(CloneTemplateRequest{Name: "legacy-clone", Scope: store.TemplateScopeGlobal})
	w := doTemplateRequest(t, srv, http.MethodPost, "/api/v1/templates/"+legacy.ID+"/clone", "application/json", body)
	mustStatus(t, w, http.StatusCreated)
	var clone store.Template
	if err := json.Unmarshal(w.Body.Bytes(), &clone); err != nil {
		t.Fatal(err)
	}
	if clone.Layout != store.TemplateLayoutBlobs {
		t.Errorf("clone layout = %q, want blobs", clone.Layout)
	}
	for p, c := range files {
		if string(readStored(t, stor, blobObjectPath(&clone, c))) != c {
			t.Errorf("clone blob of %s missing", p)
		}
	}
	if got := getTemplate(t, s, legacy.ID); got.Layout != "" {
		t.Fatal("cloning migrated the source")
	}

	// The first commit migrates the row.
	writeTemplateFileJSON(t, srv, legacy.ID, "b.md", "new b")
	got := getTemplate(t, s, legacy.ID)
	if got.Layout != store.TemplateLayoutBlobs || got.StoragePath != srv.templateBlobStoragePath(got) {
		t.Fatalf("after the first commit: layout %q path %q", got.Layout, got.StoragePath)
	}
	for _, f := range got.Files {
		if !objectExists(t, stor, templateObjectPath(got, f)) {
			t.Errorf("blob of %s missing after migration", f.Path)
		}
	}
	if !notExistOnDisk(t, stor, legacy.StoragePath) {
		t.Error("the unshared legacy tree was not removed")
	}
	if !notExistOnDisk(t, stor, got.StoragePath) {
		t.Error("something was written at the blob row's storage path itself; a co-located broker would read it")
	}
	resp = downloadTemplate(t, srv, legacy.ID)
	if resp.ContentHash != got.ContentHash {
		t.Errorf("download contentHash = %q, want %q", resp.ContentHash, got.ContentHash)
	}
	for _, f := range resp.Files {
		if !strings.Contains(f.URL, "hash=") {
			t.Errorf("blob download URL %q does not name its blob", f.URL)
		}
	}
}

// Acceptance 11: two rows that share a legacy path (a rename, then a new
// template with the old name) never read, delete or collect each other's
// objects after either one migrates.
func TestTemplateBlob_SharedLegacyPathIsolation(t *testing.T) {
	ctx := context.Background()
	stor := newCommitTestStorage(t)
	srv, s := newCommitTestServer(t, stor)

	a := seedUncommittedTemplate(t, srv, s, "", "foo", store.TemplateScopeGlobal, "", map[string]string{"x.md": "from A"})
	shared := a.StoragePath
	a.Name, a.Slug = "bar", "bar"
	if err := s.UpdateTemplate(ctx, a); err != nil {
		t.Fatal(err)
	}
	b := seedUncommittedTemplate(t, srv, s, "", "foo", store.TemplateScopeGlobal, "", map[string]string{"y.md": "from B"})
	if b.StoragePath != shared {
		t.Fatalf("precondition: rows do not share the legacy path (%q, %q)", b.StoragePath, shared)
	}

	// A migrates; B still reads the shared tree.
	writeTemplateFileJSON(t, srv, a.ID, "z.md", "A again")
	gotA := getTemplate(t, s, a.ID)
	if gotA.Layout != store.TemplateLayoutBlobs || gotA.StoragePath == shared {
		t.Fatalf("A did not migrate to its own path: %q", gotA.StoragePath)
	}
	if !objectExists(t, stor, shared+"/y.md") {
		t.Fatal("A's migration removed the legacy tree B still uses")
	}
	for _, f := range downloadTemplate(t, srv, b.ID).Files {
		if string(fetchHubURL(t, srv, f.URL)) != "from B" {
			t.Errorf("B reads %s wrongly after A migrated", f.Path)
		}
	}

	// Collection with no grace touches neither B's tree nor A's blobs.
	if _, err := srv.collectTemplateBlobGarbage(ctx, 0, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !objectExists(t, stor, shared+"/y.md") {
		t.Error("collection deleted B's legacy object")
	}
	for _, f := range gotA.Files {
		if !objectExists(t, stor, templateObjectPath(gotA, f)) {
			t.Errorf("collection deleted A's blob of %s", f.Path)
		}
	}

	// B migrates; now nothing shares the legacy path, so it is removed.
	writeTemplateFileJSON(t, srv, b.ID, "w.md", "B again")
	gotB := getTemplate(t, s, b.ID)
	if gotB.Layout != store.TemplateLayoutBlobs || gotB.StoragePath == gotA.StoragePath {
		t.Fatalf("B did not migrate to its own path: %q", gotB.StoragePath)
	}
	if !notExistOnDisk(t, stor, shared) {
		t.Error("the legacy tree was kept after the last row on it migrated")
	}
	for _, f := range downloadTemplate(t, srv, a.ID).Files {
		if transfer.HashBytes(fetchHubURL(t, srv, f.URL)) != f.Hash {
			t.Errorf("A's %s no longer hydrates after B migrated", f.Path)
		}
	}
	if _, ok := templateFileByPath(gotB.Files, "x.md"); ok {
		t.Error("B's manifest picked up A's file")
	}
}

// Acceptance 9: repair never drops entries from a blob row, even when a blob
// is missing, on the targeted and the startup repair paths.
func TestTemplateBlob_RepairNeverDropsEntries(t *testing.T) {
	ctx := context.Background()
	stor := newCommitTestStorage(t)
	srv, s := newCommitTestServer(t, stor)
	tmpl := seedCommittedTemplate(t, srv, "keep", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": commitCfgOld, "a.md": "a", "b.md": "b"})
	if err := stor.Delete(ctx, blobObjectPath(tmpl, "b")); err != nil {
		t.Fatal(err)
	}
	before := getTemplate(t, s, tmpl.ID)

	if err := srv.syncTemplateFromStorage(ctx, TemplateRepairRef{ID: tmpl.ID}); err != nil {
		t.Fatal(err)
	}
	srv.SyncAllTemplatesFromStorage(ctx)

	after := getTemplate(t, s, tmpl.ID)
	if !reflect.DeepEqual(after.Files, before.Files) || after.ContentHash != before.ContentHash || after.Layout != store.TemplateLayoutBlobs {
		t.Fatalf("repair changed a blob row: files %+v hash %q layout %q", after.Files, after.ContentHash, after.Layout)
	}
	report, err := srv.templateStore().ValidateStorage(ctx, templateToRecord(after))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Issues) != 1 || report.Issues[0].Kind != ValidationIssueMissingObject || report.Issues[0].File != "b.md" {
		t.Errorf("validation issues = %+v, want only b.md's missing blob", report.Issues)
	}
}

// Acceptance 10: a commit that read a legacy row loses against a concurrent
// migration that kept the content hash (the layout ABA case, F5a). Mutant
// (d), the CAS without Layout, fails it.
func TestCommitTemplateFiles_LayoutABAConflicts(t *testing.T) {
	ctx := context.Background()
	stor := newCommitTestStorage(t)
	srv, s := newCommitTestServer(t, stor)
	legacy := seedUncommittedTemplate(t, srv, s, "", "aba", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": commitCfgOld, "a.md": "a"})
	stale := getTemplate(t, s, legacy.ID)
	migrator := getTemplate(t, s, legacy.ID)

	if err := srv.commitTemplateFiles(ctx, migrator, migrator.Files, commitOpts{}); err != nil {
		t.Fatalf("migrating commit: %v", err)
	}
	if migrator.ContentHash != stale.ContentHash || migrator.Layout != store.TemplateLayoutBlobs {
		t.Fatalf("precondition: migration must keep the hash and move to blobs (hash %q -> %q, layout %q)", stale.ContentHash, migrator.ContentHash, migrator.Layout)
	}

	putBlobs(t, srv, stale, map[string]string{"b.md": "b"})
	err := srv.commitTemplateFiles(ctx, stale, upsertTemplateFile(stale.Files, commitManifest(map[string]string{"b.md": "b"})[0]), commitOpts{})
	if !errors.Is(err, store.ErrTemplateConflict) {
		t.Fatalf("commit from the legacy read: err = %v, want store.ErrTemplateConflict", err)
	}
	got := getTemplate(t, s, legacy.ID)
	if got.Layout != store.TemplateLayoutBlobs || !reflect.DeepEqual(got.Files, migrator.Files) {
		t.Errorf("row = layout %q files %+v, want the migration's", got.Layout, got.Files)
	}
}
