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

// workspaceWriteParent is the open parent directory of a workspace write
// target, reached one component at a time from the workspace root.
type workspaceWriteParent struct {
	// stack[0] is the workspace root (borrowed, never closed here); every
	// later entry was opened through the entry before it.
	stack []*os.Root
	// gitDir is the workspace root's .git entry, or nil when it has none.
	gitDir fs.FileInfo
	hops   int
}

// openWorkspaceWriteParent opens the directory that will hold relPath
// (slash-separated, already accepted by validateWorkspaceWritePath),
// creating missing directories on the way.
//
// Each component is opened through the handle of the directory before it,
// and the opened handle itself is compared with the root's .git entry, so a
// symlink or an ancestor swapped in the meantime cannot route the write into
// .git. A symlinked directory on the way is followed by walking its target
// the same way; ".." in a target steps back to the previous handle and never
// above the workspace root.
func openWorkspaceWriteParent(root *os.Root, relPath string) (*workspaceWriteParent, string, error) {
	p := &workspaceWriteParent{stack: []*os.Root{root}}
	if info, err := root.Stat(".git"); err == nil {
		p.gitDir = info
	}
	dir, base := path.Split(relPath)
	for _, c := range strings.Split(dir, "/") {
		if err := p.step(c, true); err != nil {
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
	if util.IsDotGitComponent(name) {
		return fmt.Errorf("%w: path resolves through a .git directory", errWorkspaceWriteRefused)
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
	next, err := cur.OpenRoot(name)
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
	if p.gitDir != nil && os.SameFile(opened, p.gitDir) {
		_ = next.Close()
		return fmt.Errorf("%w: path resolves through a .git directory", errWorkspaceWriteRefused)
	}
	p.stack = append(p.stack, next)
	return nil
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

	if info, err := dir.Lstat(base); err == nil && notRegular(info) {
		return 0, nil, fmt.Errorf("%w: target is not a regular file", errWorkspaceWriteRefused)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, nil, err
	}

	// Open without truncating, then confirm the opened file is still the
	// regular entry named base before changing it, so an entry swapped for a
	// symlink after the Lstat above is left untouched.
	f, err := dir.OpenFile(base, os.O_WRONLY|os.O_CREATE, 0644)
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
	if notRegular(named) || !os.SameFile(opened, named) {
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

// notRegular reports whether info is anything but a regular file: the files
// API never writes through a symlink or into a special file.
func notRegular(info fs.FileInfo) bool { return !info.Mode().IsRegular() }
