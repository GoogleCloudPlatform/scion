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
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
)

// Open is the single constructor of filesystem handles for a resolved
// project-scoped directory. Every operation through the returned *os.Root
// stays inside the directory: os.Root refuses a path that escapes it,
// including through a symlink.
//
// On a shared backend the directory is reached by pkg/shareddirs' anchored
// walk, which opens every component below HostBase with O_NOFOLLOW and
// checks that the Root opened is the directory the walk saw. With create,
// a missing leaf is first created by shareddirs.EnsureLeafOwned (its own
// O_NOFOLLOW walk, which applies the leaf mode and ACL, and which gives a
// root creator's new directories the Layout's owner, Resolved.Owner).
//
// On the local backend the directory itself must not be a symlink (rooting
// at a symlink would confine to wherever it points); its parents are
// host-owned and not checked, so a data directory behind a symlinked parent
// (macOS /var) keeps working. With create, a missing directory is created.
//
// Without create, a missing directory is an fs.ErrNotExist-compatible error
// and nothing is created.
func (r Resolved) Open(create bool) (*os.Root, error) {
	switch {
	case r.Backend.Shared():
		base, err := r.resolvedHostBase()
		if err != nil {
			return nil, err
		}
		if create {
			leafFd, _, err := shareddirs.EnsureLeafOwned(base, r.RelPath, r.Owner())
			if err != nil {
				return nil, fmt.Errorf("create %s %q: %w", r.Kind, r.Name, err)
			}
			if err := shareddirs.CloseFd(leafFd); err != nil {
				return nil, err
			}
		}
		return shareddirs.OpenAnchoredRoot(base, r.RelPath)
	case r.Backend == BackendLocal:
		return openConfinedDir(r.HostPath, create)
	default:
		return nil, fmt.Errorf("projectfs: cannot open a directory on backend %q", r.Backend)
	}
}

// resolvedHostBase returns HostBase with symlinks resolved. HostBase is the
// operator's mount point and may legitimately sit behind a symlink; the
// components below it are walked without following links.
func (r Resolved) resolvedHostBase() (string, error) {
	if !filepath.IsAbs(r.HostBase) {
		return "", fmt.Errorf("projectfs: host base %q is not absolute", r.HostBase)
	}
	if !filepath.IsLocal(r.RelPath) {
		return "", fmt.Errorf("projectfs: relative path %q is not local", r.RelPath)
	}
	return filepath.EvalSymlinks(r.HostBase)
}

// ResolvedHostBase returns the shared backend's HostBase with symlinks
// resolved, the base pkg/shareddirs' walks take.
func (r Resolved) ResolvedHostBase() (string, error) {
	if !r.Backend.Shared() {
		return "", fmt.Errorf("projectfs: backend %q has no host base", r.Backend)
	}
	return r.resolvedHostBase()
}

// VerifyNoSymlinks is the symlink-escape backstop on the resolved path
// itself, for callers that hand HostPath on (logs, deletion): when the
// directory exists, its real path must equal <real HostBase>/<RelPath>, so
// no structural component is a symlink. A directory that does not exist yet
// passes; it can only be created through Open, whose walk refuses links.
// It returns the real path (or the would-be path when absent).
func (r Resolved) VerifyNoSymlinks() (string, error) {
	base, err := r.ResolvedHostBase()
	if err != nil {
		return "", fmt.Errorf("resolve host base symlinks: %w", err)
	}
	want := filepath.Join(base, r.RelPath)
	got, err := filepath.EvalSymlinks(r.HostPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return want, nil
		}
		return "", fmt.Errorf("resolve %s %q: %w", r.Kind, r.Name, err)
	}
	if got != want {
		return "", fmt.Errorf("%s %q resolves through a symlink to %q, want %q", r.Kind, r.Name, got, want)
	}
	return got, nil
}

// OpenProjectTree opens the directory that holds projectID's project tree on
// a shared backend, <HostBase>/<subpath_root>, as an *os.Root, and returns it
// with the name of the project's entry in it (the project ID). Operations
// through the root never leave it, so a caller acts on the project tree by
// that name through the root (Lstat, RemoveAll), never by a path string.
//
// The root is reached by pkg/shareddirs' anchored, open-only walk from the
// resolved HostBase (OpenAnchoredRoot): every component of the subpath root
// is opened without following a symlink, and the root opened must be the
// directory the walk saw. A component that is a symlink or not a directory,
// or a directory that changed in between, is refused with an error that
// matches shareddirs.ErrUnconfinedPath. A missing HostBase or subpath root
// gives an fs.ErrNotExist-compatible error. Nothing is created.
func (l Layout) OpenProjectTree(projectID string) (*os.Root, string, error) {
	pr, err := l.ProjectRoot(projectID)
	if err != nil {
		return nil, "", err
	}
	base, err := pr.ResolvedHostBase()
	if err != nil {
		return nil, "", err
	}
	parent, err := shareddirs.OpenAnchoredRoot(base, filepath.Dir(pr.RelPath))
	if err != nil {
		return nil, "", err
	}
	return parent, filepath.Base(pr.RelPath), nil
}

// EnsureProjectConfigDir creates (or verifies) a project's configuration
// directory on a shared backend, <ProjectRoot(projectID)>/.scion (beside
// workspace/, inside no Mount), through pkg/shareddirs'
// O_NOFOLLOW component walk, and returns its host path. A newly created
// directory gets mode 2755 and no default ACL: only Scion components write
// it. Missing intermediates are created as the brokers create them. It
// always keeps the zero owner (the creator's uid and gid), never the
// Layout's nfs owner: the directory holds the project's identity
// (project-id, project-slug) and must never be agent-writable.
func (l Layout) EnsureProjectConfigDir(projectID string) (string, error) {
	cfg, err := l.ProjectConfig(projectID)
	if err != nil {
		return "", err
	}
	base, err := cfg.ResolvedHostBase()
	if err != nil {
		return "", err
	}
	fd, _, err := shareddirs.EnsurePrivateDir(base, cfg.RelPath)
	if err != nil {
		return "", fmt.Errorf("create project configuration directory: %w", err)
	}
	if err := shareddirs.CloseFd(fd); err != nil {
		return "", err
	}
	return cfg.HostPath, nil
}

// openConfinedDir opens dir as an *os.Root, refusing a dir whose own final
// component is a symlink. With create, a missing dir is created (0755);
// without it, a missing dir is an fs.ErrNotExist-compatible error.
func openConfinedDir(dir string, create bool) (*os.Root, error) {
	fi, err := os.Lstat(dir)
	switch {
	case err == nil:
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("refusing to serve %q: base path is a symlink", dir)
		}
	case os.IsNotExist(err):
		if !create {
			return nil, err
		}
		if mkErr := os.MkdirAll(dir, 0755); mkErr != nil {
			return nil, mkErr
		}
	default:
		return nil, err
	}
	return os.OpenRoot(dir)
}
