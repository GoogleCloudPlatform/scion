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

//go:build unix

package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// TestSyncToViaHub_ChangedFileIsNotUploaded pins that `scion sync to` never
// uploads a collected file that was swapped or changed after collection,
// and does not finalize the sync. The swap happens while the Hub handles
// the sync-to request, after collection and before the upload.
func TestSyncToViaHub_ChangedFileIsNotUploaded(t *testing.T) {
	cases := map[string]struct {
		swap    func(t *testing.T, ws, outside string)
		wantErr string // "" means the sync succeeds
	}{
		"unchanged": {swap: func(*testing.T, string, string) {}},
		"content changed, same size": {
			swap:    func(t *testing.T, ws, _ string) { writeUploadTestFile(t, ws, "main.go", "MAIN\n") },
			wantErr: "changed since it was collected",
		},
		"leaf link inside the root": {
			swap: func(t *testing.T, ws, _ string) {
				writeUploadTestFile(t, ws, "copy.go", "main\n")
				replaceWithSymlink(t, ws, "main.go", "copy.go")
			},
			wantErr: "no longer a regular file",
		},
		"leaf link outside the root": {
			swap: func(t *testing.T, ws, outside string) {
				replaceWithSymlink(t, ws, "main.go", filepath.Join(outside, "main.go"))
			},
			wantErr: "no longer a regular file",
		},
		"parent link outside the root": {
			swap:    func(t *testing.T, ws, outside string) { replaceWithSymlink(t, ws, "docs", outside) },
			wantErr: "path escapes from parent",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ws, outside := t.TempDir(), t.TempDir()
			writeUploadTestFile(t, ws, "main.go", "main\n")
			writeUploadTestFile(t, ws, "docs/readme.md", "readme\n")
			// Same size as the files above, so only the content differs.
			writeUploadTestFile(t, outside, "main.go", "OUTS\n")
			writeUploadTestFile(t, outside, "readme.md", "OUTSIDE\n")

			var mu sync.Mutex
			put := map[string]string{}
			finalized := false
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agents/agent-1/workspace/sync-to":
					var req struct {
						Files []transfer.FileInfo `json:"files"`
					}
					_ = json.NewDecoder(r.Body).Decode(&req)
					tc.swap(t, ws, outside)
					resp := hubclient.SyncToResponse{}
					for _, f := range req.Files {
						resp.UploadURLs = append(resp.UploadURLs, transfer.UploadURLInfo{
							Path: f.Path, URL: server.URL + "/upload/" + f.Path, Method: http.MethodPut,
						})
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(resp)
				case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/upload/"):
					data, _ := io.ReadAll(r.Body)
					put[strings.TrimPrefix(r.URL.Path, "/upload/")] = string(data)
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agents/agent-1/workspace/sync-to/finalize":
					finalized = true
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(hubclient.SyncToFinalizeResponse{Applied: true})
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			client, err := hubclient.New(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			savedExclude, savedDryRun := syncExclude, syncDryRun
			t.Cleanup(func() { syncExclude, syncDryRun = savedExclude, savedDryRun })
			syncExclude, syncDryRun = nil, false

			var syncErr error
			captureStdout(t, func() {
				syncErr = syncToViaHub(&HubContext{Client: client, Endpoint: server.URL}, "agent-1", "agent-1", ws)
			})

			mu.Lock()
			defer mu.Unlock()
			if tc.wantErr == "" {
				if syncErr != nil {
					t.Fatal(syncErr)
				}
				if put["main.go"] != "main\n" || put["docs/readme.md"] != "readme\n" || !finalized {
					t.Fatalf("put %q, finalized %v; want both files and a finalize", put, finalized)
				}
				return
			}
			if syncErr == nil || !strings.Contains(syncErr.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", syncErr, tc.wantErr)
			}
			if finalized {
				t.Error("the sync was finalized after a refused file")
			}
			for p, data := range put {
				if strings.Contains(data, "OUTS") || data == "MAIN\n" {
					t.Errorf("uploaded changed content for %s: %q", p, data)
				}
			}
		})
	}
}
