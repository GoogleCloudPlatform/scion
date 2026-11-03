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

//go:build unix

package projectfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
)

// Leaf creation (ptone/scion#5834). EnsureSharedLeaf is the one primitive
// every Scion-owned directory on a shared backend is created through, and
// Resolved.EnsureLeaf the per-Mount form of it. The provisioning parts that
// need pkg/provision (the state directory, the shared workspace
// provisioning) are in pkg/projectfs/provisioner, so this package keeps no
// dependency on pkg/provision or pkg/store.

// EnsureSharedLeaf creates (or verifies) rel below the resolved host base of
// a shared backend with the leaf contract (shareddirs.EnsureLeaf: every
// component opened without following symlinks, a new leaf mode 2775 with the
// default ACL) and returns the leaf's open descriptor, which the caller
// closes with shareddirs.CloseFd. It is EnsureSharedLeafOwned with the zero
// owner: a new leaf keeps its creator's ownership.
func EnsureSharedLeaf(hostBase, rel string) (fd int, existed bool, err error) {
	return EnsureSharedLeafOwned(hostBase, rel, shareddirs.Owner{})
}

// EnsureSharedLeafOwned is EnsureSharedLeaf that gives a leaf this call
// creates the owner (shareddirs.EnsureLeafOwned: only for a root creator
// and a non-root owner, by fchown on the walk's descriptor, before the
// leaf's mode and default ACL). A directory that already existed is never
// changed.
func EnsureSharedLeafOwned(hostBase, rel string, owner shareddirs.Owner) (fd int, existed bool, err error) {
	return shareddirs.EnsureLeafOwned(hostBase, rel, owner)
}

// EnsureLeaf creates the directory r if it is missing and reports whether it
// already existed. On a shared backend it is EnsureSharedLeafOwned below r's
// resolved host base with r's owner (Resolved.Owner), as Open creates it. On the local backend it is the directory created as
// before (mode 0755, missing parents included), unchanged.
func (r Resolved) EnsureLeaf() (existed bool, err error) {
	switch {
	case r.Backend.Shared():
		base, err := r.resolvedHostBase()
		if err != nil {
			return false, err
		}
		fd, existed, err := EnsureSharedLeafOwned(base, r.RelPath, r.Owner())
		if err != nil {
			return false, fmt.Errorf("create %s %q: %w", r.Kind, r.Name, err)
		}
		if err := shareddirs.CloseFd(fd); err != nil {
			return false, err
		}
		return existed, nil
	case r.Backend == BackendLocal:
		if r.HostPath == "" || !filepath.IsAbs(r.HostPath) {
			return false, fmt.Errorf("projectfs: local directory %q is not absolute", r.HostPath)
		}
		if fi, err := os.Stat(r.HostPath); err == nil {
			if !fi.IsDir() {
				return false, fmt.Errorf("projectfs: %s exists and is not a directory", r.HostPath)
			}
			return true, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		if err := os.MkdirAll(r.HostPath, 0o755); err != nil {
			return false, err
		}
		return false, nil
	default:
		return false, fmt.Errorf("projectfs: cannot create a directory on backend %q", r.Backend)
	}
}

// ProjectTreeIDs lists the project IDs that have a project root on l (one
// listing of the subpath root, never following a symlink; entries that are
// not project IDs are skipped). A subpath root that does not exist is an
// error (the share may not be mounted). Only l itself is listed, not its
// transitional shared-dir split: workspaces and provisioning state live on l.
func (l Layout) ProjectTreeIDs() ([]string, error) {
	if !l.Backend.Shared() {
		return nil, fmt.Errorf("projectfs: project trees are defined for shared backends, not %q", l.Backend)
	}
	root, err := l.subPathRoot()
	if err != nil {
		return nil, err
	}
	if err := l.checkShareHostBase(); err != nil {
		return nil, err
	}
	r := Resolved{Backend: l.Backend, HostBase: l.HostBase, RelPath: root}
	base, err := r.resolvedHostBase()
	if err != nil {
		return nil, fmt.Errorf("%w: host base %s: %w", ErrRefused, l.HostBase, err)
	}
	names, ok, err := shareddirs.ListDirInTree(base, root)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: %w: %s has no %s directory; the share may not be mounted", ErrRefused, ErrNoProjectTree, l.HostBase, root)
	}
	var ids []string
	for _, name := range names {
		if validProjectID(name) == nil {
			ids = append(ids, name)
		}
	}
	return ids, nil
}
