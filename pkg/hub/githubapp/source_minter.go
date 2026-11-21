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

package githubapp

import (
	"errors"
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger/source"
)

// TokenPermissionsFromScope converts a provider-neutral source.CredentialScope
// into GitHub's TokenPermissions, rejecting unknown permission names or levels.
func TokenPermissionsFromScope(scope source.CredentialScope) (TokenPermissions, error) {
	var perms TokenPermissions
	for k, rawVal := range scope.Permissions {
		val := strings.ToLower(strings.TrimSpace(rawVal))
		if val == "" {
			continue
		}
		if val != "read" && val != "write" {
			return TokenPermissions{}, fmt.Errorf("githubapp: invalid permission level %q for %q", rawVal, k)
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "contents":
			perms.Contents = val
		case "pull_requests":
			perms.PullRequests = val
		case "issues":
			perms.Issues = val
		case "metadata":
			perms.Metadata = val
		case "checks":
			perms.Checks = val
		case "actions":
			perms.Actions = val
		default:
			return TokenPermissions{}, fmt.Errorf("githubapp: unknown permission %q", k)
		}
	}
	if perms == (TokenPermissions{}) {
		return TokenPermissions{}, errors.New("githubapp: at least one permission is required")
	}
	return perms, nil
}

// TokenPermissionsToMap converts non-empty fields of perms into a map suitable
// for source.Credential.Scope.
func TokenPermissionsToMap(perms TokenPermissions) map[string]string {
	out := make(map[string]string)
	if perms.Contents != "" {
		out["contents"] = perms.Contents
	}
	if perms.PullRequests != "" {
		out["pull_requests"] = perms.PullRequests
	}
	if perms.Issues != "" {
		out["issues"] = perms.Issues
	}
	if perms.Metadata != "" {
		out["metadata"] = perms.Metadata
	}
	if perms.Checks != "" {
		out["checks"] = perms.Checks
	}
	if perms.Actions != "" {
		out["actions"] = perms.Actions
	}
	return out
}
