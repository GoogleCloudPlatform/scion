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

package runtimebroker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// errAtomicRenameUnavailable reports that the no-replace rename of the
// project config directory cannot be done atomically (another filesystem,
// or a filesystem without RENAME_NOREPLACE), so the copy path is used.
var errAtomicRenameUnavailable = errors.New("atomic no-replace rename unavailable")

// Test seams for the project config directory move.
var (
	relinkRenameDir = renameDirNoReplace
	relinkVerifyDir = verifyDirCopy
)

// projectRelinkError reports that a resume after a project re-link could
// not carry the agent's project config directory over to the current project
// ID. Error() carries no host path (Path is for logs).
type projectRelinkError struct {
	Path   string
	Reason string
}

func (e *projectRelinkError) Error() string {
	return "the workspace's project-id marker names an earlier ID of this agent's Hub project, and the project config directory could not be moved to the current ID: " +
		e.Reason + "; both directories were left unchanged"
}

// relinkSharedProjectIdentity carries a shared-workspace agent over to its
// project's current Hub ID after the project was re-linked and got a new ID
// (ptone/scion#4440). It runs just before verifySharedProjectIdentity.
//
// It acts only when all of these hold, and otherwise changes nothing (so a
// marker naming a different project still fails the identity check):
//   - the workspace's .scion (project-id file or marker file, read through
//     config.ReadWorkspaceIdentity, which applies the legacy project
//     migration) records a project ID other than hubProjectID, and both IDs
//     are canonical UUIDs (project IDs are validated before any path is
//     derived from them);
//   - it names the same logical project: the recorded slug equals
//     projectSlug when the dispatch carries one (the slug of a .scion
//     directory follows from the workspace path, as for
//     recordHubProjectIdentity);
//   - the workspace is not the broker's global project;
//   - both project config directories, derived through
//     config.ConfinedProjectConfigRoot, are direct children of the
//     project-configs directory;
//   - the project config directory <slug>__<old id8> holds this agent's
//     state directory (.scion/agents/<agentName>).
//   - the earlier project config directory's .scion/settings.yaml records
//     workspace_path naming this workspace (compared after filepath.Clean
//     and filepath.EvalSymlinks on both sides). A missing or different
//     value means no move; nothing else stands in for it. Projects set up
//     with a .scion marker file record it; git projects do not, so they
//     still fail the identity check.
//
// It then moves <slug>__<old id8> to <slug>__<new id8> and rewrites the
// marker to hubProjectID. It refuses, with a projectRelinkError and both
// directories left as they are, when the target directory already exists or
// when an agent of the old project ID may be running. The move never
// replaces an existing directory: it is an atomic no-replace rename, or, when
// that is unavailable, a copy into a newly created target that is verified
// (every entry present with the same type, size and link target) before the
// old directory is removed. Any failure before that removal leaves the old
// directory as it was and removes only the partial copy this call created.
func (s *Server) relinkSharedProjectIdentity(ctx context.Context, projectPath, projectSlug, hubProjectID, agentName string) error {
	if projectPath == "" || agentName == "" || !isCanonicalUUID(hubProjectID) {
		return nil
	}
	if agentName != filepath.Base(agentName) || agentName == "." || agentName == ".." {
		return nil
	}
	abs, err := filepath.Abs(projectPath)
	if err != nil {
		return nil
	}
	workspace := abs
	if filepath.Base(abs) == config.DotScion {
		workspace = filepath.Dir(abs)
	}
	if config.IsGlobalProjectDir(filepath.Join(workspace, config.DotScion)) {
		return nil
	}
	ident, err := config.ReadWorkspaceIdentity(workspace)
	if err != nil || ident == nil || ident.ID == "" || ident.ID == hubProjectID {
		return nil
	}
	log := s.agentLifecycleLog.With("agent", agentName, "new_id", hubProjectID)
	skip := func(reason string) error {
		log.Info("project re-link: not carrying the agent over to the current project ID", "reason", reason)
		return nil
	}
	if !isCanonicalUUID(ident.ID) {
		return skip("the marker's project ID is not a canonical UUID")
	}
	log = log.With("old_id", ident.ID)
	slug := ident.Slug
	if projectSlug != "" && projectSlug != slug {
		return skip("the marker names a different project slug")
	}
	// Both directories are derived through the confined helper from the
	// slug and the validated IDs, never from other marker text.
	oldRoot, ok := config.ConfinedProjectConfigRoot(slug, ident.ID)
	if !ok {
		return skip("the earlier project config directory is not in the expected form")
	}
	newRoot, ok := config.ConfinedProjectConfigRoot(slug, hubProjectID)
	if !ok || newRoot == oldRoot {
		return skip("the current project config directory is not in the expected form")
	}
	if !directlyUnderProjectConfigs(oldRoot) || !directlyUnderProjectConfigs(newRoot) {
		return skip("a project config directory is not directly under the project-configs directory")
	}
	if info, err := os.Lstat(oldRoot); err != nil || !info.IsDir() {
		return skip("no project config directory for the earlier ID")
	}
	if info, err := os.Lstat(filepath.Join(oldRoot, config.DotScion, "agents", agentName)); err != nil || !info.IsDir() {
		return skip("the earlier project config directory holds no state of this agent")
	}
	if !workspacePathRecordedFor(filepath.Join(oldRoot, config.DotScion), workspace) {
		return skip("the earlier project config directory does not record this workspace")
	}
	log.Info("project re-link: the marker names an earlier ID of this project; moving the project config directory", "old", oldRoot, "new", newRoot)
	if _, err := os.Lstat(newRoot); !errors.Is(err, fs.ErrNotExist) {
		log.Warn("project re-link: target project config directory already exists; not moving", "old", oldRoot, "new", newRoot)
		return &projectRelinkError{Path: newRoot, Reason: "a project config directory for the current ID already exists (move or remove one of them by hand)"}
	}
	if busy, err := s.otherProjectAgentsInUse(ctx, ident.ID, ""); err != nil || busy {
		log.Warn("project re-link: an agent of the earlier project ID may be running; not moving", "old", oldRoot)
		return &projectRelinkError{Path: oldRoot, Reason: "an agent of the earlier project ID may still be running (stop it and retry)"}
	}

	commit, rollback, err := moveProjectConfigDir(oldRoot, newRoot)
	if err != nil {
		log.Warn("project re-link: moving the project config directory failed", "old", oldRoot, "new", newRoot, "error", err)
		return &projectRelinkError{Path: oldRoot, Reason: "the move did not complete"}
	}
	if err := ident.Write(slug, hubProjectID); err != nil {
		log.Warn("project re-link: rewriting the project-id marker failed; undoing the move", "path", ident.ScionPath, "error", err)
		if rbErr := rollback(); rbErr != nil {
			log.Error("project re-link: undoing the move failed; the project config directory is at the new location", "old", oldRoot, "new", newRoot, "error", rbErr)
		}
		return &projectRelinkError{Path: ident.ScionPath, Reason: "the project-id marker could not be rewritten"}
	}
	if err := commit(); err != nil {
		log.Warn("project re-link: the earlier project config directory was copied but could not be removed", "old", oldRoot, "error", err)
	}
	log.Info("project re-link: moved the project config directory and rewrote the project-id marker", "old", oldRoot, "new", newRoot, "path", ident.ScionPath)
	return nil
}

// workspacePathRecordedFor reports whether configDir/settings.yaml is a
// regular file whose top-level workspace_path names workspace. Both paths are
// compared after filepath.Clean and filepath.EvalSymlinks; any read, parse or
// resolution error is a mismatch.
func workspacePathRecordedFor(configDir, workspace string) bool {
	settingsPath := filepath.Join(configDir, "settings.yaml")
	info, err := os.Lstat(settingsPath)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return false
	}
	var doc struct {
		WorkspacePath string `yaml:"workspace_path"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil || doc.WorkspacePath == "" || !filepath.IsAbs(doc.WorkspacePath) {
		return false
	}
	recorded, err := filepath.EvalSymlinks(filepath.Clean(doc.WorkspacePath))
	if err != nil {
		return false
	}
	actual, err := filepath.EvalSymlinks(filepath.Clean(workspace))
	if err != nil {
		return false
	}
	return filepath.Clean(recorded) == filepath.Clean(actual)
}

// isCanonicalUUID reports whether id is a UUID in its canonical lower-case
// 8-4-4-4-12 form, the form in which project IDs are minted.
func isCanonicalUUID(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u.String() == id
}

// directlyUnderProjectConfigs reports whether dir, cleaned, is a direct child
// of the broker's project-configs directory.
func directlyUnderProjectConfigs(dir string) bool {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return false
	}
	root := filepath.Clean(filepath.Join(globalDir, config.ProjectConfigsDir))
	clean := filepath.Clean(dir)
	base := filepath.Base(clean)
	return filepath.Dir(clean) == root && base != "." && base != ".." && !strings.ContainsAny(base, "/\\\x00")
}

// projectRelinkStartContextError wraps a relinkSharedProjectIdentity
// failure as a 409 startContextError.
func projectRelinkStartContextError(err error) *startContextError {
	return &startContextError{Status: http.StatusConflict, Message: err.Error(), OriginalErr: err}
}

// moveProjectConfigDir moves the directory oldRoot to newRoot, which must not
// exist. On success the directory is at newRoot; commit finishes the move
// (removing oldRoot after a copy) and rollback undoes it (renaming back, or
// removing the copy). On error oldRoot is unchanged and nothing created by
// this call is left at newRoot.
func moveProjectConfigDir(oldRoot, newRoot string) (commit, rollback func() error, err error) {
	err = relinkRenameDir(oldRoot, newRoot)
	if err == nil {
		return func() error { return nil }, func() error { return relinkRenameDir(newRoot, oldRoot) }, nil
	}
	if !errors.Is(err, errAtomicRenameUnavailable) {
		return nil, nil, err
	}

	info, err := os.Lstat(oldRoot)
	if err != nil {
		return nil, nil, err
	}
	// Mkdir fails when newRoot exists, so the copy never writes into a
	// directory this call did not create.
	if err := os.Mkdir(newRoot, info.Mode().Perm()); err != nil {
		return nil, nil, err
	}
	removeCopy := func() error { return os.RemoveAll(newRoot) }
	if err := copyDirContents(oldRoot, newRoot); err != nil {
		_ = removeCopy()
		return nil, nil, err
	}
	if err := relinkVerifyDir(oldRoot, newRoot); err != nil {
		_ = removeCopy()
		return nil, nil, err
	}
	return func() error { return os.RemoveAll(oldRoot) }, removeCopy, nil
}

// copyDirContents copies the tree under src into the existing directory dst:
// directories, regular files and symlinks (as links), keeping permission
// bits and, where permitted, ownership. Any other entry type is an error.
func copyDirContents(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		switch mode := info.Mode(); {
		case mode.IsDir():
			if err := os.Mkdir(target, mode.Perm()); err != nil {
				return err
			}
		case mode.IsRegular():
			if err := copyRegularFile(path, target, mode.Perm()); err != nil {
				return err
			}
		case mode&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported entry type %s", mode.Type())
		}
		if uid, gid, ok := fileOwner(info); ok {
			_ = os.Lchown(target, uid, gid)
		}
		return nil
	})
}

// copyRegularFile copies src to dst, which must not exist.
func copyRegularFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// verifyDirCopy checks that every entry under src is present under dst with
// the same type, and for regular files the same size and for symlinks the
// same target.
func verifyDirCopy(src, dst string) error {
	return filepath.WalkDir(src, func(path string, _ fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		want, err := os.Lstat(path)
		if err != nil {
			return err
		}
		got, err := os.Lstat(filepath.Join(dst, rel))
		if err != nil {
			return err
		}
		if want.Mode().Type() != got.Mode().Type() {
			return fmt.Errorf("copy of %s has a different type", rel)
		}
		switch {
		case want.Mode().IsRegular():
			if want.Size() != got.Size() {
				return fmt.Errorf("copy of %s has a different size", rel)
			}
		case want.Mode()&fs.ModeSymlink != 0:
			a, err := os.Readlink(path)
			if err != nil {
				return err
			}
			b, err := os.Readlink(filepath.Join(dst, rel))
			if err != nil {
				return err
			}
			if a != b {
				return fmt.Errorf("copy of %s has a different link target", rel)
			}
		}
		return nil
	})
}

// fileOwner returns the owner of info's file, when the platform reports it.
func fileOwner(info fs.FileInfo) (uid, gid int, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
