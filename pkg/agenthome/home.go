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

// Package agenthome writes into and removes from an agent home directory,
// confined to that home.
//
// An agent home is mounted into the agent's container, so its contents can
// change between the broker's operations. A Home is opened once, by the
// broker-side path of the home, and every later operation is resolved
// relative to that open directory: each directory component below the home
// is opened without following a symbolic link (a linked directory
// component is refused, never traversed), files are installed by creating
// a temporary file and renaming it over the leaf (an entry at the leaf,
// a link included, is replaced, never written through), and recursive
// removal never descends through a link. On unix this is built on the
// pkg/sciontool/dirfd primitives. Other platforms (none of which runs a
// broker) use an os.Root opened on the home with an Lstat check of every
// directory component: every operation stays inside the home, but the
// no-follow checks there are check-then-act rather than descriptor-based,
// and their errors may name paths relative to the home.
//
// Reads (ReadFile) refuse a link at any component or at the leaf; on unix
// they also refuse a file with more than one hard link.
//
// Paths passed to a Home are relative to the home, slash or OS separated.
// An absolute path or one with a ".." component is refused.
package agenthome

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ErrLinkComponent is reported (inside an *fs.PathError naming the entry)
// when a directory component of a path below the home is a symbolic link.
var ErrLinkComponent = errors.New("agenthome: refusing a symbolic link as a directory component")

// ErrNotDirectory is reported (inside an *fs.PathError naming the entry)
// when a directory component of a path below the home exists but is not a
// directory.
var ErrNotDirectory = errors.New("agenthome: directory component is not a directory")

// AgentInfoFile is the agent record file in the agent home.
const AgentInfoFile = "agent-info.json"

// MaxAgentInfoSize bounds a read of AgentInfoFile by ReadAgentInfo.
const MaxAgentInfoSize = 4 << 20

// ReadFileAt opens the agent home at homePath, reads rel (Home.ReadFile:
// no link followed at any component or at the leaf, a non-regular file such
// as a FIFO refused without blocking, and on unix a file with more than one
// hard link refused) up to max bytes, and closes the home. A missing home
// or file satisfies errors.Is(err, fs.ErrNotExist).
func ReadFileAt(homePath, rel string, max int64) ([]byte, error) {
	h, err := Open(homePath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = h.Close() }()
	return h.ReadFile(rel, max)
}

// ReadAgentInfo reads AgentInfoFile from the agent home at homePath with
// ReadFileAt, up to MaxAgentInfoSize bytes.
func ReadAgentInfo(homePath string) ([]byte, error) {
	return ReadFileAt(homePath, AgentInfoFile, MaxAgentInfoSize)
}

// Limits bounds a tree copied into the home by InstallTree. A zero field is
// not checked.
type Limits struct {
	// MaxFiles caps the number of entries: copied regular files and
	// directories, plus skipped links and special files.
	MaxFiles int
	// MaxBytes caps the total size of copied regular files.
	MaxBytes int64
	// MaxDepth caps directory nesting below the copied root.
	MaxDepth int
}

// LimitError reports that InstallTree exceeded its Limits.
type LimitError struct{ Limit string }

func (e *LimitError) Error() string { return "tree exceeds " + e.Limit }

// OpenCreate is Open that first creates the home directory at path (and
// any missing parents) with mode when it does not exist, as os.MkdirAll
// would. path is the broker-side path of the home, which lies in the
// broker-owned agent directory; nothing below the home is created here.
func OpenCreate(path string, mode os.FileMode) (*Home, error) {
	if err := os.MkdirAll(path, mode); err != nil {
		return nil, err
	}
	return Open(path)
}

// splitRel validates rel and returns its cleaned components. "." (the home
// itself) yields no components.
func splitRel(rel string) ([]string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return nil, fmt.Errorf("agenthome: %q is not a relative path", rel)
	}
	// Check the raw components before cleaning, so "a/../b" is refused too.
	for _, p := range strings.Split(filepath.ToSlash(rel), "/") {
		if p == ".." {
			return nil, fmt.Errorf("agenthome: %q does not stay below the home", rel)
		}
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	if clean == "." {
		return nil, nil
	}
	return strings.Split(clean, "/"), nil
}

// splitLeaf validates rel, which must name an entry strictly below the
// home, and returns its parent components and leaf.
func splitLeaf(rel string) (parent []string, leaf string, err error) {
	parts, err := splitRel(rel)
	if err != nil {
		return nil, "", err
	}
	if len(parts) == 0 {
		return nil, "", fmt.Errorf("agenthome: %q does not name an entry below the home", rel)
	}
	return parts[:len(parts)-1], parts[len(parts)-1], nil
}

// pathErr wraps err as an *fs.PathError naming home/parts, so callers keep
// errors.Is(err, fs.ErrPermission) and fs.ErrNotExist checks and see the
// broker-side path.
func pathErr(op, home string, parts []string, err error) error {
	return &fs.PathError{Op: op, Path: filepath.Join(append([]string{home}, parts...)...), Err: err}
}

// tempName returns a new hidden name next to leaf, for an entry that is
// renamed over leaf once complete.
func tempName(leaf string) (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	return "." + leaf + ".tmp-" + hex.EncodeToString(suffix[:]), nil
}

// copyMode is the mode InstallTree gives a copied regular file: 0644, or
// 0755 when any execute bit is set in the source.
func copyMode(src fs.FileMode) fs.FileMode {
	if src.Perm()&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

// SkipCopyName reports whether a tree copy skips the entry name: a
// __pycache__ directory or a .pyc file (Python bytecode, rebuilt on use).
// It is the one rule InstallTreeFrom, InstallTreeFS, OverlayTreeFrom and
// the agent's skills directory carry-over share.
func SkipCopyName(name string, isDir bool) bool {
	if isDir {
		return name == "__pycache__"
	}
	return strings.HasSuffix(name, ".pyc")
}

// homeFileMode is the mode InstallTreeFrom, InstallTreeFS and
// OverlayTreeFrom give a copied regular file. A file nobody but its owner
// can read stays owner-only (0600, or 0700 when any execute bit is set);
// any other file gets 0644, or 0755 when any execute bit is set. setuid,
// setgid, sticky and group or other write bits are never copied.
func homeFileMode(src fs.FileMode) fs.FileMode {
	p := src.Perm()
	exec := p&0o111 != 0
	if p&0o044 == 0 {
		if exec {
			return 0o700
		}
		return 0o600
	}
	if exec {
		return 0o755
	}
	return 0o644
}

// CopyFileMode is the mode the tree copies give a regular file whose
// source mode is src (homeFileMode), for a caller that copies a single
// file into a home with Home.WriteFile under the same rule.
func CopyFileMode(src fs.FileMode) fs.FileMode { return homeFileMode(src) }

// homeDirMode is the mode those copies give a directory they create: 0700
// when the source directory has no group or other bits, otherwise 0755.
func homeDirMode(src fs.FileMode) fs.FileMode {
	if src.Perm()&0o077 == 0 {
		return 0o700
	}
	return 0o755
}

// treeBudget tracks InstallTree's Limits.
type treeBudget struct {
	lim     Limits
	entries int
	bytes   int64
}

func (b *treeBudget) entry() error {
	b.entries++
	if b.lim.MaxFiles > 0 && b.entries > b.lim.MaxFiles {
		return &LimitError{Limit: fmt.Sprintf("%d files", b.lim.MaxFiles)}
	}
	return nil
}

func (b *treeBudget) depth(d int) error {
	if b.lim.MaxDepth > 0 && d > b.lim.MaxDepth {
		return &LimitError{Limit: fmt.Sprintf("a directory depth of %d", b.lim.MaxDepth)}
	}
	return nil
}

// remainingBytes is how many more bytes may be copied; -1 means unlimited.
func (b *treeBudget) remainingBytes() int64 {
	if b.lim.MaxBytes <= 0 {
		return -1
	}
	return b.lim.MaxBytes - b.bytes
}

func (b *treeBudget) addBytes(n int64) error {
	b.bytes += n
	if b.lim.MaxBytes > 0 && b.bytes > b.lim.MaxBytes {
		return &LimitError{Limit: formatBytes(b.lim.MaxBytes)}
	}
	return nil
}

// formatBytes formats a byte limit the way pkg/agentplugin does: whole MiB
// as "N MiB", otherwise "N bytes".
func formatBytes(n int64) string {
	if n >= 1<<20 && n%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%d bytes", n)
}

// readDirSorted lists the directory rel in src, sorted by name, so the copy
// order and the error reported on a failure do not depend on the
// filesystem.
func readDirSorted(src *os.Root, rel string) ([]fs.DirEntry, error) {
	d, err := src.Open(rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	entries, err := d.ReadDir(-1)
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, err
}

// SeedOptions configures Home.SeedTree.
type SeedOptions struct {
	// Exclude reports whether the source entry rel (slash-separated,
	// relative to the source root) is left out. An excluded directory is
	// not descended into. Nil excludes nothing.
	Exclude func(rel string) bool
	// UID and GID own every directory SeedTree creates and every file it
	// writes (fchown on the new entry's own descriptor). A failed chown
	// fails the copy.
	UID, GID int
	// Limits bounds the copy; exceeding one fails with a *LimitError.
	Limits Limits
}

// SeedResult counts what Home.SeedTree did. Skipped lists, by path
// relative to the source root, the links, special files and hard-linked
// files it skipped; Replaced lists the home entries that were links and
// were replaced.
type SeedResult struct {
	Dirs     int
	Files    int
	Bytes    int64
	Skipped  []string
	Replaced []string
}

// SeedConflictError reports that the home has a symbolic link or an entry
// of the wrong type where SeedTree would put a directory or a file. Rel is
// the entry's path relative to the home; it holds no host path.
type SeedConflictError struct {
	Rel    string
	Reason string
}

func (e *SeedConflictError) Error() string {
	return "agenthome: " + e.Rel + ": " + e.Reason
}

// seedFileMode is the mode SeedTree gives a copied file: the source's
// permission bits capped at 0755 when any execute bit is set and at 0644
// otherwise. Setuid, setgid and sticky never survive (Perm drops them).
func seedFileMode(src fs.FileMode) fs.FileMode {
	perm := src.Perm()
	if perm&0o111 != 0 {
		return perm & 0o755
	}
	return perm & 0o644
}

// seedDirMode is the mode of a directory SeedTree creates.
const seedDirMode fs.FileMode = 0o700
