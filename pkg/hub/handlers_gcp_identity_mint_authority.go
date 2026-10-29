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
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// SurfaceAgentGCPTokenMint names the GCP token mint endpoints
// (handleAgentGCPToken, handleAgentGCPIdentityToken) in the actAs audit
// records authorizeAgentGCPMint emits. Both endpoints use the one label:
// they apply the same rule to the same account.
const SurfaceAgentGCPTokenMint = "agent-gcp-token-mint"

// SurfaceGCSLinkFetch names the GCS link proxy (GET /api/v1/gcs/object) in
// the actAs audit records authorizeGCSLinkServiceAccountUse emits.
const SurfaceGCSLinkFetch = "gcs-link-fetch"

// msgGCPMintNotAuthorized is the 403 body for a mint refused by
// authorizeAgentGCPMint. It is neutral: it does not say which layer refused
// or anything about the recorded source.
const msgGCPMintNotAuthorized = "GCP service account use is not authorized"

// errParentSourceUnresolved marks a recorded source that cannot be turned
// into an actAs caller.
var errParentSourceUnresolved = errors.New("recorded source cannot be resolved")

// authorizeAgentGCPMint is the authorization event for one GCP token mint,
// shared by both mint endpoints. It runs after the live record checks
// (resolveAgentGCPAssignment, the exact GCPTokenScopeForSA check and
// resolveAgentGCPMintFacts), which stay first, and returns true only when
// every layer allows:
//
//  1. Decide(gcp_service_account.use) on the freshly loaded service account
//     for the presenting agent. The agent's token names the exact account
//     (step 5b3 and the step 7b restriction). Step 10 denies when the
//     agent's own delegation chain has an unrecorded hop, and is otherwise
//     the account's parent ceiling: the source recorded on the agent's
//     assignment must currently hold gcp_service_account.assign on this
//     account, within the recorded effect ceiling (applyResourceParentCeiling
//     in authz_resource_parent_ceiling.go). Configured defaults, passthrough
//     translation and past assignments grant nothing by themselves.
//  2. With gcpIamCheckMode enforce, store.EvaluateActAs for that recorded
//     source on the account (parentSourceActAsAllows); anything but an
//     allow, Indeterminate included, denies. With mode off the Hub policy
//     layer of step 1 alone applies, as at assignment.
//
// Every lookup error denies. It writes no response; the caller writes 403.
func (s *Server) authorizeAgentGCPMint(ctx context.Context, agentRecord *store.Agent, gcpID *store.GCPIdentityConfig) bool {
	if s.authzService == nil || agentRecord == nil || gcpID == nil || gcpID.ServiceAccountID == "" {
		return false
	}
	ident, ok := GetIdentityFromContext(ctx).(AgentIdentity)
	if !ok || ident == nil || ident.ID() != agentRecord.ID {
		return false
	}
	sa, err := s.store.GetGCPServiceAccount(ctx, gcpID.ServiceAccountID)
	if err != nil || sa == nil {
		return false
	}

	d := s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(ident),
		Credential: credentialContextForIdentity(ident),
		Resource:   gcpServiceAccountResource(sa),
		Action:     ActionUse,
		Permission: permissions.PermissionGCPServiceAccountUse,
	})
	if !d.Allowed {
		slog.Info("GCP token mint refused: service account use not authorized",
			"agent_id", agentRecord.ID, "sa_id", sa.ID, "deny_cause", string(d.DenyCause), "reason", d.Reason)
		return false
	}

	return s.parentSourceActAsAllows(ctx, d, agentRecord.ID, sa, SurfaceAgentGCPTokenMint)
}

// authorizeGCSLinkServiceAccountUse is the same authorization event for the
// GCS link proxy, which mints a read-only token for the sending agent's
// account to fetch an object the agent named in a message. As the token
// handlers do before the mint (agentStandingForbidden), it first refuses a
// sender that is not in good standing (agentStanding: a held or deleted
// agent, a held or deleted agent on its chain, or a root user who is
// inactive or no longer admitted to the project); a standing lookup fault
// refuses too. The request carries the viewer's credential, not the
// agent's, so the use decision is then made for the stored agent record
// (AuthzService.decideStoredAgentServiceAccountUse, with the same step 10
// as the token mint), then the same actAs check on the recorded source
// runs in enforce mode. sa must already have passed resolveCurrentSA for
// sender. Every lookup error denies. The caller answers every refusal with
// the proxy's uniform not-found response, never the token handlers' 403.
func (s *Server) authorizeGCSLinkServiceAccountUse(ctx context.Context, sender *store.Agent, sa *store.GCPServiceAccount) bool {
	if s.authzService == nil || sender == nil || sa == nil {
		return false
	}
	if err := s.agentStanding(ctx, sender.ID); err != nil {
		slog.Info("GCS link fetch refused: sending agent not in good standing",
			"agent_id", sender.ID, "sa_id", sa.ID, "standing", standingReason(err))
		return false
	}
	d := s.authzService.decideStoredAgentServiceAccountUse(ctx, sender, sa)
	if !d.Allowed {
		slog.Info("GCS link fetch refused: service account use not authorized",
			"agent_id", sender.ID, "sa_id", sa.ID, "deny_cause", string(d.DenyCause), "reason", d.Reason)
		return false
	}
	return s.parentSourceActAsAllows(ctx, d, sender.ID, sa, SurfaceGCSLinkFetch)
}

// parentSourceActAsAllows is the actAs layer shared by authorizeAgentGCPMint
// and authorizeGCSLinkServiceAccountUse, run after an allowed use decision
// d. With gcpIamCheckMode enforce it runs store.EvaluateActAs for the
// source recorded on d's parent proof, on sa, under surface; anything but
// an allow, Indeterminate included, denies. Any other mode returns true:
// the Hub policy layer of d alone applies, as at assignment. The actAs
// check writes its own audit record, separate from the decision audit
// record of d.
func (s *Server) parentSourceActAsAllows(ctx context.Context, d Decision, agentID string, sa *store.GCPServiceAccount, surface string) bool {
	if !d.Allowed {
		return false
	}
	if s.saAssignCheckModeValue() != SAAssignCheckEnforce {
		return true
	}
	if d.parentProof == nil {
		// Step 10 always attaches the proof for this permission; a missing
		// one is a wiring fault.
		slog.Error("service account use refused: no parent proof on the use decision",
			"agent_id", agentID, "sa_id", sa.ID, "surface", surface)
		return false
	}
	caller, err := s.parentSourceActAsPrincipal(ctx, d.parentProof.SourcePrincipal)
	if err != nil {
		slog.Warn("service account use refused: recorded source cannot be checked for actAs",
			"agent_id", agentID, "sa_id", sa.ID, "surface", surface, "error", err)
		return false
	}
	result, err := store.EvaluateActAs(ctx, store.ActAsGate{
		Checker: s.saAssignCheckerFor(),
		Caller:  caller,
		Surface: surface,
		Audit:   s.GetAuditLogger(),
	}, sa)
	if err != nil {
		// No verdict; EvaluateActAs has forced Indeterminate, which denies.
		slog.Warn("service account use: caller-permission check failed",
			"agent_id", agentID, "sa_id", sa.ID, "surface", surface, "error", err)
	}
	if result.Outcome != store.ActAsAllowed {
		slog.Info("service account use refused: recorded source may not act as the service account",
			"agent_id", agentID, "sa_id", sa.ID, "surface", surface,
			"outcome", result.Outcome.String(), "mechanism", result.Mechanism)
		return false
	}
	return true
}

// parentSourceActAsPrincipal builds the actAs caller for a recorded
// assignment source, the way callerPrincipal builds it for a live caller: a
// user with its email, an agent with its own assign-mode account email (a
// passthrough or block agent has none). A lookup error or a deleted source
// returns an error, which the caller treats as a deny.
func (s *Server) parentSourceActAsPrincipal(ctx context.Context, src ProvenancePrincipal) (store.Principal, error) {
	if src.ID == "" {
		return store.Principal{}, errParentSourceUnresolved
	}
	switch src.Kind {
	case store.DelegationPrincipalUser:
		user, err := s.store.GetUser(ctx, src.ID)
		if err != nil {
			return store.Principal{}, fmt.Errorf("source user lookup: %w", err)
		}
		if user == nil {
			return store.Principal{}, errParentSourceUnresolved
		}
		return store.Principal{Kind: store.PrincipalUser, ID: user.ID, Email: user.Email}, nil
	case store.DelegationPrincipalAgent:
		agent, err := s.store.GetAgent(ctx, src.ID)
		if err != nil {
			return store.Principal{}, fmt.Errorf("source agent lookup: %w", err)
		}
		if agent == nil || !agent.DeletedAt.IsZero() {
			return store.Principal{}, errParentSourceUnresolved
		}
		p := store.Principal{Kind: store.PrincipalAgent, ID: agent.ID}
		if agentHasAssignModeIdentity(agent) {
			p.ServiceAccountEmail = agent.AppliedConfig.GCPIdentity.ServiceAccountEmail
		}
		return p, nil
	default:
		return store.Principal{}, errParentSourceUnresolved
	}
}
