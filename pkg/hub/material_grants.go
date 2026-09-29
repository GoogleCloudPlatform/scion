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

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// runtimeProvenanceRoot derives the F.2a provenance root from the target
// agent's stored ancestry (check 3). There is no fallback to CreatedBy,
// OwnerID, the token's OriginUserID(), or scheduledCreatorIdentity: the
// stored ancestry chain is the only source. An empty ancestry (scheduler
// children and legacy rows) has no root and must be denied by the caller.
//
// ctx is unused by the F.2a rule above, but is part of the committed
// signature: a later change resolves the root from recorded edge provenance
// instead, which needs it.
func runtimeProvenanceRoot(ctx context.Context, rec *store.Agent) (ProvenanceRoot, bool) { //nolint:unparam // ctx: see doc comment
	if rec == nil || len(rec.Ancestry) == 0 {
		return ProvenanceRoot{}, false
	}
	return ProvenanceRoot{Kind: "user", ID: rec.Ancestry[0]}, true
}

// progenySourceLive resolves whether the authorship of a user-scoped item
// (meta.CreatedBy) is still live (check 8, "source live"). CreatedBy is
// typed by trying a user lookup first, then an agent lookup — never both at
// once, and never assumed. kind is "user" or "agent" once CreatedBy resolves
// to either, for the audit-only SharingSource; "" when neither resolves.
func (s *Server) progenySourceLive(ctx context.Context, meta secret.SecretMeta) (live bool, kind string, reason string, err error) {
	u, uerr := s.store.GetUser(ctx, meta.CreatedBy)
	if uerr == nil {
		if u.Status != store.UserStatusActive {
			return false, "user", ReasonSourceInactive, nil
		}
		return true, "user", ReasonAllowed, nil
	}
	if !errors.Is(uerr, store.ErrNotFound) {
		return false, "", ReasonBackendError, uerr
	}

	// Not a user (or not found as one). Try the agent lookup.
	ag, aerr := s.store.GetAgent(ctx, meta.CreatedBy)
	if aerr != nil {
		if errors.Is(aerr, store.ErrNotFound) {
			// Neither lookup found a live source.
			return false, "", ReasonSourceInactive, nil
		}
		return false, "", ReasonBackendError, aerr
	}
	// GetAgent returns soft-deleted rows; the source agent must not be one.
	if !ag.DeletedAt.IsZero() {
		return false, "agent", ReasonSourceInactive, nil
	}
	if len(ag.Ancestry) == 0 {
		return false, "agent", ReasonSourceInactive, nil
	}
	rootUser, ruErr := s.store.GetUser(ctx, ag.Ancestry[0])
	if ruErr != nil {
		if errors.Is(ruErr, store.ErrNotFound) {
			return false, "agent", ReasonSourceInactive, nil
		}
		return false, "agent", ReasonBackendError, ruErr
	}
	if rootUser.Status != store.UserStatusActive {
		return false, "agent", ReasonSourceInactive, nil
	}
	return true, "agent", ReasonAllowed, nil
}

// progenyEligibleSecretIDs makes one store.ListProgenySecrets call per
// request and returns the set of secret IDs eligible for the agent's
// ancestry. It applies the same preconditions as CheckProgenyAccess (stored,
// hub-attested identity; non-empty ancestry; a non-nil store) and is used
// only to filter metadata for agentListSecrets — by-key reads (check 8)
// still call CheckProgenyAccess per item.
func (s *Server) progenyEligibleSecretIDs(ctx context.Context, rec *store.Agent) (map[string]bool, error) {
	ident := &storedAgentIdentity{agent: rec}
	if !AncestryIsHubAttested(ident) {
		return map[string]bool{}, nil
	}
	if len(rec.Ancestry) == 0 {
		return map[string]bool{}, nil
	}
	if s.store == nil {
		return nil, fmt.Errorf("store not available (fail-closed)")
	}
	secrets, err := s.store.ListProgenySecrets(ctx, rec.Ancestry)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]bool, len(secrets))
	for _, sec := range secrets {
		ids[sec.ID] = true
	}
	return ids, nil
}
