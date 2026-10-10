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

package hub

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestLegacyStoragePath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "empty path returns empty",
			path: "",
			want: "",
		},
		{
			name: "non-hub path returns empty",
			path: "templates/global/my-template",
			want: "",
		},
		{
			name: "hub path strips prefix",
			path: "hubs/my-hub/templates/global/my-template",
			want: "templates/global/my-template",
		},
		{
			name: "hub path with project scope",
			path: "hubs/hub-1/templates/projects/g-1/t1",
			want: "templates/projects/g-1/t1",
		},
		{
			name: "hub path with harness-config",
			path: "hubs/hub-1/harness-configs/global/h1",
			want: "harness-configs/global/h1",
		},
		{
			name: "hubs/ prefix with no hub ID slash returns empty",
			path: "hubs/only-hub-id",
			want: "",
		},
		{
			name: "hubs/ prefix with hub ID and trailing slash",
			path: "hubs/my-hub/",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := legacyStoragePath(tt.path)
			if got != tt.want {
				t.Errorf("legacyStoragePath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestIsCanonicalResourceFilePath(t *testing.T) {
	for _, p := range []string{"a", "config.yaml", "dir/file.txt", "a/b/c", "..x", "dir/..x", ".hidden", "dir/.hidden"} {
		assert.True(t, isCanonicalResourceFilePath(p), "expected %q to be canonical", p)
	}
	for _, p := range []string{"", ".", "..", "../a", "a/..", "a/../b", "./a", "a/./b", "a//b", "a/", "/a", `a\b`, "a\x00b"} {
		assert.False(t, isCanonicalResourceFilePath(p), "expected %q not to be canonical", p)
	}
}

func TestValidateManifestFilePaths_RejectsRepeatedPath(t *testing.T) {
	err := validateManifestFilePaths([]store.TemplateFile{{Path: "a"}, {Path: "b"}, {Path: "a"}})
	var pathErr *invalidFilePathError
	require.ErrorAs(t, err, &pathErr)
	assert.True(t, pathErr.duplicate)
	assert.NoError(t, validateManifestFilePaths([]store.TemplateFile{{Path: "a"}, {Path: "b"}}))
}
