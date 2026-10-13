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

package hubsync

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepoUnderHome creates a git repository below home, optionally with a
// .scion directory of its own, and moves into it.
func gitRepoUnderHome(t *testing.T, home string, withScion bool) string {
	t.Helper()
	repo := filepath.Join(home, "src", "clone")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if withScion {
		if err := os.MkdirAll(filepath.Join(repo, ".scion"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, ".scion", "settings.yaml"), []byte("hub:\n  enabled: true\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(repo)
	return repo
}

func TestRefuseGlobalFallbackLink(t *testing.T) {
	home := clearHubCredentialEnv(t)

	t.Run("outside a git repository", func(t *testing.T) {
		t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(home))
		t.Chdir(home)
		if err := RefuseGlobalFallbackLink(true, false); err != nil {
			t.Errorf("err = %v, want nil", err)
		}
	})

	t.Run("git repository without .scion", func(t *testing.T) {
		gitRepoUnderHome(t, home, false)
		if err := RefuseGlobalFallbackLink(true, false); !errors.Is(err, ErrGitRepoNotProject) {
			t.Errorf("global fallback: err = %v, want ErrGitRepoNotProject", err)
		}
		if err := RefuseGlobalFallbackLink(true, true); err != nil {
			t.Errorf("explicit global: err = %v, want nil", err)
		}
		if err := RefuseGlobalFallbackLink(false, false); err != nil {
			t.Errorf("not global: err = %v, want nil", err)
		}
	})

	// With global resolution, a .scion at the repository root that is not
	// the global project is one resolution skipped as broken: still refused.
	t.Run("git repository with a broken .scion", func(t *testing.T) {
		home := clearHubCredentialEnv(t)
		repo := gitRepoUnderHome(t, home, false)
		if err := os.Symlink(filepath.Join(home, "missing"), filepath.Join(repo, ".scion")); err != nil {
			t.Fatal(err)
		}
		if err := RefuseGlobalFallbackLink(true, false); !errors.Is(err, ErrGitRepoNotProject) {
			t.Errorf("dangling .scion symlink: err = %v, want ErrGitRepoNotProject", err)
		}
	})

	t.Run("home directory is the git repository", func(t *testing.T) {
		home := clearHubCredentialEnv(t)
		if err := os.MkdirAll(filepath.Join(home, ".scion"), 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("git", "-C", home, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v: %s", err, out)
		}
		sub := filepath.Join(home, "notes")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(sub)
		if err := RefuseGlobalFallbackLink(true, false); err != nil {
			t.Errorf("err = %v, want nil (the repository's .scion is the global project)", err)
		}
	})
}

// setupGuardHub prepares the auto-link case: hub-enabled global settings
// (not registered on the fake local hub) and a git repository below HOME,
// with or without its own .scion, as the working directory. It returns HOME,
// the hub endpoint and the register-call counter.
func setupGuardHub(t *testing.T, projectID string, withScion bool) (home, endpoint string, registers func() int32) {
	t.Helper()
	home = clearHubCredentialEnv(t)
	t.Setenv("SCION_HUB_PROJECT_ID", "")
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	server, regs := unregisteredHub(t, projectID)
	setupUnregisteredGlobalProject(t, home, projectID)
	gitRepoUnderHome(t, home, withScion)
	withPromptIO(t, strings.NewReader(""), false)
	return home, server.URL, regs.Load
}

// TestEnsureHubReady_AutoLinkRefusesGlobalFallbackInGitRepo checks the
// auto-link from a git repository with no .scion under HOME: it refuses
// with the 'scion init' pointer, makes no register call and writes nothing.
// An explicit global target, and a repository with its own .scion, still
// link.
func TestEnsureHubReady_AutoLinkRefusesGlobalFallbackInGitRepo(t *testing.T) {
	const projectID = "guard-global-project-id"

	t.Run("refuses and writes nothing", func(t *testing.T) {
		home, endpoint, registers := setupGuardHub(t, projectID, false)
		globalSettings := filepath.Join(home, ".scion", "settings.yaml")
		before, err := os.ReadFile(globalSettings)
		if err != nil {
			t.Fatal(err)
		}
		_, err = EnsureHubReady("", EnsureHubReadyOptions{EndpointOverride: endpoint, SkipSync: true, AutoLinkLocal: true})
		if !errors.Is(err, ErrGitRepoNotProject) {
			t.Fatalf("err = %v, want ErrGitRepoNotProject", err)
		}
		if !strings.Contains(err.Error(), "run 'scion init' here first") || !strings.Contains(err.Error(), "--global") {
			t.Errorf("error %q does not point at 'scion init' and --global", err)
		}
		if n := registers(); n != 0 {
			t.Errorf("register calls = %d, want 0", n)
		}
		after, err := os.ReadFile(globalSettings)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Errorf("global settings changed:\n%s", after)
		}
		if _, err := os.Stat(filepath.Join(home, "src", "clone", ".scion")); !os.IsNotExist(err) {
			t.Errorf("a .scion was created in the repository (stat err = %v)", err)
		}
	})

	t.Run("explicit global still links", func(t *testing.T) {
		_, endpoint, registers := setupGuardHub(t, projectID, false)
		if _, err := EnsureHubReady("global", EnsureHubReadyOptions{
			EndpointOverride: endpoint, SkipSync: true, AutoLinkLocal: true, ExplicitProject: true,
		}); err != nil {
			t.Fatalf("EnsureHubReady: %v", err)
		}
		if n := registers(); n != 1 {
			t.Errorf("register calls = %d, want 1", n)
		}
	})

	t.Run("repository with its own .scion links", func(t *testing.T) {
		_, endpoint, registers := setupGuardHub(t, projectID, true)
		hubCtx, err := EnsureHubReady("", EnsureHubReadyOptions{EndpointOverride: endpoint, SkipSync: true, AutoLinkLocal: true})
		if err != nil {
			t.Fatalf("EnsureHubReady: %v", err)
		}
		if hubCtx.IsGlobal {
			t.Error("resolved the global project, want the repository")
		}
		if n := registers(); n != 1 {
			t.Errorf("register calls = %d, want 1", n)
		}
	})
}
