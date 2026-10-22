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
	"sort"
	"strings"
)

// StagedApplyOptions controls ApplyStagedWorkspace.
type StagedApplyOptions struct {
	// GitData also applies the git-data class (see
	// ClassifyWorkspaceSyncPath) from the staging directory into the
	// destination's root .git directory, only when that .git is a real
	// directory or absent (then it is created). Objects are only ever
	// added. The ref state (HEAD, packed-refs, index, shallow, refs/**) is
	// applied as one unit: when staging carries a repository (it has
	// .git/HEAD), ref state files the staging directory does not have are
	// removed from the destination, so a ref the source packed or deleted
	// does not come back.
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

// ErrReservedEntry marks a write refused because the destination resolves
// its target to an existing .git or .scion entry under another name (a
// case-insensitive or otherwise name-folding filesystem).
var ErrReservedEntry = errors.New("path resolves to a reserved entry")

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

// copyChunk is the buffer size of streamed copies and comparisons.
const copyChunk = 64 << 10

// fileSource is the content of one file to write.
type fileSource struct {
	size int64
	open func() (io.ReadCloser, error)
}

// stagedSource returns the file at p as a fileSource.
func stagedSource(p string, size int64) fileSource {
	return fileSource{size: size, open: func() (io.ReadCloser, error) { return os.Open(p) }}
}

// bytesSource returns data as a fileSource.
func bytesSource(data []byte) fileSource {
	return fileSource{size: int64(len(data)), open: func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}}
}

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
//   - No write lands on an existing .git or .scion entry reached under
//     another name (a name-folding filesystem): the target is compared
//     with those entries before it is opened or replaced.
//   - Files are streamed to a fresh temporary name (O_EXCL) and renamed
//     over the target, so a symlink at the target is replaced, not written
//     through. An existing file keeps its permission bits; a new one gets
//     the staged file's. Unchanged files (same size and content, compared
//     in chunks) are not rewritten.
//   - After the writes, content files in dest that staging does not have
//     are removed, and content directories that staging does not have are
//     removed once empty. With opts.GitData, the same applies to the ref
//     state (see StagedApplyOptions). Paths outside these classes (the
//     .git configuration, .scion, .DS_Store), objects, and symlinks are
//     never removed.
//
// staging is a private directory the caller filled (rclone with the
// workspace sync filters); dest must already exist.
func ApplyStagedWorkspace(staging, dest string, opts StagedApplyOptions) (StagedApplyStats, error) {
	var stats StagedApplyStats

	type stagedFile struct {
		rel  string
		full string
		mode fs.FileMode
		size int64
	}
	var contentFiles, gitFiles []stagedFile
	contentDirs := map[string]bool{}
	keep := map[string]bool{}
	carriesRepo := false

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
		if rel == dotGit+"/shallow" && !validShallowFile(p, info.Size()) {
			stats.Refused++
			return nil
		}
		f := stagedFile{rel: rel, full: p, mode: info.Mode().Perm(), size: info.Size()}
		keep[rel] = true
		if class == WorkspaceSyncGitData {
			gitFiles = append(gitFiles, f)
			if rel == dotGit+"/HEAD" {
				carriesRepo = true
			}
		} else {
			contentFiles = append(contentFiles, f)
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
		written, err := writeConfined(root, f.rel, stagedSource(f.full, f.size), f.mode, replaceLinks)
		if err != nil {
			return stats, err
		}
		if written {
			stats.Written++
		}
	}
	// Empty directories that staging has are created, as rclone creates
	// directories holding files.
	for rel := range contentDirs {
		dir, err := openDirChain(root, rel, true, replaceLinks)
		if err != nil {
			return stats, err
		}
		_ = dir.Close()
	}

	applyGitState := false
	if opts.GitData && len(gitFiles) > 0 {
		ok, err := gitDirUsable(root)
		if err != nil {
			return stats, err
		}
		if !ok {
			stats.GitDataSkipped = true
		} else {
			// Objects first, then the ref state that points at them.
			sort.SliceStable(gitFiles, func(i, j int) bool {
				return IsWorkspaceGitObject(gitFiles[i].rel, false) && !IsWorkspaceGitObject(gitFiles[j].rel, false)
			})
			for _, f := range gitFiles {
				written, err := writeConfined(root, f.rel, stagedSource(f.full, f.size), f.mode, replaceLinks)
				if err != nil {
					return stats, err
				}
				if written {
					stats.Written++
				}
			}
			applyGitState = carriesRepo
		}
	}

	deleted, err := deleteExtraneous(root, ".", keep, contentDirs, IsWorkspaceSyncContent)
	stats.Deleted += deleted
	if err != nil {
		return stats, err
	}
	if applyGitState {
		deleted, err := deleteExtraneous(root, dotGit, keep, nil, IsWorkspaceGitState)
		stats.Deleted += deleted
		if err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// maxShallowBytes bounds a staged .git/shallow file.
const maxShallowBytes = 4 << 20

// validShallowFile reports whether the staged shallow file at p holds only
// object ids: lines of exactly 40 or 64 lowercase hex digits, each ended by
// a newline.
func validShallowFile(p string, size int64) bool {
	if size <= 0 || size > maxShallowBytes {
		return false
	}
	data, err := os.ReadFile(p)
	if err != nil || len(data) == 0 || data[len(data)-1] != '\n' {
		return false
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if len(line) != 40 && len(line) != 64 {
			return false
		}
		for _, c := range line {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
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

// reservedNames are the entries a write must never reach under another
// name.
var reservedNames = []string{dotGit, dotScion}

// aliasesReserved reports whether info, the entry name in dir, is an
// existing .git or .scion entry of dir reached under a different name (a
// filesystem that folds case or ignores characters in names).
func aliasesReserved(dir *os.Root, name string, info fs.FileInfo) bool {
	for _, r := range reservedNames {
		if name == r {
			continue
		}
		if ri, err := dir.Lstat(r); err == nil && os.SameFile(ri, info) {
			return true
		}
	}
	return false
}

// openDirChain opens the directory dirRel (slash-separated, "" or "." for
// the root) under root, one level at a time, never following a symlink.
// With create, missing levels are created, and a symlink at a level is
// replaced by a real directory (replaceLinks) or refused with
// ErrSymlinkInPath (refuseLinks). Without create, a missing level returns
// fs.ErrNotExist and a symlink returns ErrStagedApplyConflict. A level that
// is an existing .git or .scion reached under another name is refused with
// ErrReservedEntry. The caller closes the returned root.
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
	if aliasesReserved(cur, name, info) {
		return nil, ErrReservedEntry
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
		if aliasesReserved(cur, name, info) {
			return nil, ErrReservedEntry
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

// writeConfined writes src to the slash-separated path rel under root
// without following a symbolic link: directory levels are opened one at a
// time (openDirChain), and the file is streamed to a fresh temporary name
// and renamed into place. Missing directories are created. A link on the
// path is handled by links. A target that is an existing .git or .scion
// entry under another name is refused (ErrReservedEntry). An existing file
// keeps its permission bits and is not rewritten when it already holds the
// same content; a new file gets newMode. It reports whether the file was
// written.
func writeConfined(root *os.Root, rel string, src fileSource, newMode fs.FileMode, links linkPolicy) (bool, error) {
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
	case err == nil && aliasesReserved(dir, base, info):
		return false, fmt.Errorf("%s: %w", rel, ErrReservedEntry)
	case err == nil && info.IsDir():
		return false, fmt.Errorf("%s: %w", rel, ErrStagedApplyConflict)
	case err == nil && info.Mode()&fs.ModeSymlink != 0 && links == refuseLinks:
		return false, fmt.Errorf("%s: %w", rel, ErrSymlinkInPath)
	case err == nil && info.Mode().IsRegular():
		mode = info.Mode().Perm()
		if info.Size() == src.size {
			if same, serr := sameContent(dir, base, src); serr == nil && same {
				return false, nil
			}
		}
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return false, err
	}

	in, err := src.open()
	if err != nil {
		return false, fmt.Errorf("failed to read staged %s: %w", rel, err)
	}
	defer func() { _ = in.Close() }()

	tmp, err := createTemp(dir, mode)
	if err != nil {
		return false, fmt.Errorf("failed to write %s: %w", rel, err)
	}
	name := path.Base(tmp.Name())
	_, werr := io.CopyBuffer(tmp, in, make([]byte, copyChunk))
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		// The permission bits are set explicitly: the create mode is
		// filtered by the umask.
		werr = dir.Chmod(name, mode)
	}
	if werr == nil {
		// The target may have appeared since the Lstat above: check it
		// again right before replacing it.
		if cur, lerr := dir.Lstat(base); lerr == nil && (aliasesReserved(dir, base, cur) || cur.IsDir()) {
			werr = fmt.Errorf("%w", ErrReservedEntry)
			if cur.IsDir() {
				werr = ErrStagedApplyConflict
			}
		}
	}
	if werr == nil {
		werr = dir.Rename(name, base)
	}
	if werr != nil {
		_ = dir.Remove(name)
		return false, fmt.Errorf("failed to write %s: %w", rel, werr)
	}
	return true, nil
}

// sameContent reports whether the regular file name in dir holds the same
// bytes as src, compared in chunks.
func sameContent(dir *os.Root, name string, src fileSource) (bool, error) {
	f, err := dir.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != src.size {
		return false, err
	}
	in, err := src.open()
	if err != nil {
		return false, err
	}
	defer func() { _ = in.Close() }()
	a, b := make([]byte, copyChunk), make([]byte, copyChunk)
	for {
		na, ea := io.ReadFull(f, a)
		nb, eb := io.ReadFull(in, b)
		if na != nb || !bytes.Equal(a[:na], b[:nb]) {
			return false, nil
		}
		aDone := ea == io.EOF || ea == io.ErrUnexpectedEOF
		bDone := eb == io.EOF || eb == io.ErrUnexpectedEOF
		switch {
		case ea != nil && !aDone:
			return false, ea
		case eb != nil && !bDone:
			return false, eb
		case aDone || bDone:
			return aDone && bDone, nil
		}
	}
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

// keptFiles indexes the destination files a deletion pass keeps, by size,
// for os.SameFile lookups.
type keptFiles map[int64][]fs.FileInfo

// indexKept resolves every kept path in scope to its destination file.
func indexKept(root *os.Root, keep map[string]bool, inScope func(rel string, isDir bool) bool) keptFiles {
	k := keptFiles{}
	for rel := range keep {
		if !inScope(rel, false) {
			continue
		}
		if info, err := root.Lstat(rel); err == nil && info.Mode().IsRegular() {
			k[info.Size()] = append(k[info.Size()], info)
		}
	}
	return k
}

// has reports whether info is the same file as a kept destination file.
func (k keptFiles) has(info fs.FileInfo) bool {
	for _, ki := range k[info.Size()] {
		if os.SameFile(ki, info) {
			return true
		}
	}
	return false
}

// deleteExtraneous removes, under the directory start of root, the regular
// files in scope (inScope, by root-relative path) that are not in keep, and
// then the directories in scope that are not in keepDirs once they are
// empty (keepDirs nil: no directory is removed). Directories out of scope
// are not entered, and symlinks are never followed or removed. It returns
// the number of files removed.
func deleteExtraneous(root *os.Root, start string, keep, keepDirs map[string]bool, inScope func(rel string, isDir bool) bool) (int, error) {
	if start != "." {
		info, err := root.Lstat(start)
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		if !info.IsDir() {
			return 0, nil
		}
	}
	var extraneous, dirs []string
	err := fs.WalkDir(root.FS(), start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		if !inScope(p, d.IsDir()) {
			if d.IsDir() && p != start && !strings.HasPrefix(start+"/", p+"/") {
				return fs.SkipDir
			}
			return nil
		}
		switch {
		case d.Type().IsRegular() && !keep[p]:
			extraneous = append(extraneous, p)
		case d.IsDir() && keepDirs != nil && !keepDirs[p]:
			dirs = append(dirs, p)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("failed to scan workspace: %w", err)
	}
	// A destination entry that is the same file as a kept entry is kept: on
	// a case-insensitive (or otherwise name-folding) filesystem, a kept
	// path spelled differently (a case-only rename) resolves to it.
	var kept keptFiles
	if len(extraneous) > 0 {
		kept = indexKept(root, keep, inScope)
	}
	deleted := 0
	for _, rel := range extraneous {
		dirRel, base := path.Split(rel)
		dir, err := openDirChain(root, strings.TrimSuffix(dirRel, "/"), false, replaceLinks)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrStagedApplyConflict) || errors.Is(err, ErrReservedEntry) {
			continue
		}
		if err != nil {
			return deleted, err
		}
		info, err := dir.Lstat(base)
		if err == nil && info.Mode().IsRegular() && !aliasesReserved(dir, base, info) && !kept.has(info) {
			if err := dir.Remove(base); err != nil && !errors.Is(err, fs.ErrNotExist) {
				_ = dir.Close()
				return deleted, fmt.Errorf("failed to remove %s: %w", rel, err)
			}
			deleted++
		}
		_ = dir.Close()
	}
	// Deepest first, so a parent is empty once its children are gone.
	sort.Slice(dirs, func(i, j int) bool { return strings.Count(dirs[i], "/") > strings.Count(dirs[j], "/") })
	for _, rel := range dirs {
		dirRel, base := path.Split(rel)
		dir, err := openDirChain(root, strings.TrimSuffix(dirRel, "/"), false, replaceLinks)
		if err != nil {
			continue
		}
		if info, err := dir.Lstat(base); err == nil && info.IsDir() && !aliasesReserved(dir, base, info) {
			// Remove fails on a directory that is not empty; that is
			// expected (it holds excluded entries or symlinks) and ignored.
			_ = dir.Remove(base)
		}
		_ = dir.Close()
	}
	return deleted, nil
}
