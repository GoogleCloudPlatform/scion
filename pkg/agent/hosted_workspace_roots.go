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
	"errors"
	"fmt"
	"log/slog"
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
// from broker-global or structural provider sources (never from project-level
// settings.yaml):
//   - the broker's hub-managed local project subtrees under ~/.scion
//     (projects, project-configs, workspace; runtime.ValidateWorkspaceSource
//     additionally enforces isScionHomeAllowedSubtree within ~/.scion),
//   - the shared workspace storage root known to the layout authority
//     (pkg/projectfs) loaded via config.LoadGlobalSettingsWithOverlayFor
//     (global-only, ignoring project-level server.* overrides),
//   - the structural project directory root for non-global projectDir (parent
//     of .scion, ignoring project-level workspace_path overrides), and
//   - the directory holding the .scion marker file when the broker-supplied
//     projectPath resolved to projectDir through a marker file (standard
//     externalized linked git project layout), so project-relative workspaces
//     resolved under that repository continue to pass containment on Start.
//
// It never returns an empty slice without an error: on a hosted broker,
// explicit workspaces are always subject to root containment.
func hostedExplicitWorkspaceRoots(ctx context.Context, projectPath, projectDir string) ([]string, error) {
	globalSettings, _, err := config.LoadGlobalSettingsWithOverlayFor(ctx)
	if err != nil {
		return nil, fmt.Errorf("load global settings for hosted workspace validation: %w", err)
	}
	return hostedExplicitWorkspaceRootsFromGlobal(globalSettings, projectPath, projectDir)
}

var resolveHostedGlobalDir = config.GetGlobalDir

func hostedExplicitWorkspaceRootsFromGlobal(globalSettings *config.VersionedSettings, projectPath, projectDir string) ([]string, error) {
	globalDir, err := resolveHostedGlobalDir()
	if err != nil {
		return nil, fmt.Errorf("resolve global scion dir for hosted workspace validation: %w", err)
	}
	if globalDir == "" {
		return nil, errors.New("resolve global scion dir for hosted workspace validation: empty global directory")
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
	if ps, _, err := config.ResolveProjectStorage(srv); err != nil {
		slog.Warn("hosted workspace validation: failed to resolve global project storage; shared storage root omitted", "error", err)
	} else if layout, layoutErr := config.ProjectStorageLayout(ps, config.DefaultVolumeMountBase); layoutErr != nil {
		slog.Warn("hosted workspace validation: failed to build global project storage layout; shared storage root omitted", "error", layoutErr)
	} else if layout.Backend.Shared() && layout.HostBase != "" {
		subPathRoot := layout.SubPathRoot
		if subPathRoot == "" {
			subPathRoot = projectfs.DefaultSubPathRoot
		}
		roots = append(roots, filepath.Join(layout.HostBase, subPathRoot))
	}

	if projectDir != "" && !config.IsGlobalProjectDir(projectDir) &&
		filepath.Base(projectDir) == config.DotScion {
		if fi, statErr := os.Stat(projectDir); statErr == nil && fi.IsDir() {
			if projectRoot := filepath.Dir(projectDir); isUsableHostedProjectRoot(projectRoot, globalDir) {
				roots = append(roots, projectRoot)
			}
		}
	}

	if markerDir := markerHoldingDir(projectPath, projectDir); isUsableHostedProjectRoot(markerDir, globalDir) {
		roots = append(roots, markerDir)
	}

	return roots, nil
}

// markerHoldingDir returns the directory containing the .scion marker file
// when projectPath resolved to projectDir through a marker file. It returns
// "" when projectPath is empty, is "global"/"home", does not point at a marker
// file, or the marker does not resolve to projectDir.
func markerHoldingDir(projectPath, projectDir string) string {
	if projectPath == "" || projectPath == "global" || projectPath == "home" || projectDir == "" {
		return ""
	}
	abs, err := filepath.Abs(projectPath)
	if err != nil {
		return ""
	}
	var markerPath, holderDir string
	if filepath.Base(abs) == config.DotScion {
		markerPath = abs
		holderDir = filepath.Dir(abs)
	} else {
		markerPath = filepath.Join(abs, config.DotScion)
		holderDir = abs
	}
	if !config.IsProjectMarkerFile(markerPath) {
		return ""
	}
	resolved, err := config.ResolveProjectMarker(markerPath)
	if err != nil || !samePath(resolved, projectDir) {
		return ""
	}
	return holderDir
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

// ValidateHostedAbsoluteWorkspace validates a caller-supplied absolute
// workspace against the hosted broker's containment roots for projectPath
// before any filesystem side effects (such as launch-marker creation).
// Empty or relative workspaces return nil.
func ValidateHostedAbsoluteWorkspace(ctx context.Context, projectPath, workspace string) error {
	if workspace == "" || !filepath.IsAbs(workspace) {
		return nil
	}
	var projectDir string
	if projectPath != "" {
		projectDir, _ = config.GetResolvedProjectDir(projectPath)
	}
	return validateHostedAbsoluteWorkspace(ctx, projectPath, projectDir, workspace)
}

// validateHostedAbsoluteWorkspace validates a caller-supplied absolute
// workspace against the hosted broker's containment roots before any
// filesystem side effects or existence probes.
func validateHostedAbsoluteWorkspace(ctx context.Context, projectPath, projectDir, workspace string) error {
	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		return fmt.Errorf("failed to resolve absolute path for workspace %s: %w", workspace, err)
	}
	roots, err := hostedExplicitWorkspaceRoots(ctx, projectPath, projectDir)
	if err != nil {
		return newWorkspaceNotPermittedError(err)
	}
	if _, err := runtime.ValidateWorkspaceSource(absWorkspace, roots...); err != nil {
		return newWorkspaceNotPermittedError(err)
	}
	return nil
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
