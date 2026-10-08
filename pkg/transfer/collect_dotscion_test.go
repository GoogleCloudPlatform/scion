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

package transfer_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// writeTree creates the given files (slash-separated, relative to root).
func writeTree(t *testing.T, root string, files ...string) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func paths(files []transfer.FileInfo) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

// TestDefaultExcludes_RootDotScion checks that the default excludes drop the
// workspace-root .scion entry in both layouts (marker file and directory)
// while keeping nested .scion entries and look-alike names.
func TestDefaultExcludes_RootDotScion(t *testing.T) {
	// The default pattern is written as a literal; keep it in step with the
	// config package's name for the entry.
	if config.DotScion != ".scion" {
		t.Fatalf("config.DotScion = %q; update transfer.DefaultExcludePatterns", config.DotScion)
	}
	if !slices.Contains(transfer.DefaultExcludePatterns, config.DotScion+"/**") {
		t.Errorf("DefaultExcludePatterns %v lacks %q", transfer.DefaultExcludePatterns, config.DotScion+"/**")
	}
	if slices.Contains(transfer.DefaultExcludePatterns, config.DotScion) {
		t.Errorf("DefaultExcludePatterns contains bare %q, which also matches nested entries", config.DotScion)
	}

	kept := []string{
		"main.go",
		"sub/.scion",
		"deep/sub/.scion/settings.yaml",
		".scionrc",
		".scion-other/x",
	}
	cases := []struct {
		name string
		root []string
	}{
		{"marker file", []string{".scion"}},
		{"directory", []string{".scion/project-id", ".scion/settings.yaml", ".scion/templates/a/b.md"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTree(t, dir, append(append([]string{}, tc.root...), kept...)...)

			collected, err := transfer.CollectFiles(dir, nil)
			if err != nil {
				t.Fatalf("CollectFiles: %v", err)
			}
			manifest, err := transfer.NewManifestBuilder(dir).Build()
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			want := slices.Sorted(slices.Values(kept))
			for label, got := range map[string][]string{
				"CollectFiles":    paths(collected),
				"ManifestBuilder": paths(manifest.Files),
			} {
				if !slices.Equal(got, want) {
					t.Errorf("%s collected %v, want %v", label, got, want)
				}
			}
		})
	}
}
