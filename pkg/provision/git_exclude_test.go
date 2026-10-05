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

package provision

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAppendGitExclude_CreatesAndIsIdempotent(t *testing.T) {
	ws := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(ws, ".git"), 0o755))

	require.NoError(t, appendGitExclude(ws, "/a"))
	require.NoError(t, appendGitExclude(ws, "/a"))
	exclude := filepath.Join(ws, ".git", "info", "exclude")
	require.NoError(t, os.WriteFile(exclude, []byte("/a\n# no trailing newline"), 0o644))
	require.NoError(t, appendGitExclude(ws, "worktrees/"))
	data, err := os.ReadFile(exclude)
	require.NoError(t, err)
	assert.Equal(t, "/a\n# no trailing newline\nworktrees/\n", string(data))
}

// A symlink at .git, .git/info or .git/info/exclude is never followed: the
// write fails and the symlink's target is untouched.
func TestAppendGitExclude_RefusesSymlinks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, ws, target string)
		// victim is the file that must stay untouched.
		victim func(target string) string
	}{
		{
			name: ".git",
			setup: func(t *testing.T, ws, target string) {
				require.NoError(t, os.MkdirAll(filepath.Join(target, "info"), 0o755))
				require.NoError(t, os.Symlink(target, filepath.Join(ws, ".git")))
			},
			victim: func(target string) string { return filepath.Join(target, "info", "exclude") },
		},
		{
			name: ".git/info",
			setup: func(t *testing.T, ws, target string) {
				require.NoError(t, os.Mkdir(filepath.Join(ws, ".git"), 0o755))
				require.NoError(t, os.Symlink(target, filepath.Join(ws, ".git", "info")))
			},
			victim: func(target string) string { return filepath.Join(target, "exclude") },
		},
		{
			name: ".git/info/exclude",
			setup: func(t *testing.T, ws, target string) {
				require.NoError(t, os.MkdirAll(filepath.Join(ws, ".git", "info"), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(target, "victim"), []byte("keep\n"), 0o644))
				require.NoError(t, os.Symlink(filepath.Join(target, "victim"), filepath.Join(ws, ".git", "info", "exclude")))
			},
			victim: func(target string) string { return filepath.Join(target, "victim") },
		},
		{
			name: "dangling .git/info/exclude",
			setup: func(t *testing.T, ws, target string) {
				require.NoError(t, os.MkdirAll(filepath.Join(ws, ".git", "info"), 0o755))
				require.NoError(t, os.Symlink(filepath.Join(target, "created"), filepath.Join(ws, ".git", "info", "exclude")))
			},
			victim: func(target string) string { return filepath.Join(target, "created") },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, target := t.TempDir(), t.TempDir()
			tc.setup(t, ws, target)
			victim := tc.victim(target)
			before, beforeErr := os.ReadFile(victim)

			require.Error(t, appendGitExclude(ws, "/.scion-provisioned"))

			after, afterErr := os.ReadFile(victim)
			assert.Equal(t, beforeErr == nil, afterErr == nil, "the target must not be created")
			assert.Equal(t, string(before), string(after), "the target must not be written")
		})
	}
}

// Anything but a regular file at exclude is refused, without blocking on a
// FIFO.
func TestAppendGitExclude_RefusesNonRegularExclude(t *testing.T) {
	ws := t.TempDir()
	info := filepath.Join(ws, ".git", "info")
	require.NoError(t, os.MkdirAll(info, 0o755))
	require.NoError(t, syscall.Mkfifo(filepath.Join(info, "exclude"), 0o644))
	require.ErrorContains(t, appendGitExclude(ws, "/x"), "not a regular file")

	ws2 := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws2, ".git", "info", "exclude"), 0o755))
	require.Error(t, appendGitExclude(ws2, "/x"))
}

// excludeLegacySentinel, reached from the root init container, does not
// write through a symlinked .git/info either.
func TestExcludeLegacySentinel_DoesNotFollowSymlinks(t *testing.T) {
	ws, target := t.TempDir(), t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(ws, ".git"), 0o755))
	require.NoError(t, os.Symlink(target, filepath.Join(ws, ".git", "info")))
	require.NoError(t, writeSentinel(filepath.Join(ws, ProvisionSentinelFile)))

	excludeLegacySentinel(ProvisionInput{Resolved: ResolvedWorkspace{HostPath: ws}, LegacyDir: ws})
	assert.NoFileExists(t, filepath.Join(target, "exclude"))
}
