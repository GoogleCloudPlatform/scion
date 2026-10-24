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

package hubclient_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

func TestManifestBuilder_RebuildUsesCurrentOptions(t *testing.T) {
	tests := []struct {
		name           string
		missingInitial bool
		update         func(*hubclient.ManifestBuilder, string)
		want           map[string]string
	}{
		{
			name: "changed base path",
			update: func(b *hubclient.ManifestBuilder, other string) {
				b.BasePath = other
			},
			want: map[string]string{"other.txt": "other content"},
		},
		{
			name: "appended ignore pattern",
			update: func(b *hubclient.ManifestBuilder, _ string) {
				b.IgnorePatterns = append(b.IgnorePatterns, "keep.txt")
			},
			want: map[string]string{},
		},
		{
			name: "replaced ignore patterns",
			update: func(b *hubclient.ManifestBuilder, _ string) {
				b.IgnorePatterns = []string{"keep.txt"}
			},
			want: map[string]string{"private.txt": "private content"},
		},
		{
			name: "nil ignore patterns",
			update: func(b *hubclient.ManifestBuilder, _ string) {
				b.IgnorePatterns = nil
			},
			want: map[string]string{"keep.txt": "keep content", "private.txt": "private content"},
		},
		{
			name: "empty ignore patterns",
			update: func(b *hubclient.ManifestBuilder, _ string) {
				b.IgnorePatterns = []string{}
			},
			want: map[string]string{"keep.txt": "keep content", "private.txt": "private content"},
		},
		{
			name:           "corrected base path after error",
			missingInitial: true,
			update: func(b *hubclient.ManifestBuilder, other string) {
				b.BasePath = other
			},
			want: map[string]string{"other.txt": "other content"},
		},
		{
			name: "unchanged options",
			want: map[string]string{"keep.txt": "keep content"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, other := t.TempDir(), t.TempDir()
			for path, content := range map[string]string{
				filepath.Join(base, "keep.txt"):    "keep content",
				filepath.Join(base, "private.txt"): "private content",
				filepath.Join(other, "other.txt"):  "other content",
			} {
				if err := os.WriteFile(path, []byte(content), 0644); err != nil {
					t.Fatal(err)
				}
			}
			b := hubclient.NewManifestBuilder(base)
			b.IgnorePatterns = []string{"private.txt"}
			if tt.missingInitial {
				b.BasePath = filepath.Join(base, "missing")
				if _, err := b.Build(); !os.IsNotExist(err) {
					t.Fatalf("initial Build() error = %v, want not-exist", err)
				}
			} else {
				assertRebuiltManifest(t, b, map[string]string{"keep.txt": "keep content"})
			}
			if tt.update != nil {
				tt.update(b, other)
			}
			assertRebuiltManifest(t, b, tt.want)
		})
	}
}

func assertRebuiltManifest(t *testing.T, b *hubclient.ManifestBuilder, want map[string]string) {
	t.Helper()
	manifest, err := b.Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	if len(manifest.Files) != len(want) {
		t.Fatalf("Build() files = %+v, want %v", manifest.Files, want)
	}
	for _, file := range manifest.Files {
		content, ok := want[file.Path]
		if !ok {
			t.Errorf("Build() included unexpected file %q", file.Path)
			continue
		}
		if file.Size != int64(len(content)) || file.Hash != transfer.HashBytes([]byte(content)) {
			t.Errorf("Build() file = %+v, want size and hash of %q", file, content)
		}
	}
}
