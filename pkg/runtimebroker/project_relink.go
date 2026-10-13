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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// errAtomicRenameUnavailable reports that the no-replace rename of the
// project config directory cannot be done atomically (another filesystem,
// or a filesystem without RENAME_NOREPLACE), so the copy path is used.
var errAtomicRenameUnavailable = errors.New("atomic no-replace rename unavailable")

// Test seams for the project config directory move.
var (
	relinkRenameDir   = renameDirNoReplace
	relinkVerifyDir   = verifyDirCopy
	relinkWriteMarker = writeWorkspaceIdentityNoFollow
	relinkRemoveTree  = removeTreeWritable
	relinkSyncDir     = syncDir
)

// errPartialCopyRemains reports that a copy made for the current ID could not
// be removed after a failure; the earlier directory is intact.
var errPartialCopyRemains = errors.New("a partial copy remains for the current ID")

// errMovedNotUndone reports that the directory was renamed to the current ID
// and could not be renamed back after a later failure.
var errMovedNotUndone = errors.New("the move could not be undone")

// projectRelinkError reports that a resume after a project re-link could
// not carry the agent's project config directory over to the current project
// ID. Error() carries no host path (Path is for logs).
type projectRelinkError struct {
	Path   string
	Reason string
	// Moved is set when the directory was moved but the marker could not be
	// rewritten and the move could not be undone.
	Moved bool
	// PartialCopy is set when a copy for the current ID could not be removed
	// after a failure; the earlier directory is intact.
	PartialCopy bool
}

func (e *projectRelinkError) Error() string {
	if e.PartialCopy {
		return "the workspace's project-id marker names an earlier ID of this agent's Hub project, and the project config directory could not be moved to the current ID: " +
			e.Reason + "; the earlier directory is unchanged, but a partial copy remains for the current ID; remove it by hand"
	}
	if e.Moved {
		return "the workspace's project-id marker names an earlier ID of this agent's Hub project; the project config directory was moved to the current ID, but " +
			e.Reason + " and the move could not be undone; the next start retries the marker rewrite"
	}
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
//     expectedProjectConfigDir (slug grammar and short-ID grammar), are
//     direct children of the project-configs directory;
//   - the project config directory <slug>__<old id8> holds this agent's
//     state directory (.scion/agents/<agentName>), with no symlinked
//     component below project-configs (projectConfigPathContained);
//   - the earlier project config directory's .scion/settings.yaml records
//     workspace_path naming this workspace (compared exactly after
//     filepath.Clean, without resolving symlinks). A missing or different
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
// (every entry present with the same type, mode, size and link target, and
// the new directories synced to disk) before the
// old directory is removed. Any failure before that removal leaves the old
// directory as it was and removes only the partial copy this call created.
//
// When an earlier call moved the directory but did not rewrite the marker
// (the broker stopped in between, or the rewrite and the undo both failed),
// the earlier directory is absent and the current one holds the agent's
// state. If the current directory passes the same checks (agent state,
// containment, recorded workspace_path), only the marker is rewritten.
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
	// One re-link per workspace at a time in this broker: everything below
	// (the decision, the move, the marker write, the recovery) runs under
	// the workspace's lock, and the state is read only after taking it, so
	// a start that waited sees what an earlier one did.
	unlock := lockRelinkWorkspace(filepath.Clean(workspace))
	defer unlock()
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
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return skip("the global directory cannot be resolved")
	}
	// Both directories are derived from the slug and the validated IDs
	// through the package's project-configs helper, never from other marker
	// text.
	oldRoot := expectedProjectConfigDir(globalDir, slug, ident.ID)
	newRoot := expectedProjectConfigDir(globalDir, slug, hubProjectID)
	if oldRoot == "" || newRoot == "" || newRoot == oldRoot {
		return skip("a project config directory is not in the expected form")
	}
	if !directlyUnderProjectConfigs(oldRoot) || !directlyUnderProjectConfigs(newRoot) {
		return skip("a project config directory is not directly under the project-configs directory")
	}
	if _, err := os.Lstat(oldRoot); errors.Is(err, fs.ErrNotExist) {
		return s.finishInterruptedRelink(log, ident, slug, hubProjectID, globalDir, newRoot, workspace, agentName)
	}
	if !projectConfigPathContained(globalDir, oldRoot) {
		return skip("no project config directory for the earlier ID")
	}
	if !projectConfigPathContained(globalDir, filepath.Join(oldRoot, config.DotScion, "agents", agentName)) {
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

	relinkBeforeMove()
	commit, rollback, err := moveProjectConfigDir(oldRoot, newRoot)
	if err != nil {
		log.Warn("project re-link: moving the project config directory failed", "old", oldRoot, "new", newRoot, "error", err)
		if errors.Is(err, errPartialCopyRemains) {
			return &projectRelinkError{Path: newRoot, Reason: "the move did not complete", PartialCopy: true}
		}
		if errors.Is(err, errMovedNotUndone) {
			return &projectRelinkError{Path: newRoot, Reason: "syncing the move to disk failed", Moved: true}
		}
		return &projectRelinkError{Path: oldRoot, Reason: "the move did not complete"}
	}
	if err := relinkWriteMarker(ident, slug, hubProjectID); errors.Is(err, errMarkerWrittenNotSynced) {
		log.Warn("project re-link: the project-id marker was rewritten, but syncing it to disk failed; keeping the move", "path", ident.ScionPath, "error", err)
	} else if err != nil {
		log.Warn("project re-link: rewriting the project-id marker failed; undoing the move", "path", ident.ScionPath, "error", err)
		if rbErr := rollback(); rbErr != nil {
			if errors.Is(rbErr, errPartialCopyRemains) {
				log.Error("project re-link: removing the copy failed; the earlier directory is intact and a partial copy remains", "old", oldRoot, "new", newRoot, "error", rbErr)
				return &projectRelinkError{Path: newRoot, Reason: "the project-id marker could not be rewritten", PartialCopy: true}
			}
			log.Error("project re-link: undoing the move failed; the project config directory is at the new location", "old", oldRoot, "new", newRoot, "error", rbErr)
			return &projectRelinkError{Path: ident.ScionPath, Reason: "the project-id marker could not be rewritten", Moved: true}
		}
		return &projectRelinkError{Path: ident.ScionPath, Reason: "the project-id marker could not be rewritten"}
	}
	if err := commit(); err != nil {
		log.Error("project re-link: the agent now uses the current ID, but the earlier project config directory could not be fully removed; remove it by hand", "old", oldRoot, "error", err)
	}
	log.Info("project re-link: moved the project config directory and rewrote the project-id marker", "old", oldRoot, "new", newRoot, "path", ident.ScionPath)
	return nil
}

// finishInterruptedRelink rewrites the marker to hubProjectID when the
// earlier project config directory is absent and newRoot, the current one,
// holds this agent's state with no symlinked component and records this
// workspace: a move that completed without its marker rewrite. Otherwise it
// changes nothing.
func (s *Server) finishInterruptedRelink(log *slog.Logger, ident *config.WorkspaceIdentity, slug, hubProjectID, globalDir, newRoot, workspace, agentName string) error {
	if !projectConfigPathContained(globalDir, filepath.Join(newRoot, config.DotScion, "agents", agentName)) ||
		!workspacePathRecordedFor(filepath.Join(newRoot, config.DotScion), workspace) {
		log.Info("project re-link: not carrying the agent over to the current project ID", "reason", "no project config directory for the earlier ID")
		return nil
	}
	if err := relinkWriteMarker(ident, slug, hubProjectID); errors.Is(err, errMarkerWrittenNotSynced) {
		log.Warn("project re-link: the project-id marker was rewritten, but syncing it to disk failed", "path", ident.ScionPath, "error", err)
	} else if err != nil {
		log.Warn("project re-link: rewriting the project-id marker after an earlier move failed", "path", ident.ScionPath, "error", err)
		return &projectRelinkError{Path: ident.ScionPath, Reason: "the project-id marker could not be rewritten", Moved: true}
	}
	log.Info("project re-link: the project config directory was already moved; rewrote the project-id marker", "new", newRoot, "path", ident.ScionPath)
	return nil
}

// workspacePathRecordedFor reports whether configDir/settings.yaml is a
// regular file whose top-level workspace_path names workspace. The recorded
// path must be absolute, and the two paths are compared exactly after
// filepath.Clean, with no symlink resolution: a workspace recorded or
// dispatched through a symlink does not match. Any read or parse error is a
// mismatch.
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
	return filepath.IsAbs(workspace) && filepath.Clean(doc.WorkspacePath) == filepath.Clean(workspace)
}

// relinkWorkspaceLocks holds a lock per workspace path (cleaned and
// absolute) for the workspaces with a re-link in progress or waiting; an
// entry is dropped when its last holder or waiter is done.
var (
	relinkWorkspaceLocksMu sync.Mutex
	relinkWorkspaceLocks   = map[string]*relinkWorkspaceLock{}
)

type relinkWorkspaceLock struct {
	mu   sync.Mutex
	refs int
}

// lockRelinkWorkspace takes the re-link lock of workspace and returns the
// function that releases it.
func lockRelinkWorkspace(workspace string) func() {
	relinkWorkspaceLocksMu.Lock()
	l := relinkWorkspaceLocks[workspace]
	if l == nil {
		l = &relinkWorkspaceLock{}
		relinkWorkspaceLocks[workspace] = l
	}
	l.refs++
	relinkWorkspaceLocksMu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		relinkWorkspaceLocksMu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(relinkWorkspaceLocks, workspace)
		}
		relinkWorkspaceLocksMu.Unlock()
	}
}

// relinkTempPrefix names the temporary files the marker write creates next
// to the marker.
const relinkTempPrefix = ".scion-relink-"

// errMarkerWrittenNotSynced reports that the marker was rewritten (the
// rename landed) but syncing its directory failed: the rewrite stands, only
// its durability is uncertain. Callers treat it as written.
var errMarkerWrittenNotSynced = errors.New("the project-id marker was rewritten, but syncing its directory failed")

// Test seams for the marker write.
var (
	// relinkMarkerDirSync syncs the marker's directory after the rename.
	relinkMarkerDirSync = func(f *os.File) error { return f.Sync() }
	// relinkBeforeMarkerWrite runs after the marker write's checks and
	// before it creates its temporary file.
	relinkBeforeMarkerWrite = func() {}
	// relinkBeforeMove runs after the re-link's checks and before the move.
	relinkBeforeMove = func() {}
	// relinkStaleTempAge is how old a temporary marker file must be before
	// the cleanup removes it, so a write in progress elsewhere is not
	// disturbed.
	relinkStaleTempAge = 2 * time.Minute
)

// writeWorkspaceIdentityNoFollow records projectID (and, for a marker file,
// slug) as ident's identity, in the form the entry already has. Every file
// operation goes through an os.Root opened on the workspace, so nothing
// outside the workspace can be written. The content goes to a new temporary
// file (relinkTempPrefix, created exclusively) in the marker's own directory,
// which is synced and renamed over the marker: a symlink at the marker path
// is replaced, not written through. Stale temporary files from an earlier
// interrupted write are removed first. The directory holding a project-id
// file must be a real directory, and the marker must not be a directory.
//
// Once the rename has landed the marker is rewritten; a failure to sync the
// directory after it is returned as errMarkerWrittenNotSynced.
func writeWorkspaceIdentityNoFollow(ident *config.WorkspaceIdentity, slug, projectID string) error {
	workspace := filepath.Dir(ident.ScionPath)
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	base := "."

	var dirRel, targetRel string
	var data []byte
	if ident.Marker == nil {
		dirRel = filepath.Join(base, filepath.Base(ident.ScionPath))
		targetRel = filepath.Join(dirRel, projectkeys.ProjectIDFile)
		data = []byte(projectID + "\n")
		if info, err := root.Lstat(dirRel); err != nil {
			return err
		} else if !info.IsDir() {
			return fmt.Errorf("the directory holding the project-id marker is not a directory")
		}
	} else {
		updated := *ident.Marker
		updated.ProjectID = projectID
		updated.ProjectSlug = slug
		if data, err = yaml.Marshal(&updated); err != nil {
			return err
		}
		dirRel = base
		targetRel = filepath.Join(base, filepath.Base(ident.ScionPath))
	}
	if info, err := root.Lstat(targetRel); err == nil && info.IsDir() {
		return fmt.Errorf("the project-id marker is a directory")
	}
	removeStaleRelinkTemps(root, dirRel)
	relinkBeforeMarkerWrite()

	var tmpRel string
	var tmp *os.File
	for i := 0; i < 10 && tmp == nil; i++ {
		var rnd [8]byte
		if _, err := rand.Read(rnd[:]); err != nil {
			return err
		}
		tmpRel = filepath.Join(dirRel, relinkTempPrefix+hex.EncodeToString(rnd[:]))
		tmp, err = root.OpenFile(tmpRel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	if tmp == nil {
		return fmt.Errorf("could not create a temporary marker file")
	}
	cleanup := func() { _ = root.Remove(tmpRel) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(0644); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := root.Rename(tmpRel, targetRel); err != nil {
		cleanup()
		return err
	}
	// The marker is rewritten from here on.
	d, err := root.Open(dirRel)
	if err != nil {
		return fmt.Errorf("%w: %v", errMarkerWrittenNotSynced, err)
	}
	defer func() { _ = d.Close() }()
	if err := relinkMarkerDirSync(d); err != nil {
		return fmt.Errorf("%w: %v", errMarkerWrittenNotSynced, err)
	}
	return nil
}

// removeStaleRelinkTemps removes regular files named relinkTempPrefix* in
// dirRel, through root, left by an interrupted marker write: only files
// last modified more than relinkStaleTempAge ago. Symlinks and directories
// are kept.
func removeStaleRelinkTemps(root *os.Root, dirRel string) {
	d, err := root.Open(dirRel)
	if err != nil {
		return
	}
	entries, err := d.ReadDir(-1)
	_ = d.Close()
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), relinkTempPrefix) {
			continue
		}
		rel := filepath.Join(dirRel, e.Name())
		if info, err := root.Lstat(rel); err == nil && info.Mode().IsRegular() && time.Since(info.ModTime()) > relinkStaleTempAge {
			_ = root.Remove(rel)
		}
	}
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
		undo := func() error { return relinkRenameDir(newRoot, oldRoot) }
		// The rename reaches the disk before the marker is rewritten.
		if err := relinkSyncDir(filepath.Dir(newRoot)); err != nil {
			if uErr := undo(); uErr != nil {
				return nil, nil, fmt.Errorf("syncing after the rename: %v; %w: %v", err, errMovedNotUndone, uErr)
			}
			return nil, nil, err
		}
		return func() error { return nil }, undo, nil
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
	if err := os.Mkdir(newRoot, 0700); err != nil {
		return nil, nil, err
	}
	removeCopy := func() error {
		if err := relinkRemoveTree(newRoot); err != nil {
			return fmt.Errorf("%w: %v", errPartialCopyRemains, err)
		}
		return nil
	}
	// fail removes the copy and returns err, or errPartialCopyRemains when
	// the copy cannot be removed.
	fail := func(err error) (func() error, func() error, error) {
		if rmErr := removeCopy(); rmErr != nil {
			return nil, nil, fmt.Errorf("%v; %w", err, rmErr)
		}
		return nil, nil, err
	}
	if err := copyDirContents(oldRoot, newRoot); err != nil {
		return fail(err)
	}
	if uid, gid, ok := fileOwner(info); ok {
		_ = os.Lchown(newRoot, uid, gid)
	}
	if err := os.Chmod(newRoot, copiedMode(info.Mode())); err != nil {
		return fail(err)
	}
	if err := relinkVerifyDir(oldRoot, newRoot); err != nil {
		return fail(err)
	}
	// The copy's directory entries reach the disk before the earlier
	// directory can be removed.
	if err := syncDirTree(newRoot); err != nil {
		return fail(err)
	}
	if err := syncDir(filepath.Dir(newRoot)); err != nil {
		return fail(err)
	}
	return func() error { return relinkRemoveTree(oldRoot) }, removeCopy, nil
}

// copyDirContents copies the tree under src into the existing directory dst:
// directories, regular files and symlinks (as links), keeping permission
// bits and, where permitted, ownership. Any other entry type is an error.
func copyDirContents(src, dst string) error {
	// Directory modes are set after the walk, deepest first, so a
	// read-only source directory does not stop its entries being copied.
	type dirMode struct {
		path string
		mode fs.FileMode
	}
	var dirs []dirMode
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
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
			if err := os.Mkdir(target, 0700); err != nil {
				return err
			}
			dirs = append(dirs, dirMode{target, mode})
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
		if info.Mode().IsRegular() {
			// OpenFile's mode is masked by the umask, and a change of owner
			// can clear setgid: set the source's mode explicitly.
			if err := os.Chmod(target, copiedMode(info.Mode())); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i].path, copiedMode(dirs[i].mode)); err != nil {
			return err
		}
	}
	return nil
}

// removeTreeWritable removes the tree at dir, first making its directories
// writable so read-only directory modes do not stop the removal. It is used
// for a copy this package created and, after a verified copy, for the
// earlier directory.
func removeTreeWritable(dir string) error {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr == nil && d.IsDir() {
			_ = os.Chmod(path, 0700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

// copiedMode is the part of a mode the copy path reproduces and verifies:
// permission bits plus setgid and sticky.
func copiedMode(m fs.FileMode) fs.FileMode {
	return m & (fs.ModePerm | fs.ModeSetgid | fs.ModeSticky)
}

// syncDirTree fsyncs every directory in the tree under root, root included.
func syncDirTree(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() {
			return nil
		}
		return syncDir(path)
	})
}

// syncDir fsyncs the directory dir.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
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
		if want.Mode()&fs.ModeSymlink == 0 && copiedMode(want.Mode()) != copiedMode(got.Mode()) {
			return fmt.Errorf("copy of %s has a different mode", rel)
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
