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

package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/GoogleCloudPlatform/scion/pkg/util/pathcanon"
)

// collectWorkspaceFiles collects the files of a local workspace for a hub
// workspace transfer: the non-git workspace bootstrap upload, `scion sync to`,
// and the local comparison set for `scion sync from` (so it matches what an
// upload would send). It applies transfer.DefaultExcludePatterns, which keep
// .git and the workspace-root .scion entry out of the transfer, plus extra.
//
// The root .scion entry is project metadata for the host it lives on and is
// never part of a workspace transfer. If a collected path is the root .scion
// entry or lies under it (for example because the defaults were changed),
// the collect fails instead of returning it.
//
// Every workspace collect in this package goes through here. A source guard
// test resolves the pkg/transfer and pkg/hubclient imports by import path in
// each file, so it fails if any other call site uses their CollectFiles or
// ManifestBuilder (under any import name), or if either package is dot- or
// blank-imported; only the listed template and harness-config uploads may use
// hubclient.CollectFiles.
func collectWorkspaceFiles(root string, extra []string) ([]transfer.FileInfo, error) {
	// transfer.CollectFiles seeds the defaults; extra only adds to them.
	files, err := transfer.CollectFiles(root, extra)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if f.Path == config.DotScion || strings.HasPrefix(f.Path, config.DotScion+"/") {
			return nil, fmt.Errorf("workspace collect included %q: the workspace-root %s entry must be excluded", f.Path, config.DotScion)
		}
	}
	return files, nil
}

// bootstrapWorkspaceDir returns the workspace root whose files the non-git
// workspace bootstrap uploads for the project resolved to resolvedPath, or ""
// when there is none. explicitPath is the project path given on the command
// line, if any.
//
// A candidate root is the directory holding the nearest .scion entry at or
// above a start directory: the explicit project path, then the working
// directory, then the workspace path recorded in the project's settings. It
// is accepted only when that .scion entry belongs to the resolved project: a
// marker file that resolves to resolvedPath, or the .scion directory
// resolvedPath names.
func bootstrapWorkspaceDir(resolvedPath, explicitPath string) string {
	var starts []string
	if explicitPath != "" && explicitPath != "global" && explicitPath != "home" {
		starts = append(starts, explicitPath)
	}
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}
	if s, err := config.LoadSettings(resolvedPath); err == nil && s != nil && s.WorkspacePath != "" {
		starts = append(starts, s.WorkspacePath)
	}
	for _, start := range starts {
		if root := workspaceRootFrom(start, resolvedPath); root != "" {
			return root
		}
	}
	return ""
}

// workspaceRootFrom returns the directory holding the nearest .scion entry at
// or above start when that entry belongs to the project resolved to
// resolvedPath, or "".
func workspaceRootFrom(start, resolvedPath string) string {
	entry, isDir := nearestDotScion(start)
	switch {
	case entry == "":
		return ""
	case isDir:
		if sameLocalPath(entry, resolvedPath) {
			return filepath.Dir(entry)
		}
	default:
		if resolved, err := config.ResolveProjectMarker(entry); err == nil && sameLocalPath(resolved, resolvedPath) {
			return filepath.Dir(entry)
		}
	}
	return ""
}

// nearestDotScion walks up from start to the first .scion entry and reports
// its path and whether it is a directory. It returns "" when there is none.
func nearestDotScion(start string) (path string, isDir bool) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", false
	}
	for {
		p := filepath.Join(dir, config.DotScion)
		if info, err := os.Stat(p); err == nil {
			return p, info.IsDir()
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// noWorkspaceRootWarning is printed when a non-git start to a remote broker
// has no workspace root to collect from.
const noWorkspaceRootWarning = "no workspace root found for this project; run from the project directory or pass --project"

// collectBootstrapWorkspace collects the files of the workspace root of the
// project resolved to resolvedPath for the non-git workspace bootstrap (see
// bootstrapWorkspaceDir). It returns the root and its files; dir is "" when
// there is no non-git workspace root to collect from. found reports whether a
// workspace root belonging to the project was found at all.
func collectBootstrapWorkspace(resolvedPath, explicitPath string) (dir string, files []transfer.FileInfo, found bool, err error) {
	dir = bootstrapWorkspaceDir(resolvedPath, explicitPath)
	if dir == "" {
		return "", nil, false, nil
	}
	if _, statErr := os.Stat(dir); statErr != nil || util.IsGitRepoDir(dir) {
		return "", nil, true, nil
	}
	files, err = collectWorkspaceFiles(dir, nil)
	if err != nil {
		return "", nil, true, err
	}
	return dir, files, true, nil
}

// sameLocalPath reports whether a and b name the same location, compared in
// pathcanon's canonical form (links resolved; case-insensitive on Windows).
func sameLocalPath(a, b string) bool {
	ab, err := pathcanon.Within(a, b)
	if err != nil || !ab {
		return false
	}
	ba, err := pathcanon.Within(b, a)
	return err == nil && ba
}
