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

package hubclient

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

func TestTemplateManifest_RecursiveExcludeBoundary(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		kept     []string
		dropped  []string
	}{
		{"defaults", nil,
			[]string{"report.DS_Store", "nested/report.DS_Store"},
			[]string{".DS_Store", "nested/.DS_Store"}},
		{"custom_directory", []string{"**/cache"},
			[]string{"mycache/keep.txt", "nested/mycache/keep.txt"},
			[]string{"cache/drop.txt", "nested/cache/drop.txt"}},
		{"recursive_glob", []string{"**/*.log"},
			[]string{"debug.log.txt", "nested/debug.log.txt"},
			[]string{"debug.log", "nested/debug.log", "a/b/debug.log"}},
		{"recursive_compound_glob", []string{"**/logs/*.json"},
			[]string{"mylogs/data.json", "nested/logs/deep/data.json", "logs/data.json.txt"},
			[]string{"logs/data.json", "a/b/logs/data.json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range append(slices.Clone(tc.kept), tc.dropped...) {
				path := filepath.Join(dir, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(name), 0644); err != nil {
					t.Fatal(err)
				}
			}
			builder := NewManifestBuilder(dir)
			builder.IgnorePatterns = append(builder.IgnorePatterns, tc.patterns...)
			manifest, err := builder.Build()
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, file := range manifest.Files {
				got = append(got, file.Path)
			}
			want := slices.Clone(tc.kept)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("template manifest paths = %v, want %v", got, want)
			}
		})
	}
}

func TestNewManifestBuilder_UsesSharedDefaultExcludes(t *testing.T) {
	saved := transfer.DefaultExcludePatterns
	t.Cleanup(func() { transfer.DefaultExcludePatterns = saved })

	// Use a distinct shared list so the test detects a hard-coded copy
	// rather than passing because the values happen to match.
	transfer.DefaultExcludePatterns = append(append([]string{}, saved...), "shared-default-sentinel")

	b := NewManifestBuilder(t.TempDir())
	if !reflect.DeepEqual(b.IgnorePatterns, transfer.DefaultExcludePatterns) {
		t.Fatalf("IgnorePatterns = %v, want shared defaults %v", b.IgnorePatterns, transfer.DefaultExcludePatterns)
	}

	// The template manifest builder and a plain transfer builder must agree.
	tb := transfer.NewManifestBuilder(t.TempDir())
	if !reflect.DeepEqual(b.IgnorePatterns, tb.ExcludePatterns) {
		t.Fatalf("IgnorePatterns = %v, transfer builder ExcludePatterns = %v", b.IgnorePatterns, tb.ExcludePatterns)
	}
}

func TestNewManifestBuilder_DoesNotAliasSharedDefaults(t *testing.T) {
	want := append([]string{}, transfer.DefaultExcludePatterns...)

	b := NewManifestBuilder(t.TempDir())
	if len(b.IgnorePatterns) > 0 {
		b.IgnorePatterns[0] = "mutated"
	}
	b.IgnorePatterns = append(b.IgnorePatterns, "extra")

	if !reflect.DeepEqual(transfer.DefaultExcludePatterns, want) {
		t.Fatalf("shared defaults changed to %v, want %v", transfer.DefaultExcludePatterns, want)
	}
}
