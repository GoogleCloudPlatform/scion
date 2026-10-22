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

import "testing"

// TestClassifyWorkspaceSyncPath pins the classifier. pkg/gcp checks the
// rclone rules against it (TestWorkspaceSyncRcloneRulesParity).
func TestClassifyWorkspaceSyncPath(t *testing.T) {
	cases := []struct {
		rel   string
		isDir bool
		want  WorkspaceSyncClass
	}{
		// Content.
		{"README.md", false, WorkspaceSyncContent},
		{"src/main.go", false, WorkspaceSyncContent},
		{"src", true, WorkspaceSyncContent},
		{".gitignore", false, WorkspaceSyncContent},
		{".github/workflows/ci.yml", false, WorkspaceSyncContent},
		{"docs/.gitkeep", false, WorkspaceSyncContent},
		{"a.git/x", false, WorkspaceSyncContent},
		{"x.scion", false, WorkspaceSyncContent},

		// Git data.
		{".git", true, WorkspaceSyncGitData},
		{".git/HEAD", false, WorkspaceSyncGitData},
		{".git/packed-refs", false, WorkspaceSyncGitData},
		{".git/index", false, WorkspaceSyncGitData},
		{".git/refs", true, WorkspaceSyncGitData},
		{".git/refs/heads/main", false, WorkspaceSyncGitData},
		{".git/refs/heads/feature/x", false, WorkspaceSyncGitData},
		{".git/refs/tags", true, WorkspaceSyncGitData},
		{".git/objects", true, WorkspaceSyncGitData},
		{".git/objects/ab/cdef0123", false, WorkspaceSyncGitData},
		{".git/objects/pack/pack-1.pack", false, WorkspaceSyncGitData},

		// Never: repository configuration and executable surfaces.
		{".git", false, WorkspaceSyncNever}, // worktree/submodule gitdir file
		{".git/config", false, WorkspaceSyncNever},
		{".git/config.worktree", false, WorkspaceSyncNever},
		{".git/hooks", true, WorkspaceSyncNever},
		{".git/hooks/pre-commit", false, WorkspaceSyncNever},
		{".git/info/attributes", false, WorkspaceSyncNever},
		{".git/info/exclude", false, WorkspaceSyncNever},
		{".git/modules/sub/config", false, WorkspaceSyncNever},
		{".git/modules/sub/hooks/post-checkout", false, WorkspaceSyncNever},
		{".git/worktrees/wt/gitdir", false, WorkspaceSyncNever},
		{".git/logs/HEAD", false, WorkspaceSyncNever},
		{".git/FETCH_HEAD", false, WorkspaceSyncNever},
		{".git/description", false, WorkspaceSyncNever},
		{".git/objects/info", true, WorkspaceSyncNever},
		{".git/objects/info/alternates", false, WorkspaceSyncNever},
		{".git/objects/info/http-alternates", false, WorkspaceSyncNever},
		{".git/objects/info/packs", false, WorkspaceSyncNever},
		{".git/HEAD", true, WorkspaceSyncNever},
		{".git/refs", false, WorkspaceSyncNever},
		{".git/refs/heads/.git", false, WorkspaceSyncNever},
		{".git/objects/.git/config", false, WorkspaceSyncNever},
		{".git/refs/.scion/x", false, WorkspaceSyncNever},

		// Never: any .git below the root.
		{"sub/.git", false, WorkspaceSyncNever},
		{"sub/.git", true, WorkspaceSyncNever},
		{"sub/.git/config", false, WorkspaceSyncNever},
		{"vendor/lib/.git/hooks/pre-commit", false, WorkspaceSyncNever},
		{"sub/.git/HEAD", false, WorkspaceSyncNever},

		// Never: project configuration at any depth.
		{".scion", false, WorkspaceSyncNever},
		{".scion", true, WorkspaceSyncNever},
		{".scion/settings.yaml", false, WorkspaceSyncNever},
		{"sub/.scion", true, WorkspaceSyncNever},
		{"sub/.scion", false, WorkspaceSyncNever},
		{"sub/.scion/settings.yaml", false, WorkspaceSyncNever},
		{"a/b/.scion/templates/t/scion-agent.yaml", false, WorkspaceSyncNever},

		// Never: .DS_Store.
		{".DS_Store", false, WorkspaceSyncNever},
		{"src/.DS_Store", false, WorkspaceSyncNever},

		// Never: case variants (a case-insensitive receiver treats them as
		// the real entry) and invisible-character variants.
		{".GIT", true, WorkspaceSyncNever},
		{".Git/config", false, WorkspaceSyncNever},
		{".GIT/refs/heads/main", false, WorkspaceSyncNever},
		{"sub/.GIT/hooks/pre-commit", false, WorkspaceSyncNever},
		{"sub/.Git", false, WorkspaceSyncNever},
		{".SCION/settings.yaml", false, WorkspaceSyncNever},
		{"proj/.Scion/settings.yaml", false, WorkspaceSyncNever},
		{".ds_store", false, WorkspaceSyncNever},
		{".git/objects/INFO/alternates", false, WorkspaceSyncNever},
		{".git/objects/Info", true, WorkspaceSyncNever},
		{".git/OBJECTS/ab/cd", false, WorkspaceSyncNever},
		{".git/Refs/heads/main", false, WorkspaceSyncNever},
		{".git/head", false, WorkspaceSyncNever},
		{".git/HOOKS/pre-commit", false, WorkspaceSyncNever},
		{".git/refs/heads/.GIT", false, WorkspaceSyncNever},
		{".git/refs/heads/.Scion/x", false, WorkspaceSyncNever},
		{".\u017fcion/settings.yaml", false, WorkspaceSyncNever}, // long s folds to s
		{"proj/.\u017fcion", true, WorkspaceSyncNever},
		{".g\u200cit/config", false, WorkspaceSyncNever}, // zero-width non-joiner
		{"sub/.gi\u200dt/hooks/x", false, WorkspaceSyncNever},
		{".sc\u00adion/x", false, WorkspaceSyncNever}, // soft hyphen
		{"x\u200c.go", false, WorkspaceSyncContent},
		{"Git/HEAD", false, WorkspaceSyncContent},
		{"scion/x", false, WorkspaceSyncContent},

		// Never: non-canonical paths.
		{"", false, WorkspaceSyncNever},
		{"/etc/passwd", false, WorkspaceSyncNever},
		{"../x", false, WorkspaceSyncNever},
		{"a/../b", false, WorkspaceSyncNever},
		{"./a", false, WorkspaceSyncNever},
		{"a//b", false, WorkspaceSyncNever},
	}
	for _, tc := range cases {
		if got := ClassifyWorkspaceSyncPath(tc.rel, tc.isDir); got != tc.want {
			t.Errorf("ClassifyWorkspaceSyncPath(%q, %v) = %v, want %v", tc.rel, tc.isDir, got, tc.want)
		}
	}
}
