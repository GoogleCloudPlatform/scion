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

package cmd

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	systemOneAllowedLiveKeyRelPath = "cmd/admin_system_one_live_test.go"
	maxBootstrapKeyScanFileBytes   = 4 * 1024 * 1024
)

// TestSystemOneLiveBootstrapKeyVarOnlyInLiveTest enforces §8 L4: the bootstrap
// live-probe environment variable name (constructed here via concatenation so
// this guard file itself does not match) may appear ONLY in the manual
// systemone_live test file and nowhere in product code, scripts, docs, or other
// tests.
//
// Tracked files come from `git ls-files` so untracked or gitignored local
// scratch notes do not cause false positives, falling back to a directory walk
// when git or a repository checkout is unavailable.
func TestSystemOneLiveBootstrapKeyVarOnlyInLiveTest(t *testing.T) {
	needle := []byte("JEV" + "KEY")

	repoRoot := findScionRepoRoot(t)
	matches, _, err := scanRepoForBootstrapKey(repoRoot, needle)
	require.NoError(t, err)
	assert.Equal(t, []string{systemOneAllowedLiveKeyRelPath}, matches,
		"the bootstrap live-probe env var literal must appear only in %s", systemOneAllowedLiveKeyRelPath)

	t.Run("git_ls_files_skips_untracked_and_catches_tracked", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not available in PATH")
		}
		tmpRepo := t.TempDir()
		runGit := func(args ...string) {
			t.Helper()
			c := exec.Command("git", append(util.HostGitArgs(), append([]string{"-C", tmpRepo}, args...)...)...)
			c.Env = envWithoutGit()
			out, err := c.CombinedOutput()
			require.NoError(t, err, "git %v: %s", args, string(out))
		}

		runGit("init", "-q")

		livePath := filepath.Join(tmpRepo, filepath.FromSlash(systemOneAllowedLiveKeyRelPath))
		require.NoError(t, os.MkdirAll(filepath.Dir(livePath), 0o755))
		require.NoError(t, os.WriteFile(livePath, needle, 0o600))
		runGit("add", systemOneAllowedLiveKeyRelPath)

		// Untracked scratch files must not fail the guard when git ls-files works.
		scratchPath := filepath.Join(tmpRepo, ".scratch", "notes.md")
		require.NoError(t, os.MkdirAll(filepath.Dir(scratchPath), 0o755))
		require.NoError(t, os.WriteFile(scratchPath, needle, 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(tmpRepo, "untracked.txt"), needle, 0o600))

		got, usedGit, err := scanRepoForBootstrapKey(tmpRepo, needle)
		require.NoError(t, err)
		assert.True(t, usedGit, "expected git ls-files path")
		assert.Equal(t, []string{systemOneAllowedLiveKeyRelPath}, got)

		// Tracked files outside the live test must be caught.
		leakPath := filepath.Join(tmpRepo, "pkg", "leaked.go")
		require.NoError(t, os.MkdirAll(filepath.Dir(leakPath), 0o755))
		require.NoError(t, os.WriteFile(leakPath, needle, 0o600))
		runGit("add", "pkg/leaked.go")

		got, usedGit, err = scanRepoForBootstrapKey(tmpRepo, needle)
		require.NoError(t, err)
		assert.True(t, usedGit)
		assert.Equal(t, []string{systemOneAllowedLiveKeyRelPath, "pkg/leaked.go"}, got)
	})

	t.Run("walks_tree_when_git_unavailable", func(t *testing.T) {
		tmpDir := t.TempDir()
		livePath := filepath.Join(tmpDir, filepath.FromSlash(systemOneAllowedLiveKeyRelPath))
		require.NoError(t, os.MkdirAll(filepath.Dir(livePath), 0o755))
		require.NoError(t, os.WriteFile(livePath, needle, 0o600))

		leakPath := filepath.Join(tmpDir, "pkg", "leaked.go")
		require.NoError(t, os.MkdirAll(filepath.Dir(leakPath), 0o755))
		require.NoError(t, os.WriteFile(leakPath, needle, 0o600))

		ignoredDirFile := filepath.Join(tmpDir, ".scion", "state.txt")
		require.NoError(t, os.MkdirAll(filepath.Dir(ignoredDirFile), 0o755))
		require.NoError(t, os.WriteFile(ignoredDirFile, needle, 0o600))

		got, usedGit, err := scanRepoForBootstrapKey(tmpDir, needle)
		require.NoError(t, err)
		assert.False(t, usedGit, "expected fallback directory walk outside a git repository")
		assert.Equal(t, []string{systemOneAllowedLiveKeyRelPath, "pkg/leaked.go"}, got)
	})
}

func scanRepoForBootstrapKey(repoRoot string, needle []byte) ([]string, bool, error) {
	if tracked, ok := gitTrackedFilesForKeyGuard(repoRoot); ok {
		var matches []string
		for _, rel := range tracked {
			fullPath := filepath.Join(repoRoot, filepath.FromSlash(rel))
			info, err := os.Lstat(fullPath)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, true, err
			}
			if !info.Mode().IsRegular() || info.Size() > maxBootstrapKeyScanFileBytes {
				continue
			}
			data, err := os.ReadFile(fullPath)
			if err != nil {
				return nil, true, err
			}
			if bytes.Contains(data, needle) {
				matches = append(matches, rel)
			}
		}
		sort.Strings(matches)
		return matches, true, nil
	}

	skipDirs := map[string]bool{
		".git":           true,
		".scion":         true,
		".scion-volumes": true,
		"node_modules":   true,
		"vendor":         true,
		"build":          true,
		"dist":           true,
		".astro":         true,
	}

	var matches []string
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxBootstrapKeyScanFileBytes {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, needle) {
			rel, err := filepath.Rel(repoRoot, path)
			if err != nil {
				return err
			}
			matches = append(matches, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	sort.Strings(matches)
	return matches, false, nil
}

func gitTrackedFilesForKeyGuard(repoRoot string) ([]string, bool) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, false
	}
	cmd := exec.Command("git", append(util.HostGitArgs(), "-C", repoRoot, "ls-files", "-z")...)
	cmd.Env = envWithoutGit()
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	var files []string
	for _, entry := range strings.Split(string(out), "\x00") {
		if entry != "" {
			files = append(files, entry)
		}
	}
	if len(files) == 0 {
		return nil, false
	}
	return files, true
}

func findScionRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		gomod := filepath.Join(dir, "go.mod")
		if data, err := os.ReadFile(gomod); err == nil {
			if strings.Contains(string(data), "module github.com/GoogleCloudPlatform/scion\n") ||
				strings.Contains(string(data), "module github.com/GoogleCloudPlatform/scion\r\n") {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate scion repository root above %s", dir)
		}
		dir = parent
	}
}
