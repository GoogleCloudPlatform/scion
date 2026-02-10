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
	"strings"

	"github.com/ptone/scion-agent/pkg/util"
)

const (
	DotScion = ".scion"
	GlobalDir = ".scion"
)

// FindProjectRoot walks up the directory tree to find the .scion directory.
func FindProjectRoot() (string, bool) {
	wd, err := os.Getwd()
	if err != nil {
		return "", false
	}

	dir := wd
	for {
		p := filepath.Join(dir, DotScion)
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			if abs, err := filepath.EvalSymlinks(p); err == nil {
				return abs, true
			}
			return p, true
		}

		parent := filepath.Dir(dir)
		if parent == dir { // Reached root
			break
		}
		dir = parent
	}
	return "", false
}

// GetResolvedProjectDir returns the active .scion directory based on precedence.
// This is a convenience wrapper around ResolveGrovePath that discards the isGlobal flag.
func GetResolvedProjectDir(explicitPath string) (string, error) {
	path, _, err := ResolveGrovePath(explicitPath)
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

// GetGroveName returns the slugified name of the grove.
func GetGroveName(projectDir string) string {
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return "unknown"
	}

	parent := filepath.Dir(abs)
	home, err := os.UserHomeDir()
	if err == nil && parent == home {
		return "global"
	}

	return slugify(filepath.Base(parent))
}

func slugify(s string) string {
	s = strings.ToLower(s)
	var res strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			res.WriteRune(r)
		} else {
			res.WriteRune('-')
		}
	}
	return strings.Trim(res.String(), "-")
}

// GetTargetProjectDir returns the directory where a grove should be initialized.
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

// ResolveGrovePath resolves a grove path to an absolute path and indicates if it's the global grove.
// If path is empty, it attempts to find the project grove or falls back to global.
// If path is "global" or "home", it returns the global grove path.
// Returns the absolute path, whether it's the global grove, and any error.
func ResolveGrovePath(path string) (string, bool, error) {
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

	isGlobal := abs == globalDir

	return abs, isGlobal, nil
}

// RequireGrovePath resolves a grove path, erroring if no project is found and global is not specified.
// This is used by commands that require an explicit grove context.
// If path is empty and no project grove is found, returns an error suggesting --global.
// Returns the absolute path, whether it's the global grove, and any error.
func RequireGrovePath(path string) (string, bool, error) {
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
