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

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// ownershipTestEnv isolates HOME and git configuration and returns a
// project root directory (not yet created) under the temp HOME.
func ownershipTestEnv(t *testing.T) (home, root string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(home))
	root = filepath.Join(home, "proj")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return home, root
}

func assertNoProjectWritten(t *testing.T, home, root string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(root, DotScion)); !os.IsNotExist(err) {
		t.Errorf("expected no %s in the project root, stat err = %v", DotScion, err)
	}
	if _, err := os.Stat(filepath.Join(home, DotScion, "project-configs")); !os.IsNotExist(err) {
		t.Errorf("expected no external project config, stat err = %v", err)
	}
}

func TestInitProject_GitOwnershipRefusalReturnsError(t *testing.T) {
	home, root := ownershipTestEnv(t)
	setupGitRepoDir(t, root)
	t.Chdir(root)
	t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")

	err := InitProject("", nil, InitProjectOpts{SkipRuntimeCheck: true})
	if !errors.Is(err, util.ErrGitDubiousOwnership) {
		t.Fatalf("expected the ownership error, got %v", err)
	}
	if !strings.Contains(err.Error(), "git config --global --add safe.directory ") {
		t.Errorf("error does not name the safe.directory fix: %v", err)
	}
	var oe *util.GitOwnershipError
	if !errors.As(err, &oe) || oe.SuggestedPath == "" {
		t.Fatalf("expected git's suggested path in the error, got %v", err)
	}
	if !strings.HasSuffix(err.Error(), "safe.directory "+oe.SuggestedPath) {
		t.Errorf("error should end with git's suggested path: %v", err)
	}
	assertNoProjectWritten(t, home, root)
}

func TestInitProject_GitOwnershipRefusalFallbackMessage(t *testing.T) {
	home, root := ownershipTestEnv(t)
	t.Chdir(root)
	orig := isGitRepoFunc
	t.Cleanup(func() { isGitRepoFunc = orig })
	isGitRepoFunc = func() (bool, error) {
		return false, &util.GitOwnershipError{Dir: root}
	}

	err := InitProject("", nil, InitProjectOpts{SkipRuntimeCheck: true})
	if !errors.Is(err, util.ErrGitDubiousOwnership) {
		t.Fatalf("expected the ownership error, got %v", err)
	}
	if !strings.Contains(err.Error(), root) || !strings.Contains(err.Error(), "top-level directory") {
		t.Errorf("fallback message should name the directory and the top-level hint: %v", err)
	}
	assertNoProjectWritten(t, home, root)
}

func TestInitProject_GitOwnershipRefusalExplicitTargetUnchanged(t *testing.T) {
	_, root := ownershipTestEnv(t)
	setupGitRepoDir(t, root)
	t.Chdir(root)
	t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")

	// With an explicit target the refusal is treated as "not a repository",
	// as before: the external layout is used.
	if err := InitProject(filepath.Join(root, DotScion), nil, InitProjectOpts{SkipRuntimeCheck: true}); err != nil {
		t.Fatalf("InitProject with explicit target: %v", err)
	}
	if !IsProjectMarkerFile(filepath.Join(root, DotScion)) {
		t.Error("expected the external layout (marker file) for an explicit target")
	}
}

func TestInitProject_CurrentDirPlainDirUsesExternalLayout(t *testing.T) {
	_, root := ownershipTestEnv(t)
	t.Chdir(root)

	if err := InitProject("", nil, InitProjectOpts{SkipRuntimeCheck: true}); err != nil {
		t.Fatalf("InitProject: %v", err)
	}
	if !IsProjectMarkerFile(filepath.Join(root, DotScion)) {
		t.Error("expected a marker file for a plain directory")
	}
}

func TestInitProject_CurrentDirTrustedRepoUsesInRepoLayout(t *testing.T) {
	_, root := ownershipTestEnv(t)
	setupGitRepoDir(t, root)
	t.Chdir(root)

	if err := InitProject("", nil, InitProjectOpts{SkipRuntimeCheck: true}); err != nil {
		t.Fatalf("InitProject: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, DotScion))
	if err != nil || !info.IsDir() {
		t.Fatalf("expected an in-repo %s directory, stat err = %v", DotScion, err)
	}
}
