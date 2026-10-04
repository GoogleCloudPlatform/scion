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
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// globalProjectSlug is the slug the CLI registers a broker's global project
// under (the global project is registered with the name "global").
const globalProjectSlug = "global"

// isGlobalProjectName reports whether a hub project with this name or slug
// is the global project.
func isGlobalProjectName(name, slug string) bool {
	if strings.EqualFold(slug, globalProjectSlug) {
		return true
	}
	return name != "" && strings.EqualFold(api.Slugify(name), globalProjectSlug)
}

// isBrokerGlobalDirPath reports whether localPath has the shape of a broker's
// global scion directory: a ".scion" entry directly inside a user home
// directory (/root, /home/<user> or /Users/<user>).
//
// The hub cannot inspect the broker's filesystem, so it relies on this shape.
// config.GetGlobalDir places the global directory at $HOME/.scion, and a
// project rooted at a home directory resolves to that same directory, so a
// path of this shape never names an ordinary project. A broker whose home
// lives elsewhere is not recognized here; the broker refuses such a path
// itself at dispatch time.
func isBrokerGlobalDirPath(localPath string) bool {
	if localPath == "" {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(localPath))
	if !strings.HasPrefix(clean, "/") || filepath.Base(clean) != config.DotScion {
		return false
	}
	home := filepath.ToSlash(filepath.Dir(clean))
	if home == "/root" {
		return true
	}
	parent := filepath.ToSlash(filepath.Dir(home))
	return (parent == "/home" || parent == "/Users") && filepath.Base(home) != ""
}

// validateProviderLocalPath rejects a provider local path that points at the
// broker's global directory for a project other than the global project.
// Dispatching such a project with that path makes the broker treat its
// global directory as the project.
func validateProviderLocalPath(projectName, projectSlug, localPath string) error {
	if !isBrokerGlobalDirPath(localPath) || isGlobalProjectName(projectName, projectSlug) {
		return nil
	}
	return fmt.Errorf("localPath %q is the broker's global scion directory and cannot be used for project %q; "+
		"register the provider without a path (the broker then uses its hub-managed project directory) "+
		"or pass the project's own directory", localPath, projectName)
}

// registerProviderLocalPath returns the local path to store for a provider
// written by project register.
//
//   - A new project, or a broker that is not yet a provider, takes the
//     requested path.
//   - A request with no path clears any stored path, so re-running provide
//     without a path repairs a provider registered with a wrong one.
//   - Otherwise an existing provider with no path keeps none, which avoids
//     converting a hub-native project into a linked one, and an existing
//     path is kept unless it is the broker's global directory for a project
//     other than the global project.
//
// requestedPath must already have passed validateProviderLocalPath.
func (s *Server) registerProviderLocalPath(ctx context.Context, project *store.Project, brokerID, requestedPath string, created bool) string {
	if created {
		return requestedPath
	}
	existing, err := s.store.GetProjectProvider(ctx, project.ID, brokerID)
	if err != nil {
		return requestedPath
	}
	if requestedPath == "" {
		return ""
	}
	if existing.LocalPath != "" && validateProviderLocalPath(project.Name, project.Slug, existing.LocalPath) != nil {
		return requestedPath
	}
	return existing.LocalPath
}
