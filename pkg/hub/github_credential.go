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
	"fmt"
	"log/slog"
	"strconv"

	"github.com/GoogleCloudPlatform/scion/pkg/ghauth"
	"github.com/GoogleCloudPlatform/scion/pkg/githubfetch"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// SecretKeyGitHubScopeKey is the dedicated hub purpose key name used to
// HMAC-key GitHub credential scope identifiers ("secret:<hmac16(token)>") so
// DB/cache rows cannot be used to confirm a guessed PAT offline.
const SecretKeyGitHubScopeKey = "github_scope_hmac_key"

// initGitHubScopeKey loads or creates the dedicated GitHub credential scope
// HMAC key through the Hub's purpose-key mechanism (signing ring in keyring
// mode; ensureSigningKey in legacy mode). If the key cannot be loaded on a
// non-strict deployment, the server logs a warning and falls back to truncated
// SHA-256 scopes rather than failing startup.
func (s *Server) initGitHubScopeKey(ctx context.Context) error {
	if s.keyringManaged() {
		keys, err := s.managedCapabilityKeys(SecretKeyGitHubScopeKey)
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.githubScopeKeys = keys
		s.mu.Unlock()
		return nil
	}
	key, err := s.ensureSigningKey(ctx, SecretKeyGitHubScopeKey, nil)
	if err == nil && len(key) == 0 {
		err = fmt.Errorf("github scope HMAC key resolved to an empty value")
	}
	if err != nil {
		_, isGCPBackend := s.secretBackend.(*secret.GCPBackend)
		if isGCPBackend || s.config.RequireStableSigningKey {
			return fmt.Errorf("github scope HMAC key: %w", err)
		}
		slog.Warn("GitHub scope HMAC key could not be loaded or persisted; falling back to SHA-256 credential scopes",
			"error", err)
		return nil
	}
	s.mu.Lock()
	s.githubScopeKeys = fixedCapabilityKeys(key)
	s.mu.Unlock()
	return nil
}

func (s *Server) githubScopeCapabilityKeys() capabilityKeys {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.githubScopeKeys
}

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
// from the Hub's dedicated SecretKeyGitHubScopeKey purpose key. If the key is
// unavailable (e.g. minimal unit-test Server or failed key derivation), it logs
// without secret material and falls back to truncated SHA-256.
func (s *Server) secretCredentialScope(token string) string {
	keys := s.githubScopeCapabilityKeys()
	if keys.configured() {
		if k, _, err := keys.issue(); err == nil {
			if mat := k.Bytes(); len(mat) > 0 {
				mac := hmac.New(sha256.New, mat)
				_, _ = mac.Write([]byte(token))
				sum := mac.Sum(nil)
				return "secret:" + hex.EncodeToString(sum[:16])
			}
		} else {
			s.resourceLog.Warn("github scope HMAC key issuance unavailable; falling back to SHA-256 scope", "error", err)
		}
	}
	return "secret:" + githubfetch.TokenFingerprint(token)[:32]
}
