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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// initLinkedProjectDir initializes the .scion directory of a provider's
// linked project path when a provider is registered with one. A variable so
// tests can register broker-shaped paths without writing outside their temp
// directories.
var initLinkedProjectDir = config.InitProject

// globalProjectSlug is the slug of a broker's global project: the CLI
// registers it under the name "global", and the combined hub and broker
// server creates it with this slug. Project slugs are unique on the hub.
const globalProjectSlug = "global"

// isGlobalHubProject reports whether a hub project with this slug is the
// global project. Only the slug counts, the same rule dispatch uses to mark
// the global project for the broker. Project create, clone, update and
// register with a git remote cannot take the slug (see
// isReservedProjectSlug); project names and labels can be set by clients and
// do not identify the global project.
func isGlobalHubProject(slug string) bool {
	return slug == globalProjectSlug
}

// isBrokerGlobalDirPath reports whether localPath has the shape of a broker's
// global scion directory, or of a project root whose .scion entry is that
// directory: a user home directory (/root, /home/<user> or /Users/<user>),
// or a ".scion" entry directly inside one.
//
// The hub cannot inspect the broker's filesystem, so this is a fast,
// shape-only check that fails a request before any write. The broker is the
// authority: it refuses a global-directory path for any other project at
// dispatch time, including homes this check does not recognize.
//
// config.GetGlobalDir places the global directory at $HOME/.scion, and a
// project rooted at a home directory resolves to that same directory, so
// neither shape names an ordinary project.
func isBrokerGlobalDirPath(localPath string) bool {
	if localPath == "" {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(localPath))
	if !strings.HasPrefix(clean, "/") {
		return false
	}
	home := clean
	if filepath.Base(clean) == config.DotScion {
		home = filepath.ToSlash(filepath.Dir(clean))
	}
	if home == "/root" {
		return true
	}
	parent := filepath.ToSlash(filepath.Dir(home))
	return (parent == "/home" || parent == "/Users") && filepath.Base(home) != config.DotScion
}

// validateProviderLocalPath rejects a provider local path that points at the
// broker's global directory for a project other than the global project.
// Dispatching such a project with that path makes the broker treat its
// global directory as the project.
func validateProviderLocalPath(projectName, projectSlug, localPath string) error {
	if !isBrokerGlobalDirPath(localPath) || isGlobalHubProject(projectSlug) {
		return nil
	}
	return fmt.Errorf("localPath %q is the broker's global scion directory and cannot be used for project %q; "+
		"register the provider without a path (the broker then uses its hub-managed project directory) "+
		"or pass the project's own directory", localPath, projectName)
}

// restrictedProviderPathPrefixes are host system directories a provider
// local path may not name or sit under.
var restrictedProviderPathPrefixes = []string{"/etc", "/usr", "/bin", "/sbin", "/sys", "/proc", "/dev", "/boot", "/lib"}

// checkProviderLocalPath validates a requested provider local path for the
// named project and returns it cleaned. The path must be absolute, must not
// name or sit under a restricted system directory, and must pass
// validateProviderLocalPath. Every broker's path is checked the same way;
// whether the directory exists is checked only for the embedded broker, by
// the caller. field names the request field in error messages.
func checkProviderLocalPath(field, projectName, projectSlug, localPath string) (string, error) {
	clean := filepath.Clean(localPath)
	if !filepath.IsAbs(clean) {
		return "", fmt.Errorf("%s must be an absolute path", field)
	}
	for _, prefix := range restrictedProviderPathPrefixes {
		if clean == prefix || strings.HasPrefix(clean, prefix+"/") {
			return "", fmt.Errorf("%s points to a restricted system directory", field)
		}
	}
	if err := validateProviderLocalPath(projectName, projectSlug, clean); err != nil {
		return "", err
	}
	return clean, nil
}

// registerProviderLocalPath returns the local path to store for a provider
// written by project register.
//
//   - A new project, or a broker that is not yet a provider, takes the
//     requested path. For a new project it is checked again against the
//     slug actually assigned; a global-directory path it may not hold is
//     dropped.
//   - A stored path that is the broker's global directory for a project
//     other than the global project is replaced by the requested path, or
//     cleared when the request has none, so re-running provide repairs it.
//   - Otherwise the stored path is kept: an existing provider with no path
//     keeps none, which avoids converting a hub-native project into a linked
//     one, and a linked path is not dropped by a register that omits it.
//
// requestedPath must already have passed checkProviderLocalPath.
func (s *Server) registerProviderLocalPath(ctx context.Context, project *store.Project, brokerID, requestedPath string, created bool) string {
	if created {
		// The request was checked against the slug register expected to
		// assign. A concurrent register can take that slug first, so check
		// again against the slug the new project actually got, and drop a
		// global-directory path it may not hold.
		if err := validateProviderLocalPath(project.Name, project.Slug, requestedPath); err != nil {
			s.projectsLogger().Warn("dropping provider local path for new project",
				"project_id", project.ID, "slug", project.Slug, "error", err.Error())
			return ""
		}
		return requestedPath
	}
	existing, err := s.store.GetProjectProvider(ctx, project.ID, brokerID)
	if err != nil {
		return requestedPath
	}
	if validateProviderLocalPath(project.Name, project.Slug, existing.LocalPath) != nil {
		return requestedPath
	}
	return existing.LocalPath
}
