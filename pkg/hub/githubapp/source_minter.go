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
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger/source"
)

// Compile-time assertion that InstallationTokenMinter satisfies source.IdentityMinter
// (app-shaped-source-seams.md §1–§2).
var _ source.IdentityMinter = (*InstallationTokenMinter)(nil)

// InstallationTokenMinter adapts *Client to the provider-free source.IdentityMinter
// interface so callers outside pkg/hub/githubapp can mint repository-scoped
// installation tokens through the source seam.
type InstallationTokenMinter struct {
	Client *Client
}

// Mint mints a repository-scoped GitHub App installation access token for t and scope.
// t.InstallationID must be a positive decimal installation ID and t.Resource must
// name the repository ("owner/repo" or bare "repo").
func (m *InstallationTokenMinter) Mint(ctx context.Context, t source.Target, scope source.CredentialScope) (source.Credential, error) {
	if m == nil || m.Client == nil {
		return source.Credential{}, errors.New("githubapp: installation token minter client is nil")
	}
	instID, err := strconv.ParseInt(strings.TrimSpace(t.InstallationID), 10, 64)
	if err != nil || instID <= 0 {
		return source.Credential{}, fmt.Errorf("githubapp: invalid installation id %q", t.InstallationID)
	}
	repo := strings.TrimSpace(t.Resource)
	if idx := strings.LastIndex(repo, "/"); idx >= 0 {
		repo = strings.TrimSpace(repo[idx+1:])
	}
	if repo == "" {
		return source.Credential{}, errors.New("githubapp: target resource repository is required")
	}

	perms, err := tokenPermissionsFromScope(scope)
	if err != nil {
		return source.Credential{}, err
	}

	tok, err := m.Client.MintInstallationToken(ctx, instID, []string{repo}, perms)
	if err != nil {
		return source.Credential{}, err
	}
	return source.Credential{
		Token:     tok.Token,
		ExpiresAt: tok.ExpiresAt,
		Scope:     tok.Permissions,
	}, nil
}

// TokenPermissionsFromScope converts a provider-neutral source.CredentialScope
// into GitHub's TokenPermissions, rejecting unknown permission names or levels.
func TokenPermissionsFromScope(scope source.CredentialScope) (TokenPermissions, error) {
	return tokenPermissionsFromScope(scope)
}

func tokenPermissionsFromScope(scope source.CredentialScope) (TokenPermissions, error) {
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
