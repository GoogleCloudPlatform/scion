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

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Send-time recipient warning (ptone/scion#4575): the hub side of
// artifacts.RecipientAuthority.
//
// These methods answer for a NAMED principal, the direct recipient of a
// message, not for the caller of ctx. They are WARNING-ONLY: their answers
// reach the sender only as the count in artifactRefsRecipientWarning, and
// nothing here grants access, serves content or is consulted by any read
// route. The identity each builds (recipientIdentity) is passed only as the
// subject of a policy read (CheckAccess, the delegation chain walk and the
// agent scope check) and never enters a context, so it can never be taken
// for the caller: the artifact service learns its caller only from ctx
// (artifactHost.Principal), which still refuses every in-process identity.
// TestRecipientIdentityNeverEntersAContext pins that.

var _ artifacts.RecipientAuthority = (*artifactHost)(nil)

// RecipientPermits implements artifacts.RecipientAuthority: whether a
// credential the principal would hold next allows permission on artifacts
// homed in scopeRef. A user's session allows every artifact permission
// (Permits); an agent's next token must carry one of the permission's
// agent scopes (the scopes a mint would issue now: role and configured
// scopes, filtered by the delegation chain's ceiling) and its delegation
// chain must allow the permission, as Permits requires of a live token.
func (h *artifactHost) RecipientPermits(ctx context.Context, kind, ref, scopeRef, permission string) (bool, error) {
	perm, ok := artifactPermission(permission)
	if !ok || scopeRef == "" {
		return false, nil
	}
	identity, err := h.recipientIdentity(ctx, kind, ref)
	if err != nil || identity == nil {
		return false, err
	}
	agent, isAgent := identity.(*agentIdentityWrapper)
	if !isAgent {
		return true, nil
	}
	return agentHasAnyScope(agent, perm.AgentScopes) && h.agentChainAllows(ctx, agent, perm), nil
}

// RecipientAuthorize implements artifacts.RecipientAuthority: whether
// policy lets the principal exercise permission on artifacts in scopeRef,
// decided by AuthzService.CheckAccess with the principal as the subject,
// the same question Authorize asks for a caller (without the credential
// step, which RecipientPermits answers).
func (h *artifactHost) RecipientAuthorize(ctx context.Context, kind, ref, scopeRef, permission string) (bool, error) {
	perm, ok := artifactPermission(permission)
	if !ok || scopeRef == "" || h.server == nil || h.server.authzService == nil {
		return false, nil
	}
	identity, err := h.recipientIdentity(ctx, kind, ref)
	if err != nil || identity == nil {
		return false, err
	}
	decision := h.server.authzService.CheckAccess(ctx, identity, Resource{
		Type:       permissions.ResourceArtifact,
		ParentType: permissions.ResourceProject,
		ParentID:   scopeRef,
	}, Action(perm.Action))
	return decision.Allowed, nil
}

// recipientIdentity builds the subject identity for a recipient check, from
// the principal's stored record: for a user, an AuthenticatedUser (a session
// carries no credential limits; the explain endpoint resolves a user the
// same way); for an agent, an agentIdentityWrapper without a token id,
// carrying the scopes a mint would issue now (as scheduled authority
// builds one). It returns nil and no error, which every caller treats as
// "not readable", for an unknown kind, a missing, deleted or inactive
// principal, or an agent whose chain ceiling structurally refuses a mint;
// an error only for a failed lookup.
//
// The result must only ever be passed as the subject of a policy read and
// never be stored in a context (TestRecipientIdentityNeverEntersAContext).
func (h *artifactHost) recipientIdentity(ctx context.Context, kind, ref string) (Identity, error) {
	if h.server == nil || h.server.store == nil || h.server.authzService == nil || ref == "" {
		return nil, nil
	}
	switch kind {
	case artifacts.PrincipalKindUser:
		user, err := h.server.store.GetUser(ctx, ref)
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if user == nil || user.Status != store.UserStatusActive {
			return nil, nil
		}
		return NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "api"), nil
	case artifacts.PrincipalKindAgent:
		agent, err := h.server.store.GetAgent(ctx, ref)
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if agent == nil || !agent.DeletedAt.IsZero() || agent.ProjectID == "" {
			return nil, nil
		}
		authz := h.server.authzService
		scopes, err := authz.ceilingFilteredAgentScopes(ctx, agent, authz.mintCandidateScopes(agent))
		if err != nil {
			if _, structural := ceilingDenyCauseForError(err); structural {
				return nil, nil
			}
			return nil, err
		}
		ancestry := make([]string, len(agent.Ancestry))
		copy(ancestry, agent.Ancestry)
		return &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: agent.ID},
			ProjectID: agent.ProjectID,
			Scopes:    scopes,
			Ancestry:  ancestry,
		}}, nil
	default:
		return nil, nil
	}
}
