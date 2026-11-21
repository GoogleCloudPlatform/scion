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

package agenthome

import (
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
)

// entryKind classifies a source entry of a tree copy.
type entryKind int

const (
	// kindSkip is an entry that is never copied or followed: a symbolic
	// link, a special file, or a regular file with more than one hard
	// link. It still counts toward Limits.MaxFiles.
	kindSkip entryKind = iota
	kindDir
	kindFile
)

// Reasons a tree copy skips a source entry, as reported by CheckTree.
const (
	skipLink     = "a symbolic link"
	skipSpecial  = "not a regular file or directory"
	skipHardLink = "a file with more than one hard link"
)

// srcEntry is one entry of a tree copy's source directory, classified
// without following a link.
type srcEntry struct {
	name string
	kind entryKind
	mode fs.FileMode
	size int64
	// skip says why a kindSkip entry is not copied (skipLink,
	// skipSpecial or skipHardLink); empty for other kinds.
	skip string
	// id identifies the entry seen when listing, so an entry replaced
	// before it is opened is refused; zero when the source has none.
	dev, ino uint64
}

// treeSource is the directory a tree copy reads from. entries lists it in
// name order with SkipCopyName entries left out; sub opens a kindDir entry
// and open a kindFile entry, each refusing an entry that is no longer the
// one listed.
type treeSource interface {
	dirMode() fs.FileMode
	entries() ([]srcEntry, error)
	sub(e srcEntry) (treeSource, error)
	open(e srcEntry) (io.ReadCloser, error)
	close() error
}

// fsSource is a treeSource over an fs.FS (the embedded platform skills,
// which hold no links).
type fsSource struct {
	fsys fs.FS
	dir  string
	mode fs.FileMode
}

func newFSSource(fsys fs.FS, dir string) (*fsSource, error) {
	dir = path.Clean(strings.ReplaceAll(dir, "\\", "/"))
	if !fs.ValidPath(dir) {
		return nil, fmt.Errorf("agenthome: %q is not a valid source path", dir)
	}
	fi, err := fs.Stat(fsys, dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("agenthome: %s is not a directory", dir)
	}
	return &fsSource{fsys: fsys, dir: dir, mode: fi.Mode()}, nil
}

func (s *fsSource) dirMode() fs.FileMode { return s.mode }

func (s *fsSource) entries() ([]srcEntry, error) {
	des, err := fs.ReadDir(s.fsys, s.dir)
	if err != nil {
		return nil, err
	}
	out := make([]srcEntry, 0, len(des))
	for _, d := range des {
		t := d.Type()
		if SkipCopyName(d.Name(), t.IsDir()) {
			continue
		}
		e := srcEntry{name: d.Name()}
		switch {
		case t.IsDir() || t.IsRegular():
			fi, err := d.Info()
			if err != nil {
				return nil, err
			}
			e.mode, e.size = fi.Mode(), fi.Size()
			e.kind = kindFile
			if t.IsDir() {
				e.kind = kindDir
			}
		default:
			e.kind = kindSkip
			e.skip = skipSpecial
			if t&fs.ModeSymlink != 0 {
				e.skip = skipLink
			}
		}
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b srcEntry) int { return strings.Compare(a.name, b.name) })
	return out, nil
}

func (s *fsSource) sub(e srcEntry) (treeSource, error) {
	return newFSSource(s.fsys, path.Join(s.dir, e.name))
}

func (s *fsSource) open(e srcEntry) (io.ReadCloser, error) {
	f, err := s.fsys.Open(path.Join(s.dir, e.name))
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		if err == nil {
			err = fmt.Errorf("agenthome: %s changed while being copied", e.name)
		}
		return nil, err
	}
	return f, nil
}

func (s *fsSource) close() error { return nil }

// RefusedEntryError reports an entry CheckTree refuses: Path is the
// entry's path relative to the checked directory, in slash form, never a
// host path; Reason says what it is (a symbolic link, not a regular file
// or directory, or a file with more than one hard link).
type RefusedEntryError struct {
	Path   string
	Reason string
}

func (e *RefusedEntryError) Error() string {
	return e.Path + " is " + e.Reason + "; replace it with a regular file or directory"
}

// CheckTree walks the directory srcRel in src, as InstallTreeFrom and
// OverlayTreeFrom would read it (without following a link), and fails
// with a *RefusedEntryError for the first entry those copies would skip:
// a symbolic link, a special file, or (on unix) a regular file with more
// than one hard link. SkipCopyName entries are ignored. A caller that must
// not drop entries silently checks the source with CheckTree before
// copying it; an entry replaced between the check and the copy is still
// never followed by the copy.
func CheckTree(src *Home, srcRel string) error {
	s, err := openSource(src, srcRel)
	if err != nil {
		return err
	}
	defer func() { _ = s.close() }()
	return checkTree(s, "")
}

func checkTree(src treeSource, prefix string) error {
	entries, err := src.entries()
	if err != nil {
		return err
	}
	for _, e := range entries {
		rel := path.Join(prefix, e.name)
		switch e.kind {
		case kindSkip:
			return &RefusedEntryError{Path: rel, Reason: e.skip}
		case kindDir:
			child, err := src.sub(e)
			if err != nil {
				return err
			}
			err = checkTree(child, rel)
			_ = child.close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// preWalk charges every entry below src to b without writing anything, so
// OverlayTreeFrom reports a limit before its first write.
func preWalk(src treeSource, b *treeBudget, depth int) error {
	entries, err := src.entries()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := b.entry(); err != nil {
			return err
		}
		switch e.kind {
		case kindDir:
			if err := b.depth(depth + 1); err != nil {
				return err
			}
			child, err := src.sub(e)
			if err != nil {
				return err
			}
			err = preWalk(child, b, depth+1)
			_ = child.close()
			if err != nil {
				return err
			}
		case kindFile:
			if err := b.addBytes(e.size); err != nil {
				return err
			}
		}
	}
	return nil
}

// copyBudgeted copies in to out, charging the bytes to b; a source that
// grows past the remaining budget fails with a *LimitError.
func copyBudgeted(out io.Writer, in io.Reader, b *treeBudget) error {
	r := in
	if rem := b.remainingBytes(); rem >= 0 {
		r = io.LimitReader(in, rem+1)
	}
	n, err := io.Copy(out, r)
	if err != nil {
		return err
	}
	return b.addBytes(n)
}
