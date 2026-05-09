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
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// normalizeCloneURLLabel preserves explicit clone URL overrides while still
// normalizing schemeless git remotes stored in project labels.
func normalizeCloneURLLabel(cloneURL string) string {
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

	return util.ToHTTPSCloneURL(cloneURL)
}

func resolveCloneURL(override, gitRemote string) string {
	override = normalizeCloneURLLabel(override)
	if override != "" {
		return override
	}

	return normalizeCloneURLLabel(gitRemote)
}
