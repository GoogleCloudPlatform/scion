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
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// hostedExplicitWorkspaceRoots returns the containment roots permitted for an
// explicit workspace on a hosted Runtime Broker. Every root comes exclusively
// from broker-global sources (never from project-level settings.yaml):
//   - the broker's hub-managed local project subtrees under ~/.scion
//     (projects, project-configs, workspace; runtime.ValidateWorkspaceSource
//     additionally enforces isScionHomeAllowedSubtree within ~/.scion),
//   - the shared workspace storage root known to the layout authority
//     (pkg/projectfs) loaded via config.LoadGlobalSettingsWithOverlayFor
//     (global-only, ignoring project-level server.* overrides), and
//   - the structural project directory root for non-global projectDir (parent
//     of .scion, ignoring project-level workspace_path overrides) so a
//     project-relative workspace resolved via resolveWorkspaceSubdir continues
//     to pass containment on Start.
//
// It never returns an empty slice without an error: on a hosted broker,
// explicit workspaces are always subject to root containment.
func hostedExplicitWorkspaceRoots(ctx context.Context, projectDir string) ([]string, error) {
	globalSettings, _, err := config.LoadGlobalSettingsWithOverlayFor(ctx)
	if err != nil {
		return nil, fmt.Errorf("load global settings for hosted workspace validation: %w", err)
	}
	return hostedExplicitWorkspaceRootsFromGlobal(globalSettings, projectDir)
}

func hostedExplicitWorkspaceRootsFromGlobal(globalSettings *config.VersionedSettings, projectDir string) ([]string, error) {
	globalDir, err := config.GetGlobalDir()
	if err != nil || globalDir == "" {
		return nil, fmt.Errorf("resolve global scion dir for hosted workspace validation: %w", err)
	}

	roots := []string{
		filepath.Join(globalDir, projectkeys.ProjectsDir),
		filepath.Join(globalDir, projectkeys.ProjectConfigsDir),
		filepath.Join(globalDir, "workspace"),
	}

	var srv *config.V1ServerConfig
	if globalSettings != nil {
		srv = globalSettings.Server
	}
	if ps, _, err := config.ResolveProjectStorage(srv); err == nil {
		if layout, layoutErr := config.ProjectStorageLayout(ps, config.DefaultVolumeMountBase); layoutErr == nil {
			if layout.Backend.Shared() && layout.HostBase != "" {
				subPathRoot := layout.SubPathRoot
				if subPathRoot == "" {
					subPathRoot = projectfs.DefaultSubPathRoot
				}
				roots = append(roots, filepath.Join(layout.HostBase, subPathRoot))
			}
		}
	}

	if projectDir != "" && !config.IsGlobalProjectDir(projectDir) {
		projectRoot := structuralProjectRoot(projectDir)
		if isUsableHostedProjectRoot(projectRoot, globalDir) {
			roots = append(roots, projectRoot)
		}
	}

	return roots, nil
}

// structuralProjectRoot returns the directory root implied by projectDir's
// own path (parent of .scion, or projectDir itself) without reading
// project-level settings.WorkspacePath.
func structuralProjectRoot(projectDir string) string {
	if filepath.Base(projectDir) == config.DotScion {
		return filepath.Dir(projectDir)
	}
	return projectDir
}

// isUsableHostedProjectRoot reports whether candidate can be added to the
// hosted explicit-workspace root list without tripping ValidateWorkspaceSource's
// root floors (filesystem root, $HOME, or ~/.scion itself).
func isUsableHostedProjectRoot(candidate, globalDir string) bool {
	if candidate == "" || !filepath.IsAbs(candidate) {
		return false
	}
	cleaned := filepath.Clean(candidate)
	if filepath.Dir(cleaned) == cleaned {
		return false
	}
	if samePath(cleaned, globalDir) {
		return false
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && samePath(cleaned, home) {
		return false
	}
	return true
}

// newWorkspaceNotPermittedError wraps a hosted explicit-workspace refusal as a
// typed LaunchFailure so async launches report workspace_not_permitted with
// fixed public text and never leak broker host paths.
func newWorkspaceNotPermittedError(err error) error {
	return &runtime.LaunchFailure{
		Code: api.LaunchCodeWorkspaceNotPermitted,
		Step: api.LaunchStepLaunching,
		Err:  err,
	}
}
