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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/templatecache"
	"github.com/stretchr/testify/require"
)

// Tests that a file's executable bit survives the hub resource path
// (ptone/scion#5652): a local directory is collected and uploaded the way
// `scion templates sync` / `scion harness-config sync` do it (hubclient
// CollectFiles + upload URLs + finalize), stored by the hub (local storage
// and a GCS-style backend), and hydrated by the broker's resolver into its
// content-addressed cache, the directory provisioning reads.

// mockStorageTransport serves mockStorage signed URLs
// (https://storage.example.com/<bucket>/<object>) from the mock, PUT and
// GET, and sends every other request to next.
type mockStorageTransport struct {
	stor   storage.Storage
	bucket string
	next   http.RoundTripper
}

func (m *mockStorageTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != "storage.example.com" {
		return m.next.RoundTrip(req)
	}
	obj := strings.TrimPrefix(req.URL.Path, "/"+m.bucket+"/")
	resp := func(code int, body []byte) *http.Response {
		return &http.Response{StatusCode: code, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{}, Request: req}
	}
	switch req.Method {
	case http.MethodPut:
		data, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		if _, err := m.stor.Upload(req.Context(), obj, bytes.NewReader(data), storage.UploadOptions{}); err != nil {
			return nil, err
		}
		return resp(http.StatusOK, nil), nil
	case http.MethodGet:
		rc, _, err := m.stor.Download(req.Context(), obj)
		if errors.Is(err, storage.ErrNotFound) {
			return resp(http.StatusNotFound, nil), nil
		}
		if err != nil {
			return nil, err
		}
		defer func() { _ = rc.Close() }()
		data, err := io.ReadAll(rc)
		if err != nil {
			return nil, err
		}
		return resp(http.StatusOK, data), nil
	}
	return resp(http.StatusMethodNotAllowed, nil), nil
}

// modeBackend is one hub storage backend a round trip runs against.
type modeBackend struct {
	name string
	new  func(t *testing.T) (storage.Storage, http.RoundTripper)
}

func modeBackends() []modeBackend {
	return []modeBackend{
		{"local", func(t *testing.T) (storage.Storage, http.RoundTripper) {
			return newCommitTestStorage(t), http.DefaultTransport
		}},
		{"gcs", func(t *testing.T) (storage.Storage, http.RoundTripper) {
			gcs := newMockStorage("gcs-bucket")
			gcs.provider = storage.ProviderGCS
			return gcs, &mockStorageTransport{stor: gcs, bucket: "gcs-bucket", next: http.DefaultTransport}
		}},
	}
}

// modeFile is a file of a source directory and the mode it is created with.
type modeFile struct {
	content string
	mode    os.FileMode
}

// writeModeDir writes files under a new directory with exactly their modes
// (setuid included) and returns it.
func writeModeDir(t *testing.T, files map[string]modeFile) string {
	t.Helper()
	dir := t.TempDir()
	for p, f := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(f.content), 0o600))
		require.NoError(t, os.Chmod(full, f.mode))
	}
	return dir
}

// startModeHub runs srv over HTTP and returns a client for it whose HTTP
// requests (signed URLs included) go through rt.
func startModeHub(t *testing.T, srv *Server, rt http.RoundTripper) hubclient.Client {
	t.Helper()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	c, err := hubclient.New(ts.URL, hubclient.WithBearerToken(testDevToken), hubclient.WithHTTPClient(&http.Client{Transport: rt}))
	require.NoError(t, err)
	return c
}

// modeUploadFiles PUTs each collected file to its upload URL.
func modeUploadFiles(t *testing.T, ctx context.Context, urls []hubclient.UploadURLInfo, files []hubclient.FileInfo, put func(ctx context.Context, url, method string, headers map[string]string, r io.Reader) error) {
	t.Helper()
	byPath := make(map[string]hubclient.FileInfo, len(files))
	for _, f := range files {
		byPath[f.Path] = f
	}
	require.Len(t, urls, len(files))
	for _, u := range urls {
		f, ok := byPath[u.Path]
		require.True(t, ok, "upload URL for unknown file %s", u.Path)
		data, err := os.ReadFile(f.FullPath)
		require.NoError(t, err)
		require.NoError(t, put(ctx, u.URL, u.Method, u.Headers, bytes.NewReader(data)), "upload %s", u.Path)
	}
}

func uploadRequests(files []hubclient.FileInfo) []hubclient.FileUploadRequest {
	reqs := make([]hubclient.FileUploadRequest, len(files))
	for i, f := range files {
		reqs[i] = hubclient.FileUploadRequest{Path: f.Path, Size: f.Size}
	}
	return reqs
}

func modeManifestFiles(files []hubclient.FileInfo) []hubclient.TemplateFile {
	out := make([]hubclient.TemplateFile, len(files))
	for i, f := range files {
		out[i] = hubclient.TemplateFile{Path: f.Path, Size: f.Size, Hash: f.Hash, Mode: f.Mode}
	}
	return out
}

// assertModes checks the permission bits (and the absence of setuid,
// setgid and sticky) of each file under dir.
func assertModes(t *testing.T, dir string, want map[string]os.FileMode) {
	t.Helper()
	for p, m := range want {
		info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p)))
		require.NoError(t, err, p)
		if got := info.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky); got != m {
			t.Errorf("%s: mode = %v, want %v", p, got, m)
		}
	}
}

// TestTemplateFileMode_RoundTripsThroughHub uploads a template with an
// executable plugin command, a regular file and a setuid executable, and
// checks the broker's hydrated copy: 0755, 0644, 0755 without setuid.
func TestTemplateFileMode_RoundTripsThroughHub(t *testing.T) {
	src := map[string]modeFile{
		"scion-agent.yaml":                 {commitCfgOld, 0o644},
		"plugins/demo/bin/server":          {"#!/bin/sh\necho mcp\n", 0o755},
		"plugins/demo/README.md":           {"docs\n", 0o644},
		"plugins/demo/bin/suid":            {"#!/bin/sh\necho suid\n", 0o4755},
		"plugins/demo/bin/owner-exec-only": {"#!/bin/sh\n", 0o700},
	}
	want := map[string]os.FileMode{
		"scion-agent.yaml":                 0o644,
		"plugins/demo/bin/server":          0o755,
		"plugins/demo/README.md":           0o644,
		"plugins/demo/bin/suid":            0o755,
		"plugins/demo/bin/owner-exec-only": 0o755,
	}
	for _, b := range modeBackends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			stor, rt := b.new(t)
			srv, s := newCommitTestServer(t, stor)
			client := startModeHub(t, srv, rt)

			// Upload, as `scion templates sync` does.
			files, err := hubclient.CollectFiles(writeModeDir(t, src), nil)
			require.NoError(t, err)
			created, err := client.Templates().Create(ctx, &hubclient.CreateTemplateRequest{Name: "mode-" + b.name, Scope: store.TemplateScopeGlobal})
			require.NoError(t, err)
			id := created.Template.ID
			up, err := client.Templates().RequestUploadURLs(ctx, id, uploadRequests(files))
			require.NoError(t, err)
			modeUploadFiles(t, ctx, up.UploadURLs, files, client.Templates().UploadFile)
			_, err = client.Templates().Finalize(ctx, id, &hubclient.TemplateManifest{Version: "1.0", Files: modeManifestFiles(files)}, created.Template.ContentHash, up.UploadID)
			require.NoError(t, err)

			// Stored: the manifest records the mode.
			stored := getTemplate(t, s, id)
			modes := make(map[string]string)
			for _, f := range stored.Files {
				modes[f.Path] = f.Mode
			}
			require.Equal(t, "0755", modes["plugins/demo/bin/server"])

			// Download response: normalised modes.
			dl, err := client.Templates().RequestDownloadURLs(ctx, id)
			require.NoError(t, err)
			for _, f := range dl.Files {
				if wantMode := want[f.Path]; f.Mode != map[os.FileMode]string{0o755: "0755", 0o644: "0644"}[wantMode] {
					t.Errorf("download %s: mode = %q, want %o", f.Path, f.Mode, wantMode)
				}
			}

			// Hydrated by the broker.
			cache, err := templatecache.New(t.TempDir(), templatecache.DefaultMaxSize)
			require.NoError(t, err)
			path, err := templatecache.NewHydrator(cache, client).Hydrate(ctx, id)
			require.NoError(t, err)
			assertModes(t, path, want)

			// A second hydration is a cache hit with the same modes.
			again, err := templatecache.NewHydrator(cache, client).Hydrate(ctx, id)
			require.NoError(t, err)
			require.Equal(t, path, again)

			// Copied on into an agent's directory.
			dest := filepath.Join(t.TempDir(), "agent-template")
			require.NoError(t, cache.CopyToDir(path, dest))
			assertModes(t, dest, want)
		})
	}
}

// TestTemplateFileMode_ChmodOnlyCommitRehydrates pins that a commit that
// only sets an executable bit (same content, so the same content hash) is
// not served from the cache entry of the earlier version.
func TestTemplateFileMode_ChmodOnlyCommitRehydrates(t *testing.T) {
	ctx := context.Background()
	stor, rt := modeBackends()[0].new(t)
	srv, s := newCommitTestServer(t, stor)
	client := startModeHub(t, srv, rt)
	cache, err := templatecache.New(t.TempDir(), templatecache.DefaultMaxSize)
	require.NoError(t, err)

	src := map[string]modeFile{"scion-agent.yaml": {commitCfgOld, 0o644}, "bin/tool": {"#!/bin/sh\n", 0o644}}
	dir := writeModeDir(t, src)
	files, err := hubclient.CollectFiles(dir, nil)
	require.NoError(t, err)
	created, err := client.Templates().Create(ctx, &hubclient.CreateTemplateRequest{Name: "chmod-only", Scope: store.TemplateScopeGlobal})
	require.NoError(t, err)
	id := created.Template.ID
	up, err := client.Templates().RequestUploadURLs(ctx, id, uploadRequests(files))
	require.NoError(t, err)
	modeUploadFiles(t, ctx, up.UploadURLs, files, client.Templates().UploadFile)
	v1, err := client.Templates().Finalize(ctx, id, &hubclient.TemplateManifest{Version: "1.0", Files: modeManifestFiles(files)}, created.Template.ContentHash, up.UploadID)
	require.NoError(t, err)

	first, err := templatecache.NewHydrator(cache, client).Hydrate(ctx, id)
	require.NoError(t, err)
	assertModes(t, first, map[string]os.FileMode{"bin/tool": 0o644})

	// chmod +x and finalize again; nothing needs uploading.
	require.NoError(t, os.Chmod(filepath.Join(dir, "bin", "tool"), 0o755))
	files, err = hubclient.CollectFiles(dir, nil)
	require.NoError(t, err)
	v2, err := client.Templates().Finalize(ctx, id, &hubclient.TemplateManifest{Version: "1.0", Files: modeManifestFiles(files)}, v1.ContentHash, "")
	require.NoError(t, err)
	require.Equal(t, v1.ContentHash, v2.ContentHash, "precondition: a mode-only commit keeps the content hash")
	// The commit is neither refused by the content-hash precondition nor
	// skipped as unchanged: the row records the new mode.
	for _, f := range getTemplate(t, s, id).Files {
		if f.Path == "bin/tool" {
			require.Equal(t, "0755", f.Mode, "mode-only finalize was not persisted")
		}
	}

	second, err := templatecache.NewHydrator(cache, client).Hydrate(ctx, id)
	require.NoError(t, err)
	require.NotEqual(t, first, second, "the 0644 cache entry was reused")
	assertModes(t, second, map[string]os.FileMode{"bin/tool": 0o755})
}

// TestHarnessConfigFileMode_ProvisionScriptRoundTrips uploads a
// harness-config bundle with an executable provision script (as the pi
// bundle of ptone/scion#5541 has) and checks the broker's hydrated copy.
func TestHarnessConfigFileMode_ProvisionScriptRoundTrips(t *testing.T) {
	src := map[string]modeFile{
		"config.yaml":           {hcCfgA, 0o644},
		"provision.py":          {"#!/usr/bin/env python3\nprint('ok')\n", 0o755},
		"home/.config/settings": {"{}\n", 0o644},
	}
	want := map[string]os.FileMode{"config.yaml": 0o644, "provision.py": 0o755, "home/.config/settings": 0o644}
	for _, b := range modeBackends() {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			stor, rt := b.new(t)
			srv, _ := newCommitTestServer(t, stor)
			client := startModeHub(t, srv, rt)

			// Upload, as `scion harness-config sync` does.
			files, err := hubclient.CollectFiles(writeModeDir(t, src), nil)
			require.NoError(t, err)
			created, err := client.HarnessConfigs().Create(ctx, &hubclient.CreateHarnessConfigRequest{Name: "mode-hc-" + b.name, Harness: "claude", Scope: store.HarnessConfigScopeGlobal})
			require.NoError(t, err)
			id := created.HarnessConfig.ID
			up, err := client.HarnessConfigs().RequestUploadURLs(ctx, id, uploadRequests(files))
			require.NoError(t, err)
			modeUploadFiles(t, ctx, up.UploadURLs, files, client.HarnessConfigs().UploadFile)
			_, err = client.HarnessConfigs().Finalize(ctx, id, &hubclient.HarnessConfigManifest{Version: "1.0", Files: modeManifestFiles(files)}, "", up.UploadID)
			require.NoError(t, err)

			cache, err := templatecache.New(t.TempDir(), templatecache.DefaultMaxSize)
			require.NoError(t, err)
			path, err := templatecache.NewHarnessConfigResolver(cache, client).Resolve(ctx, id)
			require.NoError(t, err)
			assertModes(t, path, want)
		})
	}
}

// TestHarnessConfigFileWrite_KeepsExecutableBit pins that editing an
// executable harness-config file through the file API keeps its mode
// rather than resetting it to 0644.
func TestHarnessConfigFileWrite_KeepsExecutableBit(t *testing.T) {
	srv, s := newCommitTestServer(t, newCommitTestStorage(t))
	hc := seedBlobHC(t, srv, "keep-exec", map[string]string{"config.yaml": hcCfgA, "provision.py": "v1"})
	got := getHC(t, s, hc.ID)
	for i := range got.Files {
		if got.Files[i].Path == "provision.py" {
			got.Files[i].Mode = "0755"
		}
	}
	require.NoError(t, setHarnessConfigContentForTest(context.Background(), s, got))

	writeHCFile(t, srv, hc.ID, "provision.py", "v2")
	writeHCFile(t, srv, hc.ID, "new.sh", "new")
	after := getHC(t, s, hc.ID)
	modes := make(map[string]string)
	for _, f := range after.Files {
		modes[f.Path] = f.Mode
	}
	require.Equal(t, "0755", modes["provision.py"], "edit dropped the executable bit")
	require.NotEqual(t, "0755", modes["new.sh"])

	dl := downloadHC(t, srv, hc.ID)
	for _, f := range dl.Files {
		switch f.Path {
		case "provision.py":
			require.Equal(t, "0755", f.Mode)
		case "new.sh", "config.yaml":
			require.Equal(t, "0644", f.Mode)
		}
	}
}

// TestDownloadMode_LegacyEntryWithoutModeIs0644 pins that a manifest entry
// recorded before modes were carried (no Mode) downloads as 0644.
func TestDownloadMode_LegacyEntryWithoutModeIs0644(t *testing.T) {
	srv, s := newCommitTestServer(t, newCommitTestStorage(t))
	tmpl := seedCommittedTemplate(t, srv, "legacy-mode", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": commitCfgOld, "bin/old": "x"})
	got := getTemplate(t, s, tmpl.ID)
	for i := range got.Files {
		got.Files[i].Mode = ""
	}
	require.NoError(t, s.UpdateTemplateContent(context.Background(), got, store.TemplateContentPrecondition{ContentHash: got.ContentHash, Layout: got.Layout}))

	for _, f := range downloadTemplate(t, srv, tmpl.ID).Files {
		require.Equal(t, "0644", f.Mode, f.Path)
	}
}

// TestDownloadMode_RowStoredBeforeFixServesItsMode pins that a row written
// before ptone/scion#5652 needs no re-sync: CLIs already sent each file's
// mode in the finalize manifest and the hub stored it, so once the download
// response carries modes such a row serves its executable bit as is.
func TestDownloadMode_RowStoredBeforeFixServesItsMode(t *testing.T) {
	srv, s := newCommitTestServer(t, newCommitTestStorage(t))
	tmpl := seedCommittedTemplate(t, srv, "pre-fix-row", store.TemplateScopeGlobal, "", map[string]string{"scion-agent.yaml": commitCfgOld, "plugins/p/bin/cmd": "#!/bin/sh\n"})
	// The row as an old CLI's finalize left it: the manifest mode the CLI
	// collected ("0755", or e.g. "0775" under another umask).
	got := getTemplate(t, s, tmpl.ID)
	for i := range got.Files {
		if got.Files[i].Path == "plugins/p/bin/cmd" {
			got.Files[i].Mode = "0775"
		}
	}
	require.NoError(t, s.UpdateTemplateContent(context.Background(), got, store.TemplateContentPrecondition{ContentHash: got.ContentHash, Layout: got.Layout}))

	for _, f := range downloadTemplate(t, srv, tmpl.ID).Files {
		want := "0644"
		if f.Path == "plugins/p/bin/cmd" {
			want = "0755"
		}
		require.Equal(t, want, f.Mode, f.Path)
	}
}

func TestNewFileMode(t *testing.T) {
	files := []store.TemplateFile{{Path: "exec", Mode: "04755"}, {Path: "plain", Mode: "0600"}, {Path: "legacy"}}
	for p, want := range map[string]string{"exec": "0755", "plain": "0644", "legacy": "0644", "new": "0644"} {
		if got := newFileMode(files, p); got != want {
			t.Errorf("newFileMode(%q) = %q, want %q", p, got, want)
		}
	}
}

// TestHarnessConfigFileMode_ChmodOnlyCommitRehydrates is the harness-config
// variant of TestTemplateFileMode_ChmodOnlyCommitRehydrates: a finalize
// that only sets an executable bit (same content and content hash, no
// upload) is persisted by commitHarnessConfigFiles and not served from
// the earlier version's cache entry.
func TestHarnessConfigFileMode_ChmodOnlyCommitRehydrates(t *testing.T) {
	ctx := context.Background()
	stor, rt := modeBackends()[0].new(t)
	srv, s := newCommitTestServer(t, stor)
	client := startModeHub(t, srv, rt)
	cache, err := templatecache.New(t.TempDir(), templatecache.DefaultMaxSize)
	require.NoError(t, err)

	dir := writeModeDir(t, map[string]modeFile{"config.yaml": {hcCfgA, 0o644}, "provision.py": {"#!/usr/bin/env python3\n", 0o644}})
	files, err := hubclient.CollectFiles(dir, nil)
	require.NoError(t, err)
	created, err := client.HarnessConfigs().Create(ctx, &hubclient.CreateHarnessConfigRequest{Name: "hc-chmod-only", Harness: "claude", Scope: store.HarnessConfigScopeGlobal})
	require.NoError(t, err)
	id := created.HarnessConfig.ID
	up, err := client.HarnessConfigs().RequestUploadURLs(ctx, id, uploadRequests(files))
	require.NoError(t, err)
	modeUploadFiles(t, ctx, up.UploadURLs, files, client.HarnessConfigs().UploadFile)
	v1, err := client.HarnessConfigs().Finalize(ctx, id, &hubclient.HarnessConfigManifest{Version: "1.0", Files: modeManifestFiles(files)}, "", up.UploadID)
	require.NoError(t, err)

	resolver := templatecache.NewHarnessConfigResolver(cache, client)
	first, err := resolver.Resolve(ctx, id)
	require.NoError(t, err)
	assertModes(t, first, map[string]os.FileMode{"provision.py": 0o644})

	// chmod +x and finalize again with no upload, as harness-config sync does.
	require.NoError(t, os.Chmod(filepath.Join(dir, "provision.py"), 0o755))
	files, err = hubclient.CollectFiles(dir, nil)
	require.NoError(t, err)
	v2, err := client.HarnessConfigs().Finalize(ctx, id, &hubclient.HarnessConfigManifest{Version: "1.0", Files: modeManifestFiles(files)}, "", "")
	require.NoError(t, err)
	require.Equal(t, v1.ContentHash, v2.ContentHash, "precondition: a mode-only commit keeps the content hash")
	for _, f := range getHC(t, s, id).Files {
		if f.Path == "provision.py" {
			require.Equal(t, "0755", f.Mode, "mode-only finalize was not persisted")
		}
	}

	second, err := resolver.Resolve(ctx, id)
	require.NoError(t, err)
	require.NotEqual(t, first, second, "the 0644 cache entry was reused")
	assertModes(t, second, map[string]os.FileMode{"provision.py": 0o755})
}
