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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	"github.com/GoogleCloudPlatform/scion/pkg/ghauth"
	"github.com/GoogleCloudPlatform/scion/pkg/githubfetch"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ghCredRequest identifies the resource scope, GitHub repository, and caller
// for unified Hub GitHub credential resolution.
type ghCredRequest struct {
	Scope     string // store.TemplateScopeProject | store.TemplateScopeGlobal | store.TemplateScopeUser
	ScopeID   string
	Owner     string
	Repo      string
	Requester UserIdentity // optional; defaults to GetUserIdentityFromContext(ctx)
}

// githubCredential resolves the GitHub credential and principal scope for a
// Hub fetch following the unified precedence:
//
//   - project: GH_{OWNER}__{REPO} > GH_{OWNER} > GitHub App installation token > GITHUB_TOKEN > anon
//   - global (hub): hub secret GH_{OWNER}__{REPO} > GH_{OWNER} > GITHUB_TOKEN > anon
//   - user: user secret GH_{OWNER}__{REPO} > GH_{OWNER} > GITHUB_TOKEN > anon (only when requester is that user)
//
// When a project has a GitHub App installation whose token mint fails,
// resolution falls through to GITHUB_TOKEN (or anon) and records a non-secret
// Hint on the returned Credential so downstream 401/403/404 errors can report
// that the App token could not be minted.
func (s *Server) githubCredential(ctx context.Context, r ghCredRequest) githubfetch.Credential {
	if r.Owner == "" || r.Repo == "" {
		return githubfetch.Credential{Scope: githubfetch.AnonScope, Kind: githubfetch.CredAnonymous}
	}

	switch r.Scope {
	case store.TemplateScopeProject:
		if r.ScopeID == "" {
			return githubfetch.Credential{Scope: githubfetch.AnonScope, Kind: githubfetch.CredAnonymous}
		}
		return s.projectGitHubCredential(ctx, r.ScopeID, r.Owner, r.Repo)
	case store.TemplateScopeGlobal:
		return s.scopedGitHubCredential(ctx, secret.ScopeHub, s.HubID(), r.Owner, r.Repo)
	case store.TemplateScopeUser:
		user := r.Requester
		if user == nil {
			user = GetUserIdentityFromContext(ctx)
		}
		if r.ScopeID == "" || user == nil || user.ID() != r.ScopeID {
			return githubfetch.Credential{Scope: githubfetch.AnonScope, Kind: githubfetch.CredAnonymous}
		}
		return s.scopedGitHubCredential(ctx, secret.ScopeUser, r.ScopeID, r.Owner, r.Repo)
	}
	return githubfetch.Credential{Scope: githubfetch.AnonScope, Kind: githubfetch.CredAnonymous}
}

func (s *Server) projectGitHubCredential(ctx context.Context, projectID, owner, repo string) githubfetch.Credential {
	if val, key := s.ghConventionCredential(ctx, secret.ScopeProject, projectID, owner, repo); val != "" {
		s.resourceLog.Info("using project credential for resource import", "projectID", projectID, "secret", key)
		return githubfetch.Credential{
			Token: val,
			Scope: s.secretCredentialScope(val),
			Kind:  githubfetch.CredSecret,
		}
	}

	var hint string
	project, err := s.store.GetProject(ctx, projectID)
	if err == nil && project != nil && project.GitHubInstallationID != nil {
		token, _, mintErr := s.MintGitHubAppTokenForProject(ctx, project)
		if mintErr == nil && token != "" {
			return githubfetch.Credential{
				Token: token,
				Scope: "app:" + strconv.FormatInt(*project.GitHubInstallationID, 10),
				Kind:  githubfetch.CredAppInstallation,
			}
		}
		if mintErr != nil {
			s.resourceLog.Warn("GitHub App token mint failed; falling through to fallback credential",
				"projectID", projectID, "installationID", *project.GitHubInstallationID, "error", mintErr)
			hint = "GitHub App token could not be minted"
		}
	}

	if val := s.credentialForImport(ctx, secret.ScopeProject, projectID, ghauth.DefaultKey); val != "" {
		s.resourceLog.Info("using project credential for resource import", "projectID", projectID, "secret", ghauth.DefaultKey)
		if hint != "" {
			hint += "; fell back to GITHUB_TOKEN"
		}
		return githubfetch.Credential{
			Token: val,
			Scope: s.secretCredentialScope(val),
			Kind:  githubfetch.CredSecret,
			Hint:  hint,
		}
	}

	return githubfetch.Credential{
		Scope: githubfetch.AnonScope,
		Kind:  githubfetch.CredAnonymous,
		Hint:  hint,
	}
}

func (s *Server) scopedGitHubCredential(ctx context.Context, credScope, scopeID, owner, repo string) githubfetch.Credential {
	val, key := s.ghConventionCredential(ctx, credScope, scopeID, owner, repo)
	if val == "" {
		val, key = s.credentialForImport(ctx, credScope, scopeID, ghauth.DefaultKey), ghauth.DefaultKey
	}
	if val == "" {
		return githubfetch.Credential{Scope: githubfetch.AnonScope, Kind: githubfetch.CredAnonymous}
	}
	s.resourceLog.Info("using credential for resource import", "scope", credScope, "scopeID", scopeID, "secret", key)
	return githubfetch.Credential{
		Token: val,
		Scope: s.secretCredentialScope(val),
		Kind:  githubfetch.CredSecret,
	}
}

// secretCredentialScope returns "secret:<16-byte-hex>" keyed via HMAC-SHA256
// from the Hub's stable user token signing key (derived with domain label
// "githubfetch-scope"). When no signing key is initialized (e.g. a minimal
// unit-test Server), it falls back to a truncated SHA-256 digest of token.
func (s *Server) secretCredentialScope(token string) string {
	key := s.githubScopeHMACKey()
	if len(key) > 0 {
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write([]byte(token))
		sum := mac.Sum(nil)
		return "secret:" + hex.EncodeToString(sum[:16])
	}
	return "secret:" + githubfetch.TokenFingerprint(token)[:32]
}

func (s *Server) githubScopeHMACKey() []byte {
	if s.userTokenService != nil && len(s.userTokenService.config.SigningKey) > 0 {
		mac := hmac.New(sha256.New, s.userTokenService.config.SigningKey)
		_, _ = mac.Write([]byte("githubfetch-scope"))
		return mac.Sum(nil)
	}
	return nil
}
