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

package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

func TestSyncFromViaHub_VerifiesDownloadedHash(t *testing.T) {
	oldDryRun, oldExclude, oldFormat := syncDryRun, syncExclude, outputFormat
	syncDryRun, syncExclude, outputFormat = false, nil, "plain"
	t.Cleanup(func() {
		syncDryRun, syncExclude, outputFormat = oldDryRun, oldExclude, oldFormat
	})

	for _, body := range []string{"correct", "changed"} {
		t.Run(body, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "file.txt")
			if err := os.WriteFile(path, []byte("original"), 0644); err != nil {
				t.Fatal(err)
			}
			hash := transfer.HashBytes([]byte("correct"))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agents/test-agent/workspace/sync-from":
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(hubclient.SyncFromResponse{
						Manifest: &transfer.Manifest{Version: "1.0", Files: []transfer.FileInfo{
							{Path: "file.txt", Size: 7, Hash: hash},
						}},
						DownloadURLs: []transfer.DownloadURLInfo{
							{Path: "file.txt", URL: "http://" + r.Host + "/content", Size: 7, Hash: hash},
						},
					})
				case r.Method == http.MethodGet && r.URL.Path == "/content":
					_, _ = w.Write([]byte(body))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client, err := hubclient.New(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			err = syncFromViaHub(&HubContext{Client: client}, "test-agent", "test-agent", dir)
			want := body
			if body == "changed" {
				want = "original"
				if err == nil || !strings.Contains(err.Error(), "hash mismatch") || !strings.Contains(err.Error(), "file.txt") {
					t.Errorf("sync error = %v, want hash mismatch for file.txt", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != want {
				t.Errorf("local file = %q, %v; want %q", data, err, want)
			}
		})
	}
}
