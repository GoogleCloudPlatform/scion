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
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Purge of test identities with admin roles (ptone/scion#4240, admin tier
// phase A3; design §4.5, §4.6).
//
// An admin-kind identity is purged by the same 3b engine as a member-tier
// one (purgeTestIdentity: everything stamped with it as the provenance
// root), plus every grant row stamped with its issuance ID
// (purgeTestAdminGrants): role bindings, group memberships, custom role
// definitions, invite codes and broker join tokens. The grants go first,
// so the authority the identity handed out ends before anything else is
// touched. A custom role that a binding without the stamp still names is
// kept and reported, never deleted under a binding the identity did not
// make. Nothing without the stamp is removed: changes the identity made to
// other rows are listed in the residue report and never reverted.
//
// Callers: the expiry reaper (expired admin kinds, after the grace
// period), DELETE /api/v1/test-identities/{id}?purge=true for the issuer or
// a human super-admin, and revoke-all (grants only; the identity's
// stamped resources stay for the purge route).
//
// Callers of the purge route are always human: test identities of every
// kind are refused by testIdentityCaller (a deliberate deviation from admin
// tier design §4.6, which let a test super-admin reach the cross-issuer
// cleanup; confirmed by test-ident-lead, design-decisions.md).

// testAdminGrantPurgeMutation is the audit type of one grant row the purge
// removed.
const testAdminGrantPurgeMutation = "test_admin_grant_purge"

// testAdminIssuanceFromAudit returns the issuance and issuer of an
// admin-kind identity whose row may be gone, from its test_admin_issue
// audit record. ok is false when there is no such record.
func (s *Server) testAdminIssuanceFromAudit(ctx context.Context, identityID string) (issuanceID, issuerID string, ok bool, err error) {
	if !isUUID(identityID) {
		return "", "", false, nil
	}
	recs, _, err := s.store.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: testAdminIssueMutation,
		TargetType:   testIdentityAuditTargetType,
		TargetID:     identityID,
		Limit:        1,
	})
	if err != nil {
		return "", "", false, err
	}
	if len(recs) == 0 {
		return "", "", false, nil
	}
	var sum struct {
		IssuanceID string `json:"issuance_id"`
		IssuedBy   string `json:"issued_by"`
	}
	if err := json.Unmarshal([]byte(recs[0].AfterSummary), &sum); err != nil || sum.IssuanceID == "" {
		return "", "", false, nil
	}
	return sum.IssuanceID, sum.IssuedBy, true, nil
}

// canPurgeTestAdminIdentity reports whether caller may purge the admin-kind
// identity u: its issuer, or a super-admin. The caller is a human user
// (testIdentityCaller refuses every test kind) and never a workload
// delegation (those manage member-tier identities only).
func (s *Server) canPurgeTestAdminIdentity(ctx context.Context, caller UserIdentity, issuerID string) bool {
	if GetCredentialContextFromContext(ctx).Kind == CredentialKindWorkloadDelegation {
		return false
	}
	if issuerID != "" && issuerID == caller.ID() {
		return true
	}
	if s.testAdminPolicyRefusesUser(ctx, caller.ID(), "test_admin_purge.go:canPurgeTestAdminIdentity") {
		return false
	}
	return s.authzService.IsSystemAdmin(ctx, caller.ID())
}

// purgeTestAdminGrants removes every grant row stamped with run.issuanceID,
// one row per transaction with its test_admin_grant_purge audit record,
// and counts them in report. A grant that is gone, or no longer carries
// the stamp, is skipped. A role definition still bound is kept
// (report.KeptGrants). A store failure leaves the row for the next run
// (report.NotDone). Users who lost a binding or a membership get a
// membership-loss check, so their agents are re-evaluated.
func (s *Server) purgeTestAdminGrants(ctx context.Context, run fixturePurgeRun, basis fixturePurgeBasis, report *TestIdentityPurgeReport) error {
	if run.issuanceID == "" {
		return nil
	}
	grants, truncated, err := s.store.ListIssuanceStampedGrants(ctx, run.issuanceID, fixturePurgeListLimit)
	if err != nil {
		return fmt.Errorf("list issuance-stamped grants: %w", err)
	}
	if truncated {
		report.Truncated = true
	}
	basis.IssuanceID = run.issuanceID
	actor := auditActorFromContext(ctx)
	for i := range grants {
		g := grants[i]
		before, _ := json.Marshal(g)
		record := &store.MutationAuditRecord{
			MutationType:  testAdminGrantPurgeMutation,
			TargetType:    string(g.Kind),
			TargetID:      g.ID,
			BeforeSummary: string(before),
			AfterSummary:  fixturePurgeSummary(nil, basis),
			Timestamp:     time.Now(),
			CorrelationID: basis.RunID,
		}
		actor.ApplyActor(record)
		applyHubActorFallback(record)
		deleted := false
		err := s.store.WithTx(ctx, func(tx store.Store) error {
			ok, err := tx.DeleteIssuanceStampedGrant(ctx, g, run.issuanceID)
			if err != nil || !ok {
				return err
			}
			deleted = true
			if err := enqueueGrantPurgeLossTx(ctx, tx, g, actor); err != nil {
				return err
			}
			return writeFixturePurgeAuditTx(ctx, tx, record, basis)
		})
		switch {
		case errors.Is(err, store.ErrStampedRoleInUse):
			report.KeptGrants = append(report.KeptGrants, TestIdentityPurgeItem{Type: string(g.Kind), ID: g.ID, Status: "kept", Reason: "role_in_use"})
			continue
		case err != nil:
			slog.ErrorContext(ctx, "test identity purge: grant delete failed", "kind", g.Kind, "id", g.ID, "error", err)
			report.NotDone = append(report.NotDone, TestIdentityPurgeItem{Type: string(g.Kind), ID: g.ID, Status: "failed", Reason: "store_error"})
			continue
		}
		if !deleted {
			continue
		}
		report.Deleted.Grants++
		switch g.Kind {
		case store.IssuanceGrantRoleBinding:
			if g.PrincipalType == store.RoleBindingPrincipalUser {
				s.publishConduitAuthzChanged(conduitAuthzMatch{UserID: g.PrincipalID})
			}
		case store.IssuanceGrantGroupMembership:
			if g.PrincipalType == store.GroupMemberTypeUser {
				s.publishConduitAuthzChanged(conduitAuthzMatch{UserID: g.PrincipalID})
			}
		}
	}
	if report.Deleted.Grants > 0 {
		s.drainMembershipLossChecks(ctx)
	}
	return nil
}

// enqueueGrantPurgeLossTx enqueues the membership-loss check for the user
// who lost grant g: the bound project for a project role binding, every
// project otherwise.
func enqueueGrantPurgeLossTx(ctx context.Context, tx store.Store, g store.IssuanceStampedGrant, actor AuditActor) error {
	switch g.Kind {
	case store.IssuanceGrantRoleBinding:
		if g.PrincipalType != store.RoleBindingPrincipalUser {
			return nil
		}
		if g.ScopeType == store.RoleScopeProject {
			return enqueueMembershipLossTx(ctx, tx, g.PrincipalID, g.ScopeID, store.MembershipLossTriggerAdminBindingDelete, actor)
		}
		return enqueueMembershipLossTx(ctx, tx, g.PrincipalID, "", store.MembershipLossTriggerSystemScopeChange, actor)
	case store.IssuanceGrantGroupMembership:
		if g.PrincipalType != store.GroupMemberTypeUser {
			return nil
		}
		return enqueueMembershipLossTx(ctx, tx, g.PrincipalID, "", store.MembershipLossTriggerGroupChange, actor)
	}
	return nil
}

// testAdminGroupEdgeRemoveMutation is the audit type of one child-group
// edge removed at an admin-kind identity's expiry.
const testAdminGroupEdgeRemoveMutation = "test_admin_group_edge_remove"

// detachTestAdminGroups removes, at an admin-kind identity's expiry, every
// child-group edge that makes a group stamped with it (as its provenance
// root) a child of another group (review O1). A child-group edge has no
// expiry of its own: the store admits such an edge under shaping only for
// a group the identity created, and its bound would otherwise be the
// purge, a grace period later. Members a person added to that group stop
// inheriting the parent's authority here. Each removal writes a mutation
// audit row and queues a membership-loss check for the group's users. It
// returns the number of edges removed.
func (s *Server) detachTestAdminGroups(ctx context.Context, rootID string) (int, error) {
	set, err := s.store.ListFixtureRootedResources(ctx, rootID, fixturePurgeListLimit)
	if err != nil {
		return 0, fmt.Errorf("list stamped groups: %w", err)
	}
	actor := AuditActor{PrincipalKind: membershipLossSystemActorKind, PrincipalID: membershipLossSystemActorID}
	runID := api.NewUUID()
	removed := 0
	for i := range set.Groups {
		g := &set.Groups[i]
		if g.Fixture.RootID != rootID {
			continue
		}
		parents, err := s.store.GetDirectParentGroupIDs(ctx, g.ID)
		if err != nil {
			return removed, fmt.Errorf("parents of group %s: %w", g.ID, err)
		}
		if len(parents) == 0 {
			continue
		}
		users, err := transitiveGroupMemberUsers(ctx, s.store, g.ID)
		if err != nil {
			return removed, fmt.Errorf("members of group %s: %w", g.ID, err)
		}
		// The provenance this removal relied on: the group's own stamp
		// (provenance_basis, Phase 3c). One run per expiry.
		edgeBasis := fixturePurgeBasis{
			RootID:   rootID,
			IssuerID: g.Fixture.IssuerID,
			Basis:    fixturePurgeBasisName,
			RunID:    runID,
			Trigger:  testIdentityExpireMutation,
		}
		for _, parentID := range parents {
			record := &store.MutationAuditRecord{
				MutationType:  testAdminGroupEdgeRemoveMutation,
				TargetType:    "group",
				TargetID:      g.ID,
				BeforeSummary: marshalDeletionAuditJSON(map[string]string{"parent_group_id": parentID, "child_group_id": g.ID, "fixture_root_id": rootID}),
				Timestamp:     time.Now(),
				CorrelationID: runID,
			}
			actor.ApplyActor(record)
			applyHubActorFallback(record)
			err := s.store.WithTx(ctx, func(tx store.Store) error {
				if err := tx.RemoveChildGroupEdge(ctx, parentID, g.ID); err != nil {
					return err
				}
				for _, uid := range users {
					if err := enqueueMembershipLossTx(ctx, tx, uid, "", store.MembershipLossTriggerGroupChange, actor); err != nil {
						return err
					}
				}
				return writeFixturePurgeAuditTx(ctx, tx, record, edgeBasis)
			})
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return removed, fmt.Errorf("remove edge %s -> %s: %w", parentID, g.ID, err)
			}
			removed++
		}
	}
	return removed, nil
}
