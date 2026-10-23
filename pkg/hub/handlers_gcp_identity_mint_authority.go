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
//     (step 5b3 and the step 7b restriction), and step 10 is the account's
//     parent ceiling: the source recorded on the agent's assignment must
//     currently hold gcp_service_account.assign on this account, within the
//     recorded effect ceiling (EvaluateServiceAccountParentCeiling).
//     Configured defaults, passthrough translation and past assignments
//     grant nothing by themselves.
//  2. With gcpIamCheckMode enforce, store.EvaluateActAs for that recorded
//     source on the account; anything but an allow, Indeterminate included,
//     denies. With mode off the Hub policy layer of step 1 alone applies, as
//     at assignment.
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

	if s.saAssignCheckModeValue() != SAAssignCheckEnforce {
		return true
	}
	if d.parentProof == nil {
		// Step 10 always attaches the proof for this permission; a missing
		// one is a wiring fault.
		slog.Error("GCP token mint refused: no parent proof on the use decision",
			"agent_id", agentRecord.ID, "sa_id", sa.ID)
		return false
	}
	caller, err := s.parentSourceActAsPrincipal(ctx, d.parentProof.SourcePrincipal)
	if err != nil {
		slog.Warn("GCP token mint refused: recorded source cannot be checked for actAs",
			"agent_id", agentRecord.ID, "sa_id", sa.ID, "error", err)
		return false
	}
	result, err := store.EvaluateActAs(ctx, store.ActAsGate{
		Checker: s.saAssignCheckerFor(),
		Caller:  caller,
		Surface: SurfaceAgentGCPTokenMint,
		Audit:   s.GetAuditLogger(),
	}, sa)
	if err != nil {
		// No verdict; EvaluateActAs has forced Indeterminate, which denies.
		slog.Warn("GCP token mint: caller-permission check failed",
			"agent_id", agentRecord.ID, "sa_id", sa.ID, "error", err)
	}
	if result.Outcome != store.ActAsAllowed {
		slog.Info("GCP token mint refused: recorded source may not act as the service account",
			"agent_id", agentRecord.ID, "sa_id", sa.ID,
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
