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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func hashTestSource(t *testing.T, scheme, content string) string {
	t.Helper()
	if scheme == "file" {
		path := filepath.Join(t.TempDir(), "source")
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		return "file://" + path
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(content))
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func TestClient_DownloadFiles_VerifiesHashBeforeWrite(t *testing.T) {
	for _, scheme := range []string{"file", "http"} {
		for _, tt := range []struct {
			name     string
			content  string
			hash     string
			existing bool
			wantErr  bool
		}{
			{"mismatch preserves existing file", "changed", HashBytes([]byte("correct")), true, true},
			{"mismatch leaves new path absent", "changed", HashBytes([]byte("correct")), false, true},
			{"matching hash replaces file", "correct", HashBytes([]byte("correct")), true, false},
			{"omitted hash remains optional", "correct", "", false, false},
			{"empty content has a valid hash", "", HashBytes(nil), true, false},
		} {
			t.Run(scheme+"/"+tt.name, func(t *testing.T) {
				dest := t.TempDir()
				path := filepath.Join(dest, "nested", "file.txt")
				if tt.existing {
					if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("original"), 0644); err != nil {
						t.Fatal(err)
					}
				}
				entry := DownloadURLInfo{
					Path: "nested/file.txt", URL: hashTestSource(t, scheme, tt.content),
					Size: int64(len(tt.content)), Hash: tt.hash,
				}
				progressCalls := 0
				err := NewClient(nil).DownloadFiles(t.Context(), []DownloadURLInfo{entry}, dest, func(file FileInfo, n int64) error {
					progressCalls++
					if file.Path != entry.Path || file.Hash != entry.Hash || n != entry.Size {
						t.Errorf("unexpected progress: file=%+v, bytes=%d", file, n)
					}
					return nil
				})
				if tt.wantErr {
					if err == nil || !strings.Contains(err.Error(), "hash mismatch") || !strings.Contains(err.Error(), entry.Path) {
						t.Errorf("DownloadFiles error = %v, want a hash mismatch naming %s", err, entry.Path)
					}
					if progressCalls != 0 {
						t.Errorf("reported %d completed files for a hash mismatch", progressCalls)
					}
					if !tt.existing {
						if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
							t.Errorf("created a parent directory for rejected content: %v", err)
						}
						return
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					if progressCalls != 1 {
						t.Errorf("progress calls = %d, want 1", progressCalls)
					}
				}
				want := tt.content
				if tt.wantErr {
					want = "original"
				}
				data, err := os.ReadFile(path)
				if err != nil || string(data) != want {
					t.Errorf("destination = %q, %v; want %q", data, err, want)
				}
			})
		}
	}
}

func TestClient_DownloadFiles_HashMismatchStopsBatch(t *testing.T) {
	for _, scheme := range []string{"file", "http"} {
		t.Run(scheme, func(t *testing.T) {
			dest := t.TempDir()
			var urls []DownloadURLInfo
			for _, name := range []string{"first.txt", "bad.txt", "last.txt"} {
				if err := os.WriteFile(filepath.Join(dest, name), []byte("original"), 0644); err != nil {
					t.Fatal(err)
				}
				body := "correct"
				if name == "bad.txt" {
					body = "changed" // Same length, different contents.
				}
				urls = append(urls, DownloadURLInfo{
					Path: name, URL: hashTestSource(t, scheme, body),
					Size: int64(len(body)), Hash: HashBytes([]byte("correct")),
				})
			}
			var completed []string
			err := NewClient(nil).DownloadFiles(t.Context(), urls, dest, func(file FileInfo, n int64) error {
				completed = append(completed, file.Path)
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "hash mismatch") || !strings.Contains(err.Error(), "bad.txt") {
				t.Errorf("DownloadFiles error = %v, want hash mismatch for bad.txt", err)
			}
			if !slices.Equal(completed, []string{"first.txt"}) {
				t.Errorf("completed = %v, want only first.txt", completed)
			}
			for _, name := range []string{"first.txt", "bad.txt", "last.txt"} {
				want := "original"
				if name == "first.txt" {
					want = "correct"
				}
				data, err := os.ReadFile(filepath.Join(dest, name))
				if err != nil || string(data) != want {
					t.Errorf("%s = %q, %v; want %q", name, data, err, want)
				}
			}
		})
	}
}
