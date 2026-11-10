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

package hubclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

func writeMultipartTestFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func swapMultipartForSymlink(t *testing.T, p, target string) {
	t.Helper()
	if err := os.RemoveAll(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

// TestUploadFilesMultipart_SendsOnlyVerifiedBytes pins that the multipart
// harness-config upload reads every file through the root it is given:
// unchanged files are sent byte for byte, and a file swapped or changed
// after collection (inside or outside the root) fails the upload before
// any request is sent.
func TestUploadFilesMultipart_SendsOnlyVerifiedBytes(t *testing.T) {
	const content = "first\n"
	cases := map[string]struct {
		swap    func(t *testing.T, dir, outside string)
		wantErr string // "" means the upload succeeds
	}{
		"unchanged": {swap: func(*testing.T, string, string) {}},
		"content changed, same size": {
			swap:    func(t *testing.T, dir, _ string) { writeMultipartTestFile(t, dir, "sub/a.txt", "FIRST\n") },
			wantErr: "changed since it was collected",
		},
		"leaf link inside the root": {
			swap: func(t *testing.T, dir, _ string) {
				writeMultipartTestFile(t, dir, "copy.txt", content)
				swapMultipartForSymlink(t, filepath.Join(dir, "sub", "a.txt"), "../copy.txt")
			},
			wantErr: "no longer a regular file",
		},
		"leaf link outside the root": {
			swap: func(t *testing.T, dir, outside string) {
				swapMultipartForSymlink(t, filepath.Join(dir, "sub", "a.txt"), filepath.Join(outside, "a.txt"))
			},
			wantErr: "no longer a regular file",
		},
		"parent link outside the root": {
			swap: func(t *testing.T, dir, outside string) {
				swapMultipartForSymlink(t, filepath.Join(dir, "sub"), outside)
			},
			wantErr: "path escapes from parent",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir, outside := t.TempDir(), t.TempDir()
			writeMultipartTestFile(t, outside, "a.txt", content)
			writeMultipartTestFile(t, dir, "sub/a.txt", content)
			writeMultipartTestFile(t, dir, "z.txt", "last\n")
			files, err := transfer.CollectFiles(dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()

			var mu sync.Mutex
			got := map[string]string{}
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				requests++
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/harness-configs/hc-1/files" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				mr, err := r.MultipartReader()
				if err != nil {
					t.Errorf("multipart reader: %v", err)
					return
				}
				for {
					part, err := mr.NextPart()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Errorf("next part: %v", err)
						return
					}
					data, _ := io.ReadAll(part)
					got[part.FormName()] = string(data)
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			c, err := New(server.URL)
			if err != nil {
				t.Fatal(err)
			}

			tc.swap(t, dir, outside)
			err = c.HarnessConfigs().UploadFilesMultipart(context.Background(), "hc-1", root, files)

			mu.Lock()
			defer mu.Unlock()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if got["sub/a.txt"] != content || got["z.txt"] != "last\n" || len(got) != 2 {
					t.Fatalf("sent %q, want both files unchanged", got)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
			if requests != 0 {
				t.Fatalf("%d request(s) sent after a refused file, want none", requests)
			}
		})
	}
}
