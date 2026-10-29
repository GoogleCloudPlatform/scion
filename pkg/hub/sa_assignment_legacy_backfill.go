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

// Legacy service-account assignment backfill.
//
// The GCP token mint and the GCS link proxy decide gcp_service_account.use
// with a resource parent ceiling (the service-account parent ceiling) that
// requires exactly one active agent_service_account_assignments row. Rows
// are written by agent create, SA PATCH and reincarnate. Agents created
// before the assignment table existed have no row and are refused with
// ceiling_provenance_missing, so every long-running legacy agent loses its
// GCP tokens on upgrade.
//
// The backfill writes the row agent create would have written. Agent create
// records the same provenance and frozen ceiling on the delegation edge and
// on the assignment (commitAgentCreate), so the row is taken from the
// agent's single active, recorded delegation edge in its own project:
//   - an edge recorded from an accepted credential (session, uat, agent,
//     dev_local, scheduler) is copied verbatim;
//   - an edge recorded by delegation provenance adoption (source credential
//     system_migration) is not an accepted assignment source, so the row
//     records the edge delegator as the source with the credential kind for
//     its principal kind (agent for an agent, session for a user) and the
//     edge's ceiling.
//
// The edge's ceiling must allow gcp_service_account.assign and the delegator
// must be live (an active user, or an agent that is not deleted); otherwise
// the agent is skipped with a reason and left denied. Only agents with no
// active row are touched. The backfill never deactivates or rewrites an
// existing row.

// SAAssignmentLegacyBackfillSkip names an agent the backfill left without a
// row, and why.
type SAAssignmentLegacyBackfillSkip struct {
	AgentID string
	Reason  string
}

// SAAssignmentLegacyBackfillResult reports one backfill pass.
type SAAssignmentLegacyBackfillResult struct {
	// Examined counts live assign-mode agents with no active row.
	Examined int
	// Written lists the agents that received a row.
	Written []string
	// Skipped lists the agents left without a row.
	Skipped []SAAssignmentLegacyBackfillSkip
}

// Skip reasons.
const (
	saBackfillSkipNoEdge             = "no_active_edge"
	saBackfillSkipAmbiguousEdges     = "ambiguous_edges"
	saBackfillSkipSentinelEdge       = "migration_sentinel_edge"
	saBackfillSkipUnrecordedEdge     = "unrecorded_edge"
	saBackfillSkipCeilingLacksAssign = "ceiling_lacks_assign"
	saBackfillSkipSourceNotAccepted  = "source_credential_not_accepted"
	saBackfillSkipSourceInconsistent = "source_inconsistent"
	saBackfillSkipDelegatorMissing   = "delegator_missing"
	saBackfillSkipDelegatorInactive  = "delegator_inactive"
	saBackfillSkipRowAppeared        = "row_appeared"
)

const saBackfillListPageSize = 500

// BackfillLegacySAAssignments writes the missing assignment row for every
// live (not soft-deleted) agent whose applied GCP identity is in assign
// mode and which has no active row. A store error aborts the pass and is
// returned; rows already written stay written, and a rerun skips them.
// Per-agent refusals are reported in the result and are not errors.
func BackfillLegacySAAssignments(ctx context.Context, s store.Store) (SAAssignmentLegacyBackfillResult, error) {
	var res SAAssignmentLegacyBackfillResult
	cursor := ""
	for {
		page, err := s.ListAgents(ctx, store.AgentFilter{}, store.ListOptions{Limit: saBackfillListPageSize, Cursor: cursor})
		if err != nil {
			return res, fmt.Errorf("listing agents: %w", err)
		}
		if page == nil {
			return res, errors.New("listing agents: nil result")
		}
		for i := range page.Items {
			agent := &page.Items[i]
			if !agent.DeletedAt.IsZero() || agentAssignedServiceAccountID(agent) == "" {
				continue
			}
			rows, err := s.GetActiveAgentServiceAccountAssignments(ctx, agent.ID)
			if err != nil {
				return res, fmt.Errorf("agent %s: assignment lookup: %w", agent.ID, err)
			}
			if len(rows) > 0 {
				continue
			}
			res.Examined++
			row, reason, err := legacySAAssignmentFromEdge(ctx, s, agent)
			if err != nil {
				return res, fmt.Errorf("agent %s: %w", agent.ID, err)
			}
			if reason == "" {
				reason, err = writeLegacySAAssignment(ctx, s, row)
				if err != nil {
					return res, fmt.Errorf("agent %s: writing assignment: %w", agent.ID, err)
				}
			}
			if reason != "" {
				res.Skipped = append(res.Skipped, SAAssignmentLegacyBackfillSkip{AgentID: agent.ID, Reason: reason})
				continue
			}
			res.Written = append(res.Written, agent.ID)
		}
		if page.NextCursor == "" {
			return res, nil
		}
		cursor = page.NextCursor
	}
}

// writeLegacySAAssignment inserts row in a transaction, re-checking that the
// agent still has no active row so a concurrent write is never replaced.
func writeLegacySAAssignment(ctx context.Context, s store.Store, row *store.AgentServiceAccountAssignment) (string, error) {
	appeared := false
	err := s.WithTx(ctx, func(tx store.Store) error {
		rows, err := tx.GetActiveAgentServiceAccountAssignments(ctx, row.AgentID)
		if err != nil {
			return err
		}
		if len(rows) > 0 {
			appeared = true
			return nil
		}
		return tx.ReplaceAgentServiceAccountAssignment(ctx, row)
	})
	if err != nil {
		return "", err
	}
	if appeared {
		return saBackfillSkipRowAppeared, nil
	}
	return "", nil
}

// legacySAAssignmentFromEdge builds the backfill row for agent from its
// active delegation edge in its own project. A non-empty reason means the
// agent is skipped; an error is a store failure.
func legacySAAssignmentFromEdge(ctx context.Context, s store.Store, agent *store.Agent) (*store.AgentServiceAccountAssignment, string, error) {
	edges, err := s.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agent.ID)
	if err != nil {
		return nil, "", fmt.Errorf("delegation edge lookup: %w", err)
	}
	var active []*store.DelegationEdge
	for _, e := range filterEdgesByScope(edges, store.RoleScopeProject, agent.ProjectID) {
		if e.Active {
			active = append(active, e)
		}
	}
	switch len(active) {
	case 0:
		return nil, saBackfillSkipNoEdge, nil
	case 1:
	default:
		return nil, saBackfillSkipAmbiguousEdges, nil
	}
	edge := active[0]

	if isMigrationSentinel(edge) {
		return nil, saBackfillSkipSentinelEdge, nil
	}
	if !knownProvenanceVersion(edge.ProvenanceVersion) ||
		(edge.Kind != store.EffectCeilingBounded && edge.Kind != store.EffectCeilingPrincipal) {
		return nil, saBackfillSkipUnrecordedEdge, nil
	}
	if !EffectCeilingAllows(edge.EffectCeiling, gcpServiceAccountAssignPermission, false) {
		return nil, saBackfillSkipCeilingLacksAssign, nil
	}

	var prov store.AuthorityProvenance
	switch edge.SourceCredentialKind {
	case store.SourceCredentialSession, store.SourceCredentialUAT, store.SourceCredentialAgent,
		store.SourceCredentialDevLocal, store.SourceCredentialScheduler:
		prov = edge.AuthorityProvenance
	case store.SourceCredentialSystemMigration:
		prov = store.AuthorityProvenance{
			ProvenanceVersion:   store.ProvenanceVersionV1,
			SourcePrincipalKind: edge.DelegatorType,
			SourcePrincipalID:   edge.DelegatorID,
		}
		switch edge.DelegatorType {
		case store.DelegationPrincipalAgent:
			prov.SourceCredentialKind = store.SourceCredentialAgent
		case store.DelegationPrincipalUser:
			prov.SourceCredentialKind = store.SourceCredentialSession
		default:
			return nil, saBackfillSkipSourceNotAccepted, nil
		}
	default:
		return nil, saBackfillSkipSourceNotAccepted, nil
	}

	row := newSAAssignment(agent, store.SAAssignmentOriginLegacyBackfill, edge.EffectCeiling, prov)
	if !assignmentProvenanceRecorded(*row) {
		return nil, saBackfillSkipUnrecordedEdge, nil
	}
	if err := checkAssignmentSourcePrincipal(*row); err != nil {
		if errors.Is(err, errSourceNotAllowed) {
			return nil, saBackfillSkipSourceNotAccepted, nil
		}
		return nil, saBackfillSkipSourceInconsistent, nil
	}

	reason, err := legacyBackfillSourceLive(ctx, s, row.SourcePrincipalKind, row.SourcePrincipalID)
	if err != nil || reason != "" {
		return nil, reason, err
	}
	return row, "", nil
}

// legacyBackfillSourceLive applies the parent ceiling's liveness rule for a
// recorded source: an active user, or an agent that is not deleted.
func legacyBackfillSourceLive(ctx context.Context, s store.Store, kind, id string) (string, error) {
	switch kind {
	case store.DelegationPrincipalUser:
		user, err := s.GetUser(ctx, id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && user == nil) {
			return saBackfillSkipDelegatorMissing, nil
		}
		if err != nil {
			return "", fmt.Errorf("source user lookup: %w", err)
		}
		if user.Status != store.UserStatusActive {
			return saBackfillSkipDelegatorInactive, nil
		}
		return "", nil
	case store.DelegationPrincipalAgent:
		src, err := s.GetAgent(ctx, id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && src == nil) {
			return saBackfillSkipDelegatorMissing, nil
		}
		if err != nil {
			return "", fmt.Errorf("source agent lookup: %w", err)
		}
		if !src.DeletedAt.IsZero() {
			return saBackfillSkipDelegatorMissing, nil
		}
		return "", nil
	default:
		return saBackfillSkipSourceInconsistent, nil
	}
}
