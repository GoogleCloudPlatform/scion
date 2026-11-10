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

package artifacts

import (
	"context"
	"log/slog"
)

// The recorded delegating user (ptone/scion#6028). An agent's delegating
// user is a steward of the agent's artifacts (design D25) and stays one
// after the agent is deleted. The hub resolves the delegating user from
// the agent's live delegation chain, which is gone once the agent is
// deleted, so the user is recorded on the artifact when the agent creates
// it (artifact.sponsor_ref, Artifact.SponsorRef). The steward delegator
// clause and the review delegating-user clause read the recorded value
// first; the live chain is consulted only for an artifact with nothing
// recorded. The creation-time value stands: a later change of the agent's
// delegation, a new version or a move never updates it. Artifacts created
// before sponsors were recorded are filled once by BackfillSponsors, for
// agents that are still live.
//
// sponsor_ref has exactly two writers: createArtifact, with the value
// SponsorRecorder answered (sponsorFor), and SetAgentSponsor, with the
// value the backfill's resolver answered. Both come from the host's
// resolution of the agent's delegation chain, never from the request.

// SponsorRecorder is an optional extension of Host (ptone/scion#6028). The
// service asks it, when an agent creates an artifact in a project scope,
// for the agent's delegating user to record on the artifact. A host
// without it records nothing, so the steward and review clauses fall back
// to the agent's live delegation chain.
//
// It must not depend on the caller beyond the agent id: the backfill asks
// it outside any request.
type SponsorRecorder interface {
	// DelegatingUser returns the id of the user at the root of agent
	// agentID's recorded delegation chain, resolved now with the checks
	// the steward and review clauses apply to the live chain, or "" when
	// there is none (an unrecorded, revoked or ambiguous edge, a deleted
	// agent on the chain, a root user who is inactive or not admitted).
	// err is a lookup failure, which means nothing about the agent.
	DelegatingUser(ctx context.Context, agentID string) (string, error)
}

// sponsorFor returns the sponsor to record on an artifact that the caller
// (kind, ref) creates in scope kind scopeKind: the host's DelegatingUser
// for an agent creating in a project scope, and "" for anything else, for
// a host without SponsorRecorder, and when the lookup fails (logged; the
// artifact is then created with nothing recorded, and the live chain
// decides for it). Creating the artifact never fails because of it.
func (s *Service) sponsorFor(ctx context.Context, kind, ref, scopeKind string) string {
	if kind != PrincipalKindAgent || ref == "" || scopeKind != ScopeKindProject {
		return ""
	}
	rec, ok := s.host.(SponsorRecorder)
	if !ok {
		return ""
	}
	user, err := rec.DelegatingUser(ctx, ref)
	if err != nil {
		slog.DebugContext(ctx, "artifacts: delegating user not recorded", "agent_id", ref, "error", err)
		return ""
	}
	return user
}
