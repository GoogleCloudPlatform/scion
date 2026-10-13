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

package agentplugin

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Limits bounds a bundle copy.
type Limits struct {
	// MaxFiles caps the number of copied entries (regular files and
	// directories).
	MaxFiles int
	// MaxBytes caps the total size of copied regular files.
	MaxBytes int64
	// MaxDepth caps directory nesting below the copied root.
	MaxDepth int
}

// DefaultLimits are the per-plugin copy limits.
var DefaultLimits = Limits{MaxFiles: 10000, MaxBytes: 100 << 20, MaxDepth: 32}

// LimitError reports that a copy exceeded Limits.
type LimitError struct{ Limit string }

func (e *LimitError) Error() string { return "bundle exceeds " + e.Limit }

// CopyReport summarises a CopyTree run.
type CopyReport struct {
	Files   int
	Bytes   int64
	Skipped []string // rel paths of symlinks / special files not copied
}

// CopyTree copies srcRel (in srcRoot) to dstRel (in dstRoot), which must not
// exist; its parent must. Only directories and regular files are copied;
// every entry is Lstat'ed and a symlink or special file anywhere is skipped
// and reported (never followed). Modes: dirs 0755; files 0644, or 0755 if any
// exec bit is set in the source (setuid/setgid/sticky never copied).
// Exceeding lim aborts with a *LimitError and removes dstRel; any other error
// also removes dstRel.
//
// It uses the same primitives as carryOverSkillsDir
// (pkg/agent/skills_dir_switch.go) but across two roots.
func CopyTree(srcRoot *os.Root, srcRel string, dstRoot *os.Root, dstRel string, lim Limits) (CopyReport, error) {
	var rep CopyReport
	srcRel, dstRel = filepath.Clean(srcRel), filepath.Clean(dstRel)
	if dstRel == "." {
		return rep, fmt.Errorf("destination must not be the root")
	}
	fi, err := srcRoot.Lstat(srcRel)
	if err != nil {
		return rep, err
	}
	if !fi.IsDir() {
		return rep, fmt.Errorf("%s is not a directory", srcRel)
	}
	if _, err := dstRoot.Lstat(dstRel); err == nil {
		return rep, fmt.Errorf("destination %s already exists", dstRel)
	} else if !os.IsNotExist(err) {
		return rep, err
	}
	c := &treeCopier{src: srcRoot, dst: dstRoot, lim: lim, rep: &rep}
	if err := dstRoot.Mkdir(dstRel, 0o755); err != nil {
		return rep, err
	}
	if err := c.copyDir(srcRel, dstRel, "", 0); err != nil {
		_ = dstRoot.RemoveAll(dstRel)
		return rep, err
	}
	return rep, nil
}

type treeCopier struct {
	src, dst *os.Root
	lim      Limits
	rep      *CopyReport
	entries  int
}

func (c *treeCopier) count() error {
	c.entries++
	if c.lim.MaxFiles > 0 && c.entries > c.lim.MaxFiles {
		return &LimitError{Limit: fmt.Sprintf("%d files", c.lim.MaxFiles)}
	}
	return nil
}

// copyDir copies the contents of the directory src into the existing
// directory dst. rel is the path relative to the copy root, for reporting.
func (c *treeCopier) copyDir(src, dst, rel string, depth int) error {
	if err := c.dst.Chmod(dst, 0o755); err != nil {
		return err
	}
	entries, err := readRootDir(c.src, src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		s, d := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		r := filepath.ToSlash(filepath.Join(rel, e.Name()))
		// Re-check with Lstat: the directory entry type is advisory.
		fi, err := c.src.Lstat(s)
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0, !fi.IsDir() && !fi.Mode().IsRegular():
			c.rep.Skipped = append(c.rep.Skipped, r)
		case fi.IsDir():
			if c.lim.MaxDepth > 0 && depth+1 > c.lim.MaxDepth {
				return &LimitError{Limit: fmt.Sprintf("a directory depth of %d", c.lim.MaxDepth)}
			}
			if err := c.count(); err != nil {
				return err
			}
			if err := c.dst.Mkdir(d, 0o755); err != nil {
				return err
			}
			if err := c.copyDir(s, d, r, depth+1); err != nil {
				return err
			}
		default:
			if err := c.count(); err != nil {
				return err
			}
			if err := c.copyFile(s, d, fi); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *treeCopier) copyFile(src, dst string, fi os.FileInfo) error {
	in, err := c.src.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	ofi, err := in.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(fi, ofi) || !ofi.Mode().IsRegular() {
		return fmt.Errorf("%s changed while being copied", src)
	}
	mode := os.FileMode(0o644)
	if fi.Mode().Perm()&0o111 != 0 {
		mode = 0o755
	}
	out, err := c.dst.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	var budget int64 = -1
	if c.lim.MaxBytes > 0 {
		budget = c.lim.MaxBytes - c.rep.Bytes
	}
	var n int64
	if budget >= 0 {
		n, err = io.Copy(out, io.LimitReader(in, budget+1))
	} else {
		n, err = io.Copy(out, in)
	}
	if err == nil {
		err = out.Chmod(mode) // not subject to the umask
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	c.rep.Bytes += n
	if budget >= 0 && n > budget {
		return &LimitError{Limit: formatBytes(c.lim.MaxBytes)}
	}
	c.rep.Files++
	return nil
}

func formatBytes(n int64) string {
	if n >= 1<<20 && n%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%d bytes", n)
}

// readRootDir lists the directory rel inside root, sorted by name.
func readRootDir(root *os.Root, rel string) ([]os.DirEntry, error) {
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	entries, err := f.ReadDir(-1)
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, err
}

// IsLimitError reports whether err is (or wraps) a *LimitError.
func IsLimitError(err error) bool {
	var le *LimitError
	return errors.As(err, &le)
}
