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

// globalProjectSlug is the slug of a broker's global project: the CLI
// registers it under the name "global", and the combined hub and broker
// server creates it with this slug.
const globalProjectSlug = "global"

// globalProjectLabel marks the global project created by the combined hub and
// broker server.
const globalProjectLabel = "scion.io/global"

// isGlobalHubProject reports whether a hub project with this name, slug and
// labels is the global project.
func isGlobalHubProject(name, slug string, labels map[string]string) bool {
	if strings.EqualFold(slug, globalProjectSlug) || labels[globalProjectLabel] == "true" {
		return true
	}
	return name != "" && strings.EqualFold(api.Slugify(name), globalProjectSlug)
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
func validateProviderLocalPath(projectName, projectSlug string, labels map[string]string, localPath string) error {
	if !isBrokerGlobalDirPath(localPath) || isGlobalHubProject(projectName, projectSlug, labels) {
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
//   - A stored path that is the broker's global directory for a project
//     other than the global project is replaced by the requested path, or
//     cleared when the request has none, so re-running provide repairs it.
//   - Otherwise the stored path is kept: an existing provider with no path
//     keeps none, which avoids converting a hub-native project into a linked
//     one, and a linked path is not dropped by a register that omits it.
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
	if validateProviderLocalPath(project.Name, project.Slug, project.Labels, existing.LocalPath) != nil {
		return requestedPath
	}
	return existing.LocalPath
}
