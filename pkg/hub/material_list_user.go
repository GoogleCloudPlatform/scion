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
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// agentListUserSecrets is the user part of the agent secret list
// (agentListSecrets). It lists the root user's user-scope secrets that the
// by-key read (check 8, authorizeRuntimeUserItem) would return, appending to
// secrets and items. ok is false on a request-level failure, which the caller
// turns into the whole-request exit; nothing from the user part is listed
// then.
//
// The agent's delegation chain selects the rule, as for check 8:
//   - a chain with recorded provenance: the chain verdict is computed once
//     per request; if it denies, the user part is empty and one
//     request-level denied_by_policy item is recorded. Otherwise every
//     candidate that passes the progeny filters must also pass the source
//     check (S4b, S5-S8 of EvaluateUserSecretParentCeiling) and the agent's
//     own access constraints for secret.use on that secret (Decide step 7c),
//     evaluated per item against inputs loaded once per request;
//   - a chain without recorded provenance: the user part is empty and one
//     request-level chain_unrecorded item is recorded, as check 8 refuses
//     every user-scope read for that chain (one rate-limited Warn per
//     agent, shared with the read).
func (s *Server) agentListUserSecrets(
	ctx context.Context,
	agentID string,
	ident AgentIdentity,
	facts *TargetFacts,
	secrets []AgentSecretMeta,
	items []MaterialSelectionEventItem,
) ([]AgentSecretMeta, []MaterialSelectionEventItem, bool) {
	if s.authzService == nil {
		return secrets, items, false
	}
	chain := s.authzService.userSecretChainVerdict(ctx, ident, nil)
	switch {
	case chain.decision.DenyCause == DenyCauseCeilingError:
		return secrets, items, false
	case !chain.decision.Allowed && chain.unrecorded:
		s.warnUnrecordedChainRefusal(ctx, "list", agentID, chain)
		items = append(items, materialSelectionItem(ItemResult{
			Candidate: Candidate{Kind: MaterialKindSecret, Scope: store.ScopeUser, ScopeID: facts.Root.ID, Grant: GrantProgeny},
			Reason:    ReasonChainUnrecorded,
		}, "", chain.decision.Reason))
		return secrets, items, true
	case !chain.decision.Allowed:
		items = append(items, materialSelectionItem(ItemResult{
			Candidate: Candidate{Kind: MaterialKindSecret, Scope: store.ScopeUser, ScopeID: facts.Root.ID, Grant: GrantProgeny},
			Reason:    ReasonDeniedByPolicy,
		}, "", chain.decision.Reason))
		return secrets, items, true
	}

	// The agent's principal closure, keyed as Decide keys it for step 7c.
	principals, err := s.authzService.authorizationPrincipals(ctx, ident)
	if err != nil {
		return secrets, items, false
	}
	closure := make(map[string]struct{}, len(principals))
	for _, p := range principals {
		closure[p.Type+":"+p.ID] = struct{}{}
	}
	in, err := s.authzService.loadUserSecretSourceInputs(ctx, chain)
	if err != nil {
		return secrets, items, false
	}
	eligible := make(map[string]bool, len(in.progeny))
	for _, sec := range in.progeny {
		eligible[sec.ID] = true
	}

	metas, err := s.secretBackend.List(ctx, secret.Filter{
		Scope:   store.ScopeUser,
		ScopeID: facts.Root.ID,
	})
	if err != nil {
		return secrets, items, false
	}
	for _, m := range metas {
		if m.SecretType == store.SecretTypeInternal {
			continue
		}
		if !m.AllowProgeny || !eligible[m.ID] {
			continue
		}
		live, kind, _, lerr := s.progenySourceLive(ctx, m)
		if lerr != nil {
			slog.Error("agent list secrets: progeny source liveness check failed",
				"agent_id", agentID, "key", m.Name, "err", lerr)
			items = append(items, materialSelectionItem(ItemResult{
				Candidate: Candidate{Kind: MaterialKindSecret, Key: m.Name, Scope: store.ScopeUser, ScopeID: facts.Root.ID, Grant: GrantProgeny, Meta: m},
				Reason:    ReasonBackendError,
			}, "", ""))
			continue
		}
		if !live {
			continue
		}

		resource := userSecretResource(m.ID, m.ScopeID)
		// (i) the source check. It does no I/O; a ceiling_error is still a
		// per-row backend error, never "not shared".
		d := userSecretSourceCheck(chain, in, resource, nil)
		if !d.Allowed {
			if d.DenyCause == DenyCauseCeilingError {
				slog.Error("agent list secrets: user secret source check failed",
					"agent_id", agentID, "key", m.Name, "reason", d.Reason)
				items = append(items, materialSelectionItem(ItemResult{
					Candidate: Candidate{Kind: MaterialKindSecret, Key: m.Name, Scope: store.ScopeUser, ScopeID: facts.Root.ID, Grant: GrantProgeny, Meta: m},
					Reason:    ReasonBackendError,
				}, "", ""))
			}
			continue
		}
		// (ii) the agent's own access constraints, with the resource context
		// Decide step 6 builds for the same resource.
		if !restrictionsAllow(restrictionsFromConstraints(in.constraints, closure, ResourceContext{
			ResourceType: resource.Type,
			ResourceID:   resource.ID,
			OwnerID:      resource.OwnerID,
			ProjectID:    projectIDForResource(resource),
			Ancestry:     resource.Ancestry,
		}), secretUsePermission) {
			continue
		}

		var src *SourceRef
		if kind != "" {
			src = &SourceRef{Kind: kind, ID: m.CreatedBy}
		}
		secrets = append(secrets, AgentSecretMeta{Key: m.Name, Type: m.SecretType, Target: m.Target})
		items = append(items, materialSelectionItem(ItemResult{
			Candidate: Candidate{Kind: MaterialKindSecret, Key: m.Name, Scope: store.ScopeUser, ScopeID: facts.Root.ID, Grant: GrantProgeny, SharingSource: src, Meta: m},
			Allowed:   true,
			Reason:    ReasonAllowed,
		}, "", ""))
	}
	return secrets, items, true
}

// restrictionsAllow reports whether every restriction allows permissionID. A
// nil Check denies, as in the kernel.
func restrictionsAllow(restrictions []Restriction, permissionID string) bool {
	for _, r := range restrictions {
		if r.Check == nil || !r.Check(permissionID) {
			return false
		}
	}
	return true
}
