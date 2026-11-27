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

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Non-access-granting named-principal checks — the hub side of
// artifacts.RecipientAuthority. Allowed callers (pinned in
// pkg/hub/message_artifacts_recipient_test.go):
//   - RecipientMayNotRead (ptone/scion#4575): send-time warning when a message
//     recipient may not be able to read an attached artifact.
//   - readableOpenReviews (ptone/scion#7650): filters open review sessions to
//     reviewers who can still read the artifact.
//   - ReapLostAccessReviewDrafts (ptone/scion#8475): deletes saved review
//     drafts whose author no longer has review access (definitive deny only).
//   - classifyReviewer (ptone/scion#9296): classifies a requested reviewer as
//     unavailable, can-review, or needs-raise; never grants access itself (any
//     raised grant is written separately under the requester's own
//     adminAuthorityWith).
//   - ReapReviewRequests (ptone/scion#9296): expires open review requests on a
//     definitive deny (lookup errors leave the request open).
//   - handleListReviewRequests (ptone/scion#9296): computes advisory
//     reviewerCanReview on the publisher's review-requests list.
//
// These methods answer for a NAMED principal, not for the caller of ctx. They
// are NON-ACCESS-GRANTING: nothing here ever grants access, serves content, or
// is consulted by any read route; besides advisory warnings, status badges, and
// filtering, a recipient check may also withhold or remove the principal's own
// data on a definitive deny. The identity ResolveRecipient builds
// (recipientIdentity) is used only as the subject of policy reads (CheckAccess,
// the delegation chain walk and the agent scope check, all run under
// maskAllAuthzMemo) and never enters a context, so it can never be taken for
// the caller: the artifact service learns its caller only from ctx
// (artifactHost.Principal), which still refuses every in-process identity.
// TestRecipientIdentityNeverEntersAContext pins that.

var _ artifacts.RecipientAuthority = (*artifactHost)(nil)

// ResolveRecipient implements artifacts.RecipientAuthority: it builds the
// recipient's subject identity once (recipientIdentity) for every question
// one recipient check asks. A nil subject means the principal can read
// nothing (unknown kind; missing, deleted or inactive principal; an agent
// whose chain ceiling structurally refuses a mint).
func (h *artifactHost) ResolveRecipient(ctx context.Context, kind, ref string) (artifacts.RecipientSubject, error) {
	identity, err := h.recipientIdentity(ctx, kind, ref)
	if err != nil || identity == nil {
		return nil, err
	}
	return &recipientSubject{host: h, identity: identity}, nil
}

// recipientSubject is a resolved recipient: artifacts.RecipientSubject
// answered from its subject identity. It lives for one recipient check.
type recipientSubject struct {
	host     *artifactHost
	identity Identity
	// chain memoizes an agent's delegation chain walk (result and error)
	// per permission for the life of the check: the walk is in the agent's
	// own project, whatever scope is asked, so it is the same every time.
	chain map[string]chainWalk
}

// chainWalk is one memoized chain walk.
type chainWalk struct {
	ok  bool
	err error
}

// Permits implements artifacts.RecipientSubject: whether a credential the
// principal would hold next allows permission on artifacts homed in
// scopeRef. A user's session allows every artifact permission (Permits);
// an agent's next token must carry one of the permission's agent scopes
// (the scopes a mint would issue now: role and configured scopes, filtered
// by the delegation chain's ceiling) and its delegation chain must allow
// the permission, as Permits requires of a live token. A failed chain walk
// is returned as an error, not as a refusal.
func (r *recipientSubject) Permits(ctx context.Context, scopeRef, permission string) (bool, error) {
	perm, ok := artifactPermission(permission)
	if !ok || scopeRef == "" {
		return false, nil
	}
	agent, isAgent := r.identity.(*agentIdentityWrapper)
	if !isAgent {
		return true, nil
	}
	if !agentHasAnyScope(agent, perm.AgentScopes) {
		return false, nil
	}
	if w, done := r.chain[perm.ID]; done {
		return w.ok, w.err
	}
	ok, err := r.host.agentChainAllowsErr(maskAllAuthzMemo(ctx), agent, perm)
	if r.chain == nil {
		r.chain = map[string]chainWalk{}
	}
	r.chain[perm.ID] = chainWalk{ok: ok, err: err}
	return ok, err
}

// Authorize implements artifacts.RecipientSubject: whether policy lets the
// principal exercise permission on artifacts in scopeRef, decided by
// AuthzService.CheckAccess with the principal as the subject, the same
// question Authorize asks for a caller (without the credential step, which
// Permits answers), under maskAllAuthzMemo so nothing memoized for the
// caller's request is observed. An indeterminate decision (a store or
// resolution fault) is returned as an error so callers can distinguish a
// transient lookup failure from a policy deny and fail safe.
func (r *recipientSubject) Authorize(ctx context.Context, scopeRef, permission string) (bool, error) {
	perm, ok := artifactPermission(permission)
	h := r.host
	if !ok || scopeRef == "" || h.server == nil || h.server.authzService == nil {
		return false, nil
	}
	decision := h.server.authzService.CheckAccess(maskAllAuthzMemo(ctx), r.identity, Resource{
		Type:       permissions.ResourceArtifact,
		ParentType: permissions.ResourceProject,
		ParentID:   scopeRef,
	}, Action(perm.Action))
	if decision.IsIndeterminate() {
		return false, fmt.Errorf("artifacts: authorization check indeterminate: %s", decision.Reason)
	}
	return decision.Allowed, nil
}

// recipientIdentity builds the subject identity for a recipient check, from
// the principal's stored record: for a user, an AuthenticatedUser (a session
// carries no credential limits; the explain endpoint resolves a user the
// same way); for an agent, an agentIdentityWrapper without a token id,
// carrying the scopes a mint would issue now (as scheduled authority
// builds one). It returns nil and no error, which every caller treats as
// "not readable", for an unknown kind, a missing, deleted or inactive
// principal, an expired or disabled test fixture user, the local development
// user while local-development authority is off, or an agent whose chain
// ceiling structurally refuses a mint; an error only for a failed lookup.
//
// It is called once per recipient check (ResolveRecipient). The result
// must only ever be used as the subject of a policy read and never be
// stored in a context (TestRecipientIdentityNeverEntersAContext).
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
		if testFixtureRejection(user, h.server.testIdentities.enabled, h.server.testIdentities.clock()) != "" {
			return nil, nil
		}
		if user.ID == DevUserID && !h.server.authzService.devLocalAuthorityEnabled() {
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
		scopes, err := authz.ceilingFilteredAgentScopes(maskAllAuthzMemo(ctx), agent, authz.mintCandidateScopes(agent))
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
