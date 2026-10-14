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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Project removal for the test identity purge (ptone/scion#4240, Phase 3b).
//
// Delete (project_deletion_service.go) admits only a session or dev
// credential of a direct owner or super-admin. The purge runs for a test
// identity's issuer, or as the expiry reaper, with neither. It removes a
// project through purgeFixtureProject instead: an unexported method that
// takes no ProjectDeleteRequest, no actor and no credential, and is called
// only from the purge (fixture_purge.go). Nothing decoded from a request
// can select it; TestFixturePurge_ProjectPathUnreachableFromRequests keeps
// it that way.

// Mutation audit type for a stamped project the purge keeps.
const testIdentityPurgeProjectKeptMutation = "test_identity_purge_project_kept"

// Reasons a stamped project is kept, or left for a later run.
const (
	fixtureProjectKeptOtherOwner     = "non_fixture_owner"
	fixtureProjectKeptOtherResources = "non_fixture_resources"
	fixtureProjectPendingAgents      = "fixture_agents_remain"
)

// fixtureProjectPurgeDecisionHook is a test seam: when non-nil it runs
// inside purgeFixtureProject's transaction, under the project row lock,
// after the ownership and contents reads and before the keep or delete. A
// test uses it to start a concurrent owner add at exactly that point. Nil
// in production.
var fixtureProjectPurgeDecisionHook func(projectID string)

// fixtureProjectOutcome is what purgeFixtureProject did with one project.
type fixtureProjectOutcome struct {
	// Project is the row as read under the lock (nil when it was gone).
	Project *store.Project
	// Gone: the project no longer exists; nothing was done.
	Gone bool
	// Deleted: the project and its project-scoped state were removed.
	Deleted bool
	// Summary is the cascade of a deleted project.
	Summary CascadeSummary
	// Kept: the project stays (KeptReason). The root identity's own role
	// bindings on it were removed (BindingsRemoved); Ownerless is true when
	// the removed bindings included the only owner binding.
	Kept            bool
	KeptReason      string
	BindingsRemoved int
	Ownerless       bool
	// Pending: agents stamped with the root are still in the project (a
	// delete that did not finish); a later run removes the project.
	Pending bool
}

// errFixtureProjectNotStamped refuses a project whose stamp does not name
// the purge's root. The purge selects projects by their stamp, so this is
// only reached if the row changed under it; it is never deleted.
var errFixtureProjectNotStamped = errors.New("project is not stamped with this test identity")

// purgeFixtureProject removes the project stamped with basis.RootID, or
// keeps it, in ONE transaction that holds the project row lock
// (LockProjectForMembership: SELECT ... FOR UPDATE on PostgreSQL, the
// single writer on SQLite) from the ownership check to the delete. Every
// membership change takes the same lock, so an owner added concurrently is
// either seen here (the project is kept) or added after the delete (and
// fails on the missing project); a project with a non-fixture owner is
// never deleted.
//
// The project is kept when any role binding other than the root's own
// names the owner role, or when it holds an agent, schedule or group that
// is not stamped with the root. A kept project loses only the
// root identity's own role bindings, and one
// test_identity_purge_project_kept audit row records them. The project is
// left for a later run while stamped agents remain in it. Otherwise its
// agent rows are locked, its project-scoped state is cascaded as Delete
// does (cascadeSecurityState), the row is deleted, and one project_delete
// audit row carries the provenance basis. External effects (broker and
// filesystem cleanup) are the caller's, after a successful return.
func (svc *ProjectDeletionService) purgeFixtureProject(ctx context.Context, projectID string, basis fixturePurgeBasis) (fixtureProjectOutcome, error) {
	var out fixtureProjectOutcome
	err := svc.store.WithTx(ctx, func(tx store.Store) error {
		out = fixtureProjectOutcome{}
		if err := tx.LockProjectForMembership(ctx, projectID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				out.Gone = true
				return nil
			}
			return fmt.Errorf("lock project: %w", err)
		}
		project, err := tx.GetProject(ctx, projectID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				out.Gone = true
				return nil
			}
			return fmt.Errorf("read project: %w", err)
		}
		out.Project = project
		if project.Fixture.RootID != basis.RootID {
			return errFixtureProjectNotStamped
		}

		ownerRole, err := tx.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
		if err != nil {
			return fmt.Errorf("read project owner role: %w", err)
		}
		bindings, err := tx.ListRoleBindingsForScope(ctx, store.RoleScopeProject, projectID)
		if err != nil {
			return fmt.Errorf("list project role bindings: %w", err)
		}
		var rootBindings []*store.RoleBinding
		otherOwner, rootOwner := false, false
		for _, b := range bindings {
			if b.PrincipalType == store.RoleBindingPrincipalUser && b.PrincipalID == basis.RootID {
				rootBindings = append(rootBindings, b)
				if b.RoleDefinitionID == ownerRole.ID {
					rootOwner = true
				}
				continue
			}
			// Any other principal holding the owner role, active or not,
			// keeps the project.
			if b.RoleDefinitionID == ownerRole.ID {
				otherOwner = true
			}
		}

		otherResources, fixtureAgents, err := fixtureProjectContents(ctx, tx, projectID, basis.RootID)
		if err != nil {
			return err
		}
		if h := fixtureProjectPurgeDecisionHook; h != nil {
			h(projectID)
		}

		if otherOwner || otherResources {
			out.Kept = true
			out.KeptReason = fixtureProjectKeptOtherOwner
			if !otherOwner {
				out.KeptReason = fixtureProjectKeptOtherResources
			}
			for _, b := range rootBindings {
				if err := tx.DeleteRoleBinding(ctx, b.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("remove the test identity's project binding: %w", err)
				}
				out.BindingsRemoved++
			}
			out.Ownerless = rootOwner && !otherOwner
			if out.BindingsRemoved == 0 {
				return nil
			}
			after, _ := json.Marshal(struct {
				Reason          string            `json:"reason"`
				BindingsRemoved int               `json:"bindings_removed"`
				Ownerless       bool              `json:"ownerless"`
				ProvenanceBasis fixturePurgeBasis `json:"provenance_basis"`
			}{out.KeptReason, out.BindingsRemoved, out.Ownerless, basis})
			record := &store.MutationAuditRecord{
				MutationType:  testIdentityPurgeProjectKeptMutation,
				TargetType:    "project",
				TargetID:      project.ID,
				AfterSummary:  string(after),
				CorrelationID: basis.RunID,
			}
			return svc.createFixturePurgeAudit(ctx, tx, record)
		}
		if fixtureAgents {
			out.Pending = true
			return nil
		}

		// Lock the project's agent rows before the cascade, as Delete does
		// (its step 8 explains the lock order).
		if err := tx.LockProjectAgents(ctx, projectID); err != nil {
			return fmt.Errorf("lock project agents: %w", err)
		}
		summary, err := svc.cascadeSecurityState(ctx, tx, projectID)
		if err != nil {
			return fmt.Errorf("cascade security state: %w", err)
		}
		if err := tx.DeleteProject(ctx, projectID); err != nil {
			return fmt.Errorf("delete project: %w", err)
		}
		after, _ := json.Marshal(struct {
			CascadeSummary
			ProvenanceBasis fixturePurgeBasis `json:"provenance_basis"`
		}{summary, basis})
		record := &store.MutationAuditRecord{
			MutationType: "project_delete",
			TargetType:   "project",
			TargetID:     project.ID,
			BeforeSummary: marshalDeletionAuditJSON(map[string]string{
				"project_id":   project.ID,
				"project_name": project.Name,
				"project_slug": project.Slug,
				"owner_id":     project.OwnerID,
			}),
			AfterSummary:  string(after),
			CorrelationID: basis.RunID,
		}
		if err := svc.createFixturePurgeAudit(ctx, tx, record); err != nil {
			return fmt.Errorf("audit record: %w", err)
		}
		out.Deleted = true
		out.Summary = summary
		return nil
	})
	if err != nil {
		return fixtureProjectOutcome{}, err
	}
	return out, nil
}

// fixtureProjectScanLimit bounds each per-project read of
// fixtureProjectContents. Any row it finds settles the answer except a
// stamped one, so a project with more stamped rows than this is treated as
// pending, never as empty.
const fixtureProjectScanLimit = 200

// fixtureProjectContents reports, reading through tx, whether projectID
// holds an agent (soft-deleted included), schedule or group that is not
// stamped with rootID (other), and whether it still holds agents stamped
// with rootID (fixtureAgents). The project's implicit agents and members
// groups are created with the project and carry its stamp, so they count
// as stamped.
func fixtureProjectContents(ctx context.Context, tx store.Store, projectID, rootID string) (other, fixtureAgents bool, err error) {
	agents, err := tx.ListAgents(ctx, store.AgentFilter{ProjectID: projectID, IncludeDeleted: true},
		store.ListOptions{Limit: fixtureProjectScanLimit, SkipTotalCount: true})
	if err != nil {
		return false, false, fmt.Errorf("list project agents: %w", err)
	}
	for _, a := range agents.Items {
		if a.Fixture.RootID != rootID {
			return true, false, nil
		}
		fixtureAgents = true
	}
	if agents.NextCursor != "" {
		// More rows than were read: never treat the project as empty.
		fixtureAgents = true
	}
	schedules, err := tx.ListSchedules(ctx, store.ScheduleFilter{ProjectID: projectID},
		store.ListOptions{Limit: fixtureProjectScanLimit, SkipTotalCount: true})
	if err != nil {
		return false, false, fmt.Errorf("list project schedules: %w", err)
	}
	for _, sc := range schedules.Items {
		if sc.Fixture.RootID != rootID {
			return true, fixtureAgents, nil
		}
	}
	groups, err := tx.ListGroups(ctx, store.GroupFilter{ProjectID: projectID},
		store.ListOptions{Limit: fixtureProjectScanLimit, SkipTotalCount: true})
	if err != nil {
		return false, false, fmt.Errorf("list project groups: %w", err)
	}
	for _, g := range groups.Items {
		if g.Fixture.RootID != rootID {
			return true, fixtureAgents, nil
		}
	}
	return false, fixtureAgents, nil
}

// createFixturePurgeAudit writes a purge audit row in tx. The expiry
// reaper has no request identity, so a row with no actor is attributed to
// the hub (applyHubActorFallback) rather than refused by the store.
func (svc *ProjectDeletionService) createFixturePurgeAudit(ctx context.Context, tx store.Store, record *store.MutationAuditRecord) error {
	auditActorFromContext(ctx).ApplyActor(record)
	applyHubActorFallback(record)
	return svc.createAuditRecord(ctx, tx, record)
}
