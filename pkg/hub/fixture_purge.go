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
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Test identity purge (ptone/scion#4240, Phase 3b).
//
// DELETE /api/v1/test-identities/{id}?purge=true and the expiry reaper
// (test_identity_reaper.go) remove a hub test identity together with what it
// created, directly or through its agents: every agent, schedule, group and
// project whose creation-time provenance stamp (fixture_provenance.go) has
// fixture_root_id == the identity. The identity ID is the only input. The
// targets come from the stamp alone, never from the request, from owner_id
// or from created_by, so a resource without the stamp is never touched; an
// agent the identity owns that carries no stamp is reported as unproven.
//
// Order: agents (deepest ancestry first, broker files removed, through the
// delete engine), then schedules and groups, then projects (kept when they
// have another owner or other resources: purgeFixtureProject), then the
// identity itself (the same cascade as the 2c delete). Each removal writes
// one mutation audit row whose summary carries provenance_basis, the stamp
// it relied on; the identity's row is test_identity_purge (request) or
// test_identity_expire (reaper), written in the identity delete's
// transaction. A delete that does not finish (in_doubt on the broker, a
// broker that is not reachable) is reported, the identity is kept, the purge
// is incomplete, and running it again finishes it. A purge with nothing left
// to do reports empty counts.

// Mutation audit types of the purge.
const (
	testIdentityPurgeMutation  = "test_identity_purge"
	testIdentityExpireMutation = "test_identity_expire"
	// fixturePurgeGroupDeleteMutation records a group the purge removed.
	fixturePurgeGroupDeleteMutation = "group_delete"
)

// Executor kinds recorded on the purge's audit rows.
const (
	fixturePurgeExecutorRequest = "system:test_identity_purge"
	fixturePurgeExecutorReaper  = "system:test_identity_reaper"
)

// fixturePurgeBasisName is provenance_basis.basis: the column the purge
// selected the resource by.
const fixturePurgeBasisName = "fixture_root_id"

// fixturePurgeListLimit bounds each kind of stamped resource one run
// handles; a run that hits it is incomplete and the next run continues.
const fixturePurgeListLimit = 1000

// fixturePurgeAgentWait bounds how long a run waits for one agent's delete
// engine before reporting the delete as still running. A variable so tests
// can shorten it.
var fixturePurgeAgentWait = 2 * deleteDispatchBudget

// fixturePurgeBasis is the provenance a purge delete relied on. It is the
// provenance_basis object in each per-resource audit summary.
type fixturePurgeBasis struct {
	RootID   string `json:"fixture_root_id"`
	IssuerID string `json:"fixture_issuer_id"`
	Basis    string `json:"basis"`
	// RunID is shared by every audit row of one purge run (also its
	// correlation ID when the request has none).
	RunID string `json:"purge_run_id"`
	// Trigger is test_identity_purge or test_identity_expire.
	Trigger string `json:"trigger"`
}

// fixturePurgeBasisKey is the context key of the basis. It is unexported
// and set only by purgeTestIdentity.
type fixturePurgeBasisKey struct{}

func withFixturePurgeBasis(ctx context.Context, b fixturePurgeBasis) context.Context {
	return context.WithValue(ctx, fixturePurgeBasisKey{}, b)
}

// fixturePurgeBasisFor returns the purge basis on ctx when the resource's
// own stamp names the same root, and nil otherwise (an ordinary delete, or
// a resource the purge did not select by its stamp).
func fixturePurgeBasisFor(ctx context.Context, stamp store.FixtureProvenance) *fixturePurgeBasis {
	b, ok := ctx.Value(fixturePurgeBasisKey{}).(fixturePurgeBasis)
	if !ok || b.RootID == "" || stamp.RootID != b.RootID {
		return nil
	}
	return &b
}

// TestIdentityPurgeCounts counts what one purge run removed.
type TestIdentityPurgeCounts struct {
	Agents    int `json:"agents"`
	Schedules int `json:"schedules"`
	Groups    int `json:"groups"`
	Projects  int `json:"projects"`
}

// TestIdentityPurgeItem is one resource the purge did not remove.
type TestIdentityPurgeItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	ProjectID string `json:"projectId,omitempty"`
	// Status is in_doubt, failed, deleting, broker_unavailable, pending or
	// unproven (see the report fields).
	Status string `json:"status"`
	// Reason is the delete failure code or a short reason.
	Reason string `json:"reason,omitempty"`
}

// TestIdentityPurgeKeptProject is a stamped project the purge kept.
type TestIdentityPurgeKeptProject struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
	// BindingsRemoved is the number of the identity's own role bindings
	// removed from the project; Ownerless is true when that left the
	// project without an owner binding.
	BindingsRemoved int  `json:"bindingsRemoved"`
	Ownerless       bool `json:"ownerless,omitempty"`
}

// TestIdentityPurgeReport is the result of a purge run.
type TestIdentityPurgeReport struct {
	IdentityID string `json:"identityId"`
	// Complete is true when nothing stamped with the identity is left to
	// remove (kept projects aside) and the identity is gone.
	Complete bool `json:"complete"`
	// IdentityDeleted is true when this run deleted the identity's row.
	IdentityDeleted bool                    `json:"identityDeleted"`
	Deleted         TestIdentityPurgeCounts `json:"deleted"`
	// NotDone lists stamped resources whose removal did not finish in
	// this run; running the purge again retries them.
	NotDone []TestIdentityPurgeItem `json:"notDone,omitempty"`
	// Unproven lists agents tied to the identity by owner, creator or
	// ancestry that do not carry its stamp. The purge never removes them;
	// they need a real administrator.
	Unproven []TestIdentityPurgeItem `json:"unproven,omitempty"`
	// UnprovenTruncated is true when more unproven agents exist than
	// Unproven lists.
	UnprovenTruncated bool `json:"unprovenTruncated,omitempty"`
	// KeptProjects lists stamped projects kept because of another owner or
	// other resources.
	KeptProjects []TestIdentityPurgeKeptProject `json:"keptProjects,omitempty"`
	// IdentityKeptReason says why the identity row was not deleted.
	IdentityKeptReason string `json:"identityKeptReason,omitempty"`
	// IdentityKeptDetails carries the blocking agents or projects.
	IdentityKeptDetails any `json:"identityKeptDetails,omitempty"`
	// Truncated is true when more stamped resources exist than one run
	// handles.
	Truncated bool `json:"truncated,omitempty"`
}

// emptyTestIdentityPurgeReport is the answer when nothing is selected: an
// already-purged identity, an ID that is not a test identity, or one the
// caller may not purge. They are indistinguishable.
func emptyTestIdentityPurgeReport(id string) *TestIdentityPurgeReport {
	return &TestIdentityPurgeReport{IdentityID: id, Complete: true}
}

// fixturePurgeRun is one purge of one test identity.
type fixturePurgeRun struct {
	rootID   string
	issuerID string
	// user is the identity's row, nil when it is already gone (only its
	// stamped leftovers are removed then).
	user *store.User
	// trigger is testIdentityPurgeMutation or testIdentityExpireMutation.
	trigger string
	// executor is fixturePurgeExecutorRequest or fixturePurgeExecutorReaper.
	executor string
	// requestedBy is recorded as the agent deletes' requester.
	requestedBy string
	// actor attributes the identity-level audit row.
	actor AuditActor
}

// purgeTestIdentity runs one purge and returns its report. A returned
// error is a store failure that stopped the run; the report is still the
// partial one.
func (s *Server) purgeTestIdentity(ctx context.Context, run fixturePurgeRun) (*TestIdentityPurgeReport, error) {
	report := &TestIdentityPurgeReport{IdentityID: run.rootID}
	runID := logging.RequestIDFromContext(ctx)
	if runID == "" {
		runID = api.NewUUID()
	}
	basis := fixturePurgeBasis{
		RootID:   run.rootID,
		IssuerID: run.issuerID,
		Basis:    fixturePurgeBasisName,
		RunID:    runID,
		Trigger:  run.trigger,
	}
	ctx = withFixturePurgeBasis(ctx, basis)
	ctx = ContextWithExecutor(ctx, ExecutorContext{Kind: run.executor, ID: "test_identity:" + run.rootID})
	if run.actor.CorrelationID == "" {
		run.actor.CorrelationID = runID
	}
	run.actor.ExecutorKind, run.actor.ExecutorID = run.executor, "test_identity:"+run.rootID

	set, err := s.store.ListFixtureRootedResources(ctx, run.rootID, fixturePurgeListLimit)
	if err != nil {
		return report, fmt.Errorf("list stamped resources: %w", err)
	}
	report.Truncated = set.Truncated

	// 1. Agents, deepest ancestry first, so a sub-agent goes before the
	// agent that created it. A scheduled child has no ancestry.
	agents := append([]store.Agent(nil), set.Agents...)
	sort.SliceStable(agents, func(i, j int) bool {
		if len(agents[i].Ancestry) != len(agents[j].Ancestry) {
			return len(agents[i].Ancestry) > len(agents[j].Ancestry)
		}
		return agents[i].ID < agents[j].ID
	})
	for i := range agents {
		a := &agents[i]
		if a.Fixture.RootID != run.rootID {
			continue
		}
		deleted, item := s.purgeFixtureAgent(ctx, a, run.requestedBy)
		if deleted {
			report.Deleted.Agents++
		} else if item != nil {
			report.NotDone = append(report.NotDone, *item)
		}
	}

	// 2. Schedules.
	for i := range set.Schedules {
		sc := &set.Schedules[i]
		deleted, err := s.purgeFixtureSchedule(ctx, sc, basis)
		if err != nil {
			slog.ErrorContext(ctx, "test identity purge: schedule delete failed", "schedule_id", sc.ID, "error", err)
			report.NotDone = append(report.NotDone, TestIdentityPurgeItem{Type: "schedule", ID: sc.ID, ProjectID: sc.ProjectID, Status: "failed", Reason: "store_error"})
			continue
		}
		if deleted {
			report.Deleted.Schedules++
		}
	}

	// 3. Groups. A group scoped to a project stamped with the same root is
	// handled with that project in step 4: removed by the project delete's
	// cascade (counted in that project's project_delete row, not here), or,
	// when the project is kept, removed then unless it is the project's own
	// agents or members group.
	stampedProjects := map[string]bool{}
	for _, p := range set.Projects {
		stampedProjects[p.ID] = true
	}
	projectGroups := map[string][]*store.Group{}
	for i := range set.Groups {
		g := &set.Groups[i]
		if g.ProjectID != "" && stampedProjects[g.ProjectID] {
			projectGroups[g.ProjectID] = append(projectGroups[g.ProjectID], g)
			continue
		}
		deleted, err := s.purgeFixtureGroup(ctx, g, basis)
		if err != nil {
			slog.ErrorContext(ctx, "test identity purge: group delete failed", "group_id", g.ID, "error", err)
			report.NotDone = append(report.NotDone, TestIdentityPurgeItem{Type: "group", ID: g.ID, ProjectID: g.ProjectID, Status: "failed", Reason: "store_error"})
			continue
		}
		if deleted {
			report.Deleted.Groups++
		}
	}

	// 4. Projects.
	for i := range set.Projects {
		p := &set.Projects[i]
		if p.Fixture.RootID != run.rootID {
			continue
		}
		if kept := s.purgeFixtureProjectStep(ctx, p, basis, report); kept {
			for _, g := range projectGroups[p.ID] {
				if isProjectImplicitGroup(g) {
					continue
				}
				deleted, err := s.purgeFixtureGroup(ctx, g, basis)
				if err != nil {
					slog.ErrorContext(ctx, "test identity purge: group delete failed", "group_id", g.ID, "error", err)
					report.NotDone = append(report.NotDone, TestIdentityPurgeItem{Type: "group", ID: g.ID, ProjectID: g.ProjectID, Status: "failed", Reason: "store_error"})
					continue
				}
				if deleted {
					report.Deleted.Groups++
				}
			}
		}
	}

	// 5. Agents tied to the identity without its stamp: never removed,
	// always reported.
	unproven, unprovenTruncated, err := s.unprovenFixtureAgents(ctx, run.rootID)
	if err != nil {
		return report, fmt.Errorf("list unproven agents: %w", err)
	}
	report.Unproven = unproven
	report.UnprovenTruncated = unprovenTruncated

	// 6. The identity.
	if run.user == nil {
		report.Complete = len(report.NotDone) == 0 && !report.Truncated && len(report.Unproven) == 0
		return report, nil
	}
	if len(report.NotDone) > 0 || report.Truncated {
		report.IdentityKeptReason = "resources_not_done"
		return report, nil
	}
	if err := s.purgeFixtureUser(ctx, run, report); err != nil {
		return report, err
	}
	report.Complete = report.IdentityDeleted
	return report, nil
}

// purgeFixtureAgent removes one stamped agent and reports whether this run
// deleted it; when it did not, item says why (nil when the agent was
// already gone). A live agent goes through the delete engine with broker
// files and branch removed and no soft-delete retention; a soft-deleted
// one, already torn down on its broker, has its row removed.
func (s *Server) purgeFixtureAgent(ctx context.Context, listed *store.Agent, requestedBy string) (bool, *TestIdentityPurgeItem) {
	notDone := func(status, reason string) (bool, *TestIdentityPurgeItem) {
		return false, &TestIdentityPurgeItem{Type: "agent", ID: listed.ID, ProjectID: listed.ProjectID, Status: status, Reason: reason}
	}
	a, err := s.store.GetAgent(ctx, listed.ID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return notDone("failed", "store_error")
	}
	if a.Fixture.RootID != listed.Fixture.RootID {
		// The stamp is immutable; a differing read is not this agent.
		return notDone("failed", "stamp_mismatch")
	}

	if !a.DeletedAt.IsZero() {
		// Soft-deleted: remove the row, only while it is still
		// soft-deleted (a restore in between wins).
		n, err := s.finalizeAgentDeletion(ctx, a.ID,
			store.DeletionPredicate{States: []string{store.DeletionStateNone}, DeletedAtNotNull: true},
			store.DeletionFinalizeHard, store.DeletionFields{})
		if err != nil {
			slog.ErrorContext(ctx, "test identity purge: removing a soft-deleted agent failed", "agent_id", a.ID, "error", err)
			return notDone("failed", store.DeletionCodeFinalizeFailed)
		}
		if n == 0 {
			if _, gerr := s.store.GetAgent(ctx, a.ID); errors.Is(gerr, store.ErrNotFound) {
				return false, nil
			}
			return notDone("pending", "changed_concurrently")
		}
		return true, nil
	}

	if deletionActive(a) {
		return notDone("deleting", "")
	}
	createdNoLaunch := a.Phase == string(state.PhaseCreated) && a.LaunchState != store.LaunchStateActive
	finalizing := a.DeletionState == store.DeletionStateFinalizing
	if !isManagedAgentRuntime(a.Runtime) && !createdNoLaunch && !finalizing && !s.brokerReachable(ctx, a) {
		return notDone("broker_unavailable", "")
	}
	plan, err := s.claimAgentDeletion(ctx, a.ID, agentDeleteParams{
		deleteFiles:  true,
		removeBranch: true,
		hard:         true,
		requestedBy:  requestedBy,
	})
	if err != nil {
		return notDone("failed", "store_error")
	}
	if plan == nil {
		if _, gerr := s.store.GetAgent(ctx, a.ID); errors.Is(gerr, store.ErrNotFound) {
			return false, nil
		}
		return notDone("deleting", "")
	}
	s.events.PublishAgentStatus(ctx, plan.snapshot)
	done := s.runAgentDeletion(ctx, plan)
	timer := time.NewTimer(fixturePurgeAgentWait)
	defer timer.Stop()
	select {
	case out := <-done:
		switch out.kind {
		case deletionOutcomeDeleted:
			return true, nil
		case deletionOutcomeFailed:
			status := "failed"
			if out.code == store.DeletionCodeInDoubt {
				status = store.DeletionCodeInDoubt
			}
			return notDone(status, out.code)
		default:
			return notDone("deleting", "")
		}
	case <-timer.C:
		return notDone("deleting", "")
	}
}

// purgeFixtureSchedule removes one stamped schedule with its audit row.
func (s *Server) purgeFixtureSchedule(ctx context.Context, sc *store.Schedule, basis fixturePurgeBasis) (bool, error) {
	b := fixturePurgeBasisFor(ctx, sc.Fixture)
	if b == nil {
		return false, nil
	}
	record := newScheduleAudit(ctx, mutationTypeScheduleDelete, sc.ID)
	record.AfterSummary = fixturePurgeSummary(map[string]any{"project_id": sc.ProjectID, "name": sc.Name}, *b)
	if record.CorrelationID == "" {
		record.CorrelationID = basis.RunID
	}
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		if err := tx.DeleteSchedule(ctx, sc.ID); err != nil {
			return err
		}
		return tx.CreateMutationAudit(ctx, record)
	})
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// purgeFixtureGroup removes one stamped group with its audit row, then
// re-evaluates the project access of the users who were in it, as the
// group API's delete does.
func (s *Server) purgeFixtureGroup(ctx context.Context, g *store.Group, basis fixturePurgeBasis) (bool, error) {
	b := fixturePurgeBasisFor(ctx, g.Fixture)
	if b == nil {
		return false, nil
	}
	lossUsers, err := transitiveGroupMemberUsers(ctx, s.store, g.ID)
	if err != nil {
		return false, err
	}
	record := &store.MutationAuditRecord{
		MutationType:  fixturePurgeGroupDeleteMutation,
		TargetType:    "group",
		TargetID:      g.ID,
		BeforeSummary: marshalDeletionAuditJSON(map[string]string{"group_id": g.ID, "slug": g.Slug, "project_id": g.ProjectID}),
		AfterSummary:  fixturePurgeSummary(nil, *b),
		Timestamp:     time.Now(),
		CorrelationID: basis.RunID,
	}
	auditActorFromContext(ctx).ApplyActor(record)
	applyHubActorFallback(record)
	err = s.store.WithTx(ctx, func(tx store.Store) error {
		if err := tx.DeleteGroup(ctx, g.ID); err != nil {
			return err
		}
		return tx.CreateMutationAudit(ctx, record)
	})
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	s.enqueueGroupChangeLoss(ctx, lossUsers)
	s.publishConduitAuthzChanged(conduitAuthzMatch{})
	return true, nil
}

// purgeFixtureProjectStep removes or keeps one stamped project
// (purgeFixtureProject) and records the result in report. A removed
// project's external effects run after its transaction commits, as for
// the project API's delete.
func (s *Server) purgeFixtureProjectStep(ctx context.Context, p *store.Project, basis fixturePurgeBasis, report *TestIdentityPurgeReport) (kept bool) {
	if s.deletionService == nil {
		report.NotDone = append(report.NotDone, TestIdentityPurgeItem{Type: "project", ID: p.ID, Status: "failed", Reason: "deletion_service_unavailable"})
		return false
	}
	effects := s.preEnumerateDeletionEffects(ctx, p.ID)
	out, err := s.deletionService.purgeFixtureProject(ctx, p.ID, basis)
	if err != nil {
		slog.ErrorContext(ctx, "test identity purge: project step failed", "project_id", p.ID, "error", err)
		report.NotDone = append(report.NotDone, TestIdentityPurgeItem{Type: "project", ID: p.ID, Status: "failed", Reason: "store_error"})
		return false
	}
	switch {
	case out.Gone:
	case out.Deleted:
		report.Deleted.Projects++
		s.executePostDeletionEffects(ctx, p.ID, out.Project, effects)
	case out.Kept:
		report.KeptProjects = append(report.KeptProjects, TestIdentityPurgeKeptProject{
			ID: p.ID, Reason: out.KeptReason, BindingsRemoved: out.BindingsRemoved, Ownerless: out.Ownerless,
		})
		if out.BindingsRemoved > 0 {
			s.publishConduitAuthzChanged(conduitAuthzMatch{UserID: basis.RootID})
		}
	case out.Pending:
		report.NotDone = append(report.NotDone, TestIdentityPurgeItem{Type: "project", ID: p.ID, Status: "pending", Reason: fixtureProjectPendingAgents})
	}
	return out.Kept
}

// isProjectImplicitGroup reports whether g is a project's own agents or
// members group, which the hub creates with the project and which stays
// with a kept project.
func isProjectImplicitGroup(g *store.Group) bool {
	return g.GroupType == store.GroupTypeProjectAgents || store.IsProjectMembersGroup(g)
}

// unprovenAgentsLimit bounds the unproven agents one report lists.
const unprovenAgentsLimit = 200

// unprovenFixtureAgents lists agents tied to rootID by owner, ancestry or
// creator (the links the user delete guard follows) whose stamp does not
// name rootID. They are reported, never removed. truncated is true when a
// link had more agents than one page holds.
func (s *Server) unprovenFixtureAgents(ctx context.Context, rootID string) (out []TestIdentityPurgeItem, truncated bool, err error) {
	seen := map[string]bool{}
	for _, filter := range []store.AgentFilter{
		{OwnerID: rootID, IncludeDeleted: true},
		{AncestorID: rootID, IncludeDeleted: true},
		{CreatedBy: rootID, IncludeDeleted: true},
	} {
		page, err := s.store.ListAgents(ctx, filter, store.ListOptions{Limit: unprovenAgentsLimit, SkipTotalCount: true})
		if err != nil {
			return nil, false, err
		}
		if page.NextCursor != "" || len(page.Items) >= unprovenAgentsLimit {
			truncated = true
		}
		for _, a := range page.Items {
			if seen[a.ID] || a.Fixture.RootID == rootID {
				continue
			}
			seen[a.ID] = true
			out = append(out, TestIdentityPurgeItem{Type: "agent", ID: a.ID, ProjectID: a.ProjectID, Status: "unproven", Reason: "no_fixture_stamp"})
		}
	}
	return out, truncated, nil
}

// purgeFixtureUser deletes the identity's row with the 2c cascade
// (deleteUserRowCascadeTx) and writes the identity-level audit row in the
// same transaction. A refusal by the cascade (agents still tied to the
// identity, or the last owner of an unstamped project) keeps the identity
// and is recorded in the report.
func (s *Server) purgeFixtureUser(ctx context.Context, run fixturePurgeRun, report *TestIdentityPurgeReport) error {
	u := run.user
	now := s.testIdentities.clock()
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		if err := deleteUserRowCascadeTx(ctx, tx, u.ID, s.membershipNow()); err != nil {
			return err
		}
		record := &store.MutationAuditRecord{
			MutationType:  run.trigger,
			TargetType:    testIdentityAuditTargetType,
			TargetID:      u.ID,
			BeforeSummary: testIdentityDeleteSummary(u),
			AfterSummary:  testIdentityPurgeSummary(report, run),
			Timestamp:     now,
		}
		run.actor.ApplyActor(record)
		applyHubActorFallback(record)
		if err := s.testIdentities.hooks.writeAudit(ctx, tx, record); err != nil {
			return fmt.Errorf("audit test identity purge: %w", err)
		}
		return nil
	})
	if err == nil {
		report.IdentityDeleted = true
		s.afterUserDeleted(ctx, u.ID)
		return nil
	}
	var lastOwnerErr *lastProjectOwnerDeleteError
	var ownsAgentsErr *userOwnsAgentsDeleteError
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Deleted concurrently: nothing is left to do.
		report.IdentityDeleted = false
		return nil
	case errors.As(err, &ownsAgentsErr):
		report.IdentityKeptReason = "owns_agents"
		report.IdentityKeptDetails = map[string]any{"agents": ownsAgentsErr.agents}
		return nil
	case errors.As(err, &lastOwnerErr):
		report.IdentityKeptReason = "last_owner"
		report.IdentityKeptDetails = map[string]any{"projects": lastOwnerErr.projects}
		return nil
	case errors.Is(err, errUserRoleBindingsChanged):
		report.IdentityKeptReason = "bindings_changed"
		return nil
	}
	return err
}

// fixturePurgeSummary renders a per-resource audit summary: fields plus
// provenance_basis.
func fixturePurgeSummary(fields map[string]any, basis fixturePurgeBasis) string {
	m := map[string]any{}
	for k, v := range fields {
		m[k] = v
	}
	m["provenance_basis"] = basis
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// testIdentityPurgeSummary is the AfterSummary of the identity-level row.
func testIdentityPurgeSummary(report *TestIdentityPurgeReport, run fixturePurgeRun) string {
	b, _ := json.Marshal(struct {
		Deleted      TestIdentityPurgeCounts `json:"deleted"`
		KeptProjects int                     `json:"kept_projects"`
		IssuerID     string                  `json:"fixture_issuer_id"`
		Basis        string                  `json:"basis"`
		RunID        string                  `json:"purge_run_id"`
	}{report.Deleted, len(report.KeptProjects), run.issuerID, fixturePurgeBasisName, run.actor.CorrelationID})
	return string(b)
}
