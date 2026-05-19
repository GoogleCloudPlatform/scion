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

package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

const (
	DotScion  = ".scion"
	GlobalDir = ".scion"

	ProjectConfigsDir = "project-configs"
	ProjectsDir       = "projects"
	GroveConfigsDir   = "grove-configs"
	GrovesDir         = "groves"
)

// FindProjectRoot walks up the directory tree to find the .scion directory or marker file.
// When .scion is a file (project marker), it resolves to the external project-config path.
// In hub context (SCION_HUB_ENDPOINT set), if no .scion is found on the filesystem,
// returns a synthetic path based on CWD so that settings loading can proceed using
// environment variables for hub connectivity.
func FindProjectRoot() (string, bool) {
	wd, err := os.Getwd()
	if err != nil {
		return "", false
	}

	dir := wd
	for {
		p := filepath.Join(dir, DotScion)
		info, err := os.Stat(p)
		if err == nil {
			if info.IsDir() {
				if abs, err := filepath.EvalSymlinks(p); err == nil {
					return abs, true
				}
				return p, true
			}
			// .scion is a file (project marker) — resolve to external path
			if resolved, err := ResolveProjectMarker(p); err == nil {
				// Verify the resolved external path actually exists on this
				// filesystem. Inside a container the marker may reference a
				// host-side project-config directory that doesn't exist locally.
				if _, statErr := os.Stat(resolved); statErr == nil {
					return resolved, true
				}
			}
			// Marker file exists but external path can't be resolved
			// (e.g., inside a container where ~/.scion/project-configs/ doesn't exist).
			// In hub context, return a synthetic path — the CLI will use the
			// Hub API and env vars rather than filesystem-based project data.
			if IsHubContext() {
				return filepath.Join(filepath.Dir(p), DotScion), true
			}
		}

		parent := filepath.Dir(dir)
		if parent == dir { // Reached root
			break
		}
		dir = parent
	}

	// Hub context fallback: if hub endpoint is available via env vars,
	// the CLI is running inside a hub-connected container. Return a
	// synthetic .scion path so that settings loading proceeds using
	// env vars (SCION_HUB_ENDPOINT, SCION_PROJECT_ID, etc.) for hub connectivity.
	if IsHubContext() {
		return filepath.Join(wd, DotScion), true
	}

	return "", false
}

// GetResolvedProjectDir returns the active .scion directory based on precedence.
// This is a convenience wrapper around ResolveProjectPath that discards the isGlobal flag.
func GetResolvedProjectDir(explicitPath string) (string, error) {
	path, _, err := ResolveProjectPath(explicitPath)
	return path, err
}

func GetProjectDir() (string, error) {
	// 1. Walk up to find .scion
	if p, ok := FindProjectRoot(); ok {
		return p, nil
	}

	// 2. Fallback to current directory (legacy/non-repo behavior)
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(wd, DotScion), nil
}

// GetProjectName returns the slugified name of the project.
func GetProjectName(projectDir string) string {
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return "unknown"
	}

	parent := filepath.Dir(abs)
	home, err := os.UserHomeDir()
	if err == nil && parent == home {
		return "global"
	}

	baseName := filepath.Base(parent)
	// Check for external project-config directory pattern (slug__shortuuid)
	if slug := ExtractSlugFromExternalDir(baseName); slug != "" {
		return slug
	}

	return api.Slugify(baseName)
}

// GetTargetProjectDir returns the directory where a project should be initialized.
func GetTargetProjectDir() (string, error) {
	// 1. Root of the current git repo if run inside a repo
	if util.IsGitRepo() {
		root, err := util.RepoRoot()
		if err == nil {
			return filepath.Join(root, DotScion), nil
		}
	}

	// 2. Current directory
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(wd, DotScion), nil
}

func GetGlobalDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, GlobalDir), nil
}

// GetProjectConfigDir returns the directory where project config files (settings.yaml,
// templates/) live. For git projects with split storage (project-id file exists), this
// is the external path under ~/.scion/project-configs/. For all other projects
// (non-git, global), projectDir is returned as-is since it is already the config dir.
func GetProjectConfigDir(projectDir string) string {
	if extDir, err := GetGitProjectExternalConfigDir(projectDir); err == nil && extDir != "" {
		return extDir
	}
	return projectDir
}

func GetProjectTemplatesDir() (string, error) {
	p, err := GetProjectDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(p, "templates"), nil
}

func GetGlobalTemplatesDir() (string, error) {
	g, err := GetGlobalDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(g, "templates"), nil
}

func GetProjectAgentsDir() (string, error) {
	p, err := GetProjectDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(p, "agents"), nil
}

func GetProjectKubernetesConfigPath() (string, error) {
	p, err := GetProjectDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(p, "kubernetes-config.json"), nil
}

func GetGlobalAgentsDir() (string, error) {
	g, err := GetGlobalDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(g, "agents"), nil
}

// ResolveProjectPath resolves a grove path to an absolute path and indicates if it's the global grove.
// If path is empty, it attempts to find the project grove or falls back to global.
// If path is "global" or "home", it returns the global grove path.
// Returns the absolute path, whether it's the global grove, and any error.
func ResolveProjectPath(path string) (string, bool, error) {
	if path == "" {
		// Try to find project grove first
		if p, ok := FindProjectRoot(); ok {
			// Check if the found project root is actually the global directory
			globalDir, _ := GetGlobalDir()
			if p == globalDir {
				return p, true, nil
			}
			return p, false, nil
		}
		// Fallback to global
		g, err := GetGlobalDir()
		return g, true, err
	}

	if path == "global" || path == "home" {
		g, err := GetGlobalDir()
		return g, true, err
	}

	// Check if path is the global dir
	globalDir, _ := GetGlobalDir()

	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false, err
	}

	// If the path doesn't end with .scion, check if it contains a .scion entry.
	// This allows users to pass a project root (e.g. /path/to/project) and have it
	// resolve to /path/to/project/.scion, matching how FindProjectRoot discovers groves.
	if filepath.Base(abs) != DotScion {
		candidate := filepath.Join(abs, DotScion)
		if info, err := os.Stat(candidate); err == nil {
			if info.IsDir() {
				if evaluated, err := filepath.EvalSymlinks(candidate); err == nil {
					abs = evaluated
				} else {
					abs = candidate
				}
			} else {
				// .scion is a marker file — resolve to external path
				if resolved, err := ResolveProjectMarker(candidate); err == nil {
					abs = resolved
				}
			}
		}
	} else {
		// Path ends in .scion — check if it's a marker file (not a directory)
		if info, err := os.Stat(abs); err == nil && !info.IsDir() {
			if resolved, err := ResolveProjectMarker(abs); err == nil {
				abs = resolved
			}
		} else if err == nil && info.IsDir() {
			if evaluated, err := filepath.EvalSymlinks(abs); err == nil {
				abs = evaluated
			}
		}
	}

	isGlobal := abs == globalDir

	return abs, isGlobal, nil
}

// RequireProjectPath resolves a grove path, erroring if no project is found and global is not specified.
// This is used by commands that require an explicit grove context.
// If path is empty and no project grove is found, returns an error suggesting --global.
// Returns the absolute path, whether it's the global grove, and any error.
func RequireProjectPath(path string) (string, bool, error) {
	// Explicit global request
	if path == "global" || path == "home" {
		g, err := GetGlobalDir()
		return g, true, err
	}

	// Explicit path specified
	if path != "" {
		globalDir, _ := GetGlobalDir()
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", false, err
		}
		// If the path doesn't end with .scion, check if it contains a .scion entry.
		if filepath.Base(abs) != DotScion {
			candidate := filepath.Join(abs, DotScion)
			if info, err := os.Stat(candidate); err == nil {
				if info.IsDir() {
					if evaluated, err := filepath.EvalSymlinks(candidate); err == nil {
						abs = evaluated
					} else {
						abs = candidate
					}
				} else {
					// .scion is a marker file — resolve to external path
					if resolved, err := ResolveProjectMarker(candidate); err == nil {
						abs = resolved
					}
				}
			}
		} else {
			// Path ends in .scion — check if it's a marker file
			if info, err := os.Stat(abs); err == nil && !info.IsDir() {
				if resolved, err := ResolveProjectMarker(abs); err == nil {
					abs = resolved
				}
			} else if err == nil && info.IsDir() {
				if evaluated, err := filepath.EvalSymlinks(abs); err == nil {
					abs = evaluated
				}
			}
		}
		isGlobal := abs == globalDir
		return abs, isGlobal, nil
	}

	// No path specified - require project grove to exist
	if p, ok := FindProjectRoot(); ok {
		return p, false, nil
	}

	// No project found and no explicit path - error
	return "", false, fmt.Errorf("not in a scion project. Use --global for global grove or run 'scion init' to create a project grove")
}
