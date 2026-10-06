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

//go:build !windows

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// A daemon supervisor started without an explicit working directory (for
// example a systemd unit with no WorkingDirectory=) leaves its child
// process with a cwd of "/". isRootWorkingDir is the guard
// chdirHomeIfAtFilesystemRoot uses to detect that case.
func TestIsRootWorkingDirDetectsRoot(t *testing.T) {
	require.True(t, isRootWorkingDir("/"))
}

func TestIsRootWorkingDirRejectsNonRoot(t *testing.T) {
	for _, wd := range []string{"/home/scion", "/home/scion/", "", "relative", "//double-slash-prefix"} {
		t.Run(wd, func(t *testing.T) {
			require.False(t, isRootWorkingDir(wd))
		})
	}
}

// resolvedWd returns the current working directory with symlinks resolved,
// so a comparison against a t.TempDir() (which on some platforms is itself
// behind a symlink) isn't thrown off by that.
func resolvedWd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	resolved, err := filepath.EvalSymlinks(wd)
	require.NoError(t, err)
	return resolved
}

func resolvedPath(t *testing.T, p string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(p)
	require.NoError(t, err)
	return resolved
}

// Starting at "/" (the state a systemd unit with no WorkingDirectory= leaves
// it in) must move the process to $HOME.
func TestChdirHomeIfAtFilesystemRootMovesFromRoot(t *testing.T) {
	t.Chdir("/")
	home := t.TempDir()
	t.Setenv("HOME", home)

	chdirHomeIfAtFilesystemRoot()

	require.Equal(t, resolvedPath(t, home), resolvedWd(t))
}

// A process that was never at "/" in the first place (the common case: the
// container images set WORKDIR, and daemon-mode children run in the global
// dir) must be left exactly where it was.
func TestChdirHomeIfAtFilesystemRootLeavesNonRootUnchanged(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", t.TempDir())

	chdirHomeIfAtFilesystemRoot()

	require.Equal(t, resolvedPath(t, dir), resolvedWd(t))
}

// A $HOME that is set but points at a missing directory must not turn this
// best-effort fallback into a crash or a hang: the function logs the failed
// chdir and returns, leaving the process at "/" rather than panicking or
// erroring.
func TestChdirHomeIfAtFilesystemRootHomeMissingStaysAtRoot(t *testing.T) {
	t.Chdir("/")
	t.Setenv("HOME", filepath.Join(t.TempDir(), "does-not-exist"))

	require.NotPanics(t, chdirHomeIfAtFilesystemRoot)

	require.Equal(t, "/", resolvedWd(t))
}

// An unset (or empty) $HOME is a different failure from the case above:
// os.UserHomeDir() itself returns an error, before any chdir is attempted.
// The helper must log that and return, leaving the process at "/", the same
// as every other failure path here.
func TestChdirHomeIfAtFilesystemRootHomeUnsetStaysAtRoot(t *testing.T) {
	t.Chdir("/")
	t.Setenv("HOME", "")

	var logged string
	require.NotPanics(t, func() {
		logged = captureLog(t, chdirHomeIfAtFilesystemRoot)
	})

	require.Equal(t, "/", resolvedWd(t))
	require.Contains(t, logged, "could not determine home directory")
}
