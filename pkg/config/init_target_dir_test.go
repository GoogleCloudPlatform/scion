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
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// The tests in this file use the real git check (no isGitRepoFunc override)
// so that the layout decision is made from the directories on disk.

// gitCommitAll stages and commits everything in dir.
func gitCommitAll(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"add", "-A"},
		{"commit", "-m", "add files"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
}

// plainDir creates a directory under home that is not inside a repository.
func plainDir(t *testing.T, home, name string) string {
	t.Helper()
	dir := filepath.Join(home, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func assertInRepoProjectID(t *testing.T, scionDir, want string) {
	t.Helper()
	info, err := os.Lstat(scionDir)
	if err != nil || !info.IsDir() {
		t.Fatalf("expected an in-repo %s directory, stat err = %v", DotScion, err)
	}
	got, err := ReadProjectID(scionDir)
	if err != nil {
		t.Fatalf("ReadProjectID: %v", err)
	}
	if got != want {
		t.Errorf("project-id = %q, want %q", got, want)
	}
}

func TestInitProject_ExplicitTargetInRepoFromOutsideRepoUsesInRepoLayout(t *testing.T) {
	home, clone := ownershipTestEnvNamed(t, "clone")
	setupGitRepoDir(t, clone)
	t.Chdir(plainDir(t, home, "elsewhere"))

	id := api.NewUUID()
	scionDir := filepath.Join(clone, DotScion)
	if err := InitProject(scionDir, nil, InitProjectOpts{SkipRuntimeCheck: true, ProjectID: id}); err != nil {
		t.Fatalf("InitProject: %v", err)
	}
	assertInRepoProjectID(t, scionDir, id)
}

func TestInitProject_ExplicitTargetPlainFromInsideRepoUsesExternalLayout(t *testing.T) {
	home, repo := ownershipTestEnvNamed(t, "repo")
	setupGitRepoDir(t, repo)
	t.Chdir(repo)
	target := plainDir(t, home, "plain")

	if err := InitProject(filepath.Join(target, DotScion), nil, InitProjectOpts{SkipRuntimeCheck: true}); err != nil {
		t.Fatalf("InitProject: %v", err)
	}
	if !IsProjectMarkerFile(filepath.Join(target, DotScion)) {
		t.Error("expected the external layout (marker file) for a target outside any repository")
	}
	// The repository the process runs in is not touched.
	if _, err := os.Lstat(filepath.Join(repo, ".gitignore")); !os.IsNotExist(err) {
		t.Errorf("expected no .gitignore written in the current directory's repository, stat err = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(repo, DotScion)); !os.IsNotExist(err) {
		t.Errorf("expected no %s in the current directory's repository, stat err = %v", DotScion, err)
	}
}

func TestInitProject_ExplicitTargetLeavesGitignoreUntouched(t *testing.T) {
	_, clone := ownershipTestEnvNamed(t, "clone")
	setupGitRepoDir(t, clone)
	const content = "build/\n"
	gitignore := filepath.Join(clone, ".gitignore")
	if err := os.WriteFile(gitignore, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommitAll(t, clone)
	// Run from inside the clone, so the current directory's repository is
	// the one that holds the target: only the explicit target keeps its
	// .gitignore unchanged.
	t.Chdir(clone)

	if err := InitProject(filepath.Join(clone, DotScion), nil, InitProjectOpts{SkipRuntimeCheck: true, ProjectID: api.NewUUID()}); err != nil {
		t.Fatalf("InitProject: %v", err)
	}
	got, err := os.ReadFile(gitignore)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf(".gitignore changed to %q, want %q", got, content)
	}
}

func TestInitProject_ExplicitTargetCommittedScionDirTakesGivenProjectID(t *testing.T) {
	home, clone := ownershipTestEnvNamed(t, "clone")
	setupGitRepoDir(t, clone)
	scionDir := filepath.Join(clone, DotScion)
	if err := os.MkdirAll(filepath.Join(scionDir, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteProjectID(scionDir, api.NewUUID()); err != nil {
		t.Fatal(err)
	}
	gitCommitAll(t, clone)
	t.Chdir(plainDir(t, home, "elsewhere"))

	id := api.NewUUID()
	if err := InitProject(scionDir, nil, InitProjectOpts{SkipRuntimeCheck: true, ProjectID: id}); err != nil {
		t.Fatalf("InitProject: %v", err)
	}
	assertInRepoProjectID(t, scionDir, id)
}

func TestInitProject_ExplicitTargetCommittedMarkerFileKept(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sameID bool
	}{
		{"same project ID", true},
		{"different project ID", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, clone := ownershipTestEnvNamed(t, "clone")
			setupGitRepoDir(t, clone)
			markerPath := filepath.Join(clone, DotScion)
			committedID := api.NewUUID()
			if err := WriteProjectMarker(markerPath, &ProjectMarker{
				ProjectID: committedID, ProjectName: "clone", ProjectSlug: "clone",
			}); err != nil {
				t.Fatal(err)
			}
			gitCommitAll(t, clone)
			t.Chdir(plainDir(t, home, "elsewhere"))

			id := api.NewUUID()
			if tc.sameID {
				id = committedID
			}
			if err := InitProject(markerPath, nil, InitProjectOpts{SkipRuntimeCheck: true, ProjectID: id}); err != nil {
				t.Fatalf("InitProject: %v", err)
			}
			if !IsProjectMarkerFile(markerPath) {
				t.Fatal("expected the committed marker file to stay a marker file")
			}
			marker, err := ReadProjectMarker(markerPath)
			if err != nil {
				t.Fatal(err)
			}
			if marker.ProjectID != id {
				t.Errorf("marker project ID = %q, want %q", marker.ProjectID, id)
			}
			ext, err := marker.ExternalProjectPath()
			if err != nil {
				t.Fatal(err)
			}
			if info, err := os.Stat(ext); err != nil || !info.IsDir() {
				t.Errorf("expected the external project directory %s, stat err = %v", ext, err)
			}
		})
	}
}

func TestInitProject_CurrentDirRepoStillUpdatesGitignore(t *testing.T) {
	_, repo := ownershipTestEnvNamed(t, "repo")
	setupGitRepoDir(t, repo)
	t.Chdir(repo)

	if err := InitProject("", nil, InitProjectOpts{SkipRuntimeCheck: true}); err != nil {
		t.Fatalf("InitProject: %v", err)
	}
	info, err := os.Lstat(filepath.Join(repo, DotScion))
	if err != nil || !info.IsDir() {
		t.Fatalf("expected an in-repo %s directory, stat err = %v", DotScion, err)
	}
	got, err := os.ReadFile(filepath.Join(repo, ".gitignore"))
	if err != nil {
		t.Fatalf("expected a .gitignore in the repository: %v", err)
	}
	if !strings.Contains(string(got), ".scion/agents") {
		t.Errorf(".gitignore = %q, want an entry for .scion/agents", got)
	}
}

// Relative targets resolve against the current directory, and a target in
// a subdirectory of a repository takes the in-repo layout without touching
// the repository's .gitignore.
func TestInitProject_ExplicitTargetRelativeAndSubdirectory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		inRepo  bool   // whether the cwd is the repository (else a plain dir)
		target  string // relative to the cwd unless abs is set
		abs     bool   // join target onto the cwd to make it absolute
		wantDir bool   // in-repo layout (directory) rather than a marker file
	}{
		{name: "relative target in a plain directory", target: DotScion, wantDir: false},
		{name: "relative target in a repository", inRepo: true, target: DotScion, wantDir: true},
		{name: "relative subdirectory target in a repository", inRepo: true, target: filepath.Join("sub", DotScion), wantDir: true},
		{name: "absolute subdirectory target in a repository", inRepo: true, target: filepath.Join("sub", DotScion), abs: true, wantDir: true},
		{name: "relative target with a trailing separator in a repository", inRepo: true, target: DotScion + string(filepath.Separator), wantDir: true},
		{name: "absolute target with a trailing separator in a repository", inRepo: true, target: DotScion + string(filepath.Separator), abs: true, wantDir: true},
		{name: "relative target with a trailing separator in a plain directory", target: DotScion + string(filepath.Separator), wantDir: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, repo := ownershipTestEnvNamed(t, "repo")
			cwd := plainDir(t, home, "plain")
			if tc.inRepo {
				setupGitRepoDir(t, repo)
				cwd = repo
			}
			if err := os.MkdirAll(filepath.Join(cwd, "sub"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Chdir(cwd)

			target := tc.target
			if tc.abs {
				// Not filepath.Join: it would drop a trailing separator.
				target = cwd + string(filepath.Separator) + target
			}
			id := api.NewUUID()
			if err := InitProject(target, nil, InitProjectOpts{SkipRuntimeCheck: true, ProjectID: id}); err != nil {
				t.Fatalf("InitProject(%q): %v", target, err)
			}
			full := filepath.Join(cwd, tc.target)
			if tc.wantDir {
				assertInRepoProjectID(t, full, id)
			} else if !IsProjectMarkerFile(full) {
				t.Fatalf("expected a marker file at %s", full)
			}
			if _, err := os.Lstat(filepath.Join(cwd, ".gitignore")); !os.IsNotExist(err) {
				t.Errorf("expected no .gitignore written for an explicit target, stat err = %v", err)
			}
		})
	}
}

// A symbolic link at an explicit target is refused, and nothing is written
// through it.
func TestInitProject_ExplicitTargetSymlinkRefused(t *testing.T) {
	for _, tc := range []struct {
		name          string
		toFile        bool
		trailingSlash bool
	}{
		{"link to a directory", false, false},
		{"link to a file", true, false},
		{"link to a directory, target with a trailing separator", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, clone := ownershipTestEnvNamed(t, "clone")
			setupGitRepoDir(t, clone)
			dest := filepath.Join(home, "outside")
			if tc.toFile {
				if err := os.WriteFile(dest, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(dest, 0o755); err != nil {
				t.Fatal(err)
			}
			scionDir := filepath.Join(clone, DotScion)
			if err := os.Symlink(dest, scionDir); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			gitCommitAll(t, clone)
			t.Chdir(plainDir(t, home, "elsewhere"))

			target := scionDir
			if tc.trailingSlash {
				target += string(filepath.Separator)
			}
			err := InitProject(target, nil, InitProjectOpts{SkipRuntimeCheck: true, ProjectID: api.NewUUID()})
			if err == nil || !strings.Contains(err.Error(), "is a symbolic link") {
				t.Fatalf("InitProject: got %v, want a symbolic link error", err)
			}
			if tc.toFile {
				got, err := os.ReadFile(dest)
				if err != nil {
					t.Fatal(err)
				}
				if len(got) != 0 {
					t.Errorf("link destination file changed to %q, want empty", got)
				}
			} else {
				entries, err := os.ReadDir(dest)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 0 {
					t.Errorf("link destination has %d entries, want none", len(entries))
				}
			}
			if info, err := os.Lstat(scionDir); err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Errorf("expected %s to stay a symbolic link, stat err = %v", scionDir, err)
			}
		})
	}
}

// Entries of a committed .scion directory that are symbolic links leading
// out of it are refused: InitProject reports an error and leaves everything
// outside the project directory unchanged.
func TestInitProject_ExplicitTargetEntryLinksOutsideRefused(t *testing.T) {
	otherID := api.NewUUID()
	for _, tc := range []struct {
		name  string
		entry string // entry of .scion that is a link to dest
		// destKind is what exists at dest before InitProject: "none",
		// "file" (holding otherID) or "dir" (empty).
		destKind string
		// wantStep is the setup step the error must name.
		wantStep string
	}{
		{"project-id link to a missing path", "project-id", "none", "failed to read project-id"},
		{"project-id link to a file holding another project ID", "project-id", "file", "failed to read project-id"},
		{"agents link to a directory", "agents", "dir", "failed to create agents directory"},
		{"agents link to a missing path", "agents", "none", "failed to create agents directory"},
		{"templates link to a directory", "templates", "dir", "failed to create templates directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, clone := ownershipTestEnvNamed(t, "clone")
			setupGitRepoDir(t, clone)
			outside := plainDir(t, home, "outside")
			dest := filepath.Join(outside, "dest")
			switch tc.destKind {
			case "file":
				if err := os.WriteFile(dest, []byte(otherID+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "dir":
				if err := os.Mkdir(dest, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			scionDir := filepath.Join(clone, DotScion)
			if err := os.Mkdir(scionDir, 0o755); err != nil {
				t.Fatal(err)
			}
			// Keep the directory in the commit even when the link is the
			// only other entry.
			if err := os.WriteFile(filepath.Join(scionDir, "README"), []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(dest, filepath.Join(scionDir, tc.entry)); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			gitCommitAll(t, clone)
			t.Chdir(plainDir(t, home, "elsewhere"))

			err := InitProject(scionDir, nil, InitProjectOpts{SkipRuntimeCheck: true, ProjectID: api.NewUUID()})
			if err == nil || !strings.Contains(err.Error(), "cannot initialize project: "+tc.wantStep) {
				t.Fatalf("InitProject: got %v, want a cannot initialize project error naming %q", err, tc.wantStep)
			}
			// The refusal must come from the os.Root opened on the project
			// directory, which names the entry relative to that directory,
			// not from a call that resolves the full path.
			var pe *fs.PathError
			if !errors.As(err, &pe) || pe.Path != tc.entry {
				t.Errorf("InitProject: got %v, want a path error for the entry %q relative to the project directory", err, tc.entry)
			}

			switch tc.destKind {
			case "none":
				if _, err := os.Lstat(dest); !os.IsNotExist(err) {
					t.Errorf("expected nothing created at the link destination, stat err = %v", err)
				}
			case "file":
				got, err := os.ReadFile(dest)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != otherID+"\n" {
					t.Errorf("link destination file changed to %q, want %q", got, otherID+"\n")
				}
			case "dir":
				entries, err := os.ReadDir(dest)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 0 {
					t.Errorf("link destination has %d entries, want none", len(entries))
				}
			}
			entries, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.destKind != "none" {
				want = 1
			}
			if len(entries) != want {
				t.Errorf("directory outside the project has %d entries, want %d", len(entries), want)
			}
			if info, err := os.Lstat(filepath.Join(scionDir, tc.entry)); err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Errorf("expected %s to stay a symbolic link, stat err = %v", tc.entry, err)
			}
		})
	}
}
