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
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/gcp"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// clonedProjectIdentityTimeout bounds the hub-start pass that aligns the
// workspace identity of hub-cloned projects with their hub project IDs.
const clonedProjectIdentityTimeout = 5 * time.Minute

// alignOutcome is the result of aligning one workspace identity.
type alignOutcome string

const (
	// alignAlreadyMatching: the workspace identity already is the hub
	// project ID. Nothing was changed.
	alignAlreadyMatching alignOutcome = "already_matching"
	// alignNoIdentity: the workspace has no .scion entry. Nothing was
	// changed.
	alignNoIdentity alignOutcome = "no_identity"
	// alignAligned: the hub project ID was recorded as the workspace
	// identity (after moving the project config directory when there was
	// one to move).
	alignAligned alignOutcome = "aligned"
	// alignSkippedInUse: an agent of the project may be using its project
	// config directory. Nothing was changed.
	alignSkippedInUse alignOutcome = "skipped_in_use"
	// alignSkippedTargetExists: both the current project config directory
	// and the one named after the hub project ID hold files. Nothing was
	// changed; the two need to be reconciled by hand.
	alignSkippedTargetExists alignOutcome = "skipped_target_exists"
	// alignSkippedUnexpectedIdentity: the recorded identity does not name
	// a directory directly under the project-configs directory in the
	// <slug>__<id8> form. Nothing was changed.
	alignSkippedUnexpectedIdentity alignOutcome = "skipped_unexpected_identity"
)

// identityAlignment describes what alignWorkspaceProjectIdentity did.
type identityAlignment struct {
	Outcome alignOutcome
	// Relocated is true when the project config directory was moved to
	// the name derived from the hub project ID.
	Relocated bool
}

// Changed reports whether the workspace identity was rewritten.
func (a identityAlignment) Changed() bool { return a.Outcome == alignAligned }

// alignError is a failure of alignWorkspaceProjectIdentity at a named step.
type alignError struct {
	step string
	err  error
}

func (e *alignError) Error() string { return e.step + ": " + e.err.Error() }
func (e *alignError) Unwrap() error { return e.err }

func alignFailed(step string, err error) error { return &alignError{step: step, err: err} }

// confinedProjectConfigRoot returns ~/.scion/project-configs/<slug>__<id8>
// for slug and projectID. ok is false unless projectID is a valid project
// ID and config.ConfinedProjectConfigRoot accepts slug and projectID (its
// path-element rule includes validateProjectSlug's).
func confinedProjectConfigRoot(slug, projectID string) (root string, ok bool) {
	if !shareddirs.ValidProjectID(projectID) {
		return "", false
	}
	return config.ConfinedProjectConfigRoot(slug, projectID)
}

// alignWorkspaceProjectIdentity makes projectID (the hub project ID) the
// identity of the hub-cloned workspace at workspacePath, so that its project
// config directory is named <slug>__<first 8 chars of projectID>.
//
// Both the recorded identity and the hub record must name a directory
// directly under ~/.scion/project-configs in the <slug>__<id8> form;
// otherwise nothing is changed (alignSkippedUnexpectedIdentity).
//
// When the workspace holds a different identity, its project config
// directory is first moved to the hub-derived name, then the identity is
// rewritten. A hub-derived directory made up only of empty directories is
// replaced. If both directories hold files, nothing is changed
// (alignSkippedTargetExists). Moving before rewriting keeps the operation
// repeatable: a run interrupted between the two steps finishes on the next
// run. Once the identity matches, the call is a no-op.
//
// inUse is called after all checks and immediately before the first change;
// when it reports true, nothing is changed (alignSkippedInUse).
func alignWorkspaceProjectIdentity(workspacePath, slug, projectID string, inUse func() (bool, error)) (identityAlignment, error) {
	current, err := config.ReadWorkspaceIdentity(workspacePath)
	if err != nil {
		return identityAlignment{}, alignFailed("read workspace identity", err)
	}
	if current == nil {
		return identityAlignment{Outcome: alignNoIdentity}, nil
	}
	if current.Matches(slug, projectID) {
		return identityAlignment{Outcome: alignAlreadyMatching}, nil
	}

	target, ok := confinedProjectConfigRoot(slug, projectID)
	if !ok {
		return identityAlignment{Outcome: alignSkippedUnexpectedIdentity}, nil
	}
	var previous string
	if current.ID != "" {
		if previous, ok = confinedProjectConfigRoot(current.Slug, current.ID); !ok {
			return identityAlignment{Outcome: alignSkippedUnexpectedIdentity}, nil
		}
	}

	move, err := planConfigRootMove(previous, target)
	if err != nil {
		return identityAlignment{}, alignFailed("inspect project config directories", err)
	}
	if move == moveBlocked {
		return identityAlignment{Outcome: alignSkippedTargetExists}, nil
	}

	busy, err := inUse()
	if err != nil {
		return identityAlignment{}, alignFailed("list project agents", err)
	}
	if busy {
		return identityAlignment{Outcome: alignSkippedInUse}, nil
	}

	res := identityAlignment{Outcome: alignAligned}
	if move == moveReplaceEmpty {
		if err := removeEmptyDirs(target); err != nil {
			return identityAlignment{}, alignFailed("clear empty project config directory", err)
		}
	}
	if move == moveRename || move == moveReplaceEmpty {
		if err := os.Rename(previous, target); err != nil {
			return identityAlignment{}, alignFailed("move project config directory", err)
		}
		res.Relocated = true
	}
	if err := current.Write(slug, projectID); err != nil {
		return identityAlignment{Outcome: alignAligned, Relocated: res.Relocated}, alignFailed("write workspace identity", err)
	}
	return res, nil
}

// configRootMove is the planned handling of the project config directory.
type configRootMove int

const (
	// moveNone: there is no previous directory to move (none recorded,
	// absent, or the same directory as the target).
	moveNone configRootMove = iota
	// moveRename: the previous directory is moved to the absent target.
	moveRename
	// moveReplaceEmpty: the target holds only empty directories; they are
	// removed and the previous directory is moved in their place.
	moveReplaceEmpty
	// moveBlocked: the target holds files; nothing is moved.
	moveBlocked
)

// planConfigRootMove decides how the project config directory previous is
// brought to target. It only inspects the filesystem.
func planConfigRootMove(previous, target string) (configRootMove, error) {
	if previous == "" || previous == target {
		return moveNone, nil
	}
	if _, err := os.Lstat(previous); err != nil {
		if os.IsNotExist(err) {
			return moveNone, nil
		}
		return moveNone, err
	}
	info, err := os.Lstat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return moveRename, nil
		}
		return moveNone, err
	}
	if !info.IsDir() {
		return moveBlocked, nil
	}
	empty, err := onlyEmptyDirs(target)
	if err != nil {
		return moveNone, err
	}
	if !empty {
		return moveBlocked, nil
	}
	return moveReplaceEmpty, nil
}

// onlyEmptyDirs reports whether root is a directory tree containing nothing
// but directories.
func onlyEmptyDirs(root string) (bool, error) {
	empty := true
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			empty = false
			return filepath.SkipAll
		}
		return nil
	})
	return empty, err
}

// removeEmptyDirs removes a tree made up only of directories, deepest first.
// os.Remove fails on any directory that is not empty, so no file is removed.
func removeEmptyDirs(root string) error {
	var dirs []string
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dirs = append(dirs, path)
		return nil
	}); err != nil {
		return err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, dir := range dirs {
		if err := os.Remove(dir); err != nil {
			return err
		}
	}
	return nil
}

// projectConfigInUse reports whether an agent of the project may be using
// its project config directory: any non-deleted agent whose phase is not
// stopped or error.
func (s *Server) projectConfigInUse(ctx context.Context, projectID string) (bool, error) {
	cursor := ""
	for {
		result, err := s.store.ListAgents(ctx, store.AgentFilter{ProjectID: projectID}, store.ListOptions{
			Limit:          200,
			Cursor:         cursor,
			SkipTotalCount: true,
		})
		if err != nil {
			return false, err
		}
		for _, a := range result.Items {
			switch state.Phase(a.Phase) {
			case state.PhaseStopped, state.PhaseError:
			default:
				return true, nil
			}
		}
		if result.NextCursor == "" {
			return false, nil
		}
		cursor = result.NextCursor
	}
}

// alignErrorAttrs returns log attributes for an alignment failure that carry
// no filesystem paths: the failed step, and for filesystem errors the
// operation and the underlying error only.
func alignErrorAttrs(err error) []any {
	attrs := []any{}
	var ae *alignError
	if errors.As(err, &ae) {
		attrs = append(attrs, "step", ae.step)
	}
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	switch {
	case errors.As(err, &pathErr):
		attrs = append(attrs, "op", pathErr.Op, "error", fmt.Sprint(pathErr.Err))
	case errors.As(err, &linkErr):
		attrs = append(attrs, "op", linkErr.Op, "error", fmt.Sprint(linkErr.Err))
	}
	return attrs
}

// identityAlignmentCounts summarizes one alignClonedProjectIdentities pass.
type identityAlignmentCounts struct {
	aligned, alreadyMatching, noIdentity, skippedInUse, skippedTargetExists,
	skippedUnexpectedIdentity, failed, notReached int
	hubWorkspaceRecorded, hubWorkspaceRecordFailed int
}

func (c *identityAlignmentCounts) add(outcome alignOutcome) {
	switch outcome {
	case alignAligned:
		c.aligned++
	case alignAlreadyMatching:
		c.alreadyMatching++
	case alignNoIdentity:
		c.noIdentity++
	case alignSkippedInUse:
		c.skippedInUse++
	case alignSkippedTargetExists:
		c.skippedTargetExists++
	case alignSkippedUnexpectedIdentity:
		c.skippedUnexpectedIdentity++
	}
}

func (c identityAlignmentCounts) attrs() []any {
	return []any{
		"aligned", c.aligned,
		"already_matching", c.alreadyMatching,
		"no_identity", c.noIdentity,
		"skipped_in_use", c.skippedInUse,
		"skipped_target_exists", c.skippedTargetExists,
		"skipped_unexpected_identity", c.skippedUnexpectedIdentity,
		"failed", c.failed,
		"not_reached", c.notReached,
		"hub_workspace_recorded", c.hubWorkspaceRecorded,
		"hub_workspace_record_failed", c.hubWorkspaceRecordFailed,
	}
}

// startClonedProjectIdentityAlignment runs alignClonedProjectIdentities in
// the background at hub start.
func (s *Server) startClonedProjectIdentityAlignment(ctx context.Context) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("cloned project identity: recovered from panic", "panic", fmt.Sprint(r))
			}
		}()
		ctx, cancel := context.WithTimeout(ctx, clonedProjectIdentityTimeout)
		defer cancel()
		s.alignClonedProjectIdentities(ctx)
	}()
}

// alignClonedProjectIdentities writes the hub workspace record of every
// project whose workspace this hub keeps at the conventional local path (see
// recordHubWorkspace), makes the hub project ID the workspace identity of
// every hub-cloned (shared-workspace git) project on this hub, and logs one
// summary line of counts. Each project that is skipped or
// fails is logged by project ID; a skipped or failed project is handled
// again on the next hub start.
func (s *Server) alignClonedProjectIdentities(ctx context.Context) identityAlignmentCounts {
	logger := s.projectsLogger()
	var counts identityAlignmentCounts
	cursor := ""
	total, seen := 0, 0
	for {
		result, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{
			Limit:  500,
			Cursor: cursor,
		})
		if err != nil {
			if ctx.Err() != nil {
				counts.notReached = total - seen
				logger.Warn("cloned project identity: pass stopped before all projects were reached", counts.attrs()...)
				return counts
			}
			logger.Warn("cloned project identity: failed to list projects", "error", err.Error())
			counts.failed++
			logger.Info("cloned project identity: pass finished", counts.attrs()...)
			return counts
		}
		if cursor == "" {
			total = result.TotalCount
		}
		for i := range result.Items {
			if ctx.Err() != nil {
				counts.notReached = max(total-seen, len(result.Items)-i)
				logger.Warn("cloned project identity: pass stopped before all projects were reached", counts.attrs()...)
				return counts
			}
			seen++
			project := &result.Items[i]
			if recorded, err := s.recordHubWorkspace(project); err != nil {
				counts.hubWorkspaceRecordFailed++
				logger.Warn("cloned project identity: failed to write hub workspace record",
					append([]any{"project_id", project.ID}, alignErrorAttrs(err)...)...)
			} else if recorded {
				counts.hubWorkspaceRecorded++
			}
			if !project.IsSharedWorkspace() {
				continue
			}
			outcome, ok := s.alignClonedProjectIdentity(ctx, project)
			if !ok {
				counts.failed++
				continue
			}
			counts.add(outcome)
		}
		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}
	logger.Info("cloned project identity: pass finished", counts.attrs()...)
	return counts
}

// alignClonedProjectIdentity aligns one hub-cloned project and logs any skip
// or failure by project ID. ok is false when the project failed.
func (s *Server) alignClonedProjectIdentity(ctx context.Context, project *store.Project) (alignOutcome, bool) {
	logger := s.projectsLogger()

	workspacePath, err := s.hubManagedProjectPath(project.Slug)
	if err != nil {
		logger.Warn("cloned project identity: no workspace location for project", "project_id", project.ID)
		return "", false
	}

	res, err := alignWorkspaceProjectIdentity(workspacePath, project.Slug, project.ID, func() (bool, error) {
		return s.projectConfigInUse(ctx, project.ID)
	})
	if err != nil {
		attrs := append([]any{"project_id", project.ID, "relocated", res.Relocated}, alignErrorAttrs(err)...)
		logger.Warn("cloned project identity: failed to align workspace identity", attrs...)
		return "", false
	}

	switch res.Outcome {
	case alignAligned:
		logger.Info("cloned project identity: workspace identity set to hub project ID",
			"project_id", project.ID, "relocated", res.Relocated)
	case alignNoIdentity:
		logger.Info("cloned project identity: workspace has no project identity, skipped", "project_id", project.ID)
	case alignSkippedInUse:
		logger.Info("cloned project identity: project has agents in use, skipped until a later hub start",
			"project_id", project.ID)
	case alignSkippedTargetExists:
		logger.Warn("cloned project identity: project config directory for the hub project ID already holds files, skipped",
			"project_id", project.ID)
	case alignSkippedUnexpectedIdentity:
		logger.Warn("cloned project identity: recorded workspace identity is not in the expected form, skipped",
			"project_id", project.ID)
	}
	return res.Outcome, true
}

// sameExistingPath reports whether a and b name the same existing directory
// once symlinks are resolved.
func sameExistingPath(a, b string) bool {
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		return false
	}
	return filepath.Clean(ra) == filepath.Clean(rb)
}

// recordHubWorkspace writes the hub workspace record
// (<globalDir>/hub-workspaces/<slug>, holding the hub project ID) for a
// project whose workspace this hub keeps at the conventional local path
// ~/.scion/projects/<slug>. recorded is false for projects without a hub
// workspace, or whose hub workspace is elsewhere (e.g. on a shared volume).
func (s *Server) recordHubWorkspace(project *store.Project) (recorded bool, err error) {
	if !syncsHubProjectWorkspace(project) {
		return false, nil
	}
	workspacePath, err := s.hubManagedProjectPath(project.Slug)
	if err != nil {
		return false, err
	}
	local, err := localProjectPath(project.Slug)
	if err != nil {
		return false, err
	}
	if !sameExistingPath(workspacePath, local) {
		return false, nil
	}
	record, err := config.HubWorkspaceRecordPath(project.Slug)
	if err != nil {
		return false, err
	}
	if current, err := config.ReadWorkspaceRecord(record); err == nil && current == project.ID {
		return true, nil
	}
	if err := config.WriteWorkspaceRecord(record, project.ID); err != nil {
		return false, err
	}
	return true, nil
}

// syncFromGCSIntoHubWorkspace downloads a workspace upload into a hub
// workspace. A variable so tests can stand in for GCS.
var syncFromGCSIntoHubWorkspace = gcp.SyncFromGCS

// Errors returned by syncHubWorkspaceFromGCS. They carry no paths; details
// are logged by project-neutral step, operation and errno.
var (
	errHubIdentityUnreadable = errors.New("hub workspace identity could not be read; workspace download skipped")
	errHubIdentityNotKept    = errors.New("hub workspace identity could not be kept after workspace download")
)

// syncHubWorkspaceFromGCS downloads a broker's workspace upload into the hub
// workspace at workspacePath, keeping the hub workspace's own project
// identity as it was before the download: the .scion entry keeps its form
// (marker file, directory, or none) and its identity content (marker
// content, or .scion/project-id content or absence). The hub workspace
// identity is set only by the hub itself. When the identity cannot be read
// beforehand, nothing is downloaded; when it cannot be put back, an error
// is returned.
func (s *Server) syncHubWorkspaceFromGCS(ctx context.Context, bucket, prefix, workspacePath string) error {
	saved, err := captureWorkspaceIdentity(workspacePath)
	if err != nil {
		s.workspaceLog.Warn("hub workspace identity could not be read, workspace download skipped",
			alignErrorAttrs(alignFailed("read hub workspace identity", err))...)
		return errHubIdentityUnreadable
	}
	syncErr := syncFromGCSIntoHubWorkspace(ctx, bucket, prefix, workspacePath)
	if err := saved.restore(); err != nil {
		s.workspaceLog.Warn("hub workspace identity could not be kept after workspace download",
			alignErrorAttrs(alignFailed("keep hub workspace identity", err))...)
		return errors.Join(syncErr, errHubIdentityNotKept)
	}
	return syncErr
}

// identityEntryKind is the on-disk form of a workspace .scion entry.
type identityEntryKind int

const (
	identityEntryNone identityEntryKind = iota
	identityEntryMarker
	identityEntryDir
)

// savedWorkspaceIdentity is a workspace's project identity as captured by
// captureWorkspaceIdentity.
type savedWorkspaceIdentity struct {
	scionPath string
	kind      identityEntryKind
	// content is the marker file content (identityEntryMarker) or the
	// project-id file content (identityEntryDir with hasProjectID).
	content      []byte
	hasProjectID bool
}

func captureWorkspaceIdentity(workspacePath string) (*savedWorkspaceIdentity, error) {
	saved := &savedWorkspaceIdentity{scionPath: filepath.Join(workspacePath, config.DotScion)}
	info, err := os.Lstat(saved.scionPath)
	switch {
	case os.IsNotExist(err):
		return saved, nil
	case err != nil:
		return nil, err
	case info.Mode().IsRegular():
		saved.kind = identityEntryMarker
		saved.content, err = os.ReadFile(saved.scionPath)
		return saved, err
	case info.IsDir():
		saved.kind = identityEntryDir
		data, err := os.ReadFile(saved.projectIDPath())
		switch {
		case err == nil:
			saved.content, saved.hasProjectID = data, true
		case !os.IsNotExist(err):
			return nil, err
		}
		return saved, nil
	default:
		return nil, fmt.Errorf("unsupported .scion entry type")
	}
}

func (w *savedWorkspaceIdentity) projectIDPath() string {
	return filepath.Join(w.scionPath, projectkeys.ProjectIDFile)
}

// restore puts the captured identity back. The captured form is
// authoritative: a .scion entry of another form left by the download is
// removed first (the hub workspace had no entry of that form, so nothing the
// hub kept is lost). Files other than the identity inside a .scion
// directory are left as the download wrote them.
func (w *savedWorkspaceIdentity) restore() error {
	info, err := os.Lstat(w.scionPath)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	switch w.kind {
	case identityEntryNone:
		if !exists {
			return nil
		}
		return os.RemoveAll(w.scionPath)

	case identityEntryMarker:
		// writeIfDifferent replaces an entry of another form.
		return writeIfDifferent(w.scionPath, w.content)

	case identityEntryDir:
		if exists && !info.IsDir() {
			if err := os.Remove(w.scionPath); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(w.scionPath, 0755); err != nil {
			return err
		}
		if !w.hasProjectID {
			return os.RemoveAll(w.projectIDPath())
		}
		return writeIfDifferent(w.projectIDPath(), w.content)
	}
	return nil
}

// writeIfDifferent makes path a regular file holding content. An entry of
// another type at path (directory, symlink) is removed first, so the write
// never goes through it.
func writeIfDifferent(path string, content []byte) error {
	info, err := os.Lstat(path)
	switch {
	case err == nil && !info.Mode().IsRegular():
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	case err == nil:
		if current, err := os.ReadFile(path); err == nil && string(current) == string(content) {
			return nil
		}
	case !os.IsNotExist(err):
		return err
	}
	return os.WriteFile(path, content, 0644)
}
