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

package hub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// snapshotWorkspaceDir copies the directory workspacePath (relative to the
// project's workspace root, e.g. "/.scion/templates") into a private
// temporary directory and returns it, with a cleanup func the caller must
// call. The workspace can be agent-writable (the uuid tree on a shared
// backend, or the workspace on the local backend), so every read goes
// through an *os.Root: the workspace is opened with Resolved.Open, the
// sub-directory with Root.OpenRoot (a path or symlink leading out of it is
// refused), and the tree is walked through the root's FS. Symbolic links
// and other non-regular files inside it are skipped, never followed. The
// copy is bounded (workspaceSnapshotMaxFiles, workspaceSnapshotMaxBytes).
// The import and discovery then read only the private copy, so nothing the
// workspace changes afterwards (a swapped symlink) can redirect them.
// Errors carry no host path.
func (s *Server) snapshotWorkspaceDir(ctx context.Context, project *store.Project, workspacePath string) (string, func(), error) {
	resolved, err := s.resolveProjectWorkspace(ctx, project)
	if err != nil {
		return "", nil, fmt.Errorf("failed to resolve project workspace: %w", err)
	}
	// As before: a leading slash means the workspace root; a path that
	// cleans to outside it is refused.
	rel := strings.TrimPrefix(filepath.Clean(workspacePath), "/")
	if rel == "" {
		rel = "."
	}
	if rel != "." && !filepath.IsLocal(rel) {
		return "", nil, fmt.Errorf("workspace path must be within the project workspace")
	}
	root, err := resolved.Open(false)
	if err != nil {
		return "", nil, fmt.Errorf("workspace path not found or not a directory: %s", workspacePath)
	}
	defer func() { _ = root.Close() }()
	sub, err := root.OpenRoot(rel)
	if err != nil {
		return "", nil, fmt.Errorf("workspace path not found or not a directory: %s", workspacePath)
	}
	defer func() { _ = sub.Close() }()

	tmp, err := os.MkdirTemp("", "scion-workspace-import-")
	if err != nil {
		return "", nil, fmt.Errorf("failed to prepare the workspace import")
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	// The copy keeps the directory's own base name: discovery names a
	// resource directory given directly by its base name.
	base := path.Base(rel)
	if rel == "." {
		base = "workspace"
	}
	dstRoot := filepath.Join(tmp, base)
	if err := os.Mkdir(dstRoot, 0o755); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("failed to prepare the workspace import")
	}
	files, total := 0, int64(0)
	walkErr := fs.WalkDir(sub.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		dst := filepath.Join(dstRoot, filepath.FromSlash(p))
		switch {
		case d.IsDir():
			return os.Mkdir(dst, 0o755)
		case !d.Type().IsRegular():
			return nil // symlinks and special files are skipped
		}
		files++
		if files > workspaceSnapshotMaxFiles {
			return errWorkspaceSnapshotTooLarge
		}
		// Opened non-blocking and without following a final symlink, and
		// checked to still be a regular file: an entry swapped for a FIFO
		// or a link after the walk classified it is skipped, never read
		// (a FIFO would block the open or the read).
		in, err := sub.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
		if err != nil {
			if errors.Is(err, syscall.ELOOP) {
				return nil
			}
			return err
		}
		defer func() { _ = in.Close() }()
		if fi, err := in.Stat(); err != nil {
			return err
		} else if !fi.Mode().IsRegular() {
			return nil
		}
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		n, err := io.Copy(out, io.LimitReader(in, workspaceSnapshotMaxBytes-total+1))
		total += n
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if total > workspaceSnapshotMaxBytes {
			return errWorkspaceSnapshotTooLarge
		}
		return nil
	})
	if walkErr != nil {
		cleanup()
		if errors.Is(walkErr, errWorkspaceSnapshotTooLarge) {
			return "", nil, walkErr
		}
		s.projectsLogger().Warn("workspace import snapshot failed", "project_id", project.ID, "error", walkErr)
		return "", nil, fmt.Errorf("workspace path could not be read: %s", workspacePath)
	}
	return dstRoot, cleanup, nil
}
