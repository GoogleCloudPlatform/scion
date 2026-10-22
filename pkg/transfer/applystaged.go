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

package transfer

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// StagedApplyOptions controls ApplyStagedWorkspace.
type StagedApplyOptions struct {
	// GitData also applies the git-data class (see
	// ClassifyWorkspaceSyncPath) from the staging directory into the
	// destination's root .git directory. Git data is never deleted from
	// the destination, and it is applied only when the destination's .git
	// is a real directory or absent (then it is created).
	GitData bool
}

// StagedApplyStats reports what ApplyStagedWorkspace did.
type StagedApplyStats struct {
	// Written is the number of files written (new or changed).
	Written int
	// Deleted is the number of destination files removed because the
	// staging directory no longer has them.
	Deleted int
	// Refused is the number of staged entries that were not applied
	// because their class does not allow it (for example .git/config or a
	// nested .scion entry left in the bucket by an earlier upload).
	Refused int
	// GitDataSkipped is set when git data was staged but not applied
	// because the destination's .git is not a real directory.
	GitDataSkipped bool
}

// ErrStagedApplyConflict marks an apply that met a destination entry of the
// wrong type: a regular file where the staging directory has a directory,
// or a directory where it has a file. The apply fails rather than replace
// either, as rclone's sync does.
var ErrStagedApplyConflict = errors.New("destination entry has the wrong type")

// ErrSymlinkInPath marks a write refused because its path crosses a
// symbolic link (a directory level or the file itself) in a destination
// whose links are kept (see linkPolicy).
var ErrSymlinkInPath = errors.New("path crosses a symbolic link")

// errSwapped marks a directory that changed between its Lstat and its open.
var errSwapped = errors.New("destination directory changed during apply")

// linkPolicy says what a confined write does with a symbolic link on its
// path. Neither policy ever follows one.
type linkPolicy int

const (
	// replaceLinks replaces a link at a directory level with a real
	// directory, and a link at the file with the file (a workspace apply
	// makes the destination match the source).
	replaceLinks linkPolicy = iota
	// refuseLinks fails the write with ErrSymlinkInPath (a user's checkout,
	// whose links are the user's).
	refuseLinks
)

// ApplyStagedWorkspace makes the workspace at dest match the staging
// directory staging, the way an rclone sync of the workspace would, but
// confined to dest:
//
//   - Only paths that ClassifyWorkspaceSyncPath allows are applied: content
//     always, git data only with opts.GitData. Anything else found in
//     staging is counted in Refused and left out.
//   - Every directory level of dest is opened as its own os.Root, after an
//     Lstat that shows a real directory and with a check that the opened
//     directory is the one that Lstat saw. A symlink is therefore never
//     followed, including one swapped in while the apply runs. A symlink
//     where staging has a directory is replaced by a real directory.
//   - Files are written to a fresh temporary name (O_EXCL) and renamed over
//     the target, so a symlink at the target is replaced, not written
//     through. An existing file keeps its permission bits; a new one gets
//     the staged file's.
//   - Unchanged files (same content) are not rewritten.
//   - After the writes, content files in dest that staging does not have
//     are removed. Paths outside the content class (.git, .scion,
//     .DS_Store) and symlinks are never removed, and directories are left
//     in place.
//
// staging is a private directory the caller filled (rclone with the
// workspace sync filters); dest must already exist.
func ApplyStagedWorkspace(staging, dest string, opts StagedApplyOptions) (StagedApplyStats, error) {
	var stats StagedApplyStats

	type stagedFile struct {
		rel  string
		full string
		mode fs.FileMode
	}
	var contentFiles, gitFiles []stagedFile
	contentDirs := map[string]bool{}
	staged := map[string]bool{}

	err := filepath.WalkDir(staging, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(staging, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		class := ClassifyWorkspaceSyncPath(rel, d.IsDir())
		allowed := class == WorkspaceSyncContent || (opts.GitData && class == WorkspaceSyncGitData)
		if !allowed {
			stats.Refused++
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if class == WorkspaceSyncContent {
				contentDirs[rel] = true
			}
			return nil
		}
		if !d.Type().IsRegular() {
			stats.Refused++
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		f := stagedFile{rel: rel, full: p, mode: info.Mode().Perm()}
		if class == WorkspaceSyncGitData {
			gitFiles = append(gitFiles, f)
		} else {
			contentFiles = append(contentFiles, f)
			staged[rel] = true
		}
		return nil
	})
	if err != nil {
		return stats, fmt.Errorf("failed to read staged workspace: %w", err)
	}

	root, err := os.OpenRoot(dest)
	if err != nil {
		return stats, fmt.Errorf("failed to open workspace: %w", err)
	}
	defer func() { _ = root.Close() }()

	for _, f := range contentFiles {
		written, err := writeStaged(root, f.rel, f.full, f.mode)
		if err != nil {
			return stats, err
		}
		if written {
			stats.Written++
		}
	}
	// Empty directories that staging has are created, as rclone creates
	// directories holding files (directories are never removed).
	for rel := range contentDirs {
		dir, err := openDirChain(root, rel, true, replaceLinks)
		if err != nil {
			return stats, err
		}
		_ = dir.Close()
	}

	if opts.GitData && len(gitFiles) > 0 {
		ok, err := gitDirUsable(root)
		if err != nil {
			return stats, err
		}
		if !ok {
			stats.GitDataSkipped = true
		} else {
			for _, f := range gitFiles {
				written, err := writeStaged(root, f.rel, f.full, f.mode)
				if err != nil {
					return stats, err
				}
				if written {
					stats.Written++
				}
			}
		}
	}

	deleted, err := deleteExtraneous(root, staged)
	stats.Deleted = deleted
	if err != nil {
		return stats, err
	}
	return stats, nil
}

// gitDirUsable reports whether git data may be applied: the destination's
// root .git is a real directory, or absent (it is then created). A .git
// file (a worktree's or submodule's gitdir pointer) or a symlink is left
// alone.
func gitDirUsable(root *os.Root) (bool, error) {
	info, err := root.Lstat(dotGit)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := root.Mkdir(dotGit, 0o755); err != nil {
			return false, fmt.Errorf("failed to create %s: %w", dotGit, err)
		}
		return true, nil
	case err != nil:
		return false, fmt.Errorf("failed to inspect %s: %w", dotGit, err)
	}
	return info.IsDir(), nil
}

// openDirChain opens the directory dirRel (slash-separated, "" for the
// root) under root, one level at a time, never following a symlink. With
// create, missing levels are created, and a symlink at a level is replaced
// by a real directory (replaceLinks) or refused with ErrSymlinkInPath
// (refuseLinks). Without create, a missing level returns fs.ErrNotExist and
// a symlink returns ErrStagedApplyConflict. The caller closes the returned
// root.
func openDirChain(root *os.Root, dirRel string, create bool, links linkPolicy) (*os.Root, error) {
	cur, err := root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	if dirRel == "" || dirRel == "." {
		return cur, nil
	}
	for _, name := range strings.Split(dirRel, "/") {
		next, err := openLevel(cur, name, create, links)
		_ = cur.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", dirRel, err)
		}
		cur = next
	}
	return cur, nil
}

// openLevel opens the directory name directly under cur (see openDirChain).
func openLevel(cur *os.Root, name string, create bool, links linkPolicy) (*os.Root, error) {
	info, err := cur.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) && create {
		if err := cur.Mkdir(name, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		info, err = cur.Lstat(name)
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		if !create {
			return nil, ErrStagedApplyConflict
		}
		if links == refuseLinks {
			return nil, ErrSymlinkInPath
		}
		if err := cur.Remove(name); err != nil {
			return nil, err
		}
		if err := cur.Mkdir(name, 0o755); err != nil {
			return nil, err
		}
		if info, err = cur.Lstat(name); err != nil {
			return nil, err
		}
	}
	if !info.IsDir() {
		return nil, ErrStagedApplyConflict
	}
	next, err := cur.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := next.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		_ = next.Close()
		return nil, errSwapped
	}
	return next, nil
}

// writeStaged writes the staged file src to rel under root (see
// ApplyStagedWorkspace). It reports whether the file was written.
func writeStaged(root *os.Root, rel, src string, stagedMode fs.FileMode) (bool, error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return false, fmt.Errorf("failed to read staged %s: %w", rel, err)
	}
	return writeConfined(root, rel, data, stagedMode, replaceLinks)
}

// writeConfined writes data to the slash-separated path rel under root
// without following a symbolic link: directory levels are opened one at a
// time (openDirChain), and the file is written to a fresh temporary name
// and renamed into place. Missing directories are created. A link on the
// path is handled by links. An existing file keeps its permission bits and
// is not rewritten when its content is already data; a new file gets
// newMode. It reports whether the file was written.
func writeConfined(root *os.Root, rel string, data []byte, newMode fs.FileMode, links linkPolicy) (bool, error) {
	dirRel, base := path.Split(rel)
	dirRel = strings.TrimSuffix(dirRel, "/")
	dir, err := openDirChain(root, dirRel, true, links)
	if err != nil {
		return false, err
	}
	defer func() { _ = dir.Close() }()

	mode := newMode
	info, err := dir.Lstat(base)
	switch {
	case err == nil && info.IsDir():
		return false, fmt.Errorf("%s: %w", rel, ErrStagedApplyConflict)
	case err == nil && info.Mode()&fs.ModeSymlink != 0 && links == refuseLinks:
		return false, fmt.Errorf("%s: %w", rel, ErrSymlinkInPath)
	case err == nil && info.Mode().IsRegular():
		mode = info.Mode().Perm()
		if same, serr := sameContent(dir, base, data); serr == nil && same {
			return false, nil
		}
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return false, err
	}

	tmp, err := createTemp(dir, mode)
	if err != nil {
		return false, fmt.Errorf("failed to write %s: %w", rel, err)
	}
	name := tmp.Name()
	_, werr := io.Copy(tmp, bytes.NewReader(data))
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		// The permission bits are set explicitly: the create mode is
		// filtered by the umask.
		werr = dir.Chmod(path.Base(name), mode)
	}
	if werr == nil {
		werr = dir.Rename(path.Base(name), base)
	}
	if werr != nil {
		_ = dir.Remove(path.Base(name))
		return false, fmt.Errorf("failed to write %s: %w", rel, werr)
	}
	return true, nil
}

// sameContent reports whether the regular file name in dir holds data.
func sameContent(dir *os.Root, name string, data []byte) (bool, error) {
	f, err := dir.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(data)) {
		return false, err
	}
	have, err := io.ReadAll(io.LimitReader(f, int64(len(data))+1))
	if err != nil {
		return false, err
	}
	return bytes.Equal(have, data), nil
}

// createTemp creates a new file with a random name in dir. O_EXCL makes the
// create fail rather than follow anything already at that name.
func createTemp(dir *os.Root, mode fs.FileMode) (*os.File, error) {
	for range 10 {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		name := ".sync-tmp-" + hex.EncodeToString(b[:])
		f, err := dir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return f, err
	}
	return nil, errors.New("could not create a temporary file")
}

// deleteExtraneous removes the regular content files under root that are
// not in keep. Directories outside the content class are not entered, and
// symlinks are not followed or removed.
func deleteExtraneous(root *os.Root, keep map[string]bool) (int, error) {
	var extraneous []string
	err := fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		if !IsWorkspaceSyncContent(p, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() && !keep[p] {
			extraneous = append(extraneous, p)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("failed to scan workspace: %w", err)
	}
	deleted := 0
	for _, rel := range extraneous {
		dirRel, base := path.Split(rel)
		dir, err := openDirChain(root, strings.TrimSuffix(dirRel, "/"), false, replaceLinks)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrStagedApplyConflict) {
			continue
		}
		if err != nil {
			return deleted, err
		}
		info, err := dir.Lstat(base)
		if err == nil && info.Mode().IsRegular() {
			if err := dir.Remove(base); err != nil && !errors.Is(err, fs.ErrNotExist) {
				_ = dir.Close()
				return deleted, fmt.Errorf("failed to remove %s: %w", rel, err)
			}
			deleted++
		}
		_ = dir.Close()
	}
	return deleted, nil
}
