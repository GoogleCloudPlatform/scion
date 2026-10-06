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
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// localStorageHarnessConfigWithContent sets up a server backed by real local
// storage and a harness-config whose files (path -> content) are uploaded
// below its storage path, with matching sizes in the record.
func localStorageHarnessConfigWithContent(t *testing.T, files map[string][]byte) (*Server, *store.HarnessConfig) {
	t.Helper()
	srv, s, _ := testHarnessConfigFileServer(t)
	stor, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	srv.SetStorage(stor)

	hc := &store.HarnessConfig{
		ID:            tid("hc-remote-download"),
		Name:          "remote-hc",
		Slug:          "remote-hc",
		Harness:       "claude",
		Scope:         store.HarnessConfigScopeGlobal,
		Status:        store.HarnessConfigStatusActive,
		StoragePath:   "harness-configs/global/remote-hc",
		StorageBucket: "b",
	}
	for p, c := range files {
		if _, err := stor.Upload(context.Background(), hc.StoragePath+"/"+p, bytes.NewReader(c), storage.UploadOptions{}); err != nil {
			t.Fatalf("upload %s: %v", p, err)
		}
		hc.Files = append(hc.Files, store.TemplateFile{Path: p, Size: int64(len(c)), Hash: "sha256:placeholder"})
	}
	hc.ContentHash = computeContentHash(hc.Files)
	if err := s.CreateHarnessConfig(context.Background(), hc); err != nil {
		t.Fatalf("CreateHarnessConfig: %v", err)
	}
	return srv, hc
}

// TestHarnessConfigDownload_LocalStorageReturnsHubServedURLs is the
// regression test for ptone/scion#3530. A hub that stores harness configs
// locally used to hand out file:// download URLs naming hub-host paths, which
// a remote broker cannot read. Like the template and skill download handlers,
// the harness-config download handler must rewrite them to hub-served URLs,
// and following those URLs must return each file's exact bytes — including a
// file larger than the inline-view limit.
func TestHarnessConfigDownload_LocalStorageReturnsHubServedURLs(t *testing.T) {
	large := bytes.Repeat([]byte("0123456789abcdef"), (maxHarnessConfigFileSize/16)+64)
	files := map[string][]byte{
		"config.yaml":        []byte("harness: claude\nimage: example/claude:latest\n"),
		"home/.claude.json":  []byte(`{"theme":"dark"}`),
		"home/bin/large.bin": large,
	}
	srv, hc := localStorageHarnessConfigWithContent(t, files)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/harness-configs/"+hc.ID+"/download", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("download: status %d: %s", rec.Code, rec.Body.String())
	}
	var dl DownloadResponse
	if err := json.NewDecoder(rec.Body).Decode(&dl); err != nil {
		t.Fatalf("decode download response: %v", err)
	}
	if len(dl.Files) != len(files) {
		t.Fatalf("got %d download URLs, want %d", len(dl.Files), len(files))
	}

	for _, f := range dl.Files {
		if strings.HasPrefix(f.URL, "file://") {
			t.Errorf("%s: download URL is a hub-host file:// path: %s", f.Path, f.URL)
			continue
		}
		u, err := url.Parse(f.URL)
		if err != nil {
			t.Fatalf("%s: parse URL %q: %v", f.Path, f.URL, err)
		}
		if u.Scheme != "http" || u.Host == "" {
			t.Errorf("%s: want an absolute hub URL, got %q", f.Path, f.URL)
		}
		wantPath := "/api/v1/harness-configs/" + hc.ID + "/files/" + f.Path
		if u.Path != wantPath || u.Query().Get("raw") != "1" {
			t.Errorf("%s: URL = %q, want path %q with raw=1", f.Path, f.URL, wantPath)
		}

		got := doRequest(t, srv, http.MethodGet, u.RequestURI(), nil)
		if got.Code != http.StatusOK {
			t.Errorf("%s: following download URL: status %d: %s", f.Path, got.Code, truncateForLog(got.Body.String()))
			continue
		}
		if !bytes.Equal(got.Body.Bytes(), files[f.Path]) {
			t.Errorf("%s: downloaded %d bytes that do not match the stored %d bytes", f.Path, got.Body.Len(), len(files[f.Path]))
		}
	}
}

// TestHarnessConfigFileRead_RawStreamsLargeFile pins that raw mode streams a
// file above the inline-view limit as-is, while JSON mode still refuses it.
func TestHarnessConfigFileRead_RawStreamsLargeFile(t *testing.T) {
	large := bytes.Repeat([]byte{0x00, 0xff, 'x', '\n'}, (maxHarnessConfigFileSize/4)+1024)
	srv, hc := localStorageHarnessConfigWithContent(t, map[string][]byte{"big.bin": large})
	base := "/api/v1/harness-configs/" + hc.ID + "/files/big.bin"

	rec := doRequest(t, srv, http.MethodGet, base+"?raw=1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("raw read: status %d: %s", rec.Code, truncateForLog(rec.Body.String()))
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", ct)
	}
	if !bytes.Equal(rec.Body.Bytes(), large) {
		t.Errorf("raw body (%d bytes) does not match stored file (%d bytes)", rec.Body.Len(), len(large))
	}

	rec = doRequest(t, srv, http.MethodGet, base, nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("JSON read of a large file: status %d, want 413", rec.Code)
	}
}

// TestHarnessConfigFileRead_RawAndJSONRejectSamePaths pins that raw mode
// resolves exactly the files JSON mode does: the raw branch runs after the
// shared path validation and record lookup, so a traversal, absolute or
// unknown path is rejected identically in both modes, even when the bad path
// is in the config's file list and something exists on disk at that path.
func TestHarnessConfigFileRead_RawAndJSONRejectSamePaths(t *testing.T) {
	srv, _, hc, root := localStorageHarnessConfig(t,
		[]string{"config.yaml", outsidePath, "/etc/hostname", `home\evil.txt`},
		[]string{"config.yaml"})
	writeOutsideFile(t, root)

	base := "/api/v1/harness-configs/" + hc.ID + "/files/"
	for _, p := range []string{
		url.PathEscape(outsidePath),
		"%2Fetc%2Fhostname",
		"home%5Cevil.txt",
		"unknown.txt",
		"home/missing.json",
	} {
		t.Run(p, func(t *testing.T) {
			jsonRec := doRequest(t, srv, http.MethodGet, base+p, nil)
			rawRec := doRequest(t, srv, http.MethodGet, base+p+"?raw=1", nil)
			if jsonRec.Code == http.StatusOK || rawRec.Code == http.StatusOK {
				t.Fatalf("expected rejection, got json=%d raw=%d", jsonRec.Code, rawRec.Code)
			}
			if jsonRec.Code != rawRec.Code {
				t.Errorf("raw and JSON modes disagree: json=%d raw=%d", jsonRec.Code, rawRec.Code)
			}
			if strings.Contains(rawRec.Body.String(), "keep") {
				t.Errorf("raw mode leaked content outside the config: %s", rawRec.Body.String())
			}
		})
	}

	// Sanity: a valid path succeeds in both modes.
	if rec := doRequest(t, srv, http.MethodGet, base+"config.yaml", nil); rec.Code != http.StatusOK {
		t.Errorf("JSON read of config.yaml: status %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doRequest(t, srv, http.MethodGet, base+"config.yaml?raw=1", nil); rec.Code != http.StatusOK || rec.Body.String() != "x\n" {
		t.Errorf("raw read of config.yaml: status %d body %q", rec.Code, rec.Body.String())
	}
}

func truncateForLog(s string) string {
	if len(s) > 512 {
		return s[:512] + "..."
	}
	return s
}
