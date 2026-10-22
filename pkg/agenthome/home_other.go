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
	"path/filepath"
)

// Home is an agent home opened once; see the package documentation. On
// this platform every path is resolved inside an os.Root opened on the
// home (which keeps every operation inside the home), and every directory
// component is checked with Lstat first, so a linked component is refused.
type Home struct {
	path string
	root *os.Root
}

// Open opens the agent home at path, the broker-side path of the home.
func Open(path string) (*Home, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	return &Home{path: path, root: root}, nil
}

// Path returns the broker-side path the home was opened with.
func (h *Home) Path() string { return h.path }

// Close releases the home's root.
func (h *Home) Close() error { return h.root.Close() }

// checkDirs verifies that each of parts (relative to the home) is a real
// directory, creating missing ones with mode when create is set.
func (h *Home) checkDirs(parts []string, create bool, mode os.FileMode) error {
	for i := range parts {
		rel := path.Join(parts[:i+1]...)
		fi, err := h.root.Lstat(rel)
		if errors.Is(err, fs.ErrNotExist) && create {
			if merr := h.root.Mkdir(rel, mode.Perm()); merr != nil && !errors.Is(merr, fs.ErrExist) {
				return pathErr("mkdir", h.path, parts[:i+1], merr)
			}
			fi, err = h.root.Lstat(rel)
		}
		if err != nil {
			return pathErr("lstat", h.path, parts[:i+1], err)
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return pathErr("open", h.path, parts[:i+1], ErrLinkComponent)
		}
		if !fi.IsDir() {
			return pathErr("open", h.path, parts[:i+1], ErrNotDirectory)
		}
	}
	return nil
}

// MkdirAll makes rel a chain of real directories below the home.
func (h *Home) MkdirAll(rel string, mode os.FileMode) error {
	parts, err := splitRel(rel)
	if err != nil {
		return err
	}
	return h.checkDirs(parts, true, mode)
}

// WriteFile writes data to rel with mode through a temporary file renamed
// over rel. The parent directory must exist.
func (h *Home) WriteFile(rel string, data []byte, mode os.FileMode) error {
	parent, leaf, err := splitLeaf(rel)
	if err != nil {
		return err
	}
	if err := h.checkDirs(parent, false, 0); err != nil {
		return err
	}
	tmp, err := tempName(leaf)
	if err != nil {
		return err
	}
	tmpRel := path.Join(append(parent, tmp)...)
	f, err := h.root.OpenFile(tmpRel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return pathErr("write", h.path, append(parent, leaf), err)
	}
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Chmod(mode)
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = h.root.Rename(tmpRel, path.Join(append(parent, leaf)...))
	}
	if werr != nil {
		_ = h.root.Remove(tmpRel)
		return pathErr("write", h.path, append(parent, leaf), werr)
	}
	return nil
}

// ReadFile reads the regular file rel, up to max bytes, refusing a link at
// any component or at the leaf.
func (h *Home) ReadFile(rel string, max int64) ([]byte, error) {
	parent, leaf, err := splitLeaf(rel)
	if err != nil {
		return nil, err
	}
	if err := h.checkDirs(parent, false, 0); err != nil {
		return nil, err
	}
	p := path.Join(append(parent, leaf)...)
	fi, err := h.root.Lstat(p)
	if err != nil {
		return nil, pathErr("read", h.path, append(parent, leaf), err)
	}
	if !fi.Mode().IsRegular() {
		return nil, pathErr("read", h.path, append(parent, leaf), fmt.Errorf("agenthome: not a regular file"))
	}
	f, err := h.root.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, pathErr("read", h.path, append(parent, leaf), fmt.Errorf("agenthome: file too large"))
	}
	return data, nil
}

func (h *Home) parentPresent(parent []string) (bool, error) {
	if err := h.checkDirs(parent, false, 0); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Remove removes the entry rel (a link itself, never its target). A
// missing entry or parent is not an error.
func (h *Home) Remove(rel string) error {
	parent, leaf, err := splitLeaf(rel)
	if err != nil {
		return err
	}
	if ok, err := h.parentPresent(parent); err != nil || !ok {
		return err
	}
	if err := h.root.Remove(path.Join(append(parent, leaf)...)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// RemoveAll removes rel and everything below it (os.Root.RemoveAll removes
// a link itself and does not follow it). A missing entry or parent is not
// an error.
func (h *Home) RemoveAll(rel string) error {
	parent, leaf, err := splitLeaf(rel)
	if err != nil {
		return err
	}
	if ok, err := h.parentPresent(parent); err != nil || !ok {
		return err
	}
	return h.root.RemoveAll(path.Join(append(parent, leaf)...))
}

// InstallTree replaces dstRel in the home with a copy of the directory
// srcRel in src; see the unix implementation for the contract.
func (h *Home) InstallTree(src *os.Root, srcRel, dstRel string, lim Limits) error {
	parent, leaf, err := splitLeaf(dstRel)
	if err != nil {
		return err
	}
	fi, err := src.Lstat(srcRel)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("agenthome: %s is not a directory", srcRel)
	}
	if err := h.checkDirs(parent, true, 0o755); err != nil {
		return err
	}
	tmp, err := tempName(leaf)
	if err != nil {
		return err
	}
	tmpRel := path.Join(append(parent, tmp)...)
	if err := h.root.Mkdir(tmpRel, 0o755); err != nil {
		return err
	}
	installed := false
	defer func() {
		if !installed {
			_ = h.root.RemoveAll(tmpRel)
		}
	}()
	b := &treeBudget{lim: lim}
	if err := h.copyTree(src, filepath.Clean(srcRel), tmpRel, b, 0); err != nil {
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

func (h *Home) copyTree(src *os.Root, srcRel, dstRel string, b *treeBudget, depth int) error {
	entries, err := readDirSorted(src, srcRel)
	if err != nil {
		return err
	}
	for _, e := range entries {
		s, t := filepath.Join(srcRel, e.Name()), path.Join(dstRel, e.Name())
		fi, err := src.Lstat(s)
		if err != nil {
			return err
		}
		switch {
		case fi.IsDir():
			if err := b.depth(depth + 1); err != nil {
				return err
			}
			if err := b.entry(); err != nil {
				return err
			}
			if err := h.root.Mkdir(t, 0o755); err != nil {
				return err
			}
			if err := h.copyTree(src, s, t, b, depth+1); err != nil {
				return err
			}
		case fi.Mode().IsRegular():
			if err := b.entry(); err != nil {
				return err
			}
			if err := h.copyFile(src, s, t, copyMode(fi.Mode()), b); err != nil {
				return err
			}
		default:
			if err := b.entry(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h *Home) copyFile(src *os.Root, s, t string, mode os.FileMode, b *treeBudget) error {
	in, err := src.Open(s)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := h.root.OpenFile(t, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	var r io.Reader = in
	if rem := b.remainingBytes(); rem >= 0 {
		r = io.LimitReader(in, rem+1)
	}
	n, err := io.Copy(out, r)
	if err == nil {
		err = b.addBytes(n)
	}
	if err == nil {
		err = out.Chmod(mode)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}
