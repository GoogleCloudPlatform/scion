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

package oidckeys

import (
	"context"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// OIDCDeps is everything the OIDC identity provider endpoints need from the
// hub. Package hub implements it with a live view over *Server
// (deps_oidckeys.go): every getter reads the server at call time, so a
// field changed after New() is seen by the next request.
type OIDCDeps interface {
	// KeyManager is the active key manager. The routes are registered only
	// when it is non-nil; direct handler callers must ensure the same.
	KeyManager() *OIDCKeyManager
	// IssuerURL is the issuer the discovery document and tokens name.
	IssuerURL() string
	// TokenLifetime is the validity of an issued identity token.
	TokenLifetime() time.Duration
	// TokenRateLimiter limits identity token requests per agent. It
	// returns a nil interface (never a typed nil) when no limiter is set.
	TokenRateLimiter() OIDCRateLimiter
	// Agents reads agent records.
	Agents() OIDCAgentReader
	// CallerAgent returns the authenticated agent of the request, and
	// false when the request carries no agent token.
	CallerAgent(ctx context.Context) (OIDCCallerAgent, bool)
}

// OIDCRateLimiter is a per-key request limiter.
type OIDCRateLimiter interface {
	Allow(key string) bool
}

// OIDCAgentReader reads agent records.
type OIDCAgentReader interface {
	GetAgent(ctx context.Context, id string) (*store.Agent, error)
}

// OIDCCallerAgent is the OIDC endpoints' view of the authenticated agent
// token.
type OIDCCallerAgent struct {
	Subject   string
	ProjectID string
	// Ancestry is the token's slice, not a copy: [root_user, ...,
	// parent_agent].
	Ancestry []string
	// CanRequestIdentityToken reports whether the token carries the
	// identity-token scope, as the hub evaluates it.
	CanRequestIdentityToken bool
}

// OIDCHandlers serves the OIDC identity provider endpoints. It holds only
// its Deps and is built once per server.
type OIDCHandlers struct {
	d OIDCDeps
}

// NewOIDCHandlers returns the OIDC endpoint handlers over d.
func NewOIDCHandlers(d OIDCDeps) *OIDCHandlers {
	return &OIDCHandlers{d: d}
}
