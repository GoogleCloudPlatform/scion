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

package hubsync

import (
	"errors"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// ErrGitRepoNotProject is returned when 'scion hub link' or the hub
// auto-link would link the global project from inside a git repository that
// is not a scion project.
var ErrGitRepoNotProject = errors.New("this git repository is not a scion project yet; run 'scion init' here first (or pass --global to link the global project)")

// RefuseGlobalFallbackLink returns ErrGitRepoNotProject when a link would
// fall back to the global project from inside a git repository that has no
// .scion of its own. Without this, linking from a fresh clone under $HOME
// links (and records the hub project ID of) the global project instead of
// the repository.
//
// isGlobal is the project resolution result. explicitTarget reports that the
// user named the project (--global, or --project): an explicit global target
// is never refused.
func RefuseGlobalFallbackLink(isGlobal, explicitTarget bool) error {
	if !isGlobal || explicitTarget {
		return nil
	}
	root, err := util.RepoRoot()
	if err != nil || root == "" {
		return nil
	}
	// Resolution fell back to the global project, so any .scion at the
	// repository root is either the global project itself (the home
	// directory is the repository) or one resolution skipped as broken (a
	// dangling symlink, or a marker whose external directory is missing).
	// Only the first may link the global project.
	if config.IsGlobalProjectDir(filepath.Join(root, config.DotScion)) {
		return nil
	}
	return ErrGitRepoNotProject
}
