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

package githubfetch

import (
	"crypto/sha256"
	"encoding/hex"
)

// CredKind classifies where a Credential came from, for error messages and hints.
type CredKind int

const (
	CredAnonymous CredKind = iota
	CredAppInstallation
	CredSecret
	CredEnv
)

// Credential carries a GitHub token (empty for anonymous) and the cache and
// cooldown scope of the GitHub principal it represents.
//
// Scope is never derived from a Scion project ID; it identifies the GitHub
// principal:
//   - "anon" for unauthenticated requests
//   - "app:<installationID>" for GitHub App installation tokens
//   - "secret:<hmac16(token)>" for Hub-stored secrets
//   - "env:<sha256_16(token)>" for CLI / broker environment tokens
type Credential struct {
	Token string
	Scope string
	Kind  CredKind
	// Hint carries non-secret diagnostic context (for example, that a GitHub
	// App token mint failed and fell back to GITHUB_TOKEN) surfaced on auth
	// failures. Must never contain secret material.
	Hint string
}

// IsAnonymous reports whether c carries no token.
func (c Credential) IsAnonymous() bool {
	return c.Token == ""
}

// EffectiveScope returns c.Scope, falling back to AnonScope when empty.
func (c Credential) EffectiveScope() string {
	if c.Scope == "" {
		if c.Token != "" {
			return "env:" + TokenFingerprint(c.Token)[:32]
		}
		return AnonScope
	}
	return c.Scope
}

// TokenFingerprint returns the full lowercase hex SHA-256 digest of token.
func TokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
