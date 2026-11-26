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

package hub

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// maxWorkspaceSymlinkHops bounds the symlinks followed while opening the
// parent directory of a workspace write.
const maxWorkspaceSymlinkHops = 40

// errWorkspaceWriteRefused is wrapped by every refusal of openWorkspaceWriteParent
// and writeWorkspaceFile that is a policy decision (a .git directory on the
// way, a symlink as the target) rather than an I/O failure.
var errWorkspaceWriteRefused = errors.New("refused")

// errWorkspaceThroughDotGit is the refusal of a path that reaches a .git
// directory (by name or as the workspace root's .git entry). It wraps
// errWorkspaceWriteRefused; it is answered with the .git refusal text.
var errWorkspaceThroughDotGit = fmt.Errorf("%w: path resolves through a .git directory", errWorkspaceWriteRefused)

// errWorkspaceThroughDotScion is the refusal of a write or delete path that
// reaches a .scion directory (by name or as the workspace root's .scion
// entry). It wraps errWorkspaceWriteRefused; it is answered with the .scion
// refusal text.
var errWorkspaceThroughDotScion = fmt.Errorf("%w: path resolves through a .scion directory", errWorkspaceWriteRefused)

// workspaceWriteParent is the open parent directory of a workspace write
// target, reached one component at a time from the workspace root.
type workspaceWriteParent struct {
	// stack[0] is the workspace root (borrowed, never closed here); every
	// later entry was opened through the entry before it.
	stack []*os.Root
	// gitDir is the workspace root's .git entry, or nil when it has none.
	gitDir fs.FileInfo
	// scionDir is the workspace root's .scion entry, or nil when it has none
	// or when blockDotScion is false (read walks).
	scionDir      fs.FileInfo
	blockDotScion bool
	hops          int
}

// openWorkspaceWriteParent opens the directory that will hold relPath
// (slash-separated, already accepted by validateWorkspaceWritePath),
// creating missing directories on the way.
//
// Each component is opened through the handle of the directory before it,
// and the opened handle itself is compared with the root's .git and .scion
// entries, so a symlink or an ancestor swapped in the meantime cannot route
// the write into .git or .scion. A symlinked directory on the way is followed
// by walking its target the same way; ".." in a target steps back to the
// previous handle and never above the workspace root.
func openWorkspaceWriteParent(root *os.Root, relPath string) (*workspaceWriteParent, string, error) {
	return openWorkspaceParent(root, relPath, true, true)
}

// openWorkspaceReadTarget opens the parent of relPath like
// openWorkspaceWriteParent, but creates nothing, allows .scion (still
// refusing .git), and refuses when the final component is a symlink: a
// download serves only an entry that is itself in the opened parent. It also
// returns the target's Lstat info, which the caller compares with the handle
// it opens. A missing parent or target is an fs.ErrNotExist error.
func openWorkspaceReadTarget(root *os.Root, relPath string) (*workspaceWriteParent, string, fs.FileInfo, error) {
	p, base, err := openWorkspaceParent(root, relPath, false, false)
	if err != nil {
		return nil, "", nil, err
	}
	if base == "" {
		base = "." // a trailing slash names the parent itself
	}
	info, err := p.Dir().Lstat(base)
	if err != nil {
		p.Close()
		return nil, "", nil, err
	} else if info.Mode().Type() == fs.ModeSymlink {
		p.Close()
		return nil, "", nil, fmt.Errorf("%w: target is a symlink", errWorkspaceWriteRefused)
	}
	return p, base, info, nil
}

// openWorkspaceDeleteTarget opens the parent of relPath like
// openWorkspaceReadTarget (creating nothing, blocking both .git and .scion)
// and returns it with the final component and a function that closes the walk.
// The final component may be a symlink: a delete removes the link itself.
func openWorkspaceDeleteTarget(root *os.Root, relPath string) (*os.Root, string, func(), error) {
	p, base, err := openWorkspaceParent(root, relPath, false, true)
	if err != nil {
		return nil, "", nil, err
	}
	return p.Dir(), base, p.Close, nil
}

// openWorkspaceParent is the walk behind openWorkspaceWriteParent,
// openWorkspaceReadTarget, and openWorkspaceDeleteTarget; create makes
// missing directories on the way, and blockDotScion refuses .scion traversal.
func openWorkspaceParent(root *os.Root, relPath string, create, blockDotScion bool) (*workspaceWriteParent, string, error) {
	p := &workspaceWriteParent{
		stack:         []*os.Root{root},
		blockDotScion: blockDotScion,
	}
	if info, err := root.Stat(".git"); err == nil {
		p.gitDir = info
	}
	if blockDotScion {
		if info, err := root.Stat(".scion"); err == nil {
			p.scionDir = info
		}
	}
	dir, base := path.Split(relPath)
	for _, c := range strings.Split(dir, "/") {
		if err := p.step(c, create); err != nil {
			p.Close()
			return nil, "", err
		}
	}
	return p, base, nil
}

// Dir returns the opened parent directory.
func (p *workspaceWriteParent) Dir() *os.Root { return p.stack[len(p.stack)-1] }

// Close closes every handle this walk opened.
func (p *workspaceWriteParent) Close() {
	for len(p.stack) > 1 {
		p.pop()
	}
}

func (p *workspaceWriteParent) pop() {
	_ = p.stack[len(p.stack)-1].Close()
	p.stack = p.stack[:len(p.stack)-1]
}

// step moves the walk into the child directory name of the current handle.
func (p *workspaceWriteParent) step(name string, create bool) error {
	switch name {
	case "", ".":
		return nil
	case "..":
		if len(p.stack) == 1 {
			return fmt.Errorf("%w: path leaves the workspace", errWorkspaceWriteRefused)
		}
		p.pop()
		return nil
	}
	if create && util.IsDotGitComponent(name) {
		// Request components are refused lexically before the walk; this
		// keeps the walk from ever creating a .git directory.
		return errWorkspaceThroughDotGit
	}
	if create && p.blockDotScion && util.IsDotScionComponent(name) {
		// Request components are refused lexically before the walk; this
		// keeps the walk from ever creating a .scion directory.
		return errWorkspaceThroughDotScion
	}
	cur := p.Dir()
	info, err := cur.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) && create {
		if err := cur.Mkdir(name, 0755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		info, err = cur.Lstat(name)
	}
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		p.hops++
		if p.hops > maxWorkspaceSymlinkHops {
			return fmt.Errorf("%w: too many symlinks", errWorkspaceWriteRefused)
		}
		target, err := cur.Readlink(name)
		if err != nil {
			return err
		}
		if path.IsAbs(target) || strings.HasPrefix(target, `\`) {
			return fmt.Errorf("%w: absolute symlink on the way", errWorkspaceWriteRefused)
		}
		for _, c := range strings.Split(target, "/") {
			if err := p.step(c, false); err != nil {
				return err
			}
		}
		return nil
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", errWorkspaceWriteRefused, name)
	}
	// The trailing slash makes the open fail on a non-directory (a FIFO swapped
	// in after the Lstat) instead of blocking on it.
	next, err := cur.OpenRoot(name + "/")
	if err != nil {
		return err
	}
	opened, err := next.Stat(".")
	if err != nil {
		_ = next.Close()
		return err
	}
	// The entry changed between Lstat and OpenRoot (for example into a
	// symlink): refuse rather than trust what was opened.
	if !os.SameFile(opened, info) {
		_ = next.Close()
		return fmt.Errorf("%w: directory changed while opening it", errWorkspaceWriteRefused)
	}
	if p.isDotGit(name, opened) {
		_ = next.Close()
		return errWorkspaceThroughDotGit
	}
	if p.blockDotScion && p.isDotScion(name, opened) {
		_ = next.Close()
		return errWorkspaceThroughDotScion
	}
	p.stack = append(p.stack, next)
	return nil
}

// isDotGit reports whether the directory opened as name is a .git directory:
// by name under util.IsDotGitComponent, or as the workspace root's .git entry
// (compared through the opened handle, so an alias is caught too).
func (p *workspaceWriteParent) isDotGit(name string, opened fs.FileInfo) bool {
	if util.IsDotGitComponent(name) {
		return true
	}
	return p.gitDir != nil && os.SameFile(opened, p.gitDir)
}

// isDotScion reports whether the directory opened as name is a .scion
// directory: by name under util.IsDotScionComponent, or as the workspace
// root's .scion entry (compared through the opened handle, so a symlink or
// alias is caught too).
func (p *workspaceWriteParent) isDotScion(name string, opened fs.FileInfo) bool {
	if util.IsDotScionComponent(name) {
		return true
	}
	return p.scionDir != nil && os.SameFile(opened, p.scionDir)
}

// writeWorkspaceFile writes src to relPath under root through
// openWorkspaceWriteParent, refusing when the final component is a symlink
// (the files API never creates one), and returns the written size and the
// file's info.
func writeWorkspaceFile(root *os.Root, relPath string, src io.Reader) (int64, fs.FileInfo, error) {
	parent, base, err := openWorkspaceWriteParent(root, relPath)
	if err != nil {
		return 0, nil, err
	}
	defer parent.Close()
	dir := parent.Dir()

	before, err := dir.Lstat(base)
	exists := err == nil
	if exists && notRegular(before) {
		return 0, nil, fmt.Errorf("%w: target is not a regular file", errWorkspaceWriteRefused)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, nil, err
	}

	// Open without truncating, then confirm the opened file is still the
	// regular entry named base before changing it, so an entry swapped for a
	// symlink after the Lstat above is left untouched.
	f, err := openWorkspaceTarget(dir, base, exists)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return 0, nil, err
	}
	named, err := dir.Lstat(base)
	if err != nil {
		return 0, nil, err
	}
	if notRegular(opened) || notRegular(named) || !os.SameFile(opened, named) || (exists && !os.SameFile(opened, before)) {
		return 0, nil, fmt.Errorf("%w: target changed while opening it", errWorkspaceWriteRefused)
	}
	if err := f.Truncate(0); err != nil {
		return 0, nil, err
	}
	written, err := io.Copy(f, src)
	if err != nil {
		return written, nil, err
	}
	if err := f.Close(); err != nil {
		return written, nil, err
	}
	info, err := dir.Stat(base)
	if err != nil {
		return written, nil, err
	}
	return written, info, nil
}

// openWorkspaceTarget opens base in dir for writing, without truncating. An
// entry Lstat found absent is created with O_EXCL, which never follows a
// symlink, so an entry that appeared in between (a symlink included) is
// refused rather than followed or created through; an entry found present is
// opened without O_CREATE, so a dangling link swapped in creates nothing.
func openWorkspaceTarget(dir *os.Root, base string, exists bool) (*os.File, error) {
	return openWorkspaceFileForWrite(dir, base, exists)
}

// notRegular reports whether info is anything but a regular file: the files
// API never writes through a symlink or into a special file.
func notRegular(info fs.FileInfo) bool { return !info.Mode().IsRegular() }
