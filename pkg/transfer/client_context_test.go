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

package transfer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClient_TransferEndedContext(t *testing.T) {
	for _, ended := range []string{"canceled", "deadline"} {
		for _, scheme := range []string{"file", "http"} {
			for _, operation := range []string{"upload", "download", "download-to-file"} {
				t.Run(ended+"/"+scheme+"/"+operation, func(t *testing.T) {
					ctx, cancel := context.WithCancel(t.Context())
					cancel()
					wantErr := context.Canceled
					if ended == "deadline" {
						ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
						defer cancel()
						wantErr = context.DeadlineExceeded
					}
					dir := t.TempDir()
					source := filepath.Join(dir, "source.txt")
					dest := filepath.Join(dir, "dest.txt")
					for path, data := range map[string]string{source: "replacement", dest: "keep"} {
						if err := os.WriteFile(path, []byte(data), 0600); err != nil {
							t.Fatal(err)
						}
					}
					var requests atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						requests.Add(1)
						_, _ = w.Write([]byte("replacement"))
					}))
					defer server.Close()
					signedURL := "file://" + source
					if operation == "upload" {
						signedURL = "file://" + dest
					}
					if scheme == "http" {
						signedURL = server.URL
					}
					client := NewClient(server.Client())
					content := strings.NewReader("replacement")
					var data []byte
					var err error
					switch operation {
					case "upload":
						err = client.UploadFile(ctx, UploadURLInfo{URL: signedURL}, content)
					case "download":
						data, err = client.DownloadFile(ctx, signedURL)
					case "download-to-file":
						err = client.DownloadToFile(ctx, signedURL, dest)
					}
					if !errors.Is(err, wantErr) {
						t.Errorf("error = %v, want %v", err, wantErr)
					}
					if data != nil {
						t.Errorf("ended transfer returned data: %q", data)
					}
					if content.Len() != len("replacement") {
						t.Error("ended upload consumed its input")
					}
					if got := requests.Load(); got != 0 {
						t.Errorf("sent %d HTTP requests after context ended", got)
					}
					if got, err := os.ReadFile(dest); err != nil || string(got) != "keep" {
						t.Errorf("destination = %q, %v; want unchanged content", got, err)
					}
				})
			}
		}
	}
}

func TestClient_CanceledBatchBeforeFileAccess(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	client := NewClient(nil)
	t.Run("upload", func(t *testing.T) {
		dir := t.TempDir()
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		err = client.UploadFiles(ctx, root,
			[]FileInfo{{Path: "file.txt", FullPath: filepath.Join(dir, "missing-source")}},
			[]UploadURLInfo{{Path: "file.txt", URL: "file://" + filepath.Join(dir, "dest.txt")}}, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want cancellation before opening the source", err)
		}
		if !strings.Contains(err.Error(), "failed to upload file file.txt:") {
			t.Errorf("cancellation error lacks file context: %v", err)
		}
	})
	t.Run("download", func(t *testing.T) {
		dir := t.TempDir()
		dest := filepath.Join(dir, "new-destination")
		err := client.DownloadFiles(ctx, []DownloadURLInfo{{Path: "file.txt", URL: "file://" + filepath.Join(dir, "missing-source")}}, dest, nil)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
		if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("destination directory exists after cancellation: %v", err)
		}
	})
}

func TestClient_TransferCancelBetweenFiles(t *testing.T) {
	for _, operation := range []string{"upload", "download"} {
		for _, cancelAfterFirst := range []bool{false, true} {
			name := operation + "/active"
			if cancelAfterFirst {
				name = operation + "/cancel-after-first"
			}
			t.Run(name, func(t *testing.T) {
				client := NewClient(nil)
				var err error
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				source, dest := t.TempDir(), t.TempDir()
				var files []FileInfo
				var uploads []UploadURLInfo
				var downloads []DownloadURLInfo
				for _, path := range []string{"first.txt", "second.txt"} {
					src, dst := filepath.Join(source, path), filepath.Join(dest, path)
					if err := os.WriteFile(src, []byte("replacement"), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(dst, []byte("keep"), 0600); err != nil {
						t.Fatal(err)
					}
					files = append(files, FileInfo{Path: path, FullPath: src, Size: 11, Hash: HashBytes([]byte("replacement"))})
					uploads = append(uploads, UploadURLInfo{Path: path, URL: "file://" + dst})
					downloads = append(downloads, DownloadURLInfo{Path: path, URL: "file://" + src, Size: 11})
				}
				var completed []string
				progress := func(file FileInfo, _ int64) error {
					completed = append(completed, file.Path)
					if cancelAfterFirst {
						cancel()
					}
					return nil // The context, rather than a callback error, stops the batch.
				}
				if operation == "upload" {
					var root *os.Root
					root, err = os.OpenRoot(source)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = root.Close() }()
					err = client.UploadFiles(ctx, root, files, uploads, progress)
				} else {
					err = client.DownloadFiles(ctx, downloads, dest, progress)
				}
				wantCount, wantSecond := 2, "replacement"
				if cancelAfterFirst {
					wantCount, wantSecond = 1, "keep"
					if !errors.Is(err, context.Canceled) {
						t.Errorf("error = %v, want context.Canceled", err)
					}
					if operation == "upload" && (err == nil || !strings.Contains(err.Error(), "failed to upload file second.txt:")) {
						t.Errorf("cancellation error lacks file context: %v", err)
					}
				} else if err != nil {
					t.Errorf("active transfer failed: %v", err)
				}
				if len(completed) != wantCount || completed[0] != "first.txt" {
					t.Errorf("completed files = %v, want %d starting with first.txt", completed, wantCount)
				}
				for path, want := range map[string]string{"first.txt": "replacement", "second.txt": wantSecond} {
					if got, err := os.ReadFile(filepath.Join(dest, path)); err != nil || string(got) != want {
						t.Errorf("%s = %q, %v; want %q", path, got, err, want)
					}
				}
			})
		}
	}
}
