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
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// recordingHCService is a HarnessConfigService whose only implemented
// method is UploadFile.
type recordingHCService struct {
	hubclient.HarnessConfigService
	up recordingUploader
}

func (s *recordingHCService) UploadFile(ctx context.Context, url, method string, headers map[string]string, content io.Reader) error {
	return s.up.UploadFile(ctx, url, method, headers, content)
}

// uploadFamily runs one upload loop over collected files and returns what
// it sent, keyed by URL.
type uploadFamily func(t *testing.T, root *os.Root, files []transfer.FileInfo, urls []hubclient.UploadURLInfo) (map[string]string, error)

var uploadFamilies = map[string]uploadFamily{
	"shared helper": func(t *testing.T, root *os.Root, files []transfer.FileInfo, urls []hubclient.UploadURLInfo) (map[string]string, error) {
		var up recordingUploader
		err := uploadCollectedFiles(context.Background(), &up, root, collectedByPath(files), urls, nil, nil)
		return up.sent, err
	},
	"skills publish": func(t *testing.T, root *os.Root, files []transfer.FileInfo, urls []hubclient.UploadURLInfo) (map[string]string, error) {
		var up recordingUploader
		pf := make([]publishFile, len(files))
		for i, f := range files {
			pf[i] = publishFile{path: f.Path, size: f.Size, hash: f.Hash, mode: "0644"}
		}
		err := uploadSkillFiles(context.Background(), &up, root, pf, urls, io.Discard)
		return up.sent, err
	},
	"template sync": func(t *testing.T, root *os.Root, files []transfer.FileInfo, urls []hubclient.UploadURLInfo) (map[string]string, error) {
		var up recordingUploader
		var err error
		captureStdout(t, func() { err = uploadTemplateURLs(context.Background(), &up, root, files, urls, "Uploaded") })
		return up.sent, err
	},
	"template update (silent)": func(t *testing.T, root *os.Root, files []transfer.FileInfo, urls []hubclient.UploadURLInfo) (map[string]string, error) {
		var up recordingUploader
		var err error
		out := captureStdout(t, func() { err = uploadTemplateURLs(context.Background(), &up, root, files, urls, "") })
		if out != "" {
			t.Errorf("silent upload printed %q", out)
		}
		return up.sent, err
	},
	"harness-config signed URL": func(t *testing.T, root *os.Root, files []transfer.FileInfo, urls []hubclient.UploadURLInfo) (map[string]string, error) {
		svc := &recordingHCService{}
		m := make(map[string]*hubclient.FileInfo, len(files))
		for i := range files {
			m[files[i].Path] = &files[i]
		}
		var err error
		captureStdout(t, func() {
			err = uploadHarnessConfigFilesBySignedURL(context.Background(), svc, root, m, urls, "Uploaded", true)
		})
		return svc.up.sent, err
	},
}

func writeUploadTestFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// replaceWithSymlink replaces dir/rel with a symlink to target.
func replaceWithSymlink(t *testing.T, dir, rel, target string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.RemoveAll(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

// TestCollectedUploads_SendOnlyVerifiedBytes pins, for every upload loop,
// that unchanged collected files are uploaded byte for byte, and that a
// file swapped or changed after collection (inside or outside the
// collected directory) is refused before any of its bytes are sent and
// stops the upload, so no later file is sent either.
func TestCollectedUploads_SendOnlyVerifiedBytes(t *testing.T) {
	const content = "first\n" // same content and size as the swapped-in targets
	cases := map[string]struct {
		swap    func(t *testing.T, dir, outside string)
		wantErr string // "" means the upload succeeds unchanged
	}{
		"unchanged": {swap: func(*testing.T, string, string) {}},
		"content changed, same size": {
			swap:    func(t *testing.T, dir, _ string) { writeUploadTestFile(t, dir, "sub/a.txt", "FIRST\n") },
			wantErr: "changed since it was collected",
		},
		"leaf link inside the root": {
			swap: func(t *testing.T, dir, _ string) {
				writeUploadTestFile(t, dir, "copy.txt", content)
				replaceWithSymlink(t, dir, "sub/a.txt", "../copy.txt")
			},
			wantErr: "no longer a regular file",
		},
		"leaf link outside the root": {
			swap: func(t *testing.T, dir, outside string) {
				replaceWithSymlink(t, dir, "sub/a.txt", filepath.Join(outside, "a.txt"))
			},
			wantErr: "no longer a regular file",
		},
		"parent link inside the root, different content": {
			swap: func(t *testing.T, dir, _ string) {
				writeUploadTestFile(t, dir, "other/a.txt", "OTHER\n")
				replaceWithSymlink(t, dir, "sub", "other")
			},
			wantErr: "changed since it was collected",
		},
		"parent link outside the root": {
			swap:    func(t *testing.T, dir, outside string) { replaceWithSymlink(t, dir, "sub", outside) },
			wantErr: "path escapes from parent",
		},
	}
	for famName, upload := range uploadFamilies {
		for name, tc := range cases {
			t.Run(famName+"/"+name, func(t *testing.T) {
				dir, outside := t.TempDir(), t.TempDir()
				writeUploadTestFile(t, outside, "a.txt", content)
				writeUploadTestFile(t, dir, "sub/a.txt", content)
				writeUploadTestFile(t, dir, "z.txt", "last\n")
				files, err := transfer.CollectFiles(dir, nil)
				if err != nil {
					t.Fatal(err)
				}
				root := openTestRoot(t, dir)
				// sub/a.txt is uploaded first, z.txt after it.
				urls := []hubclient.UploadURLInfo{
					{Path: "sub/a.txt", URL: "u/a", Method: "PUT"},
					{Path: "z.txt", URL: "u/z", Method: "PUT"},
				}

				tc.swap(t, dir, outside)
				sent, err := upload(t, root, files, urls)

				if tc.wantErr == "" {
					if err != nil {
						t.Fatal(err)
					}
					if sent["u/a"] != content || sent["u/z"] != "last\n" || len(sent) != 2 {
						t.Fatalf("sent %q, want both files unchanged", sent)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				if len(sent) != 0 {
					t.Fatalf("sent %q after a refused file, want nothing", sent)
				}
			})
		}
	}
}

// TestCollectedUploads_FIFOSwapDoesNotBlock pins that a collected file
// swapped for a FIFO is refused without blocking the upload.
func TestCollectedUploads_FIFOSwapDoesNotBlock(t *testing.T) {
	for famName, upload := range uploadFamilies {
		t.Run(famName, func(t *testing.T) {
			dir := t.TempDir()
			writeUploadTestFile(t, dir, "a.txt", "first\n")
			files, err := transfer.CollectFiles(dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			root := openTestRoot(t, dir)
			p := filepath.Join(dir, "a.txt")
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(p, 0o600); err != nil {
				t.Skipf("mkfifo: %v", err)
			}

			done := make(chan error, 1)
			var sent map[string]string
			go func() {
				var err error
				sent, err = upload(t, root, files, []hubclient.UploadURLInfo{{Path: "a.txt", URL: "u/a", Method: "PUT"}})
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "no longer a regular file") {
					t.Fatalf("err = %v, want a not-regular error", err)
				}
				if len(sent) != 0 {
					t.Fatalf("sent %q, want nothing", sent)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("upload blocked on a FIFO")
			}
		})
	}
}
