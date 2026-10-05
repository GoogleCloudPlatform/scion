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

package util

import (
	"path/filepath"
	"strings"
)

// NormalizeCloneURL normalizes a project clone URL (a clone-url label or a git
// remote) to the form the Hub clones from. Explicit http(s)://, ssh://, git://
// and git@ URLs and local paths are preserved as-is; only schemeless remotes
// (e.g. "github.com/org/repo") are converted to an HTTPS clone URL via
// ToHTTPSCloneURL.
//
// This is the single source of truth shared by the Hub (which clones from the
// result) and the CLI (which reports it), so the two cannot drift.
func NormalizeCloneURL(cloneURL string) string {
	if cloneURL == "" {
		return ""
	}

	lower := strings.ToLower(cloneURL)
	for _, prefix := range []string{"http://", "https://", "ssh://", "git://"} {
		if strings.HasPrefix(lower, prefix) {
			return cloneURL
		}
	}
	if strings.HasPrefix(cloneURL, "git@") {
		return cloneURL
	}
	if filepath.IsAbs(cloneURL) || strings.HasPrefix(cloneURL, "./") || strings.HasPrefix(cloneURL, "../") {
		return cloneURL
	}

	return ToHTTPSCloneURL(cloneURL)
}

// ResolveCloneURL returns the URL the Hub clones a project from: the
// normalized clone-url label override when set, otherwise the normalized git
// remote.
func ResolveCloneURL(override, gitRemote string) string {
	if override = NormalizeCloneURL(override); override != "" {
		return override
	}
	return NormalizeCloneURL(gitRemote)
}
