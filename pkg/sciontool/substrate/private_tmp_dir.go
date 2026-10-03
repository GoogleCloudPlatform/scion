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

package substrate

import (
	"errors"
	"fmt"
	"os"
)

// privateRootTmpDir is the dedicated, root-owned directory root uses to
// stage content it must both write and read back before installing the
// result into a workload-owned location. Never $TMPDIR or the system temp
// directory: on a runtime where root is a security boundary, that directory
// can be world-writable with no sticky bit, which lets the workload rename
// any entry out of it and plant a symlink in its place while root is still
// using it. A package var, not a const — same reasoning as enforcedHooksDir
// above: it lets a test point both the creation and the clear at a
// throwaway directory instead of the real, root-owned "/run/scion/tmp".
// Production code never reassigns it.
var privateRootTmpDir = "/run/scion/tmp"

// privateRootTmpDirMode is the mode privateRootTmpDir is created and kept
// at: root (or, in a rootless deployment, the caller's own uid) only, no
// group or other access at all. This directory holds a private working
// copy of files that must never be listable, let alone enterable, by
// anything else.
const privateRootTmpDirMode = 0o700

// ensurePrivateTmpDir creates privateRootTmpDir (and any missing ancestor —
// typically just "/run/scion", since "/run" itself always exists), forces it
// to this process's own ownership (os.Geteuid/os.Getegid — on substrate
// that's always root, since this handler only ever runs in PID 1 before any
// privilege drop; see dirfd.EnsureDirNoFollowRootOwned's doc comment for why
// a directory owned by "whoever is running this code" rather than a
// hardcoded uid 0 is the right check for a runtime with no separate
// root/workload boundary at all) and privateRootTmpDirMode regardless
// of whatever it was before, then clears any existing content the same way
// clearEnforcedHooksDir does for EnforcedHooksDir — so stale content from an
// earlier bootstrap of a reused actor, or a leftover from before this
// directory existed under this name, can never survive into this one.
//
// This uses the same safety model mkdirAllTracked's own doc comment
// describes: it runs during bootstrap, before RunInit — and so the
// workload — ever starts, so there is no concurrent writer for the plain
// Lstat-then-Mkdir sequence below to race against. That is what makes a
// plain (non-fd-anchored) os.Chown/os.Chmod safe to use here, unlike in
// configureSharedWorkspaceGit itself, which runs later, once RunInit (and so
// potentially the workload) is already active, and which is why THAT caller
// re-verifies this exact directory's entire chain by fd, component by
// component (dirfd.EnsureDirNoFollowRootOwned), rather than trusting that
// this function ran, or ran correctly, at some earlier point.
func ensurePrivateTmpDir() error {
	if _, err := mkdirAllTracked(privateRootTmpDir, 0o755); err != nil {
		var symErr *errSymlinkComponent
		if errors.As(err, &symErr) {
			return &bootstrapPathError{
				code:   codeBootstrapPathSymlink,
				path:   privateRootTmpDir,
				detail: "private root tmp directory chain contains a symlink",
			}
		}
		var dirErr *errNonDirComponent
		if errors.As(err, &dirErr) {
			return &bootstrapPathError{
				code:   codeBootstrapPathInvalid,
				path:   privateRootTmpDir,
				detail: "private root tmp directory chain contains a non-directory component",
			}
		}
		return fmt.Errorf("ensure %s: %w", privateRootTmpDir, err)
	}
	if err := os.Chown(privateRootTmpDir, os.Geteuid(), os.Getegid()); err != nil {
		return fmt.Errorf("chown %s: %w", privateRootTmpDir, err)
	}
	if err := os.Chmod(privateRootTmpDir, privateRootTmpDirMode); err != nil {
		return fmt.Errorf("chmod %s: %w", privateRootTmpDir, err)
	}
	return clearDirContents(privateRootTmpDir)
}

// SetPrivateRootTmpDirForTest overrides the directory ensurePrivateTmpDir
// creates, chowns, and clears at every bootstrap, for the duration of a test
// — including a test in another package that drives a real Server via
// NewServer (e.g. cmd/sciontool/commands' own bootstrap-wiring tests), which
// has no other way to reach this package's unexported privateRootTmpDir var.
// Mirrors pkg/sciontool/hub.SetTokenHome's shape. The real default
// ("/run/scion/tmp") requires root to create; a test that issues a real
// bootstrap request without calling this first gets ensurePrivateTmpDir's
// genuine, fail-closed error against an unwritable "/run", exactly as
// production would. Returns a cleanup function that restores the previous
// value.
func SetPrivateRootTmpDirForTest(dir string) func() {
	orig := privateRootTmpDir
	privateRootTmpDir = dir
	return func() { privateRootTmpDir = orig }
}
