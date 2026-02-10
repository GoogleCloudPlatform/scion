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
	"testing"
	"time"
)

func TestFileInfo(t *testing.T) {
	fi := FileInfo{
		Path:     "src/main.go",
		FullPath: "/tmp/test/src/main.go",
		Size:     1024,
		Hash:     "sha256:abc123",
		Mode:     "0644",
	}

	if fi.Path != "src/main.go" {
		t.Errorf("unexpected Path: %s", fi.Path)
	}
	if fi.FullPath != "/tmp/test/src/main.go" {
		t.Errorf("unexpected FullPath: %s", fi.FullPath)
	}
	if fi.Size != 1024 {
		t.Errorf("unexpected Size: %d", fi.Size)
	}
	if fi.Hash != "sha256:abc123" {
		t.Errorf("unexpected Hash: %s", fi.Hash)
	}
	if fi.Mode != "0644" {
		t.Errorf("unexpected Mode: %s", fi.Mode)
	}
}

func TestManifest(t *testing.T) {
	m := Manifest{
		Version:     "1.0",
		ContentHash: "sha256:def456",
		Files: []FileInfo{
			{Path: "file1.txt", Size: 100, Hash: "sha256:aaa"},
			{Path: "file2.txt", Size: 200, Hash: "sha256:bbb"},
		},
	}

	if m.Version != "1.0" {
		t.Errorf("unexpected Version: %s", m.Version)
	}
	if m.ContentHash != "sha256:def456" {
		t.Errorf("unexpected ContentHash: %s", m.ContentHash)
	}
	if len(m.Files) != 2 {
		t.Errorf("unexpected file count: %d", len(m.Files))
	}
}

func TestUploadURLInfo(t *testing.T) {
	expires := time.Now().Add(15 * time.Minute)
	info := UploadURLInfo{
		Path:    "file.txt",
		URL:     "https://storage.example.com/file.txt?token=xyz",
		Method:  "PUT",
		Headers: map[string]string{"Content-Type": "application/octet-stream"},
		Expires: expires,
	}

	if info.Path != "file.txt" {
		t.Errorf("unexpected Path: %s", info.Path)
	}
	if info.Method != "PUT" {
		t.Errorf("unexpected Method: %s", info.Method)
	}
	if info.Headers["Content-Type"] != "application/octet-stream" {
		t.Errorf("unexpected Content-Type header")
	}
}

func TestDownloadURLInfo(t *testing.T) {
	info := DownloadURLInfo{
		Path: "file.txt",
		URL:  "https://storage.example.com/file.txt?token=xyz",
		Size: 500,
		Hash: "sha256:abc123",
	}

	if info.Path != "file.txt" {
		t.Errorf("unexpected Path: %s", info.Path)
	}
	if info.Size != 500 {
		t.Errorf("unexpected Size: %d", info.Size)
	}
	if info.Hash != "sha256:abc123" {
		t.Errorf("unexpected Hash: %s", info.Hash)
	}
}
