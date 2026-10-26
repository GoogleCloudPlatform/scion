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

// Package privatefs creates, verifies and replaces private files: files
// that only the current user may read, such as tokens, credentials and
// per-user state.
//
// There are two implementations behind one API:
//
//   - privatefs_unix.go is today's POSIX call sequence (O_NOFOLLOW opens,
//     0600/0700 modes, the Perm&077 and owner-uid checks), moved here
//     unchanged.
//   - privatefs_windows.go is separate Windows code. It sets no mode bits
//     and calls no Chmod: on Windows, Chmod only toggles the read-only
//     attribute and protects nothing. A private file is created with a
//     protected, owner-only DACL in its security descriptor at creation
//     (never create-then-tighten). Privacy is verified on the open handle
//     by reading its DACL. Every component below a Tree's anchor is
//     opened relative to its parent handle, and any reparse point
//     (symlink, junction or other tag) is refused, never followed.
//
// ForMode is the only translation of POSIX mode bits into a Protection.
package privatefs

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/GoogleCloudPlatform/scion/pkg/util/pathcanon"
)

// Protection is what a new file or directory gets.
type Protection int

const (
	// Inherit gives a new object the default protection. On unix that is
	// mode 0666 for files and 0777 for directories, less the umask. On
	// Windows it is the DACL inherited from the parent directory. Never
	// use it for a secret.
	Inherit Protection = iota
	// Private gives a new object owner-only access. On unix that is mode
	// 0600 for files and 0700 for directories. On Windows it is a
	// protected DACL granting full access to the current user and SYSTEM
	// only, with the current user set explicitly as the owner, applied in
	// the create call itself.
	Private
)

// ForMode is the ONLY translation of POSIX mode bits to a Protection: a
// mode granting nothing to group or others is Private, anything else is
// Inherit.
func ForMode(m fs.FileMode) Protection {
	if m&0o077 == 0 {
		return Private
	}
	return Inherit
}

// ErrReparsePoint is returned (wrapped) when a component at or below a
// Tree's anchor is a symlink, a junction or any other reparse point. On
// Windows such components are refused, never followed. On unix the
// leaf opens of Create, OpenPrivate, ReadPrivate and OpenDir refuse a
// symlink through O_NOFOLLOW (WriteFile follows one; see
// TreeOptions.FollowLinks).
var ErrReparsePoint = errors.New("symlink, junction or other reparse point refused inside a private tree")

// ErrNotPrivate is returned (wrapped) by CheckPrivate, CheckNoForeignWriters
// and the reading operations when a file or directory is accessible to
// someone other than the current user (and, on Windows, SYSTEM and
// Administrators), or is owned by someone else.
var ErrNotPrivate = errors.New("not private")

// ErrNoACLs is returned (wrapped) when a Private object is to be created on
// a volume that cannot store access control lists (FAT, exFAT, some network
// shares), so the object could not be protected. There is no fallback.
var ErrNoACLs = errors.New("the volume does not support access control lists")

// ErrHardLinked is returned (wrapped) when a file about to be written, or
// replaced on a path where a tree rule is relaxed, has more than one name
// (the one-name rule). A hard link is not a reparse point and
// is identical to its target under handle comparison, so no other rule
// catches it: a write through it would land in a file that also lives
// outside the tree. It is refused, never repaired: a second name cannot be
// seen from here. A link count that cannot be read is refused too.
var ErrHardLinked = errors.New("file has more than one name (hard link)")

// ErrReplacedConcurrently is returned (wrapped) when a file was opened to
// check its link count, but every time it was opened, its name had already
// been removed or given to another file. This happens when another writer
// renames a new file over the path or removes it. The check never accepts
// a file that has no name. It opens the path again a few times
// (replacedAttempts), and gives up with this error if the file keeps
// changing.
var ErrReplacedConcurrently = errors.New("file was replaced or removed while it was being checked")

// errNoNames marks a link-count check that found zero names on the opened
// file: the handle refers to a file that was unlinked after it was opened
// (a stale handle, not a hard link). It never leaves the package: the
// open is retried (retryReplaced), and ErrReplacedConcurrently reports a
// file that keeps changing.
var errNoNames = errors.New("file has no names (removed or replaced after it was opened)")

// replacedAttempts bounds how many times an open and link-count check runs
// while the opened file keeps turning out to have no names.
const replacedAttempts = 5

// testHookAfterLinkCheckOpen, when set (tests only), runs after a file has
// been opened for its link-count check and before the count is read. Tests
// use it to replace or remove the file at that moment.
var testHookAfterLinkCheckOpen func(name string)

func afterLinkCheckOpen(name string) {
	if testHookAfterLinkCheckOpen != nil {
		testHookAfterLinkCheckOpen(name)
	}
}

// retryReplaced runs attempt, which opens name and checks its link count,
// again while it fails with errNoNames, at most replacedAttempts times in
// all. Any other result, success or error, is returned as is. If every
// attempt finds no names, the file is refused with ErrReplacedConcurrently.
func retryReplaced(name string, attempt func() error) error {
	for i := 0; i < replacedAttempts; i++ {
		err := attempt()
		if !errors.Is(err, errNoNames) {
			return err
		}
	}
	return fmt.Errorf("refusing %s: it was replaced or removed each of the %d times it was opened: %w",
		name, replacedAttempts, ErrReplacedConcurrently)
}

// TreeOptions configure OpenTree.
type TreeOptions struct {
	// CreateAnchor creates the anchor and any missing parent directories.
	// Created directories get Private protection; existing ones are left
	// as they are.
	CreateAnchor bool

	// FollowLinks follows existing reparse points (symlinks, junctions)
	// at DIRECTORY components below the anchor instead of refusing them;
	// components that are missing are created with the protection asked
	// for. On Windows every EXISTING directory component below the anchor,
	// followed or not, must also pass CheckNoForeignWriters on its handle
	// (ErrNotPrivate otherwise), so nothing is created or written under a
	// directory another principal could swap entries in. On Windows leaf
	// files are never followed: Create, OpenPrivate, ReadPrivate,
	// WriteFile, Rename and Remove still refuse a reparse point at the
	// leaf. On unix it changes nothing: the unix MkdirAll already follows
	// directory symlinks, and leaves keep their unix behaviour (Create,
	// OpenPrivate and ReadPrivate open them O_NOFOLLOW and refuse a
	// symlink; WriteFile writes in place with os.WriteFile, which follows
	// a symlink at the leaf; Rename and Remove act on a symlink itself).
	//
	// It exists for ~/.ssh.
	// ~/.ssh is deliberately NOT a refuse-reparse tree: dotfile managers
	// (chezmoi, stow, yadm) symlink ~/.ssh or its subdirectories, so
	// refusing links there would break legitimate setups. Integrity is
	// decided on handles after following instead (the follow-then-verify
	// rule, see
	// ReplaceFollowing): CheckNoForeignWriters on the target's parent
	// directory handle and on the existing target; a foreign-owned target
	// or a foreign-writable parent is refused; a user-owned but too-open
	// target is replaced by a private temporary file through a handle
	// rename; the old security descriptor is never copied (a writable ssh
	// config is code execution via ProxyCommand). Never use FollowLinks for
	// a tree that holds secrets.
	//
	// Stated limit: the follow-then-verify rule protects the target, not
	// the mutability of the path to it. A followed link's target directory
	// is checked, but not the directories above it. A live swap is defeated
	// by the handle-based operations; a swap between sessions needs a
	// directory that passes the follow-then-verify rule, which another
	// non-admin user cannot produce. Where the target's parent is
	// writable by another principal, Warn reports it (see Warn); it is not
	// refused.
	FollowLinks bool

	// Warn, when not nil, receives a one-line diagnostic, once per
	// directory, when FollowLinks follows a link whose target's parent
	// directory grants another principal write or delete-child rights (the
	// CheckNoForeignWriters rules, owner set included). It never changes
	// the result of an operation. Windows only; on unix it is not called.
	Warn func(msg string)
}

// splitRel validates a path relative to a Tree's anchor and returns its
// components. rel is never absolute, and each component must pass
// pathcanon.ValidName, which also rejects "", "." and "..".
func splitRel(rel string) ([]string, error) {
	if rel == "" {
		return nil, &fs.PathError{Op: "privatefs", Path: rel, Err: errors.New("empty relative path")}
	}
	if isAbsLike(rel) {
		return nil, &fs.PathError{Op: "privatefs", Path: rel, Err: errors.New("path must be relative to the tree's anchor")}
	}
	var comps []string
	start := 0
	for i := 0; i <= len(rel); i++ {
		if i == len(rel) || isSeparator(rel[i]) {
			comps = append(comps, rel[start:i])
			start = i + 1
		}
	}
	for _, c := range comps {
		// ValidName rejects the empty component of "a//b" or "a/".
		if err := pathcanon.ValidName(c); err != nil {
			return nil, err
		}
	}
	return comps, nil
}
