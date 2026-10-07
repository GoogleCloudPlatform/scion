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
	"fmt"
	"log/slog"
)

// The token helpers below sign tokens without a run. They exist for tests
// only; production mint sites authorize, sign and record mandatorily
// (agent_token_mint.go).

// GenerateAgentToken generates a JWT for an agent with the specified scopes.
// Test-only: it records no credential.
func (s *AgentTokenService) GenerateAgentToken(agentID, projectID string, scopes []AgentTokenScope, ancestry []string) (string, error) {
	token, _, err := s.SignAgentToken(AgentTokenGrant{AgentID: agentID, ProjectID: projectID, Scopes: scopes, Ancestry: ancestry}, "")
	return token, err
}

// GenerateAgentToken generates a JWT for an agent.
// This is a convenience method that delegates to the token service.
// Base scopes are determined by the passed role.
// Dev-auth mode overrides to full if the role would be more restrictive,
// preserving dev-mode behavior where all agents get full access.
// Additional scopes are merged with the role-based defaults, deduplicated.
//
// Test-only: it applies no delegation ceiling, and it records the
// credential best-effort in the server's store, so it is defined only in a
// _test.go file. Every mint and
// refresh site uses AuthorizeAgentToken and a mandatory record.
func (s *Server) GenerateAgentToken(agentID, projectID string, ancestry []string, role AgentRole, additionalScopes []AgentTokenScope) (string, error) {
	s.mu.RLock()
	tokenService := s.agentTokenService
	s.mu.RUnlock()

	if tokenService == nil {
		return "", fmt.Errorf("agent token service not initialized")
	}

	// Use the specified role for base scopes.
	// Dev-auth mode overrides to full if the role would be more restrictive,
	// preserving dev-mode behavior where all agents get full access.
	effectiveRole := role
	if s.config.DevAuthToken != "" && CompareRoles(role, AgentRoleFull) < 0 {
		effectiveRole = AgentRoleFull
	}
	scopes := ScopesForRole(effectiveRole)

	// Merge additional scopes, deduplicating
	seen := make(map[AgentTokenScope]bool, len(scopes))
	for _, sc := range scopes {
		seen[sc] = true
	}
	for _, scope := range additionalScopes {
		if !seen[scope] {
			scopes = append(scopes, scope)
			seen[scope] = true
		}
	}

	token, cred, err := tokenService.SignAgentToken(AgentTokenGrant{AgentID: agentID, ProjectID: projectID, Scopes: scopes, Ancestry: ancestry}, "")
	if err != nil {
		return "", err
	}
	if s.store != nil {
		if err := s.store.CreateAgentCredential(context.Background(), cred); err != nil {
			slog.Warn("Failed to record agent credential", "agent_id", agentID, "error", err)
		}
	}
	return token, nil
}
