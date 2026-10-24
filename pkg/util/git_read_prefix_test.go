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

package util

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadRegularFilePrefix(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, []byte("version https://git-lfs.example"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("reads up to limit from a regular file", func(t *testing.T) {
		data, ok := readRegularFilePrefix(regular, 7)
		if !ok || string(data) != "version" {
			t.Fatalf("got %q, %v; want %q, true", data, ok, "version")
		}
	})

	t.Run("refuses a directory", func(t *testing.T) {
		if data, ok := readRegularFilePrefix(dir, 64); ok {
			t.Fatalf("got %q, true; want refusal", data)
		}
	})

	t.Run("refuses a missing file", func(t *testing.T) {
		if data, ok := readRegularFilePrefix(filepath.Join(dir, "missing"), 64); ok {
			t.Fatalf("got %q, true; want refusal", data)
		}
	})

	t.Run("refuses a symlink to a regular file", func(t *testing.T) {
		link := filepath.Join(dir, "link")
		if err := os.Symlink(regular, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		if data, ok := readRegularFilePrefix(link, 64); ok {
			t.Fatalf("got %q, true; want refusal", data)
		}
	})
}
