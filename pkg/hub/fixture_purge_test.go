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

//go:build !no_sqlite

package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the test identity purge and expiry reaper (ptone/scion#4240,
// Phase 3b). Every identity here is a synthetic fixture in an in-memory
// store (or the CI Postgres store for the _Postgres variant).

// purgeRecDispatcher is createAgentDispatcher recording each agent delete
// dispatch in order, with its deleteFiles and soft flags.
type purgeRecDispatcher struct {
	createAgentDispatcher
	mu      sync.Mutex
	deletes []purgeRecDelete
}

type purgeRecDelete struct {
	agentID     string
	deleteFiles bool
	soft        bool
}

func (d *purgeRecDispatcher) DispatchAgentDelete(_ context.Context, a *store.Agent, deleteFiles, _, soft bool, _ time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deletes = append(d.deletes, purgeRecDelete{agentID: a.ID, deleteFiles: deleteFiles, soft: soft})
	return nil
}

func (d *purgeRecDispatcher) order() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.deletes))
	for _, del := range d.deletes {
		out = append(out, del.agentID)
	}
	return out
}

// tiPurge sends DELETE /api/v1/test-identities/{id}?purge=true (plus
// extraQuery) with token.
func tiPurge(t *testing.T, srv *Server, token, id, extraQuery string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestWithToken(t, srv, token, http.MethodDelete, "/api/v1/test-identities/"+id+"?purge=true"+extraQuery, body)
}

// fpPurgeReport decodes the report from a 200 answer or from a 409
// purge_incomplete answer's details.
func fpPurgeReport(t *testing.T, rec *httptest.ResponseRecorder) TestIdentityPurgeReport {
	t.Helper()
	var r TestIdentityPurgeReport
	switch rec.Code {
	case http.StatusOK:
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &r), rec.Body.String())
	case http.StatusConflict:
		var env struct {
			Error struct {
				Details struct {
					Reason string                  `json:"reason"`
					Report TestIdentityPurgeReport `json:"report"`
				} `json:"details"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env), rec.Body.String())
		require.Equal(t, testIdentityReasonPurgeIncomplete, env.Error.Details.Reason, rec.Body.String())
		r = env.Error.Details.Report
	default:
		t.Fatalf("purge: unexpected %d: %s", rec.Code, rec.Body.String())
	}
	return r
}

func fpPurgeAudits(t *testing.T, s store.Store, mutationType, targetID string) []store.MutationAuditRecord {
	t.Helper()
	audits, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: mutationType, TargetID: targetID})
	require.NoError(t, err)
	out := make([]store.MutationAuditRecord, 0, len(audits))
	for _, a := range audits {
		out = append(out, *a)
	}
	return out
}

// fpRequireBasis asserts exactly one audit row of mutationType for targetID
// whose AfterSummary carries provenance_basis naming root and issuer, and
// returns that basis.
func fpRequireBasis(t *testing.T, s store.Store, mutationType, targetID, root, issuer string) fixturePurgeBasis {
	t.Helper()
	audits := fpPurgeAudits(t, s, mutationType, targetID)
	require.Len(t, audits, 1, "%s rows for %s", mutationType, targetID)
	var summary struct {
		ProvenanceBasis *fixturePurgeBasis `json:"provenance_basis"`
	}
	require.NoError(t, json.Unmarshal([]byte(audits[0].AfterSummary), &summary), audits[0].AfterSummary)
	require.NotNil(t, summary.ProvenanceBasis, "%s %s has no provenance_basis: %s", mutationType, targetID, audits[0].AfterSummary)
	b := *summary.ProvenanceBasis
	assert.Equal(t, root, b.RootID)
	assert.Equal(t, issuer, b.IssuerID)
	assert.Equal(t, fixturePurgeBasisName, b.Basis)
	assert.NotEmpty(t, b.RunID)
	assert.Equal(t, b.RunID, audits[0].CorrelationID, "the row's correlation ID is the purge run")
	return b
}

func fpAgentExists(t *testing.T, s store.Store, id string) bool {
	t.Helper()
	_, err := s.GetAgent(context.Background(), id)
	if err == nil {
		return true
	}
	require.ErrorIs(t, err, store.ErrNotFound)
	return false
}

func fpProjectExists(t *testing.T, s store.Store, id string) bool {
	t.Helper()
	_, err := s.GetProject(context.Background(), id)
	if err == nil {
		return true
	}
	require.ErrorIs(t, err, store.ErrNotFound)
	return false
}

func fpUserExists(t *testing.T, s store.Store, id string) bool {
	t.Helper()
	_, err := s.GetUser(context.Background(), id)
	if err == nil {
		return true
	}
	require.ErrorIs(t, err, store.ErrNotFound)
	return false
}

// fpStampedAgentRow writes an agent row directly with the given stamp,
// owner, ancestry and phase, on the project's broker.
func fpStampedAgentRow(t *testing.T, s store.Store, projectID, brokerID, slug, owner string, ancestry []string, stamp store.FixtureProvenance, phase state.Phase) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID:              generateID(),
		Slug:            slug,
		Name:            slug,
		ProjectID:       projectID,
		OwnerID:         owner,
		CreatedBy:       owner,
		Ancestry:        ancestry,
		Phase:           string(phase),
		RuntimeBrokerID: brokerID,
		StateVersion:    1,
		Created:         time.Now(),
		Updated:         time.Now(),
		Fixture:         stamp,
	}
	require.NoError(t, s.CreateAgent(context.Background(), a))
	return a
}

// fpProjectBrokerID returns the broker fpAddProvider registered for the
// project.
func fpProjectBrokerID(t *testing.T, s store.Store, projectID string) string {
	t.Helper()
	providers, err := s.GetProjectProviders(context.Background(), projectID)
	require.NoError(t, err)
	require.NotEmpty(t, providers)
	return providers[0].BrokerID
}

func fpAddProjectRole(t *testing.T, srv *Server, s store.Store, token, projectID, role, userID string) int {
	t.Helper()
	rd, err := s.GetRoleDefinitionByName(context.Background(), role, store.RoleScopeProject)
	require.NoError(t, err)
	rec := doRequestWithToken(t, srv, token, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{RoleDefinitionID: rd.ID, PrincipalType: "user", PrincipalID: userID})
	return rec.Code
}

// AC 3b-1 / Phase 3-1 (+ amendment B2): a purge of F removes F's agents
// including sub-agents, sub-sub-agents and a scheduled-dispatch child that
// has no OwnerID and no ancestry (selected by fixture_root_id alone), F's
// schedule, F's group, F's project and F itself. Agents go deepest
// ancestry first, each with deleteFiles=true and never soft. Every delete
// has one audit row whose provenance_basis names F, all with one
// correlation ID, and one test_identity_purge row is written for F. A
// second purge is a no-op: 200 with empty counts.
func TestFixturePurge_TransitivePurge(t *testing.T) {
	e := newFPEnv(t, "fpp-all")
	disp := &purgeRecDispatcher{}
	e.srv.SetDispatcher(disp)
	ctx := context.Background()
	fxID := e.fx.Identity.ID

	a := fpCreateAgent(t, e.srv, e.fx.AccessToken, e.project, "fpp-a")
	b := fpCreateSubAgent(t, e.srv, a, e.project, "fpp-b")
	c := fpCreateSubAgent(t, e.srv, b, e.project, "fpp-c")
	require.NoError(t, e.srv.dispatchAgentEventHandler()(ctx, withSessionRevision(store.ScheduledEvent{
		ID: "evt-fpp", ProjectID: e.project, EventType: "dispatch_agent",
		Payload: `{"agentName":"fpp-sched","task":"t"}`, CreatedBy: fxID,
	}, fxID)))
	sched, err := e.store.GetAgentBySlug(ctx, e.project, "fpp-sched")
	require.NoError(t, err)
	require.Empty(t, sched.OwnerID, "a scheduled-dispatch child has no OwnerID")
	require.Empty(t, sched.Ancestry, "a scheduled-dispatch child has no ancestry")
	require.Equal(t, fxID, sched.Fixture.RootID, "it is selected by its stamp alone")
	scheduleID := fpCreateSchedule(t, e.srv, e.fx.AccessToken, e.project, fpScheduleBody("fpp-schedule"))
	groupID := fpCreateGroup(t, e.srv, e.fx.AccessToken, CreateGroupRequest{Name: "fpp-group"})

	rec := tiPurge(t, e.srv, e.issuerTok, fxID, "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	r := fpPurgeReport(t, rec)
	assert.True(t, r.Complete)
	assert.True(t, r.IdentityDeleted)
	assert.Equal(t, TestIdentityPurgeCounts{Agents: 4, Schedules: 1, Groups: 1, Projects: 1}, r.Deleted)
	assert.Empty(t, r.NotDone)
	assert.Empty(t, r.Unproven)
	assert.Empty(t, r.KeptProjects)

	for _, id := range []string{a, b, c, sched.ID} {
		assert.False(t, fpAgentExists(t, e.store, id), "agent %s is gone", id)
	}
	_, err = e.store.GetSchedule(ctx, scheduleID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = e.store.GetGroup(ctx, groupID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.False(t, fpProjectExists(t, e.store, e.project))
	assert.False(t, fpUserExists(t, e.store, fxID))
	assert.Equal(t, http.StatusUnauthorized, tiAuthMe(t, e.srv, e.fx.AccessToken).Code)

	// Deepest first, files removed, never soft.
	order := disp.order()
	require.Len(t, order, 4, "one broker delete per agent")
	assert.Less(t, slices.Index(order, c), slices.Index(order, b), "sub-sub-agent before sub-agent")
	assert.Less(t, slices.Index(order, b), slices.Index(order, a), "sub-agent before its creator")
	for _, del := range disp.deletes {
		assert.True(t, del.deleteFiles, "deleteFiles=true for %s", del.agentID)
		assert.False(t, del.soft, "a purge never soft-deletes %s", del.agentID)
	}

	// One audit row per delete, each naming F as its basis, one run.
	var runIDs []string
	for _, id := range []string{a, b, c, sched.ID} {
		runIDs = append(runIDs, fpRequireBasis(t, e.store, mutationTypeAgentHardDelete, id, fxID, e.issuerID).RunID)
	}
	runIDs = append(runIDs,
		fpRequireBasis(t, e.store, mutationTypeScheduleDelete, scheduleID, fxID, e.issuerID).RunID,
		fpRequireBasis(t, e.store, fixturePurgeGroupDeleteMutation, groupID, fxID, e.issuerID).RunID,
		fpRequireBasis(t, e.store, "project_delete", e.project, fxID, e.issuerID).RunID)
	for _, id := range runIDs {
		assert.Equal(t, runIDs[0], id, "every row of the run shares one correlation ID")
	}
	ident := fpPurgeAudits(t, e.store, testIdentityPurgeMutation, fxID)
	require.Len(t, ident, 1, "one test_identity_purge row per identity")
	assert.Equal(t, e.issuerID, ident[0].ActorPrincipalID)
	assert.Equal(t, fixturePurgeExecutorRequest, ident[0].ExecutorKind)
	assert.Equal(t, runIDs[0], ident[0].CorrelationID)
	assert.NotContains(t, ident[0].AfterSummary+ident[0].BeforeSummary, tiTokenSecret(e.fx.AccessToken))

	// Idempotent: a second purge selects nothing.
	rec = tiPurge(t, e.srv, e.issuerTok, fxID, "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, *emptyTestIdentityPurgeReport(fxID), fpPurgeReport(t, rec))
	assert.Len(t, fpPurgeAudits(t, e.store, testIdentityPurgeMutation, fxID), 1)
}

// AC 3b-2 / Phase 3-2 (negative): a real user's agent in an F-stamped
// project survives, and so does the project, which is kept with F's
// binding removed and listed in the report (ownerless: F was its only
// owner). A project F co-owns with a real user is kept too, with only F's
// binding removed. An unstamped agent with owner_id = F (a simulated
// legacy row) is not deleted: it is reported as unproven and keeps F, so
// the purge is incomplete (409). Once a real administrator removes that
// row, a re-run completes.
func TestFixturePurge_UnstampedAndForeignResourcesSurvive(t *testing.T) {
	e := newFPEnv(t, "fpp-neg")
	e.srv.SetDispatcher(&purgeRecDispatcher{})
	ctx := context.Background()
	fxID := e.fx.Identity.ID

	// The human is a member of F's project and runs an agent there.
	require.Equal(t, http.StatusCreated, fpAddProjectRole(t, e.srv, e.store, e.fx.AccessToken, e.project, store.ProjectRoleMember, e.human.ID))
	humanAgent := fpCreateAgent(t, e.srv, e.humanTok, e.project, "fpp-human-agent")
	require.True(t, fpAgent(t, e.store, humanAgent).Fixture.IsZero())
	fxAgent := fpCreateAgent(t, e.srv, e.fx.AccessToken, e.project, "fpp-fx-agent")
	// F's own group scoped to that project (as a conversation group is).
	fxProjectGroup := &store.Group{ID: generateID(), Name: "fpp-fx-project-group", Slug: "fpp-fx-project-group",
		GroupType: store.GroupTypeExplicit, ProjectID: e.project, CreatedBy: fxID, Fixture: e.stamp()}
	require.NoError(t, e.store.CreateGroup(ctx, fxProjectGroup))

	// A second F project, co-owned by the human.
	coOwned := tiCreateProject(t, e.srv, e.fx.AccessToken, "fpp-neg-coowned")
	require.Equal(t, http.StatusCreated, fpAddProjectRole(t, e.srv, e.store, e.fx.AccessToken, coOwned, store.ProjectRoleOwner, e.human.ID))

	// A legacy row: owned by F, in the human's project, with no stamp.
	humanProject := tiCreateProject(t, e.srv, e.humanTok, "fpp-neg-human-project")
	legacy := fpStampedAgentRow(t, e.store, humanProject, "", "fpp-legacy", fxID, []string{fxID}, store.FixtureProvenance{}, state.PhaseStopped)

	rec := tiPurge(t, e.srv, e.issuerTok, fxID, "", nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	r := fpPurgeReport(t, rec)
	assert.False(t, r.Complete)
	assert.False(t, r.IdentityDeleted)
	assert.Equal(t, "owns_agents", r.IdentityKeptReason)
	assert.Equal(t, 1, r.Deleted.Agents, "only F's stamped agent")
	assert.Zero(t, r.Deleted.Projects)
	assert.Equal(t, 1, r.Deleted.Groups, "F's own group in the kept project")
	assert.False(t, r.UnprovenTruncated)
	require.Len(t, r.Unproven, 1)
	assert.Equal(t, legacy.ID, r.Unproven[0].ID)
	assert.Equal(t, "unproven", r.Unproven[0].Status)

	kept := map[string]TestIdentityPurgeKeptProject{}
	for _, k := range r.KeptProjects {
		kept[k.ID] = k
	}
	require.Contains(t, kept, e.project)
	assert.Equal(t, fixtureProjectKeptOtherResources, kept[e.project].Reason)
	assert.Equal(t, 1, kept[e.project].BindingsRemoved)
	assert.True(t, kept[e.project].Ownerless)
	require.Contains(t, kept, coOwned)
	assert.Equal(t, fixtureProjectKeptOtherOwner, kept[coOwned].Reason)
	assert.False(t, kept[coOwned].Ownerless)

	assert.False(t, fpAgentExists(t, e.store, fxAgent))
	assert.True(t, fpAgentExists(t, e.store, humanAgent), "a real user's agent is never touched")
	assert.True(t, fpAgentExists(t, e.store, legacy.ID), "an unstamped agent is never touched")
	assert.True(t, fpProjectExists(t, e.store, e.project))
	assert.True(t, fpProjectExists(t, e.store, coOwned))
	assert.True(t, fpProjectExists(t, e.store, humanProject))
	assert.True(t, fpUserExists(t, e.store, fxID))
	for _, p := range []string{e.project, coOwned} {
		bindings, err := e.store.ListRoleBindingsForScope(ctx, store.RoleScopeProject, p)
		require.NoError(t, err)
		for _, b := range bindings {
			assert.NotEqual(t, fxID, b.PrincipalID, "F's binding on kept project %s is removed", p)
		}
		fpRequireBasis(t, e.store, testIdentityPurgeProjectKeptMutation, p, fxID, e.issuerID)
	}
	// F's group in the kept project is removed with its own basis row; the
	// project's implicit members group stays with the project.
	_, err := e.store.GetGroup(ctx, fxProjectGroup.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	fpRequireBasis(t, e.store, fixturePurgeGroupDeleteMutation, fxProjectGroup.ID, fxID, e.issuerID)
	groups, err := e.store.ListGroups(ctx, store.GroupFilter{ProjectID: e.project}, store.ListOptions{Limit: 50})
	require.NoError(t, err)
	membersGroupKept := false
	for i := range groups.Items {
		if store.IsProjectMembersGroup(&groups.Items[i]) {
			membersGroupKept = true
		}
	}
	assert.True(t, membersGroupKept, "the kept project's members group stays")
	assert.Empty(t, fpPurgeAudits(t, e.store, testIdentityPurgeMutation, fxID), "no identity row while F is kept")
	assert.Empty(t, fpPurgeAudits(t, e.store, mutationTypeAgentHardDelete, legacy.ID))
	assert.Empty(t, fpPurgeAudits(t, e.store, mutationTypeAgentHardDelete, humanAgent))

	// A real administrator removes the legacy row; the re-run completes.
	require.NoError(t, e.store.DeleteAgent(ctx, legacy.ID))
	rec = tiPurge(t, e.srv, e.issuerTok, fxID, "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	r = fpPurgeReport(t, rec)
	assert.True(t, r.Complete)
	assert.True(t, r.IdentityDeleted)
	assert.Zero(t, r.Deleted.Agents)
	assert.False(t, fpUserExists(t, e.store, fxID))
	assert.True(t, fpAgentExists(t, e.store, humanAgent))
	assert.True(t, fpProjectExists(t, e.store, e.project))
	assert.Len(t, fpPurgeAudits(t, e.store, testIdentityPurgeMutation, fxID), 1)
}

// R3: a group of F's that holds the owner role on F's project does not
// keep the project. F's groups (project-scoped ones included) are removed
// before any project is evaluated, so the project is evaluated with F's
// own principals only and is deleted, not kept ownerless.
func TestFixturePurge_OwnGroupDoesNotKeepProject(t *testing.T) {
	e := newFPEnv(t, "fpp-r3")
	e.srv.SetDispatcher(&purgeRecDispatcher{})
	ctx := context.Background()
	fxID := e.fx.Identity.ID
	ownerGroup := &store.Group{ID: generateID(), Name: "fpp-r3-owner-group", Slug: "fpp-r3-owner-group",
		GroupType: store.GroupTypeExplicit, ProjectID: e.project, CreatedBy: fxID, Fixture: e.stamp()}
	require.NoError(t, e.store.CreateGroup(ctx, ownerGroup))
	ownerRole, err := e.store.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	// The store refuses a group principal for the project-owner role
	// (direct-user-only), so this shape exists only as a row written
	// outside the store (a legacy row). Write the binding for a user, then
	// turn it into the group's binding with raw SQL.
	rb, err := e.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRole.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: e.human.ID,
		ScopeType: store.RoleScopeProject, ScopeID: e.project, CreatedBy: fxID,
	})
	require.NoError(t, err)
	db, ok := e.store.(interface{ DB() *sql.DB })
	require.True(t, ok, "the test store exposes its database")
	_, err = db.DB().ExecContext(ctx, "UPDATE role_bindings SET principal_type = 'group', principal_id = ? WHERE id = ?", ownerGroup.ID, rb.ID)
	require.NoError(t, err)
	got, err := e.store.GetRoleBinding(ctx, rb.ID)
	require.NoError(t, err)
	require.Equal(t, store.RoleBindingPrincipalGroup, got.PrincipalType)
	require.Equal(t, ownerGroup.ID, got.PrincipalID)

	rec := tiPurge(t, e.srv, e.issuerTok, fxID, "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	r := fpPurgeReport(t, rec)
	assert.True(t, r.Complete)
	assert.Empty(t, r.KeptProjects, "F's own group is not a non-fixture owner")
	assert.Equal(t, 1, r.Deleted.Projects)
	assert.Equal(t, 1, r.Deleted.Groups)
	assert.False(t, fpProjectExists(t, e.store, e.project))
	_, err = e.store.GetGroup(ctx, ownerGroup.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	fpRequireBasis(t, e.store, fixturePurgeGroupDeleteMutation, ownerGroup.ID, fxID, e.issuerID)
	fpRequireBasis(t, e.store, "project_delete", e.project, fxID, e.issuerID)
	assert.Empty(t, fpPurgeAudits(t, e.store, testIdentityPurgeProjectKeptMutation, e.project))
}

// AC 3b-4 (broker offline): a stamped agent whose broker is not reachable
// is reported broker_unavailable, the identity is kept, and the purge
// answers 409; once the broker is back a re-run completes.
func TestFixturePurge_BrokerUnavailableThenRerun(t *testing.T) {
	e := newFPEnv(t, "fpp-offline")
	e.srv.SetDispatcher(&purgeRecDispatcher{})
	ctx := context.Background()
	fxID := e.fx.Identity.ID
	brokerID := fpProjectBrokerID(t, e.store, e.project)
	agent := fpStampedAgentRow(t, e.store, e.project, brokerID, "fpp-offline-agent", fxID, []string{fxID}, e.stamp(), state.PhaseRunning)

	broker, err := e.store.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	broker.Status = store.BrokerStatusOffline
	require.NoError(t, e.store.UpdateRuntimeBroker(ctx, broker))

	rec := tiPurge(t, e.srv, e.issuerTok, fxID, "", nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	r := fpPurgeReport(t, rec)
	require.Len(t, r.NotDone, 2, "the agent, and its project left for later")
	assert.Equal(t, TestIdentityPurgeItem{Type: "agent", ID: agent.ID, ProjectID: e.project, Status: "broker_unavailable"}, r.NotDone[0])
	assert.Equal(t, "project", r.NotDone[1].Type)
	assert.Equal(t, fixtureProjectPendingAgents, r.NotDone[1].Reason)
	assert.True(t, fpAgentExists(t, e.store, agent.ID))
	assert.True(t, fpProjectExists(t, e.store, e.project))
	assert.True(t, fpUserExists(t, e.store, fxID))

	broker.Status = store.BrokerStatusOnline
	require.NoError(t, e.store.UpdateRuntimeBroker(ctx, broker))
	rec = tiPurge(t, e.srv, e.issuerTok, fxID, "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	r = fpPurgeReport(t, rec)
	assert.True(t, r.Complete)
	assert.Equal(t, TestIdentityPurgeCounts{Agents: 1, Projects: 1}, r.Deleted)
	assert.False(t, fpAgentExists(t, e.store, agent.ID))
	assert.False(t, fpUserExists(t, e.store, fxID))
}

// AC 3b-4 (in_doubt): a delete whose broker intent is still outstanding
// when the wait ends is reported in_doubt (never counted as cleaned), the
// purge answers 409 and keeps the identity; once the intent ends and the
// broker answers, a re-run completes.
func TestFixturePurge_InDoubtThenRerun(t *testing.T) {
	setDeleteWaitTimeout(t, func(context.Context) time.Duration { return 100 * time.Millisecond })
	e := newFPEnv(t, "fpp-indoubt")
	ctx := context.Background()
	fxID := e.fx.Identity.ID
	brokerID := fpProjectBrokerID(t, e.store, e.project)
	agent := fpStampedAgentRow(t, e.store, e.project, brokerID, "fpp-indoubt-agent", fxID, []string{fxID}, e.stamp(), state.PhaseRunning)

	bus := NewChannelEventPublisher()
	t.Cleanup(bus.Close)
	client := &mockRuntimeBrokerClient{returnErr: ErrLifecycleDeferred}
	d := NewHTTPAgentDispatcherWithClient(e.store, client, false, slog.Default())
	d.SetCrossNodeDeps(bus, NoopCommandBus{})
	e.srv.SetDispatcher(d)

	rec := tiPurge(t, e.srv, e.issuerTok, fxID, "", nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	r := fpPurgeReport(t, rec)
	assert.Zero(t, r.Deleted.Agents, "an in_doubt delete is not counted as cleaned")
	require.NotEmpty(t, r.NotDone)
	assert.Equal(t, agent.ID, r.NotDone[0].ID)
	assert.Equal(t, store.DeletionCodeInDoubt, r.NotDone[0].Status)
	assert.False(t, r.IdentityDeleted)
	assert.True(t, fpUserExists(t, e.store, fxID))
	got := fpAgent(t, e.store, agent.ID)
	assert.Equal(t, store.DeletionCodeInDoubt, got.DeletionCode)

	// The intent ends; the broker answers directly; the re-run completes.
	pending, err := e.store.ListPendingDispatch(ctx, brokerID)
	require.NoError(t, err)
	for _, p := range pending {
		if p.AgentID == agent.ID {
			endIntent(t, e.store, p.ID, false)
		}
	}
	client.returnErr = nil
	rec = tiPurge(t, e.srv, e.issuerTok, fxID, "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	r = fpPurgeReport(t, rec)
	assert.True(t, r.Complete)
	assert.Equal(t, 1, r.Deleted.Agents)
	assert.False(t, fpAgentExists(t, e.store, agent.ID))
	assert.False(t, fpUserExists(t, e.store, fxID))
	fpRequireBasis(t, e.store, mutationTypeAgentHardDelete, agent.ID, fxID, e.issuerID)
}

// A stamped agent that was soft-deleted before the purge (soft-delete
// retention on) has its row removed by the purge, with its audit row.
func TestFixturePurge_SoftDeletedAgentRemoved(t *testing.T) {
	e := newFPEnv(t, "fpp-soft")
	e.srv.SetDispatcher(&purgeRecDispatcher{})
	e.srv.config.SoftDeleteRetention = time.Hour
	fxID := e.fx.Identity.ID
	agent := fpCreateAgent(t, e.srv, e.fx.AccessToken, e.project, "fpp-soft-agent")
	rec := doRequestWithToken(t, e.srv, e.fx.AccessToken, http.MethodDelete, "/api/v1/agents/"+agent, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	require.False(t, fpAgent(t, e.store, agent).DeletedAt.IsZero(), "soft-deleted")

	rec = tiPurge(t, e.srv, e.issuerTok, fxID, "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	r := fpPurgeReport(t, rec)
	assert.True(t, r.Complete)
	assert.Equal(t, 1, r.Deleted.Agents)
	assert.Equal(t, []TestIdentityPurgeItem{{
		Type: "agent", ID: agent, ProjectID: e.project, Status: "removed", Reason: fixturePurgeRetentionUnknown,
	}}, r.FilesRetained, "the soft delete's request is no longer recorded")
	assert.False(t, fpAgentExists(t, e.store, agent))
	fpRequireBasis(t, e.store, mutationTypeAgentHardDelete, agent, fxID, e.issuerID)
}

// [v2] T3, server-side targets: the purge takes only the identity ID. A
// request naming resources in the query or in a body is refused with 400
// and removes nothing.
func TestFixturePurge_RequestCannotNameTargets(t *testing.T) {
	e := newFPEnv(t, "fpp-t3")
	e.srv.SetDispatcher(&purgeRecDispatcher{})
	fxID := e.fx.Identity.ID
	fxAgent := fpCreateAgent(t, e.srv, e.fx.AccessToken, e.project, "fpp-t3-fx")
	humanProject := tiCreateProject(t, e.srv, e.humanTok, "fpp-t3-human")
	fpAddProvider(t, e.store, humanProject)
	humanAgent := fpCreateAgent(t, e.srv, e.humanTok, humanProject, "fpp-t3-human-agent")

	for name, rec := range map[string]*httptest.ResponseRecorder{
		"agent id in query":   tiPurge(t, e.srv, e.issuerTok, fxID, "&agentId="+humanAgent, nil),
		"project id in query": tiPurge(t, e.srv, e.issuerTok, fxID, "&projectId="+humanProject, nil),
		"body with ids":       tiPurge(t, e.srv, e.issuerTok, fxID, "", map[string]any{"agents": []string{humanAgent}, "projects": []string{humanProject}}),
		"invalid purge value": doRequestWithToken(t, e.srv, e.issuerTok, http.MethodDelete, "/api/v1/test-identities/"+fxID+"?purge=yes", nil),
	} {
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", name, rec.Body.String())
	}
	for _, id := range []string{fxAgent, humanAgent} {
		assert.True(t, fpAgentExists(t, e.store, id))
	}
	assert.True(t, fpProjectExists(t, e.store, humanProject))
	assert.True(t, fpUserExists(t, e.store, fxID))
}

// No existence oracle (ti-adm-em Q2 ruling): with ?purge=true a caller
// that may not purge the identity, a human user's ID, a random ID and a
// malformed ID all get the same 200 with an empty report, and nothing is
// selected or audited for a human ID. An identity already deleted by the
// 2c delete leaves a stamped group behind: its issuer's purge removes it,
// another issuer's gets the empty report and removes nothing.
func TestFixturePurge_NoExistenceOracle(t *testing.T) {
	e := newFPEnv(t, "fpp-oracle")
	e.srv.SetDispatcher(&purgeRecDispatcher{})
	ctx := context.Background()
	fxID := e.fx.Identity.ID
	fxAgent := fpCreateAgent(t, e.srv, e.fx.AccessToken, e.project, "fpp-oracle-fx")
	humanProject := tiCreateProject(t, e.srv, e.humanTok, "fpp-oracle-human")
	fpAddProvider(t, e.store, humanProject)
	humanAgent := fpCreateAgent(t, e.srv, e.humanTok, humanProject, "fpp-oracle-human-agent")
	_, otherTok := tiIssuer(t, e.srv, e.store, "fpp-oracle-other-issuer")

	body := func(rec *httptest.ResponseRecorder, id string) TestIdentityPurgeReport {
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		r := fpPurgeReport(t, rec)
		assert.Equal(t, id, r.IdentityID)
		r.IdentityID = ""
		return r
	}
	want := *emptyTestIdentityPurgeReport("")
	assert.Equal(t, want, body(tiPurge(t, e.srv, otherTok, fxID, "", nil), fxID), "another issuer's live fixture")
	assert.Equal(t, want, body(tiPurge(t, e.srv, e.issuerTok, e.human.ID, "", nil), e.human.ID), "a human user")
	assert.Equal(t, want, body(tiPurge(t, e.srv, otherTok, e.human.ID, "", nil), e.human.ID), "a human user, other caller")
	random := generateID()
	assert.Equal(t, want, body(tiPurge(t, e.srv, e.issuerTok, random, "", nil), random), "a random ID")
	assert.Equal(t, want, body(tiPurge(t, e.srv, e.issuerTok, "not-a-uuid", "", nil), "not-a-uuid"), "a malformed ID")

	assert.True(t, fpAgentExists(t, e.store, fxAgent))
	assert.True(t, fpAgentExists(t, e.store, humanAgent))
	assert.True(t, fpProjectExists(t, e.store, humanProject))
	assert.True(t, fpUserExists(t, e.store, e.human.ID))
	assert.Empty(t, fpPurgeAudits(t, e.store, testIdentityPurgeMutation, e.human.ID))
	assert.Empty(t, fpPurgeAudits(t, e.store, mutationTypeAgentHardDelete, humanAgent))

	// A second fixture with only a group; the 2c delete leaves the group.
	fx2 := tiIssue(t, e.srv, e.issuerTok, map[string]string{"role": "member"})
	group := fpCreateGroup(t, e.srv, fx2.AccessToken, CreateGroupRequest{Name: "fpp-oracle-group"})
	require.Equal(t, http.StatusNoContent, tiDelete(t, e.srv, e.issuerTok, fx2.Identity.ID).Code)
	assert.Equal(t, want, body(tiPurge(t, e.srv, otherTok, fx2.Identity.ID, "", nil), fx2.Identity.ID), "another issuer, deleted fixture")
	_, err := e.store.GetGroup(ctx, group)
	require.NoError(t, err, "nothing removed for another issuer")
	rec := tiPurge(t, e.srv, e.issuerTok, fx2.Identity.ID, "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	r := fpPurgeReport(t, rec)
	assert.Equal(t, 1, r.Deleted.Groups)
	assert.False(t, r.IdentityDeleted)
	_, err = e.store.GetGroup(ctx, group)
	assert.ErrorIs(t, err, store.ErrNotFound)
	fpRequireBasis(t, e.store, fixturePurgeGroupDeleteMutation, group, fx2.Identity.ID, e.issuerID)
}

// runFixturePurgeCoOwnerRace is [v2] T3, race-safe sole owner: in each
// round a real user is added as co-owner of an F-stamped project while
// the purge's project step runs. The project survives whenever the add
// succeeded: either the add committed first and the purge kept the
// project, or the purge deleted it first and the add failed. Never both
// an added owner and a deleted project.
func runFixturePurgeCoOwnerRace(t *testing.T, srv *Server, s store.Store, rounds int) {
	t.Helper()
	ctx := context.Background()
	enableTestIdentitiesForTest(srv)
	srv.SetDispatcher(&purgeRecDispatcher{})
	issuerID, issuerTok := tiIssuer(t, srv, s, "fpp-race-issuer")
	fx := tiIssue(t, srv, issuerTok, map[string]string{"role": "member"})
	human := hubMemberUser(t, s, "fpp-race-human")
	basis := fixturePurgeBasis{RootID: fx.Identity.ID, IssuerID: issuerID, Basis: fixturePurgeBasisName, RunID: "fpp-race", Trigger: testIdentityPurgeMutation}
	kept, deleted := 0, 0
	for i := 0; i < rounds; i++ {
		project := tiCreateProject(t, srv, fx.AccessToken, fmt.Sprintf("fpp-race-%d", i))
		var out fixtureProjectOutcome
		var purgeErr error
		addCode := 0
		fpRunBoth(t,
			func() { out, purgeErr = srv.deletionService.purgeFixtureProject(ctx, project, basis) },
			func() {
				addCode = fpAddProjectRole(t, srv, s, fx.AccessToken, project, store.ProjectRoleOwner, human.ID)
			})
		require.NoError(t, purgeErr)
		exists := fpProjectExists(t, s, project)
		if addCode == http.StatusCreated {
			require.True(t, exists, "round %d: an owner was added, so the project must survive", i)
			require.True(t, out.Kept, "round %d", i)
			assert.Equal(t, fixtureProjectKeptOtherOwner, out.KeptReason)
			kept++
		} else {
			require.False(t, exists, "round %d: the add failed (%d) only because the project was deleted first", i, addCode)
			require.True(t, out.Deleted, "round %d", i)
			deleted++
		}
	}
	t.Logf("co-owner race (free start): %d kept, %d deleted", kept, deleted)

	// Forced interleaving 1: the owner add commits before the purge reads
	// ownership; the project is kept.
	project := tiCreateProject(t, srv, fx.AccessToken, "fpp-race-add-first")
	require.Equal(t, http.StatusCreated, fpAddProjectRole(t, srv, s, fx.AccessToken, project, store.ProjectRoleOwner, human.ID))
	out, err := srv.deletionService.purgeFixtureProject(ctx, project, basis)
	require.NoError(t, err)
	require.True(t, out.Kept)
	assert.Equal(t, fixtureProjectKeptOtherOwner, out.KeptReason)
	require.True(t, fpProjectExists(t, s, project))

	// Forced interleaving 2 (the dangerous one): the purge has read
	// ownership and contents and holds the project row lock when the owner
	// add starts. The add must wait for the purge's transaction (Postgres:
	// the FOR UPDATE row lock every membership writer takes; SQLite: the
	// single writer), and then fail on the deleted project. It must never
	// commit an owner on a project the purge then deletes.
	ownerRole, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	project = tiCreateProject(t, srv, fx.AccessToken, "fpp-race-add-during")
	addDone := make(chan int, 1)
	addedDuringDecision := false
	fixtureProjectPurgeDecisionHook = func(id string) {
		if id != project {
			return
		}
		go func() {
			rec := doRequestWithToken(t, srv, fx.AccessToken, http.MethodPost, "/api/v1/projects/"+project+"/members",
				addProjectMemberRequest{RoleDefinitionID: ownerRole.ID, PrincipalType: "user", PrincipalID: human.ID})
			addDone <- rec.Code
		}()
		select {
		case <-addDone:
			addedDuringDecision = true
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Cleanup(func() { fixtureProjectPurgeDecisionHook = nil })
	out, err = srv.deletionService.purgeFixtureProject(ctx, project, basis)
	fixtureProjectPurgeDecisionHook = nil
	require.NoError(t, err)
	require.False(t, addedDuringDecision, "the owner add completed while the purge held the project lock")
	require.True(t, out.Deleted, "the purge read no other owner, so it deletes the project")
	var addCode int
	select {
	case addCode = <-addDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the owner add never finished after the purge committed")
	}
	assert.NotEqual(t, http.StatusCreated, addCode, "the add must fail on the deleted project")
	assert.False(t, fpProjectExists(t, s, project))
}

func TestFixturePurge_ConcurrentCoOwner(t *testing.T) {
	srv, s := testServer(t)
	runFixturePurgeCoOwnerRace(t, srv, s, 8)
}

func TestFixturePurge_ConcurrentCoOwner_Postgres(t *testing.T) {
	if !enttest.Active() {
		t.Skip("requires -tags integration and SCION_TEST_POSTGRES_URL")
	}
	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	srv, s := testServerWithStore(t, cs)
	runFixturePurgeCoOwnerRace(t, srv, s, 8)
}

// Phase 3-1 (reaper) and Phase 3-3: with a fake clock, an expired
// identity is not admitted and its running agent is held on the first
// reaper tick after the expiry (the reaper enqueues and drains at the
// drain cadence); past the grace period the next tick purges it, writing
// one test_identity_expire row attributed to the issuer with the reaper as
// executor. A live identity is left alone.
func TestTestIdentityReaper_HoldsThenPurges(t *testing.T) {
	e := newFPEnv(t, "fpp-reaper")
	e.srv.SetDispatcher(&purgeRecDispatcher{})
	ctx := context.Background()
	fx := tiIssue(t, e.srv, e.issuerTok, map[string]any{"role": "member", "lifetimeSeconds": 60})
	fxID := fx.Identity.ID
	project := tiCreateProject(t, e.srv, fx.AccessToken, "fpp-reaper-project")
	fpAddProvider(t, e.store, project)
	agent := fpCreateAgent(t, e.srv, fx.AccessToken, project, "fpp-reaper-agent")
	expiresAt := fx.Identity.ExpiresAt

	var clock time.Time
	e.srv.testIdentities.now = func() time.Time { return clock }

	// Before the expiry: nothing.
	clock = expiresAt.Add(-time.Second)
	res, err := e.srv.reapTestIdentities(ctx, clock)
	require.NoError(t, err)
	assert.Zero(t, res.Held)
	assert.Empty(t, res.Purged)
	admitted, err := e.srv.userAdmittedByID(ctx, fxID, project)
	require.NoError(t, err)
	assert.True(t, admitted, "a live identity is admitted")

	// Just past the expiry: held, not yet purged.
	clock = expiresAt.Add(time.Second)
	admitted, err = e.srv.userAdmittedByID(ctx, fxID, project)
	require.NoError(t, err)
	assert.False(t, admitted, "an expired identity is not admitted")
	res, err = e.srv.reapTestIdentities(ctx, clock)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Held)
	assert.Empty(t, res.Purged)
	held, err := e.store.HasActiveAgentHold(ctx, agent)
	require.NoError(t, err)
	assert.True(t, held, "held within one reaper tick of the expiry")
	assert.True(t, fpAgentExists(t, e.store, agent))
	// The env's own (live) fixture is untouched.
	assert.True(t, fpUserExists(t, e.store, e.fx.Identity.ID))

	// Past the grace period: purged.
	clock = expiresAt.Add(testIdentityReaperGrace + time.Second)
	res, err = e.srv.reapTestIdentities(ctx, clock)
	require.NoError(t, err)
	assert.Equal(t, []string{fxID}, res.Purged)
	assert.False(t, fpAgentExists(t, e.store, agent))
	assert.False(t, fpProjectExists(t, e.store, project))
	assert.False(t, fpUserExists(t, e.store, fxID))
	assert.True(t, fpUserExists(t, e.store, e.fx.Identity.ID), "a live identity is never reaped")
	rows := fpPurgeAudits(t, e.store, testIdentityExpireMutation, fxID)
	require.Len(t, rows, 1, "one test_identity_expire row per identity")
	assert.Equal(t, "user", rows[0].ActorPrincipalKind)
	assert.Equal(t, e.issuerID, rows[0].ActorPrincipalID, "acting for the issuer")
	assert.Equal(t, fixturePurgeExecutorReaper, rows[0].ExecutorKind)
	b := fpRequireBasis(t, e.store, mutationTypeAgentHardDelete, agent, fxID, e.issuerID)
	assert.Equal(t, testIdentityExpireMutation, b.Trigger)

	// The next tick has nothing to do.
	res, err = e.srv.reapTestIdentities(ctx, clock.Add(time.Minute))
	require.NoError(t, err)
	assert.Empty(t, res.Purged)
	assert.Empty(t, res.Incomplete)
}

// R1: identities whose purge can never complete do not starve the reaper.
// With a purge batch of 1, two stuck identities (each owning an unstamped
// legacy agent, so their purge stays incomplete) expire before a third
// one. The third is still held on the first tick after its expiry (the
// hold has its own query), and is purged within a few ticks past its
// grace period (the purge cursor moves past the stuck ones), while the
// stuck ones keep being retried.
func TestTestIdentityReaper_StuckIdentitiesDoNotStarve(t *testing.T) {
	e := newFPEnv(t, "fpp-starve")
	e.srv.SetDispatcher(&purgeRecDispatcher{})
	ctx := context.Background()
	old := testIdentityReaperBatch
	testIdentityReaperBatch = 1
	t.Cleanup(func() { testIdentityReaperBatch = old })

	base := time.Now().Add(time.Hour).Truncate(time.Second)
	var stuck []string
	for i := 0; i < 2; i++ {
		u := tiStoreFixture(t, e.store, e.issuerID, base.Add(time.Duration(i)*time.Second))
		fpStampedAgentRow(t, e.store, e.project, "", fmt.Sprintf("fpp-starve-legacy-%d", i), u.ID, []string{u.ID}, store.FixtureProvenance{}, state.PhaseStopped)
		stuck = append(stuck, u.ID)
	}
	late := tiStoreFixture(t, e.store, e.issuerID, base.Add(time.Minute))
	lateStamp := store.FixtureProvenance{RootID: late.ID, IssuerID: e.issuerID}
	lateAgent := fpStampedAgentRow(t, e.store, e.project, "", "fpp-starve-late-agent", late.ID, []string{late.ID}, lateStamp, state.PhaseRunning)
	require.NoError(t, e.store.CreateDelegationEdge(ctx, &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser, DelegatorID: late.ID,
		DelegateType: store.DelegationPrincipalAgent, DelegateID: lateAgent.ID,
		ScopeType: store.RoleScopeProject, ScopeID: e.project, Role: string(AgentRoleBaseline), Active: true,
		AuthorityProvenance: store.AuthorityProvenance{
			ProvenanceVersion: store.ProvenanceVersionV1, SourcePrincipalKind: store.DelegationPrincipalUser,
			SourcePrincipalID: late.ID, SourceCredentialKind: store.SourceCredentialSession,
		},
		EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}))

	var clock time.Time
	e.srv.testIdentities.now = func() time.Time { return clock }

	// The stuck ones are past their grace period and retried first; the
	// late one has just expired: it is held on this tick.
	clock = base.Add(testIdentityReaperGrace + 2*time.Second)
	require.True(t, clock.After(base.Add(time.Second+testIdentityReaperGrace)), "both stuck identities are past their grace period")
	require.True(t, clock.After(*late.ExpiresAt) && clock.Before(late.ExpiresAt.Add(testIdentityReaperGrace)), "the late one is within its grace period")
	incomplete := map[string]bool{}
	res, err := e.srv.reapTestIdentities(ctx, clock)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Held, "only the late identity is within its grace period")
	for _, id := range res.Incomplete {
		incomplete[id] = true
	}
	held, err := e.store.HasActiveAgentHold(ctx, lateAgent.ID)
	require.NoError(t, err)
	assert.True(t, held, "the late identity is held although stuck identities are ahead of it")

	// Past the late one's grace: within a few ticks it is purged, the stuck
	// ones stay incomplete.
	clock = late.ExpiresAt.Add(testIdentityReaperGrace + time.Second)
	purged := false
	for tick := 0; tick < 4 && !purged; tick++ {
		res, err = e.srv.reapTestIdentities(ctx, clock)
		require.NoError(t, err)
		for _, id := range res.Incomplete {
			incomplete[id] = true
		}
		purged = slices.Contains(res.Purged, late.ID)
		clock = clock.Add(time.Minute)
	}
	require.True(t, purged, "the late identity is purged despite the stuck ones")
	assert.False(t, fpUserExists(t, e.store, late.ID))
	assert.False(t, fpAgentExists(t, e.store, lateAgent.ID))
	for _, id := range stuck {
		assert.True(t, incomplete[id], "stuck identity %s is retried", id)
		assert.True(t, fpUserExists(t, e.store, id))
	}
}

// The reaper is registered only with --enable-test-identities.
func TestTestIdentityReaper_Registration(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		srv, _ := newTestIdentityServer(t, enabled)
		sched := NewScheduler(srv.store, slog.Default())
		srv.scheduler = sched
		srv.registerTestIdentityReaper()
		found := false
		for _, h := range sched.recurring {
			if h.Name == "test-identity-reaper" {
				found = true
				assert.True(t, h.Singleton)
				assert.Equal(t, testIdentityReaperIntervalMinutes, h.Interval)
			}
		}
		assert.Equal(t, enabled, found, "enabled=%v", enabled)
	}
}

// DEC-2 a, by construction: the purge removes projects through an
// unexported method that takes no request, actor or credential. The
// general request type has exactly its three fields. Force
// (ptone/scion#4811) only chooses, on the authorized request path, between
// refusing while agents remain and the cascade; it cannot reach
// purgeFixtureProject. The purge-only selectors (purgeFixtureProject, the purge basis, the hard delete flag,
// purgeTestIdentity) are reached only from the purge's own files.
func TestFixturePurge_ProjectPathUnreachableFromRequests(t *testing.T) {
	var fields []string
	rt := reflect.TypeOf(ProjectDeleteRequest{})
	for i := 0; i < rt.NumField(); i++ {
		fields = append(fields, rt.Field(i).Name)
	}
	assert.Equal(t, []string{"ProjectID", "Actor", "Force"}, fields, "ProjectDeleteRequest gains no field that selects another path")

	allowed := map[string][]string{
		"purgeFixtureProject(":   {"fixture_purge.go", "project_fixture_purge.go"},
		"withFixturePurgeBasis(": {"fixture_purge.go"},
		"purgeTestIdentity(":     {"fixture_purge.go", "handlers_test_identities.go", "test_identity_reaper.go"},
		"hard:":                  {"fixture_purge.go"},
	}
	for needle, files := range allowed {
		for _, f := range fpNonTestHubFilesContaining(t, needle) {
			assert.Contains(t, files, f, "%q appears in %s", needle, f)
		}
	}
}

// DEC-2 e: a test identity cannot create or register a broker (brokers
// carry no stamp, so a purge could not remove one). A human member can.
func TestTestIdentity_BrokerCreateRefused(t *testing.T) {
	e := newFPEnv(t, "fpp-broker")
	rec := doRequestWithToken(t, e.srv, e.fx.AccessToken, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{Name: "fpp-fixture-broker"})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), testIdentityReasonFixtureBroker)
	_, err := e.store.GetRuntimeBrokerByName(context.Background(), "fpp-fixture-broker")
	assert.ErrorIs(t, err, store.ErrNotFound)

	rec = doRequestWithToken(t, e.srv, e.humanTok, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{Name: "fpp-human-broker"})
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// F14 (DEC-2 e): an agent held by membership loss is still deletable. A
// hold gates start, scheduled fires and messages, not delete.
func TestAgentDelete_HeldAgentIsDeletable(t *testing.T) {
	f := newMSFixture(t, "f14")
	f.hold(f.childC.ID, f.userID)
	f.hold(f.agentA.ID, f.userID)
	require.True(t, f.held(f.agentA.ID))
	for _, id := range []string{f.childC.ID, f.agentA.ID} {
		rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/agents/"+id, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		assert.False(t, fpAgentExists(t, f.s, id), "held agent %s was deleted", id)
	}
}

// fpNonTestHubFilesContaining lists the non-test Go files of this package
// whose source contains needle.
func fpNonTestHubFilesContaining(t *testing.T, needle string) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	var out []string
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		require.NoError(t, err)
		if strings.Contains(string(src), needle) {
			out = append(out, name)
		}
	}
	return out
}

// fpRunBoth starts both functions together and waits for both (a local
// copy of runConcurrently, which lives in another test shard).
func fpRunBoth(t *testing.T, a, b func()) {
	t.Helper()
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(2)
	go func() { defer wg.Done(); <-start; a() }()
	go func() { defer wg.Done(); <-start; b() }()
	close(start)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("concurrent operations did not finish (deadlock?)")
	}
}

// fpAddProvider makes a broker serve projectID so agents can be created
// (as tiAddProvider, which lives in another test shard).
func fpAddProvider(t *testing.T, s store.Store, projectID string) {
	t.Helper()
	ctx := context.Background()
	broker := &store.RuntimeBroker{ID: tid("fpp-broker-" + projectID), Name: "fpp broker", Slug: "fpp-broker-" + projectID[:8], Status: store.BrokerStatusOnline, Endpoint: "http://localhost:9800"}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{ProjectID: projectID, BrokerID: broker.ID, BrokerName: broker.Name, Status: store.BrokerStatusOnline}))
}

// fpFinalizingSoftAgent creates a stamped agent and leaves it finalizing
// with a stored soft request: soft-delete retention on with files retained,
// and the soft finish of its DELETE fails once (agentDeletionFinalizeSeam).
func fpFinalizingSoftAgent(t *testing.T, e fpEnv, name string) string {
	t.Helper()
	e.srv.SetDispatcher(&purgeRecDispatcher{})
	e.srv.config.SoftDeleteRetention = time.Hour
	e.srv.config.SoftDeleteRetainFiles = true
	agent := fpCreateAgent(t, e.srv, e.fx.AccessToken, e.project, name)

	old := agentDeletionFinalizeSeam
	t.Cleanup(func() { agentDeletionFinalizeSeam = old })
	var once sync.Once
	agentDeletionFinalizeSeam = func(context.Context, store.Store, *store.Agent, store.DeletionFinalizeMode) error {
		var err error
		once.Do(func() { err = fmt.Errorf("injected finalize error") })
		return err
	}
	rec := doRequestWithToken(t, e.srv, e.fx.AccessToken, http.MethodDelete, "/api/v1/agents/"+agent, nil)
	require.NotEqual(t, http.StatusNoContent, rec.Code, rec.Body.String())

	got := fpAgent(t, e.store, agent)
	require.Equal(t, store.DeletionStateFinalizing, got.DeletionState)
	require.Equal(t, store.DeletionCodeFinalizeFailed, got.DeletionCode)
	require.True(t, got.DeletedAt.IsZero())
	req := got.ParseDeletionRequest()
	require.True(t, req.Soft, "the stored request is soft")
	require.False(t, req.DeleteFiles, "the stored soft request keeps the files")
	require.True(t, req.RemoveBranch, "the stored request removes the branch")
	return agent
}

// A purge reaching an agent whose earlier delete is finalizing with a stored
// soft request: the engine's re-claim keeps that request (ptone/scion#4183)
// and leaves a soft tombstone, which the purge removes in the same run. The
// agent counts as removed only with its row gone, and the files the earlier
// request kept are reported.
func TestFixturePurge_FinalizingSoftRequestLeavesNoTombstone(t *testing.T) {
	e := newFPEnv(t, "fpp-finsoft")
	fxID := e.fx.Identity.ID
	agent := fpFinalizingSoftAgent(t, e, "fpp-finsoft-agent")

	rec := tiPurge(t, e.srv, e.issuerTok, fxID, "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	r := fpPurgeReport(t, rec)
	assert.True(t, r.Complete)
	assert.Empty(t, r.NotDone)
	assert.Equal(t, 1, r.Deleted.Agents)
	assert.Equal(t, []TestIdentityPurgeItem{{
		Type: "agent", ID: agent, ProjectID: e.project, Status: "removed", Reason: "files_retained",
	}}, r.FilesRetained)
	assert.False(t, fpAgentExists(t, e.store, agent), "no soft tombstone outlives the purge")
	assert.False(t, fpUserExists(t, e.store, fxID))
	fpRequireBasis(t, e.store, mutationTypeAgentHardDelete, agent, fxID, e.issuerID)
}

// fpRestoreOnDeletedPublisher restores one agent right after the delete
// engine publishes its (soft) deletion, before the purge re-reads the row.
type fpRestoreOnDeletedPublisher struct {
	EventPublisher
	srv     *Server
	agentID string
	once    sync.Once
	mu      sync.Mutex
	err     error
	ran     bool
}

func (p *fpRestoreOnDeletedPublisher) PublishAgentDeleted(ctx context.Context, agentID, projectID string) {
	p.EventPublisher.PublishAgentDeleted(ctx, agentID, projectID)
	if agentID != p.agentID {
		return
	}
	p.once.Do(func() {
		var err error
		a, gerr := p.srv.store.GetAgent(context.Background(), agentID)
		if gerr != nil {
			err = gerr
		} else {
			err = p.srv.restoreAgentTx(context.Background(), a, AuditActor{})
		}
		p.mu.Lock()
		p.err, p.ran = err, true
		p.mu.Unlock()
	})
}

// The same purge with a restore between the engine's soft finish and the
// purge's hard finalize: the restore wins, the agent is kept, and the purge
// reports it not done (never removed) and keeps the identity.
func TestFixturePurge_FinalizingSoftRequestRestoreWins(t *testing.T) {
	e := newFPEnv(t, "fpp-finsoft-restore")
	fxID := e.fx.Identity.ID
	agent := fpFinalizingSoftAgent(t, e, "fpp-finsoft-restore-agent")

	base := e.srv.events
	if base == nil {
		base = noopEventPublisher{}
	}
	pub := &fpRestoreOnDeletedPublisher{EventPublisher: base, srv: e.srv, agentID: agent}
	e.srv.SetEventPublisher(pub)
	t.Cleanup(func() { e.srv.SetEventPublisher(base) })

	rec := tiPurge(t, e.srv, e.issuerTok, fxID, "", nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	pub.mu.Lock()
	ran, rerr := pub.ran, pub.err
	pub.mu.Unlock()
	require.True(t, ran, "the soft finish published the deletion")
	require.NoError(t, rerr, "the restore succeeded")

	r := fpPurgeReport(t, rec)
	assert.False(t, r.Complete)
	assert.Zero(t, r.Deleted.Agents, "a restored agent is not counted as removed")
	assert.Empty(t, r.FilesRetained)
	require.NotEmpty(t, r.NotDone)
	assert.Equal(t, TestIdentityPurgeItem{Type: "agent", ID: agent, ProjectID: e.project, Status: "pending", Reason: "changed_concurrently"}, r.NotDone[0])
	assert.False(t, r.IdentityDeleted)
	assert.True(t, fpUserExists(t, e.store, fxID))
	got := fpAgent(t, e.store, agent)
	assert.True(t, got.DeletedAt.IsZero(), "the restored agent stays live")
}

// A finalizing row with no usable stored request: the claim derives a hard
// request (the purge's hard), so the row goes, but what the earlier
// teardown kept is unknown and is reported as retention_unknown, not as
// files_and_branch_retained.
func TestFixturePurge_FinalizingWithoutStoredRequestRetentionUnknown(t *testing.T) {
	e := newFPEnv(t, "fpp-finnoreq")
	fxID := e.fx.Identity.ID
	agent := fpFinalizingSoftAgent(t, e, "fpp-finnoreq-agent")
	got := fpAgent(t, e.store, agent)
	claim := got.DeletionClaim
	empty := ""
	n, err := e.store.UpdateAgentDeletion(context.Background(), agent,
		store.DeletionPredicate{Claim: &claim, States: []string{store.DeletionStateFinalizing}},
		store.DeletionFields{Request: &empty})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	rec := tiPurge(t, e.srv, e.issuerTok, fxID, "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	r := fpPurgeReport(t, rec)
	assert.True(t, r.Complete)
	assert.Equal(t, 1, r.Deleted.Agents)
	assert.Equal(t, []TestIdentityPurgeItem{{
		Type: "agent", ID: agent, ProjectID: e.project, Status: "removed", Reason: fixturePurgeRetentionUnknown,
	}}, r.FilesRetained)
	assert.False(t, fpAgentExists(t, e.store, agent))
	assert.False(t, fpUserExists(t, e.store, fxID))
}

// fixturePurgeFilesRetained maps the request a finalizing re-claim finished
// under to a filesRetained reason.
func TestFixturePurgeFilesRetained_Reasons(t *testing.T) {
	a := &store.Agent{ID: "agent-1", ProjectID: "project-1"}
	cases := []struct {
		name string
		plan agentDeletionPlan
		want string // "" for no item
	}{
		{"both removed", agentDeletionPlan{req: store.DeletionRequestInfo{DeleteFiles: true, RemoveBranch: true}}, ""},
		{"files kept", agentDeletionPlan{req: store.DeletionRequestInfo{RemoveBranch: true}}, "files_retained"},
		{"branch kept", agentDeletionPlan{req: store.DeletionRequestInfo{DeleteFiles: true}}, "branch_retained"},
		{"both kept", agentDeletionPlan{req: store.DeletionRequestInfo{Soft: true}}, "files_and_branch_retained"},
		{"derived request", agentDeletionPlan{fallback: true, req: store.DeletionRequestInfo{}}, fixturePurgeRetentionUnknown},
		{"derived request with flags", agentDeletionPlan{fallback: true, req: store.DeletionRequestInfo{DeleteFiles: true, RemoveBranch: true}}, fixturePurgeRetentionUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fixturePurgeFilesRetained(a, &tc.plan)
			if tc.want == "" {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, TestIdentityPurgeItem{Type: "agent", ID: "agent-1", ProjectID: "project-1", Status: "removed", Reason: tc.want}, *got)
		})
	}
}
