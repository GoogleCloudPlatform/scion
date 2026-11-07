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
	"net/http"
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Project self-leave (ptone/scion#7494)
//
// LeaveProject removes the caller's own direct user bindings on a project,
// in one transaction under the project membership lock, with the same
// last-owner and constraint-admin guards as RemoveMember (the shared step
// removeProjectBindingsTx). It never touches group, agent or visitor
// bindings, another principal's bindings, or anything other than role
// bindings and the caller's project-boundary access tokens.
//
// Self governance: the target is always the caller. The governance matrix
// (isOperationPermitted) and the hub-level role-binding override
// (actorHasHubRoleBindingAuthority) are not consulted; a caller with no
// direct binding has nothing to leave and gets not_found.
// ---------------------------------------------------------------------------

// Denial codes of a project leave.
const (
	// ErrCodeMembershipGroupDerived: the caller's only membership of the
	// project comes from a group, which a leave never removes.
	ErrCodeMembershipGroupDerived = "membership_group_derived"
	// ErrCodeLeaveBindingUnclassified: one of the caller's bindings on the
	// project is of a kind leaveBindingClass does not know, so the leave
	// fails closed and nothing changes.
	ErrCodeLeaveBindingUnclassified = "leave_binding_unclassified"
)

// leaveSessionOnlyReason is the catalog's session-only reason for a
// project leave: tokens are refused until token leave has its own
// governance (a dedicated selector).
const leaveSessionOnlyReason = authzop.ReasonGovernancePending

// leaveAgentItemsMax caps LeaveResult.Agents.Items on the server (R4). The
// count stays exact; Truncated reports that items were cut.
const leaveAgentItemsMax = 50

// Values of LeaveResult.RemainingAccess.
const (
	leaveRemainingNone  = "none"
	leaveRemainingGroup = "group"
	leaveRemainingHub   = "hub"
)

// Values of MembershipCapabilities.LeaveBlockedReason.
const (
	leaveBlockedLastOwner    = "last_owner"
	leaveBlockedGroupDerived = "group_derived"
)

// LeaveResult is the response of POST /api/v1/projects/{id}/leave, for a
// real leave and for a dry run alike.
type LeaveResult struct {
	ProjectID string `json:"projectId"`
	DryRun    bool   `json:"dryRun"`
	// RemovedRoles are the role names of the caller's removed bindings,
	// sorted.
	RemovedRoles []string `json:"removedRoles"`
	// RemainingAccess is the caller's admission after the leave: none,
	// group or hub.
	RemainingAccess string `json:"remainingAccess"`
	// Agents are the agents the caller roots in the project, which the
	// membership check holds when RemainingAccess is none.
	Agents leaveAgents `json:"agents"`
	// ProjectBoundTokens counts the caller's access tokens bounded to this
	// project that the leave revoked (dry run: would revoke).
	ProjectBoundTokens leaveTokens `json:"projectBoundTokens"`
	// WillBeHeld is RemainingAccess == none with at least one agent.
	WillBeHeld bool `json:"willBeHeld"`

	// revokedTokenIDs are the revoked token IDs, for the audit record and
	// the log; never serialized.
	revokedTokenIDs []string
}

// leaveAgents is the capped agent list of a LeaveResult.
type leaveAgents struct {
	Count     int              `json:"count"`
	Truncated bool             `json:"truncated"`
	Items     []leaveAgentItem `json:"items"`
}

// leaveAgentItem is one agent of a LeaveResult. It is a dedicated type with
// exactly these three fields, not the agent model with fields omitted, so a
// field added to the agent model never reaches this response (R4).
type leaveAgentItem struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Phase string `json:"phase"`
}

// leaveTokens is the token part of a LeaveResult.
type leaveTokens struct {
	Revoked int `json:"revoked"`
}

// leaveClass is the class leaveBindingClass gives a binding.
type leaveClass int

const (
	// leaveClassUnclassified: a binding the classifier does not know. The
	// leave fails closed.
	leaveClassUnclassified leaveClass = iota
	// leaveClassRemovable: a direct allow-type user binding of the caller
	// on the project, which the leave deletes.
	leaveClassRemovable
	// leaveClassExcluded: a binding the leave never deletes (another
	// principal, scope or project, a visitor or rule-created binding).
	leaveClassExcluded
)

// leaveBindingClass classifies rb (of role rd) for a leave of projectID by
// callerID (design 3.3, R3a). It is an allow-list: a binding is removable
// only when every attribute is one the function knows to be a direct,
// allow-type user binding of the caller on the project. A principal type,
// scope type or role it does not know is unclassified, so a binding kind
// added later is refused rather than silently dropped.
// TestLeaveBindingClass_GuardUnclassified pins the fields and constants
// this function was written against.
//
// NotBefore and ExpiresAt do not change the class (a scheduled or expired
// direct binding is removed too), and neither do ID, CreatedBy, CreatedAt
// or the provenance-only IssuanceID.
func leaveBindingClass(rb *store.RoleBinding, rd *store.RoleDefinition, projectID, callerID string) leaveClass {
	if rb == nil || rd == nil || rb.RoleDefinitionID != rd.ID {
		return leaveClassUnclassified
	}
	switch rb.PrincipalType {
	case store.RoleBindingPrincipalUser:
	case store.RoleBindingPrincipalGroup, store.RoleBindingPrincipalAgent:
		return leaveClassExcluded
	default:
		return leaveClassUnclassified
	}
	switch rb.ScopeType {
	case store.RoleScopeProject:
	case store.RoleScopeSystem:
		return leaveClassExcluded
	default:
		return leaveClassUnclassified
	}
	if rb.ScopeID != projectID || rb.PrincipalID != callerID {
		return leaveClassExcluded
	}
	if rb.SourceRuleID != nil {
		// Created by a project visitor rule: the visitor revoke path.
		return leaveClassExcluded
	}
	if rd.Name == store.ProjectRoleVisitor || !rd.ConfersProjectMembership() {
		return leaveClassExcluded
	}
	if rd.ScopeType != store.RoleScopeProject {
		// A project binding of a role that is not a project role.
		return leaveClassUnclassified
	}
	switch rd.Name {
	case store.ProjectRoleOwner, store.ProjectRoleAdmin, store.ProjectRoleMember:
		if rd.System {
			return leaveClassRemovable
		}
		// A built-in name on a role that is not the seeded one.
		return leaveClassUnclassified
	}
	if store.IsBuiltInProjectMembershipRole(rd.Name) {
		// A built-in project role this function does not know.
		return leaveClassUnclassified
	}
	if rd.System {
		// A seeded project role that is not a known built-in.
		return leaveClassUnclassified
	}
	// A custom project role: allow-only, like every RoleDefinition.
	return leaveClassRemovable
}

// leaveBinding is one of the caller's project bindings with its role.
type leaveBinding struct {
	binding *store.RoleBinding
	role    *store.RoleDefinition
}

// errLeaveDryRunRollback rolls back a dry run's transaction after the
// result is computed: nothing it wrote persists.
var errLeaveDryRunRollback = errors.New("project leave dry run: roll back")

// leaveDecision builds a refusal of a project leave.
func leaveDecision(status int, code, reason string, details map[string]interface{}) *MembershipDecision {
	return &MembershipDecision{Allowed: false, DenialCode: code, Reason: reason, HTTPStatus: status, Details: details}
}

// leaveNotFoundDecision is the refusal for a caller with no direct binding
// on the project. The handler renders it exactly like a missing project.
func leaveNotFoundDecision() *MembershipDecision {
	return leaveDecision(http.StatusNotFound, ErrCodeNotFound, "Project not found", nil)
}

// leaveUnclassifiedDecision is the fail-closed refusal for a binding
// leaveBindingClass does not know.
func leaveUnclassifiedDecision() *MembershipDecision {
	return leaveDecision(http.StatusInternalServerError, ErrCodeLeaveBindingUnclassified,
		"Leave isn't available for this membership; contact a project owner.", nil)
}

// leaveGroupDerivedReason is the message of a group-derived refusal.
const leaveGroupDerivedReason = "You're a member of this project through a group. Leave the group, or ask a group admin to remove you."

// leaveLastOwnerGuidance is the guidance added to a last-owner refusal.
const leaveLastOwnerGuidance = "Transfer ownership to another member (POST /api/v1/projects/{id}/transfer-ownership) or delete the project."

// leaveDecisionError carries a leave refusal out of the transaction.
type leaveDecisionError struct{ decision *MembershipDecision }

func (e *leaveDecisionError) Error() string { return e.decision.Reason }

// leaveGroupDerivedError carries a group-derived refusal out of the
// transaction with the IDs of the groups that admit the caller. Which of
// them the caller may read is decided after the transaction, through the
// authorization service's own store.
type leaveGroupDerivedError struct{ groupIDs []string }

func (e *leaveGroupDerivedError) Error() string { return leaveGroupDerivedReason }

// LeaveProject removes every direct user binding of the actor on
// projectID (design 3.2). With dryRun it computes the same result and rolls
// back: no binding, token, loss check or audit record changes.
func (svc *ProjectMembershipService) LeaveProject(ctx context.Context, actor UserIdentity, projectID string, dryRun bool) (*LeaveResult, *MembershipDecision) {
	if isNilIdentity(actor) {
		return nil, leaveDecision(http.StatusForbidden, ErrCodeMembershipCredentialInsufficient,
			"leaving a project requires an authenticated user", nil)
	}
	// Session only (R2): a user access token never leaves a project, even
	// one checkMembershipActor would admit.
	if membershipCallerIsToken(ctx) {
		return nil, leaveDecision(http.StatusForbidden, ErrCodeMembershipCredentialInsufficient,
			"leaving a project requires an interactive session", sessionOnlyDenialDetails(leaveSessionOnlyReason))
	}
	if denial := svc.checkMembershipActor(ctx, actor.ID(), projectID); denial != nil {
		return nil, denial
	}
	actorID := actor.ID()

	// Pre-read outside the lock: a constraint-admin binding needs the
	// admin-guard lock, which is taken before the project lock (the order
	// RemoveMember and user delete use).
	pre, decision, err := svc.leaveBindingsFromStore(ctx, svc.store, actorID, projectID)
	if err != nil {
		return nil, leaveDecision(http.StatusInternalServerError, ErrCodeInternalError, err.Error(), nil)
	}
	if decision != nil {
		return nil, decision
	}
	guarded := map[string]bool{}
	for _, lb := range pre {
		if roleHasConstraintAdmin(lb.role) {
			guarded[lb.binding.ID] = true
		}
	}

	var result *LeaveResult
	txErr := svc.store.WithTx(ctx, func(tx store.Store) error {
		if len(guarded) > 0 {
			if err := lockConstraintAdminGuard(ctx, tx); err != nil {
				return err
			}
		}
		if err := tx.LockProjectForMembership(ctx, projectID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return &leaveDecisionError{decision: leaveNotFoundDecision()}
			}
			return fmt.Errorf("lock project: %w", err)
		}

		// Re-read under the lock.
		bindings, decision, err := svc.leaveBindingsFromStore(ctx, tx, actorID, projectID)
		if err != nil {
			return err
		}
		if decision != nil {
			return &leaveDecisionError{decision: decision}
		}
		if len(bindings) == 0 {
			// Not a direct member: group-derived when an active group
			// binding admits the caller, otherwise not found.
			groups, err := activeGroupProjectBindingGroups(ctx, tx, actorID, projectID, svc.nowFunc())
			if err != nil {
				return err
			}
			if len(groups) > 0 {
				return &leaveGroupDerivedError{groupIDs: groups}
			}
			return &leaveDecisionError{decision: leaveNotFoundDecision()}
		}
		for _, lb := range bindings {
			if roleHasConstraintAdmin(lb.role) && !guarded[lb.binding.ID] {
				// A constraint-admin binding appeared after the pre-read:
				// the guard lock was not taken first, so refuse rather than
				// take it out of order.
				return &leaveDecisionError{decision: leaveDecision(http.StatusConflict, ErrCodeConflict,
					"your project roles changed while leaving; try again", nil)}
			}
		}

		now := svc.nowFunc()
		var toRemove, owners []*store.RoleBinding
		roles := make([]string, 0, len(bindings))
		for _, lb := range bindings {
			toRemove = append(toRemove, lb.binding)
			if lb.role.Name == store.ProjectRoleOwner {
				owners = append(owners, lb.binding)
			}
			roles = append(roles, lb.role.Name)
		}
		sort.Strings(roles)
		if err := removeProjectBindingsTx(ctx, tx, projectID, now, toRemove, owners); err != nil {
			return err
		}

		res := &LeaveResult{ProjectID: projectID, DryRun: dryRun, RemovedRoles: roles}
		remaining, err := svc.leaveRemainingAccess(ctx, tx, actorID, projectID, now)
		if err != nil {
			return err
		}
		res.RemainingAccess = remaining
		agents, err := leaveRootedAgents(ctx, tx, actorID, projectID)
		if err != nil {
			return err
		}
		res.Agents = agents
		res.revokedTokenIDs = []string{}
		if remaining == leaveRemainingNone {
			// R1: in the same transaction as the binding delete and the
			// audit record; a dry run rolls it back with the rest.
			ids, err := revokeLeaveTokensTx(ctx, tx, actorID, projectID)
			if err != nil {
				return err
			}
			res.revokedTokenIDs = ids
		}
		res.ProjectBoundTokens.Revoked = len(res.revokedTokenIDs)
		res.WillBeHeld = remaining == leaveRemainingNone && res.Agents.Count > 0
		result = res

		if dryRun {
			return errLeaveDryRunRollback
		}
		if err := enqueueMembershipLossForPrincipalTx(ctx, tx, store.RoleBindingPrincipalUser, actorID, projectID,
			store.MembershipLossTriggerMemberLeave, auditActorFromContext(ctx)); err != nil {
			return err
		}
		return svc.createAuditRecord(ctx, tx, &store.MutationAuditRecord{
			MutationType:  "project_member_leave",
			TargetType:    "project_membership",
			TargetID:      projectID,
			BeforeSummary: leaveAuditSummary(actorID, res),
		})
	})
	var groupDerived *leaveGroupDerivedError
	if errors.As(txErr, &groupDerived) {
		return nil, leaveDecision(http.StatusConflict, ErrCodeMembershipGroupDerived, leaveGroupDerivedReason,
			map[string]interface{}{"groups": svc.readableLeaveGroups(ctx, actor, groupDerived.groupIDs)})
	}
	if txErr != nil && !errors.Is(txErr, errLeaveDryRunRollback) {
		return nil, leaveTxDecision(txErr)
	}
	if dryRun {
		return result, nil
	}

	// Post-commit. Access token validation reads the store on every
	// request (ValidateToken, bearerStillValid), so there is no cached
	// validation to invalidate: a revoked token is refused on its next
	// use. The handler publishes the conduit authorization change.
	svc.notifyMembershipLoss()
	svc.logger.Info("project member left via service",
		"project_id", projectID, "actor", actorID,
		"removed_roles", result.RemovedRoles, "remaining_access", result.RemainingAccess,
		"agents", result.Agents.Count, "revoked_tokens", result.ProjectBoundTokens.Revoked)
	return result, nil
}

// revokeLeaveTokensTx is the token step of a leave; a variable so a test
// can inject a fault and prove the leave rolls back with it.
var revokeLeaveTokensTx = revokeUserAccessTokensForUserProjectTx

// leaveTxDecision maps a leave transaction error to a refusal.
func leaveTxDecision(err error) *MembershipDecision {
	var ld *leaveDecisionError
	if errors.As(err, &ld) {
		return ld.decision
	}
	if isLastOwnerError(err) {
		d := lastOwnerDenial()
		d.Details = map[string]interface{}{"guidance": leaveLastOwnerGuidance}
		return d
	}
	if d := constraintAdminLockoutDecision(err); d != nil {
		return d
	}
	return leaveDecision(http.StatusInternalServerError, ErrCodeInternalError, err.Error(), nil)
}

// leaveAuditSummary is the before summary of a project_member_leave record.
func leaveAuditSummary(actorID string, res *LeaveResult) string {
	b, err := json.Marshal(map[string]interface{}{
		"principalId":     actorID,
		"roles":           res.RemovedRoles,
		"remainingAccess": res.RemainingAccess,
		"agentCount":      res.Agents.Count,
		"revokedTokenIds": res.revokedTokenIDs,
	})
	if err != nil {
		return fmt.Sprint(res.RemovedRoles)
	}
	return string(b)
}

// leaveBindingsFromStore returns the caller's removable bindings on the
// project, read through st. A binding leaveBindingClass cannot classify is
// a fail-closed refusal (decision non-nil); a store error is err.
func (svc *ProjectMembershipService) leaveBindingsFromStore(ctx context.Context, st store.Store, userID, projectID string) ([]leaveBinding, *MembershipDecision, error) {
	all, err := st.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	if err != nil {
		return nil, nil, fmt.Errorf("list bindings of the caller: %w", err)
	}
	var out []leaveBinding
	for _, rb := range all {
		if rb.ScopeType != store.RoleScopeProject || rb.ScopeID != projectID {
			continue
		}
		rd, err := st.GetRoleDefinition(ctx, rb.RoleDefinitionID)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve role definition %s: %w", rb.RoleDefinitionID, err)
		}
		switch leaveBindingClassFunc(rb, rd, projectID, userID) {
		case leaveClassRemovable:
			out = append(out, leaveBinding{binding: rb, role: rd})
		case leaveClassExcluded:
		default:
			svc.logger.Warn("project leave refused: binding kind not classified (fail-closed)",
				"project_id", projectID, "user_id", userID, "binding_id", rb.ID)
			return nil, leaveUnclassifiedDecision(), nil
		}
	}
	return out, nil, nil
}

// leaveBindingClassFunc is the classifier LeaveProject uses; a variable so
// a test can inject an unclassified binding (TestProjectLeave_UnclassifiedFailsClosed).
var leaveBindingClassFunc = leaveBindingClass

// leaveGroupRef is one group of a group-derived refusal.
type leaveGroupRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// readableLeaveGroups returns the groups of groupIDs (groups the actor is
// a member of, directly or nested) that the actor may read, sorted by name
// then ID. A group the actor may not read, or that fails to load, is left
// out without a trace. It runs outside any transaction.
func (svc *ProjectMembershipService) readableLeaveGroups(ctx context.Context, actor UserIdentity, groupIDs []string) []leaveGroupRef {
	out := []leaveGroupRef{}
	if svc.authz == nil {
		return out
	}
	for _, gid := range groupIDs {
		g, err := svc.store.GetGroup(ctx, gid)
		if err != nil || g == nil {
			continue
		}
		if !svc.authz.CheckAccess(ctx, actor, groupResource(g), ActionRead).Allowed {
			continue
		}
		out = append(out, leaveGroupRef{ID: g.ID, Name: g.Name})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// activeGroupProjectBindingGroups returns the IDs of the caller's
// effective groups (direct or nested) that hold an active, membership
// conferring binding on the project, sorted.
func activeGroupProjectBindingGroups(ctx context.Context, st store.Store, userID, projectID string, now time.Time) ([]string, error) {
	groupIDs, err := st.GetEffectiveGroups(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("effective groups of the caller: %w", err)
	}
	if len(groupIDs) == 0 {
		return nil, nil
	}
	refs := make([]store.PrincipalRef, 0, len(groupIDs))
	for _, gid := range groupIDs {
		refs = append(refs, store.PrincipalRef{Type: store.RoleBindingPrincipalGroup, ID: gid})
	}
	bindings, err := st.ListRoleBindingsForPrincipals(ctx, refs, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("group bindings of the caller: %w", err)
	}
	seen := map[string]bool{}
	var out []string
	for _, rb := range bindings {
		if rb.ScopeType != store.RoleScopeProject || rb.ScopeID != projectID || !isBindingActive(rb, now) {
			continue
		}
		rd, err := st.GetRoleDefinition(ctx, rb.RoleDefinitionID)
		if err != nil {
			return nil, fmt.Errorf("resolve role definition %s: %w", rb.RoleDefinitionID, err)
		}
		if !rd.ConfersProjectMembership() || seen[rb.PrincipalID] {
			continue
		}
		seen[rb.PrincipalID] = true
		out = append(out, rb.PrincipalID)
	}
	sort.Strings(out)
	return out, nil
}

// leaveRemainingAccess computes the caller's access after the bindings are
// deleted, on the transaction's post-state: none when not admitted, group
// when an active group binding on the project admits them, hub otherwise
// (hub-level authority).
func (svc *ProjectMembershipService) leaveRemainingAccess(ctx context.Context, tx store.Store, userID, projectID string, now time.Time) (string, error) {
	groups, err := activeGroupProjectBindingGroups(ctx, tx, userID, projectID, now)
	if err != nil {
		return "", err
	}
	admitted := len(groups) > 0
	if svc.userAdmitted != nil {
		if admitted, err = svc.userAdmitted(ctx, tx, userID, projectID); err != nil {
			return "", fmt.Errorf("admission after leave: %w", err)
		}
	}
	switch {
	case !admitted:
		return leaveRemainingNone, nil
	case len(groups) > 0:
		return leaveRemainingGroup, nil
	default:
		return leaveRemainingHub, nil
	}
}

// leaveRootedAgents lists the agents userID roots in projectID with the
// walk and bounds the membership loss processor uses, so the count is the
// number of agents the processor would hold. Items are capped at
// leaveAgentItemsMax (R4); a walk that reaches its bound sets Truncated.
func leaveRootedAgents(ctx context.Context, st store.Store, userID, projectID string) (leaveAgents, error) {
	out := leaveAgents{Items: []leaveAgentItem{}}
	if !isUUID(userID) || !isUUID(projectID) {
		return out, nil
	}
	res, err := st.ListDelegationDescendants(ctx, store.DescendantQuery{
		RootType:           store.DelegationPrincipalUser,
		RootID:             userID,
		ProjectID:          projectID,
		IncludeSoftDeleted: true,
		LegacyLinks:        true,
		SkipHeldForRoot:    true,
		MaxDepth:           descendantQueryBounds.MaxDepth,
		MaxNodes:           descendantQueryBounds.MaxNodes,
	})
	if err != nil && !errors.Is(err, store.ErrDescendantLimit) {
		return out, fmt.Errorf("agents rooted by the caller: %w", err)
	}
	out.Count = len(res.Agents)
	out.Truncated = err != nil || out.Count > leaveAgentItemsMax
	for _, ref := range res.Agents {
		if len(out.Items) == leaveAgentItemsMax {
			break
		}
		item := leaveAgentItem{ID: ref.AgentID}
		if a, gerr := st.GetAgent(ctx, ref.AgentID); gerr == nil && a != nil {
			item.Name, item.Phase = a.Name, a.Phase
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// removeProjectBindingsTx is the governed remove step shared by
// RemoveMember (project.membership.remove) and LeaveProject
// (project.membership.leave), so their guards cannot drift. The caller has
// taken the admin-guard lock (when a binding carries constraint admin) and
// the project membership lock, and re-read the bindings under them.
// ownerBindings are the project-owner bindings among bindings. In order:
// whether a usable owner is being removed is read on the pre-state, the
// constraint-admin lockout guard runs, every binding is deleted, and the
// last-owner rule is checked on the post-state. Any error rolls the
// caller's transaction back.
func removeProjectBindingsTx(ctx context.Context, tx store.Store, projectID string, now time.Time, bindings, ownerBindings []*store.RoleBinding) error {
	removedUsable, err := anyUsableOwnerBinding(ctx, tx, ownerBindings, now)
	if err != nil {
		return fmt.Errorf("cannot verify usable owner: %w", err)
	}
	if err := checkRoleBindingRemovalAdminLockout(ctx, tx, now, bindings...); err != nil {
		return err
	}
	for _, b := range bindings {
		if err := tx.DeleteRoleBinding(ctx, b.ID); err != nil {
			return err
		}
	}
	if len(ownerBindings) > 0 {
		return enforceOwnerRemovalTx(ctx, tx, projectID, now, removedUsable)
	}
	return nil
}

// leaveCapabilities returns the caller's leave capabilities on the
// project: canLeave when a leave would currently remove at least one
// binding, and otherwise the reason it is blocked (last_owner,
// group_derived) when there is one. The dry run is the final authority.
// Errors fail closed (cannot leave, no reason).
func (svc *ProjectMembershipService) leaveCapabilities(ctx context.Context, userID, projectID string) (bool, string) {
	bindings, decision, err := svc.leaveBindingsFromStore(ctx, svc.store, userID, projectID)
	if err != nil || decision != nil {
		return false, ""
	}
	now := svc.nowFunc()
	if len(bindings) == 0 {
		groups, err := activeGroupProjectBindingGroups(ctx, svc.store, userID, projectID, now)
		if err == nil && len(groups) > 0 {
			return false, leaveBlockedGroupDerived
		}
		return false, ""
	}
	var owners []*store.RoleBinding
	for _, lb := range bindings {
		if lb.role.Name == store.ProjectRoleOwner {
			owners = append(owners, lb.binding)
		}
	}
	if len(owners) == 0 {
		return true, ""
	}
	removesUsable, err := anyUsableOwnerBinding(ctx, svc.store, owners, now)
	if err != nil {
		return false, ""
	}
	if removesUsable {
		other, err := projectHasUsableOwner(ctx, svc.store, projectID, now, userID)
		if err != nil {
			return false, ""
		}
		if !other {
			return false, leaveBlockedLastOwner
		}
		return true, ""
	}
	n, err := projectOwnerBindingCount(ctx, svc.store, projectID, userID)
	if err != nil {
		return false, ""
	}
	if n == 0 {
		return false, leaveBlockedLastOwner
	}
	return true, ""
}
