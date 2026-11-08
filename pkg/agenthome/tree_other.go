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

//go:build !unix

package agenthome

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
)

// rootSource is a treeSource over a directory inside a Home's os.Root. On
// this platform every entry is classified with Lstat and checked again
// after it is opened (check-then-act, as for the rest of this platform's
// Home; the os.Root keeps every read inside the source directory).
type rootSource struct {
	root *os.Root
	dir  string
	mode fs.FileMode
}

func openSource(src *Home, srcRel string) (*rootSource, error) {
	parts, err := splitRel(srcRel)
	if err != nil {
		return nil, err
	}
	if err := src.checkDirs(parts, false, 0); err != nil {
		return nil, err
	}
	dir := "."
	if len(parts) > 0 {
		dir = path.Join(parts...)
	}
	fi, err := src.root.Lstat(dir)
	if err != nil {
		return nil, err
	}
	return &rootSource{root: src.root, dir: dir, mode: fi.Mode()}, nil
}

func (s *rootSource) dirMode() fs.FileMode { return s.mode }

func (s *rootSource) entries() ([]srcEntry, error) {
	d, err := s.root.Open(s.dir)
	if err != nil {
		return nil, err
	}
	names, err := d.Readdirnames(-1)
	_ = d.Close()
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	out := make([]srcEntry, 0, len(names))
	for _, name := range names {
		fi, err := s.root.Lstat(path.Join(s.dir, name))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		e := srcEntry{name: name, mode: fi.Mode(), size: fi.Size()}
		switch {
		case fi.IsDir():
			if SkipCopyName(name, true) {
				continue
			}
			e.kind = kindDir
		case fi.Mode().IsRegular():
			if SkipCopyName(name, false) {
				continue
			}
			e.kind = kindFile
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *rootSource) sub(e srcEntry) (treeSource, error) {
	dir := path.Join(s.dir, e.name)
	fi, err := s.root.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("agenthome: %s changed while being copied", e.name)
	}
	return &rootSource{root: s.root, dir: dir, mode: fi.Mode()}, nil
}

func (s *rootSource) open(e srcEntry) (io.ReadCloser, error) {
	p := path.Join(s.dir, e.name)
	before, err := s.root.Lstat(p)
	if err != nil {
		return nil, err
	}
	f, err := s.root.Open(p)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || !fi.Mode().IsRegular() || !os.SameFile(before, fi) {
		_ = f.Close()
		if err == nil {
			err = fmt.Errorf("agenthome: %s changed while being copied", e.name)
		}
		return nil, err
	}
	return f, nil
}

func (s *rootSource) close() error { return nil }

// InstallTreeFrom replaces dstRel in the home with a copy of the directory
// srcRel in src; see the unix implementation for the contract. On this
// platform the no-follow checks are check-then-act.
func (h *Home) InstallTreeFrom(src *Home, srcRel, dstRel string, lim Limits) error {
	s, err := openSource(src, srcRel)
	if err != nil {
		return err
	}
	return h.installTree(s, dstRel, lim)
}

// InstallTreeFS is InstallTreeFrom for a directory srcRel in an fs.FS.
func (h *Home) InstallTreeFS(src fs.FS, srcRel string, dstRel string, lim Limits) error {
	s, err := newFSSource(src, srcRel)
	if err != nil {
		return err
	}
	return h.installTree(s, dstRel, lim)
}

func (h *Home) installTree(src treeSource, dstRel string, lim Limits) error {
	parent, leaf, err := splitLeaf(dstRel)
	if err != nil {
		return err
	}
	if err := h.checkDirs(parent, true, 0o755); err != nil {
		return err
	}
	tmp, err := tempName(leaf)
	if err != nil {
		return err
	}
	tmpRel := path.Join(append(parent, tmp)...)
	if err := h.root.Mkdir(tmpRel, 0o700); err != nil {
		return err
	}
	installed := false
	defer func() {
		if !installed {
			_ = h.root.RemoveAll(tmpRel)
		}
	}()
	if err := h.copyFrom(src, tmpRel, &treeBudget{lim: lim}, 0); err != nil {
		return err
	}
	dst := path.Join(append(parent, leaf)...)
	if err := h.root.RemoveAll(dst); err != nil {
		return err
	}
	if err := h.root.Rename(tmpRel, dst); err != nil {
		return err
	}
	installed = true
	return nil
}

func (h *Home) copyFrom(src treeSource, dstRel string, b *treeBudget, depth int) error {
	entries, err := src.entries()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := b.entry(); err != nil {
			return err
		}
		t := path.Join(dstRel, e.name)
		switch e.kind {
		case kindDir:
			if err := b.depth(depth + 1); err != nil {
				return err
			}
			if err := h.root.Mkdir(t, 0o700); err != nil {
				return err
			}
			child, err := src.sub(e)
			if err != nil {
				return err
			}
			err = h.copyFrom(child, t, b, depth+1)
			_ = child.close()
			if err != nil {
				return err
			}
		case kindFile:
			if err := h.writeEntry(src, e, t, b, false); err != nil {
				return err
			}
		}
	}
	return h.root.Chmod(dstRel, homeDirMode(src.dirMode()))
}

// writeEntry copies the kindFile entry e of src to the new file t.
func (h *Home) writeEntry(src treeSource, e srcEntry, t string, b *treeBudget, sync bool) error {
	in, err := src.open(e)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := h.root.OpenFile(t, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	err = copyBudgeted(out, in, b)
	if err == nil {
		err = out.Chmod(homeFileMode(e.mode))
	}
	if err == nil && sync {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// OverlayTreeFrom merges the directory srcRel in src into dstRel; see the
// unix implementation for the contract. The source as listed before the
// copy is charged against the limits; a limit found then writes nothing. A
// source that changes during the copy can still fail midway with files
// already renamed in place (each file all or nothing); callers treat the
// error as fatal. On this platform the no-follow checks are check-then-act.
func (h *Home) OverlayTreeFrom(src *Home, srcRel, dstRel string, lim Limits) error {
	s, err := openSource(src, srcRel)
	if err != nil {
		return err
	}
	if err := preWalk(s, &treeBudget{lim: lim}, 0); err != nil {
		return err
	}
	parts, err := splitRel(dstRel)
	if err != nil {
		return err
	}
	if err := h.checkDirs(parts, true, 0o755); err != nil {
		return err
	}
	dir := "."
	if len(parts) > 0 {
		dir = path.Join(parts...)
	}
	return h.overlayFrom(s, dir, &treeBudget{lim: lim}, 0)
}

func (h *Home) overlayFrom(src treeSource, dstRel string, b *treeBudget, depth int) error {
	entries, err := src.entries()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := b.entry(); err != nil {
			return err
		}
		t := path.Join(dstRel, e.name)
		fi, lerr := h.root.Lstat(t)
		if lerr != nil && !errors.Is(lerr, fs.ErrNotExist) {
			return lerr
		}
		switch e.kind {
		case kindDir:
			if err := b.depth(depth + 1); err != nil {
				return err
			}
			switch {
			case lerr != nil:
				if err := h.mkdirMode(t, homeDirMode(e.mode)); err != nil {
					return err
				}
			case fi.Mode()&fs.ModeSymlink != 0:
				if err := h.root.Remove(t); err != nil {
					return err
				}
				if err := h.mkdirMode(t, homeDirMode(e.mode)); err != nil {
					return err
				}
			case !fi.IsDir():
				return fmt.Errorf("agenthome: %s exists and is not a directory", e.name)
			}
			child, err := src.sub(e)
			if err != nil {
				return err
			}
			err = h.overlayFrom(child, t, b, depth+1)
			_ = child.close()
			if err != nil {
				return err
			}
		case kindFile:
			if lerr == nil && fi.IsDir() {
				return fmt.Errorf("agenthome: %s exists and is a directory", e.name)
			}
			tmp, err := tempName(e.name)
			if err != nil {
				return err
			}
			tmpRel := path.Join(dstRel, tmp)
			if err := h.writeEntry(src, e, tmpRel, b, true); err != nil {
				_ = h.root.Remove(tmpRel)
				return err
			}
			if lerr == nil {
				// Rename does not replace an existing file on every
				// platform; remove the entry (a link itself) first.
				if err := h.root.Remove(t); err != nil && !errors.Is(err, fs.ErrNotExist) {
					_ = h.root.Remove(tmpRel)
					return err
				}
			}
			if err := h.root.Rename(tmpRel, t); err != nil {
				_ = h.root.Remove(tmpRel)
				return err
			}
		}
	}
	return nil
}

func (h *Home) mkdirMode(rel string, mode fs.FileMode) error {
	if err := h.root.Mkdir(rel, 0o700); err != nil {
		return err
	}
	return h.root.Chmod(rel, mode)
}

// Lstat describes the entry rel without following a link at the leaf; a
// linked parent component is refused.
func (h *Home) Lstat(rel string) (fs.FileInfo, error) {
	parent, leaf, err := splitLeaf(rel)
	if err != nil {
		return nil, err
	}
	if err := h.checkDirs(parent, false, 0); err != nil {
		return nil, err
	}
	return h.root.Lstat(strings.Join(append(parent, leaf), "/"))
}

// ReadDir lists the directory rel below the home ("." for the home itself)
// in name order, describing each entry with Lstat (a link is reported as a
// link, never followed); a linked component of rel is refused. On this
// platform the component check and the listing are separate steps.
func (h *Home) ReadDir(rel string) ([]fs.FileInfo, error) {
	parts, err := splitRel(rel)
	if err != nil {
		return nil, err
	}
	if err := h.checkDirs(parts, false, 0); err != nil {
		return nil, err
	}
	dir := "."
	if len(parts) > 0 {
		dir = path.Join(parts...)
	}
	d, err := h.root.Open(dir)
	if err != nil {
		return nil, err
	}
	names, err := d.Readdirnames(-1)
	_ = d.Close()
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	out := make([]fs.FileInfo, 0, len(names))
	for _, name := range names {
		fi, err := h.root.Lstat(path.Join(dir, name))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		out = append(out, fi)
	}
	return out, nil
}
