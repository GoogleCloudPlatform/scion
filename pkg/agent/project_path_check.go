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

package agent

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// ProjectPathNotInitializedCode is the stable error code a Runtime Broker
// answers ErrProjectPathNotInitialized with.
const ProjectPathNotInitializedCode = api.BrokerErrCodeProjectPathNotInitialized

// ErrProjectPathNotInitialized refuses an agent create, start or restart of a
// plain (non-git) project whose explicit project path does not hold the
// project's .scion entry: a .scion directory directly under that path, or a
// .scion marker file that resolves. A plain project's agents mount the
// directory that holds its .scion directory as their workspace, so a path
// without one would resolve to some other directory. Match it with
// errors.Is; the error value is a *ProjectPathNotInitializedError.
var ErrProjectPathNotInitialized = errors.New("project path not initialized")

// ProjectPathNotInitializedError is ErrProjectPathNotInitialized for one
// configured path. Path is the project path exactly as configured, never a
// resolved symlink or marker target. It is an agent-state conflict
// (config.IsAgentStateConflict), so a caller that does not know it still
// answers 409.
type ProjectPathNotInitializedError struct {
	Path string
}

func (e *ProjectPathNotInitializedError) Error() string {
	return fmt.Sprintf("project path %s has no .scion directory or marker inside it; create %s as a directory (not a symlink to another location) and retry",
		e.Path, scionDirFor(e.Path))
}

// scionDirFor is the .scion directory of a configured project path, which
// may name the project root or its .scion directory.
func scionDirFor(projectPath string) string {
	if filepath.Base(projectPath) == config.DotScion {
		return projectPath
	}
	return filepath.Join(projectPath, config.DotScion)
}

// Is reports ErrProjectPathNotInitialized.
func (e *ProjectPathNotInitializedError) Is(target error) bool {
	return target == ErrProjectPathNotInitialized
}

// Unwrap makes the refusal an agent-state conflict.
func (e *ProjectPathNotInitializedError) Unwrap() error { return config.ErrAgentStateConflict }

// checkExplicitProjectPath refuses an explicit project path of a plain
// (non-git) project unless the workspace its agents would mount is that path
// itself. projectPath is the path as the caller passed it (for a Runtime
// Broker, the hub's projectPath: a provider's LocalPath or the broker's own
// project directory), and projectDir is config.ResolveProjectPath's result for
// it. Not checked, because they never take their workspace from projectDir's
// parent: no explicit path (the project is found from the working directory),
// the global project, a marker-resolved project-configs directory, a git
// workspace, an explicit agent workspace, and ownWorkspace (a git clone, an
// empty-per-agent directory or a shared workspace clone: the agent gets a
// workspace of its own).
//
// Otherwise projectDir must be a .scion directory whose parent, symlinks
// resolved, is the configured path, symlinks resolved. That refuses a path
// without a .scion entry (projectDir would be the path itself, and the
// workspace its parent), an unresolvable marker (the same), and a .scion
// symlink that leads out of the path (the workspace would be the link
// target's parent). A configured path that ends in .scion names the project
// directory itself, so its parent is the configured root.
func checkExplicitProjectPath(projectPath, projectDir, workspace string, ownWorkspace bool) error {
	if projectPath == "" || projectPath == "global" || projectPath == "home" || workspace != "" || ownWorkspace {
		return nil
	}
	if config.IsGlobalProjectDir(projectDir) || isProjectConfigsPath(projectDir) || isGitWorkspaceProject(projectDir) {
		return nil
	}
	refuse := &ProjectPathNotInitializedError{Path: projectPath}
	if filepath.Base(projectDir) != config.DotScion {
		return refuse
	}
	configuredRoot, err := filepath.Abs(projectPath)
	if err != nil {
		return refuse
	}
	if filepath.Base(configuredRoot) == config.DotScion {
		configuredRoot = filepath.Dir(configuredRoot)
	}
	want, err := filepath.EvalSymlinks(configuredRoot)
	if err != nil {
		return refuse
	}
	got, err := filepath.EvalSymlinks(filepath.Dir(projectDir))
	if err != nil || got != want {
		return refuse
	}
	return nil
}

// projectPathForError is the path a ProjectPathNotInitializedError names:
// the configured project path, or the resolved project directory when the
// project was found without one.
func projectPathForError(projectPath, projectDir string) string {
	if projectPath != "" {
		return projectPath
	}
	return projectDir
}
