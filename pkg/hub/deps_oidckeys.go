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
	"time"
)

// oidcDeps implements OIDCDeps as a live view over *Server: every getter
// reads the server field when it is called, never at construction, so
// tests and code that set the OIDC fields after New() keep working.
type oidcDeps struct {
	s *Server
}

var _ OIDCDeps = oidcDeps{}

func (d oidcDeps) KeyManager() *OIDCKeyManager { return d.s.oidcKeyManager }

func (d oidcDeps) IssuerURL() string { return d.s.oidcIssuerURL }

func (d oidcDeps) TokenLifetime() time.Duration { return d.s.oidcTokenLifetime }

func (d oidcDeps) TokenRateLimiter() OIDCRateLimiter {
	if d.s.oidcTokenRateLimiter == nil {
		// Never a typed nil: the handler tests the interface for nil.
		return nil
	}
	return d.s.oidcTokenRateLimiter
}

func (d oidcDeps) Agents() OIDCAgentReader { return d.s.store }

func (d oidcDeps) CallerAgent(ctx context.Context) (OIDCCallerAgent, bool) {
	c := GetAgentFromContext(ctx)
	if c == nil {
		return OIDCCallerAgent{}, false
	}
	return OIDCCallerAgent{
		Subject:                 c.Subject,
		ProjectID:               c.ProjectID,
		Ancestry:                c.Ancestry,
		CanRequestIdentityToken: c.HasScope(ScopeIdentityToken),
	}, true
}

// oidcArea returns the server's OIDC endpoint handlers. New sets
// s.oidcHandlers before registering routes; the fallback serves only a
// Server built without New (tests) and never writes the field.
func (s *Server) oidcArea() *OIDCHandlers {
	if h := s.oidcHandlers; h != nil {
		return h
	}
	return NewOIDCHandlers(oidcDeps{s})
}
