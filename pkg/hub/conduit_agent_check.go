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
	"errors"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Refusals of the conduit agent check (errConduitAgentGone, for an agent
// row that no longer exists, is shared with the relay's revalidation).
var (
	// errConduitAgentMissingScope: the token lacks agent:port:forward.
	errConduitAgentMissingScope = errors.New("conduit: agent token lacks agent:port:forward")
	// errConduitAgentMismatch: the agent row is deleted or belongs to
	// another project than the token's.
	errConduitAgentMismatch = errors.New("conduit: agent token does not match the agent")
)

// conduitAgentScopeCheck refuses an agent token without the
// agent:port:forward scope that conduit sessions require.
func conduitAgentScopeCheck(ident AgentIdentity) error {
	if !ident.HasScope(ScopeAgentPortForward) {
		return errConduitAgentMissingScope
	}
	return nil
}

// conduitAgentRowCheck reads the agent row of ident and refuses an agent
// that no longer exists (errConduitAgentGone), is deleted, or is not in
// the token's project (errConduitAgentMismatch). Any other error is the
// store read error, wrapped: the check could not be evaluated.
func (s *Server) conduitAgentRowCheck(ctx context.Context, ident AgentIdentity) (*store.Agent, error) {
	agent, err := s.store.GetAgent(ctx, ident.ID())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errConduitAgentGone
		}
		return nil, fmt.Errorf("conduit: reading the agent: %w", err)
	}
	if agent.ProjectID != ident.ProjectID() || !agent.DeletedAt.IsZero() {
		return nil, errConduitAgentMismatch
	}
	return agent, nil
}

// conduitAgentAdmissionCheck applies the conduit agent checks that follow
// token authentication, in admission order: the scope, then the agent
// row. It returns the agent row, a refusal (errConduitAgentMissingScope,
// errConduitAgentGone or errConduitAgentMismatch), or a wrapped store
// read error when the row could not be read.
func (s *Server) conduitAgentAdmissionCheck(ctx context.Context, ident AgentIdentity) (*store.Agent, error) {
	if err := conduitAgentScopeCheck(ident); err != nil {
		return nil, err
	}
	return s.conduitAgentRowCheck(ctx, ident)
}

// conduitAgentCheckRefused reports whether err from the conduit agent
// check is a refusal rather than a failure to evaluate it.
func conduitAgentCheckRefused(err error) bool {
	return errors.Is(err, errConduitAgentMissingScope) ||
		errors.Is(err, errConduitAgentGone) ||
		errors.Is(err, errConduitAgentMismatch)
}
