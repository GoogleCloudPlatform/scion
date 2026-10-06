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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestValidateRelPath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{name: "simple file", path: "file.txt"},
		{name: "nested file", path: "a/b/c.txt"},
		{name: "dot-prefixed name", path: ".claude/settings.json"},
		{name: "name beginning with two dots", path: "..config/file"},
		{name: "empty", path: "", wantErr: true},
		{name: "absolute", path: "/etc/file", wantErr: true},
		{name: "parent element", path: "..", wantErr: true},
		{name: "leading parent element", path: "../file", wantErr: true},
		{name: "interior parent element", path: "a/../../file", wantErr: true},
		{name: "interior parent element that cleans locally", path: "a/../file", wantErr: true},
		{name: "leading dot element", path: "./file", wantErr: true},
		{name: "interior dot element", path: "a/./file", wantErr: true},
		{name: "dot", path: ".", wantErr: true},
		{name: "empty element", path: "a//file", wantErr: true},
		{name: "trailing slash", path: "a/", wantErr: true},
		{name: "backslash", path: `a\file`, wantErr: true},
		{name: "NUL byte", path: "a\x00file", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRelPath(tt.path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateRelPath(%q) error = %v, wantErr %v", tt.path, err, tt.wantErr)
			}
			if err != nil && tt.path != "" && strings.Contains(err.Error(), tt.path) {
				t.Errorf("error %q should not include the path value", err)
			}
		})
	}
}

func TestWriteFileInRoot_NestedPath(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	if err := WriteFileInRoot(root, "a/b/c.txt", []byte("nested"), 0644); err != nil {
		t.Fatalf("WriteFileInRoot failed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "a", "b", "c.txt"))
	if err != nil || string(data) != "nested" {
		t.Fatalf("nested file = %q, %v", data, err)
	}
}

// newRootTestLayout returns a base directory containing a destination
// directory and an empty sibling directory.
func newRootTestLayout(t *testing.T) (base, dest, sibling string) {
	t.Helper()
	base = t.TempDir()
	dest = filepath.Join(base, "dest")
	sibling = filepath.Join(base, "sibling")
	for _, d := range []string{dest, sibling} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	return base, dest, sibling
}

// assertOnlyDestAndSibling checks that base still holds only dest and sibling
// and that sibling is empty.
func assertOnlyDestAndSibling(t *testing.T, base, sibling string) {
	t.Helper()
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "dest,sibling" {
		t.Errorf("base directory entries = %v, want [dest sibling]", names)
	}
	siblingEntries, err := os.ReadDir(sibling)
	if err != nil {
		t.Fatal(err)
	}
	if len(siblingEntries) != 0 {
		t.Errorf("sibling directory should be empty, has %d entries", len(siblingEntries))
	}
}

func TestWriteFileInRoot_StaysInsideRootThroughSymlinkedDir(t *testing.T) {
	base, dest, sibling := newRootTestLayout(t)
	if err := os.Symlink(sibling, filepath.Join(dest, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	if err := WriteFileInRoot(root, "link/file.txt", []byte("x"), 0644); err == nil {
		t.Fatal("expected an error writing through a symlink that leaves the root")
	}
	if err := WriteFileInRoot(root, "link/sub/file.txt", []byte("x"), 0644); err == nil {
		t.Fatal("expected an error creating a directory through a symlink that leaves the root")
	}
	assertOnlyDestAndSibling(t, base, sibling)
}

func newContentServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("content"))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestClient_DownloadFiles_RejectsNonCanonicalEntry(t *testing.T) {
	server := newContentServer(t)
	client := NewClient(server.Client())

	entries := []string{
		"../sibling/file.txt",
		"a/../../sibling/file.txt",
		"../file.txt",
		"./file.txt",
		"a//file.txt",
		`a\file.txt`,
		"file\x00.txt",
		"",
	}
	for _, entry := range entries {
		t.Run(entry, func(t *testing.T) {
			base, dest, sibling := newRootTestLayout(t)
			urls := []DownloadURLInfo{
				{Path: "ok.txt", URL: server.URL + "/ok"},
				{Path: entry, URL: server.URL + "/x"},
			}
			err := client.DownloadFiles(context.Background(), urls, dest, nil)
			if err == nil {
				t.Fatal("expected an error for a non-canonical entry")
			}
			if !strings.Contains(err.Error(), "entry 1") {
				t.Errorf("error %q should name the entry index", err)
			}
			if _, statErr := os.Stat(filepath.Join(dest, "ok.txt")); !os.IsNotExist(statErr) {
				t.Errorf("no file should be written when an entry is rejected")
			}
			assertOnlyDestAndSibling(t, base, sibling)
		})
	}

	t.Run("absolute", func(t *testing.T) {
		base, dest, sibling := newRootTestLayout(t)
		urls := []DownloadURLInfo{{Path: filepath.ToSlash(filepath.Join(sibling, "file.txt")), URL: server.URL + "/x"}}
		if err := client.DownloadFiles(context.Background(), urls, dest, nil); err == nil {
			t.Fatal("expected an error for an absolute entry")
		}
		assertOnlyDestAndSibling(t, base, sibling)
	})
}

func TestClient_DownloadFiles_StaysInsideDestThroughSymlinkedDir(t *testing.T) {
	server := newContentServer(t)
	client := NewClient(server.Client())
	base, dest, sibling := newRootTestLayout(t)
	if err := os.Symlink(sibling, filepath.Join(dest, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	urls := []DownloadURLInfo{{Path: "link/file.txt", URL: server.URL + "/x"}}
	if err := client.DownloadFiles(context.Background(), urls, dest, nil); err == nil {
		t.Fatal("expected an error writing through a symlink that leaves the destination")
	}
	assertOnlyDestAndSibling(t, base, sibling)
}

func TestClient_DownloadFiles_NestedPaths(t *testing.T) {
	server := newContentServer(t)
	client := NewClient(server.Client())
	base, dest, sibling := newRootTestLayout(t)

	urls := []DownloadURLInfo{
		{Path: "a/b/c.txt", URL: server.URL + "/1"},
		{Path: "top.txt", URL: server.URL + "/2"},
	}
	if err := client.DownloadFiles(context.Background(), urls, dest, nil); err != nil {
		t.Fatalf("DownloadFiles failed: %v", err)
	}
	for _, rel := range []string{"a/b/c.txt", "top.txt"} {
		path := filepath.Join(dest, filepath.FromSlash(rel))
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "content" {
			t.Errorf("%s = %q, %v", rel, data, err)
		}
		info, err := os.Stat(path)
		if err == nil && info.Mode().Perm() != 0644&^currentUmask(t) {
			t.Errorf("%s mode = %v", rel, info.Mode().Perm())
		}
	}
	assertOnlyDestAndSibling(t, base, sibling)
}

// currentUmask reports the process umask by creating a probe file.
func currentUmask(t *testing.T) os.FileMode {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probe, nil, 0777); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(probe)
	if err != nil {
		t.Fatal(err)
	}
	return 0777 &^ info.Mode().Perm()
}

func TestClient_DownloadFiles_CreatesDestDir(t *testing.T) {
	server := newContentServer(t)
	client := NewClient(server.Client())
	dest := filepath.Join(t.TempDir(), "new", "dest")

	urls := []DownloadURLInfo{{Path: "file.txt", URL: server.URL + "/1"}}
	if err := client.DownloadFiles(context.Background(), urls, dest, nil); err != nil {
		t.Fatalf("DownloadFiles failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "file.txt")); err != nil {
		t.Fatalf("file not written: %v", err)
	}
}

func TestClient_DownloadFiles_NetworkErrorUnchanged(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client := NewClient(server.Client())

	urls := []DownloadURLInfo{{Path: "file.txt", URL: server.URL + "/1"}}
	err := client.DownloadFiles(context.Background(), urls, t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "failed to download file file.txt: download failed with status 500") {
		t.Fatalf("unexpected error: %v", err)
	}
}
