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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Legacy user secret attribution repair.
//
// Before ptone/scion#1557 (2026-09-12) an agent that captured a credential
// into its user's scope (handleAgentSecrets, harness credential capture)
// recorded created_by = "agent:<uuid>". Current code records the scope
// owner. UpsertSecret keeps created_by on update, and later captures set
// allow_progeny=true, so a credential first captured before #1557 is still
// shared with progeny but attributed to the capturing agent.
//
// The user secret parent ceiling requires the author to be the owner
// (S5 lists progeny rows by created_by in the ancestry; S7 requires
// created_by == scope_id). A legacy row fails both, so it is withheld at
// start, restart and runtime read from every agent of its owner whose
// delegation chain is recorded. The repair moves created_by to scope_id,
// the attribution #1557 gives new writes. It does not change the ceiling.
//
// A row is repaired only when the attribution is provably the owner's:
//   - the referenced agent exists (live or soft-deleted) and the scope user
//     is its ancestry root, its owner or its creator. The capture handler
//     took scope_id from the agent token's ancestry root, so this is the
//     user the agent wrote for;
//   - or the referenced agent no longer exists and the scope user does. The
//     capture handler could only write into the capturing agent's own
//     ancestry root's scope, and no other writer records "agent:<uuid>",
//     so the row is in its owner's scope by construction.
//
// Anything else is skipped with a reason and left withheld. Each repair is
// a compare-and-swap on (id, version, created_by) and writes a mutation
// audit record in the same transaction that keeps the original created_by
// (BeforeSummary). Rows already attributed to their owner, and rows whose
// created_by is not an agent reference, are not touched, so a rerun is a
// no-op.

// UserSecretAttributionRepair names a repaired row.
type UserSecretAttributionRepair struct {
	SecretID          string
	Name              string
	ScopeID           string
	OriginalCreatedBy string
	Basis             string
}

// UserSecretAttributionSkip names a row the repair left unchanged, and why.
type UserSecretAttributionSkip struct {
	SecretID  string
	Name      string
	ScopeID   string
	CreatedBy string
	Reason    string
}

// UserSecretAttributionRepairResult reports one repair pass.
type UserSecretAttributionRepairResult struct {
	// Examined counts user-scope rows whose created_by is an agent reference.
	Examined int
	Repaired []UserSecretAttributionRepair
	Skipped  []UserSecretAttributionSkip
}

// Repair bases.
const (
	userSecretAttrBasisAncestryRoot = "agent_ancestry_root"
	userSecretAttrBasisAgentOwner   = "agent_owner"
	userSecretAttrBasisAgentCreator = "agent_creator"
	userSecretAttrBasisAgentGone    = "agent_gone_scope_user_exists"
)

// Skip reasons.
const (
	userSecretAttrSkipScopeOwnerEmpty  = "scope_owner_empty"
	userSecretAttrSkipAgentRefInvalid  = "agent_ref_invalid"
	userSecretAttrSkipAgentNotOwnedBy  = "agent_not_linked_to_scope_user"
	userSecretAttrSkipScopeUserMissing = "agent_gone_scope_user_missing"
	userSecretAttrSkipRowChanged       = "row_changed"
)

const (
	userSecretAttrAgentRefPrefix = "agent:"
	// mutationTypeUserSecretAttributionRepair is the mutation audit type of
	// one repaired row.
	mutationTypeUserSecretAttributionRepair = "user_secret_attribution_repair"
	userSecretAttrAuditActor                = "user-secret-attribution-repair"
)

// RepairLegacyUserSecretAttribution rewrites created_by from an agent
// reference to the scope owner on user-scope secret rows where that is
// provably the owner's attribution (see the file comment). A store error
// aborts the pass and is returned; rows already repaired stay repaired, and
// a rerun skips them. Per-row refusals are reported in the result and are
// not errors. Secret values are never read.
func RepairLegacyUserSecretAttribution(ctx context.Context, s store.Store) (UserSecretAttributionRepairResult, error) {
	var res UserSecretAttributionRepairResult
	rows, err := s.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeUser})
	if err != nil {
		return res, fmt.Errorf("listing user secrets: %w", err)
	}
	for i := range rows {
		row := &rows[i]
		if row.Scope != store.ScopeUser || !strings.HasPrefix(row.CreatedBy, userSecretAttrAgentRefPrefix) {
			continue
		}
		res.Examined++
		skip := func(reason string) {
			res.Skipped = append(res.Skipped, UserSecretAttributionSkip{
				SecretID: row.ID, Name: row.Key, ScopeID: row.ScopeID, CreatedBy: row.CreatedBy, Reason: reason,
			})
		}
		basis, reason, err := legacyUserSecretAttributionBasis(ctx, s, row)
		if err != nil {
			return res, fmt.Errorf("secret %s: %w", row.ID, err)
		}
		if reason != "" {
			skip(reason)
			continue
		}
		applied, err := applyUserSecretAttributionRepair(ctx, s, row, basis)
		if err != nil {
			return res, fmt.Errorf("secret %s: repairing attribution: %w", row.ID, err)
		}
		if !applied {
			skip(userSecretAttrSkipRowChanged)
			continue
		}
		res.Repaired = append(res.Repaired, UserSecretAttributionRepair{
			SecretID: row.ID, Name: row.Key, ScopeID: row.ScopeID, OriginalCreatedBy: row.CreatedBy, Basis: basis,
		})
	}
	return res, nil
}

// legacyUserSecretAttributionBasis decides whether row may be attributed to
// its scope owner. It returns the basis, or a skip reason, or a store error.
func legacyUserSecretAttributionBasis(ctx context.Context, s store.Store, row *store.Secret) (string, string, error) {
	owner := row.ScopeID
	if owner == "" {
		return "", userSecretAttrSkipScopeOwnerEmpty, nil
	}
	agentID := strings.TrimPrefix(row.CreatedBy, userSecretAttrAgentRefPrefix)
	if _, perr := uuid.Parse(agentID); perr != nil {
		return "", userSecretAttrSkipAgentRefInvalid, nil
	}
	agent, err := s.GetAgent(ctx, agentID)
	switch {
	case err == nil && agent != nil:
		switch {
		case len(agent.Ancestry) > 0 && agent.Ancestry[0] == owner:
			return userSecretAttrBasisAncestryRoot, "", nil
		case agent.OwnerID == owner:
			return userSecretAttrBasisAgentOwner, "", nil
		case agent.CreatedBy == owner:
			return userSecretAttrBasisAgentCreator, "", nil
		}
		return "", userSecretAttrSkipAgentNotOwnedBy, nil
	case err == nil || errors.Is(err, store.ErrNotFound):
		// The agent no longer exists.
	default:
		return "", "", fmt.Errorf("agent lookup: %w", err)
	}
	user, err := s.GetUser(ctx, owner)
	if errors.Is(err, store.ErrNotFound) || (err == nil && user == nil) {
		return "", userSecretAttrSkipScopeUserMissing, nil
	}
	if err != nil {
		return "", "", fmt.Errorf("scope user lookup: %w", err)
	}
	return userSecretAttrBasisAgentGone, "", nil
}

// applyUserSecretAttributionRepair rewrites row's created_by and writes its
// audit record in one transaction. applied is false when the row changed
// since it was listed.
func applyUserSecretAttributionRepair(ctx context.Context, s store.Store, row *store.Secret, basis string) (bool, error) {
	before, err := json.Marshal(map[string]string{"created_by": row.CreatedBy})
	if err != nil {
		return false, err
	}
	after, err := json.Marshal(map[string]string{
		"created_by": row.ScopeID,
		"basis":      basis,
		"name":       row.Key,
		"scope":      row.Scope,
		"scope_id":   row.ScopeID,
	})
	if err != nil {
		return false, err
	}
	applied := false
	err = s.WithTx(ctx, func(tx store.Store) error {
		ok, err := tx.AttributeUserSecretToOwner(ctx, row.ID, row.Version, row.CreatedBy)
		if err != nil || !ok {
			return err
		}
		applied = true
		return tx.CreateMutationAudit(ctx, &store.MutationAuditRecord{
			ID:                 uuid.New().String(),
			Timestamp:          time.Now().UTC(),
			MutationType:       mutationTypeUserSecretAttributionRepair,
			ActorPrincipalKind: "system",
			ActorPrincipalID:   userSecretAttrAuditActor,
			TargetType:         "secret",
			TargetID:           row.ID,
			BeforeSummary:      string(before),
			AfterSummary:       string(after),
		})
	})
	if err != nil {
		return false, err
	}
	return applied, nil
}
