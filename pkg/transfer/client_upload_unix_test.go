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

package transfer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestClient_UploadFiles_RefusesChangedFiles pins that UploadFiles reads
// each file through the root it is given: a collected file swapped or
// changed after collection (a leaf link inside or outside the root, a
// parent link outside it, a FIFO, or new content) fails the upload, no
// object is left for it, no later file is uploaded and progress is not
// reported for it.
func TestClient_UploadFiles_RefusesChangedFiles(t *testing.T) {
	const content = "first\n"
	cases := map[string]struct {
		swap    func(t *testing.T, dir, outside string)
		wantErr string
		// atEnd: the change is only detected at the end of the read, after the
		// file:// destination directory was created; the partially written
		// object must have been removed.
		atEnd bool
	}{
		"content changed, same size": {
			swap:    func(t *testing.T, dir, _ string) { writeReadTestFile(t, dir, "sub/a.txt", "FIRST\n") },
			wantErr: "changed since it was collected",
			atEnd:   true,
		},
		"leaf link inside the root": {
			swap: func(t *testing.T, dir, _ string) {
				writeReadTestFile(t, dir, "copy.txt", content)
				swapForSymlink(t, filepath.Join(dir, "sub", "a.txt"), "../copy.txt")
			},
			wantErr: "no longer a regular file",
		},
		"leaf link outside the root": {
			swap: func(t *testing.T, dir, outside string) {
				swapForSymlink(t, filepath.Join(dir, "sub", "a.txt"), filepath.Join(outside, "a.txt"))
			},
			wantErr: "no longer a regular file",
		},
		"parent link outside the root": {
			swap:    func(t *testing.T, dir, outside string) { swapForSymlink(t, filepath.Join(dir, "sub"), outside) },
			wantErr: "path escapes from parent",
		},
		"FIFO": {
			swap: func(t *testing.T, dir, _ string) {
				p := filepath.Join(dir, "sub", "a.txt")
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(p, 0o600); err != nil {
					t.Skipf("mkfifo: %v", err)
				}
			},
			wantErr: "no longer a regular file",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir, outside, dest := t.TempDir(), t.TempDir(), t.TempDir()
			writeReadTestFile(t, outside, "a.txt", content)
			writeReadTestFile(t, dir, "sub/a.txt", content)
			writeReadTestFile(t, dir, "z.txt", "last\n")
			files, err := CollectFiles(dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			var urls []UploadURLInfo
			for _, f := range files {
				urls = append(urls, UploadURLInfo{Path: f.Path, URL: "file://" + filepath.Join(dest, filepath.FromSlash(f.Path)), Method: "PUT"})
			}

			tc.swap(t, dir, outside)
			var reported []string
			done := make(chan error, 1)
			go func() {
				done <- NewClient(nil).UploadFiles(context.Background(), root, files, urls, func(f FileInfo, _ int64) error {
					reported = append(reported, f.Path)
					return nil
				})
			}()
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("UploadFiles blocked")
			}

			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
			if len(reported) != 0 {
				t.Errorf("progress reported %v, want nothing", reported)
			}
			// sub/a.txt sorts first, so z.txt is never uploaded.
			if _, err := os.Lstat(filepath.Join(dest, "z.txt")); !os.IsNotExist(err) {
				t.Errorf("z.txt was uploaded after a refused file (lstat err %v)", err)
			}
			if tc.atEnd {
				if _, err := os.Lstat(filepath.Join(dest, "sub", "a.txt")); !os.IsNotExist(err) {
					t.Errorf("an object was left for the refused file (lstat err %v)", err)
				}
			} else {
				// Refused at open: nothing at all was written.
				entries, _ := os.ReadDir(dest)
				if len(entries) != 0 {
					t.Errorf("upload destination has %d entries, want none", len(entries))
				}
			}
		})
	}
}

func swapForSymlink(t *testing.T, p, target string) {
	t.Helper()
	if err := os.RemoveAll(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}
