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
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// clonedProjectIdentityTimeout bounds the hub-start pass that aligns the
// workspace identity of hub-cloned projects with their hub project IDs.
const clonedProjectIdentityTimeout = 5 * time.Minute

// identityAlignment describes what alignWorkspaceProjectIdentity changed.
type identityAlignment struct {
	// Changed is true when the workspace identity was rewritten.
	Changed bool
	// PreviousID is the identity the workspace held before the rewrite.
	PreviousID string
	// ConfigDir is the project config directory named after the hub
	// project ID (~/.scion/project-configs/<slug>__<id8>).
	ConfigDir string
	// Relocated is true when the previous project config directory was
	// moved to ConfigDir.
	Relocated bool
	// RetainedDir is set when the previous project config directory was
	// left in place because ConfigDir already held files.
	RetainedDir string
}

// workspaceIdentity is the project identity recorded in a workspace's .scion
// entry, in either of its two on-disk forms.
type workspaceIdentity struct {
	scionPath string
	// marker is set when .scion is a marker file; nil when it is a
	// directory holding a project-id file.
	marker *config.ProjectMarker
	id     string
	slug   string
}

// readWorkspaceIdentity reads the project identity of the workspace at
// workspacePath. It returns (nil, nil) when the workspace has no .scion entry.
func readWorkspaceIdentity(workspacePath string) (*workspaceIdentity, error) {
	scionPath := filepath.Join(workspacePath, config.DotScion)
	info, err := os.Stat(scionPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	if !info.IsDir() {
		marker, err := config.ReadProjectMarker(scionPath)
		if err != nil {
			return nil, err
		}
		return &workspaceIdentity{scionPath: scionPath, marker: marker, id: marker.ProjectID, slug: marker.ProjectSlug}, nil
	}

	id, err := config.ReadProjectID(scionPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return &workspaceIdentity{
		scionPath: scionPath,
		id:        id,
		slug:      api.Slugify(config.GetProjectName(scionPath)),
	}, nil
}

// matches reports whether the identity already is projectID under slug.
func (w *workspaceIdentity) matches(slug, projectID string) bool {
	if w.id != projectID {
		return false
	}
	return w.marker == nil || w.slug == slug
}

// write records projectID (and, for marker files, slug) as the identity.
func (w *workspaceIdentity) write(slug, projectID string) error {
	if w.marker == nil {
		return config.WriteProjectID(w.scionPath, projectID)
	}
	updated := *w.marker
	updated.ProjectID = projectID
	updated.ProjectSlug = slug
	return config.WriteProjectMarker(w.scionPath, &updated)
}

// projectConfigRoot returns ~/.scion/project-configs/<slug>__<id8>.
func projectConfigRoot(slug, projectID string) (string, error) {
	ext, err := config.ProjectMarker{ProjectID: projectID, ProjectSlug: slug}.ExternalProjectPath()
	if err != nil {
		return "", err
	}
	return filepath.Dir(ext), nil
}

// alignWorkspaceProjectIdentity makes projectID (the hub project ID) the
// identity of the hub-cloned workspace at workspacePath, so that its project
// config directory is named <slug>__<first 8 chars of projectID>.
//
// When the workspace holds a different identity, its project config
// directory is first moved to the hub-derived name, then the identity is
// rewritten. If the hub-derived directory already holds files, nothing is
// moved: the previous directory is kept as is and reported in RetainedDir.
// Moving before rewriting keeps the operation repeatable: a run interrupted
// between the two steps finishes on the next run. Once the identity matches,
// the call is a no-op.
//
// The caller must ensure no agent of the project is using the project
// config directory.
func alignWorkspaceProjectIdentity(workspacePath, slug, projectID string) (identityAlignment, error) {
	var res identityAlignment
	if projectID == "" || slug == "" {
		return res, fmt.Errorf("project ID and slug are required")
	}

	current, err := readWorkspaceIdentity(workspacePath)
	if err != nil {
		return res, fmt.Errorf("read workspace project identity: %w", err)
	}
	if current == nil || current.matches(slug, projectID) {
		return res, nil
	}

	target, err := projectConfigRoot(slug, projectID)
	if err != nil {
		return res, err
	}
	res.ConfigDir = target
	res.PreviousID = current.id

	if current.id != "" && current.slug != "" {
		previous, err := projectConfigRoot(current.slug, current.id)
		if err != nil {
			return res, err
		}
		relocated, retained, err := moveProjectConfigRoot(previous, target)
		if err != nil {
			return res, err
		}
		res.Relocated = relocated
		if retained {
			res.RetainedDir = previous
		}
	}

	if err := current.write(slug, projectID); err != nil {
		return res, fmt.Errorf("write workspace project identity: %w", err)
	}
	res.Changed = true
	return res, nil
}

// moveProjectConfigRoot moves the project config directory previous to
// target. It does nothing when previous does not exist or both name the same
// directory. A target made up only of empty directories is replaced; a target
// holding any file is kept and retained is returned true.
func moveProjectConfigRoot(previous, target string) (relocated, retained bool, err error) {
	if filepath.Clean(previous) == filepath.Clean(target) {
		return false, false, nil
	}
	if _, err := os.Lstat(previous); err != nil {
		if os.IsNotExist(err) {
			return false, false, nil
		}
		return false, false, err
	}

	if _, err := os.Lstat(target); err == nil {
		empty, err := onlyEmptyDirs(target)
		if err != nil {
			return false, false, err
		}
		if !empty {
			return false, true, nil
		}
		if err := removeEmptyDirs(target); err != nil {
			return false, false, err
		}
	} else if !os.IsNotExist(err) {
		return false, false, err
	}

	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return false, false, err
	}
	if err := os.Rename(previous, target); err != nil {
		return false, false, fmt.Errorf("move project config directory: %w", err)
	}
	return true, false, nil
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
// its project config directory. Agents that are created but not started,
// stopped, or in error do not hold it; any other phase does.
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
			case state.PhaseCreated, state.PhaseStopped, state.PhaseError:
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

// alignClonedProjectIdentities makes the hub project ID the workspace
// identity of every hub-cloned (shared-workspace git) project on this hub.
// A project whose identity already matches is left untouched. A project with
// an agent that may be using its project config directory is skipped and
// handled on a later hub start. Failures are logged per project and do not
// stop the pass.
func (s *Server) alignClonedProjectIdentities(ctx context.Context) {
	logger := s.projectsLogger()
	const pageSize = 500
	cursor := ""
	for {
		result, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{
			Limit:  pageSize,
			Cursor: cursor,
		})
		if err != nil {
			logger.Warn("cloned project identity: failed to list projects", "error", err.Error())
			return
		}
		for i := range result.Items {
			if ctx.Err() != nil {
				return
			}
			s.alignClonedProjectIdentity(ctx, &result.Items[i])
		}
		if result.NextCursor == "" || len(result.Items) < pageSize {
			return
		}
		cursor = result.NextCursor
	}
}

// alignClonedProjectIdentity aligns one project; see
// alignClonedProjectIdentities.
func (s *Server) alignClonedProjectIdentity(ctx context.Context, project *store.Project) {
	if !project.IsSharedWorkspace() {
		return
	}
	logger := s.projectsLogger()

	workspacePath, err := s.hubManagedProjectPath(project.Slug)
	if err != nil {
		return
	}
	current, err := readWorkspaceIdentity(workspacePath)
	if err != nil {
		logger.Warn("cloned project identity: failed to read workspace identity",
			"project_id", project.ID, "error", err.Error())
		return
	}
	if current == nil || current.matches(project.Slug, project.ID) {
		return
	}

	inUse, err := s.projectConfigInUse(ctx, project.ID)
	if err != nil {
		logger.Warn("cloned project identity: failed to list project agents",
			"project_id", project.ID, "error", err.Error())
		return
	}
	if inUse {
		logger.Info("cloned project identity: project has agents in use, will retry on next hub start",
			"project_id", project.ID)
		return
	}

	res, err := alignWorkspaceProjectIdentity(workspacePath, project.Slug, project.ID)
	if err != nil {
		logger.Warn("cloned project identity: failed to align workspace identity",
			"project_id", project.ID, "error", err.Error())
		return
	}
	if res.RetainedDir != "" {
		logger.Warn("cloned project identity: project config directory already holds files, previous directory kept",
			"project_id", project.ID, "config_dir", res.ConfigDir, "previous_dir", res.RetainedDir)
	}
	if res.Changed {
		logger.Info("cloned project identity: workspace identity set to hub project ID",
			"project_id", project.ID, "previous_id", res.PreviousID,
			"config_dir", res.ConfigDir, "relocated", res.Relocated)
	}
}
