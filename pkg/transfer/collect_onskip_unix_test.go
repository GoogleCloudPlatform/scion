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

//go:build !windows

package transfer

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestManifestBuilder_OnSkipReportsNonRegularFiles pins that OnSkip is
// called for each skipped non-regular entry (not for directories or
// excluded paths) and that it does not change what is collected.
func TestManifestBuilder_OnSkipReportsNonRegularFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub", "deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a.txt", "sub/b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(p)), []byte(p), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("a.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("deeper", filepath.Join(dir, "sub", "dirlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../a.txt", filepath.Join(dir, ".git", "excluded-link")); err != nil {
		t.Fatal(err)
	}

	without, err := NewManifestBuilder(dir).CollectFiles()
	if err != nil {
		t.Fatal(err)
	}

	var skipped []string
	b := NewManifestBuilder(dir)
	b.OnSkip = func(rel string) { skipped = append(skipped, rel) }
	with, err := b.CollectFiles()
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(with, without) {
		t.Errorf("OnSkip changed the collected files:\nwith:    %+v\nwithout: %+v", with, without)
	}
	if len(with) != 2 || with[0].Path != "a.txt" || with[1].Path != "sub/b.txt" {
		t.Errorf("collected %+v, want only a.txt and sub/b.txt", with)
	}
	if want := []string{"link.txt", "sub/dirlink"}; !reflect.DeepEqual(skipped, want) {
		t.Errorf("OnSkip got %v, want %v", skipped, want)
	}
}
