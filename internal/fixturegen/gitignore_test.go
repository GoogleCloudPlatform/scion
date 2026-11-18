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

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRootGitignoreAnchorsFixturegen verifies that the root .gitignore ignores
// a root-level compiled fixturegen binary without matching the tracked
// internal/fixturegen/ package directory or refusing `git add` on new files
// under internal/fixturegen/.
func TestRootGitignoreAnchorsFixturegen(t *testing.T) {
	t.Parallel()

	_, err := exec.LookPath("git")
	require.NoError(t, err, "git must be on PATH")

	gitignoreBytes, err := os.ReadFile(filepath.Join("..", "..", ".gitignore"))
	require.NoError(t, err, "reading repository root .gitignore")

	tmpDir := t.TempDir()
	globalCfg := filepath.Join(tmpDir, "gitconfig")
	require.NoError(t, os.WriteFile(globalCfg, nil, 0o644))

	repoDir := filepath.Join(tmpDir, "repo")
	require.NoError(t, os.MkdirAll(filepath.Join(repoDir, "internal", "fixturegen"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".gitignore"), gitignoreBytes, 0o644))

	var baseEnv []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			baseEnv = append(baseEnv, kv)
		}
	}
	baseEnv = append(baseEnv,
		"GIT_CONFIG_GLOBAL="+globalCfg,
		"GIT_CONFIG_NOSYSTEM=1",
	)

	runGit := func(args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = baseEnv
		out, runErr := cmd.CombinedOutput()
		if runErr == nil {
			return string(out), 0
		}
		var exitErr *exec.ExitError
		require.ErrorAs(t, runErr, &exitErr, "git %v failed unexpectedly: %v\n%s", args, runErr, out)
		return string(out), exitErr.ExitCode()
	}

	out, code := runGit("init", "-q", ".")
	require.Zero(t, code, "git init failed: %s", out)
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".git", "info", "exclude"), nil, 0o644))

	// Root-level compiled binary file must remain ignored (not directory-only).
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "fixturegen"), []byte("binary"), 0o755))
	out, code = runGit("check-ignore", "-v", "--no-index", "fixturegen")
	assert.Zero(t, code, "root-level fixturegen binary should be ignored by .gitignore; got: %s", out)

	// Package files under internal/fixturegen/ must not be ignored, and staging
	// a new file there must succeed without -f.
	out, code = runGit("check-ignore", "-v", "--no-index", "internal/fixturegen/spec.go")
	assert.Equal(t, 1, code, "internal/fixturegen/spec.go must not be ignored; matched: %s", strings.TrimSpace(out))

	newFile := filepath.Join(repoDir, "internal", "fixturegen", "new_table.go")
	require.NoError(t, os.WriteFile(newFile, []byte("package main\n"), 0o644))
	out, code = runGit("add", "internal/fixturegen/new_table.go")
	assert.Zero(t, code, "git add internal/fixturegen/new_table.go without -f should succeed; got: %s", strings.TrimSpace(out))
}
