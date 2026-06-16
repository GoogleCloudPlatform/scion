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
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// pathEqual compares two cleaned absolute paths for equality,
// case-insensitive on platforms with case-insensitive filesystems (macOS, Windows).
func pathEqual(a, b string) bool {
	return strings.EqualFold(a, b)
}

// pathHasPrefix checks whether path starts with prefix as a directory boundary,
// case-insensitive on platforms with case-insensitive filesystems (macOS, Windows).
func pathHasPrefix(path, prefix string) bool {
	return strings.HasPrefix(strings.ToLower(path), strings.ToLower(prefix))
}

// PathClass describes what kind of path was resolved.
type PathClass struct {
	Resolved      string `json:"resolved"`
	Exists        bool   `json:"exists"`
	IsDir         bool   `json:"isDir"`
	IsGit         bool   `json:"isGit"`
	IsManaged     bool   `json:"isManaged"`
	AlreadyLinked bool   `json:"alreadyLinked"`
}

// ClassifyPath resolves and classifies a candidate path.
// managedRoot is the hub-managed project directory (e.g. ~/.scion/projects/).
// It queries existing providers to detect already-linked paths.
func ClassifyPath(ctx context.Context, s store.Store, path, managedRoot string) (PathClass, error) {
	var pc PathClass

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		resolved = filepath.Clean(path)
		if !filepath.IsAbs(resolved) {
			return pc, err
		}
		pc.Resolved = resolved
		pc.Exists = false
		return pc, nil
	}

	resolved = filepath.Clean(resolved)
	if !filepath.IsAbs(resolved) {
		abs, err := filepath.Abs(resolved)
		if err != nil {
			return pc, err
		}
		resolved = abs
	}
	pc.Resolved = resolved

	info, err := os.Stat(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			pc.Exists = false
			return pc, nil
		}
		return pc, err
	}
	pc.Exists = true
	pc.IsDir = info.IsDir()

	if pc.IsDir {
		gitPath := filepath.Join(resolved, ".git")
		if _, err := os.Stat(gitPath); err == nil {
			pc.IsGit = true
		}
	}

	if managedRoot != "" {
		cleanManaged := filepath.Clean(managedRoot)
		if pathHasPrefix(resolved, cleanManaged+string(filepath.Separator)) || pathEqual(resolved, cleanManaged) {
			pc.IsManaged = true
		}
		// Also check legacy groves path
		legacyRoot := strings.Replace(cleanManaged, string(filepath.Separator)+"projects", string(filepath.Separator)+"groves", 1)
		if !pathEqual(legacyRoot, cleanManaged) {
			if pathHasPrefix(resolved, legacyRoot+string(filepath.Separator)) || pathEqual(resolved, legacyRoot) {
				pc.IsManaged = true
			}
		}
	}

	if s != nil && pc.IsDir {
		// Cap at 10000 projects; installations with more will not detect duplicates beyond this limit.
		result, err := s.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 10000})
		if err == nil && result != nil {
			for _, proj := range result.Items {
				providers, err := s.GetProjectProviders(ctx, proj.ID)
				if err != nil {
					continue
				}
				for _, p := range providers {
					if p.LocalPath == "" {
						continue
					}
					provResolved, err := filepath.EvalSymlinks(p.LocalPath)
					if err != nil {
						provResolved = filepath.Clean(p.LocalPath)
					}
					if provResolved == resolved {
						pc.AlreadyLinked = true
						break
					}
				}
				if pc.AlreadyLinked {
					break
				}
			}
		}
	}

	return pc, nil
}

// managedProjectRoot returns the hub-managed project directory (e.g. ~/.scion/projects/).
func managedProjectRoot() (string, error) {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(globalDir, "projects"), nil
}
