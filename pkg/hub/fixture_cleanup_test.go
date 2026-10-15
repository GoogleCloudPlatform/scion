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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the issuer-scoped cleanup and the provenance explain endpoint
// (ptone/scion#4240, Phase 3c). Every identity, user, project and agent
// here is synthetic, in an in-memory store.

const (
	fcCleanupPath    = "/api/v1/test-identities/cleanup"
	fcProvenancePath = "/api/v1/test-identities/provenance"
)

// fcShift moves the test identity clock d ahead, so identities issued with
// a 60 s lifetime are expired while those with the default lifetime (1 h)
// stay live.
func fcShift(srv *Server, d time.Duration) {
	srv.testIdentities.now = func() time.Time { return time.Now().Add(d) }
}

// fcIssueShort issues a member identity that expires after 60 s.
func fcIssueShort(t *testing.T, srv *Server, token string) TestIdentityTokenResponse {
	t.Helper()
	return tiIssue(t, srv, token, map[string]any{"role": "member", "lifetimeSeconds": 60})
}

// fcReport decodes a cleanup report from a 200 answer or from a 409
// purge_incomplete answer's details.
func fcReport(t *testing.T, rec *httptest.ResponseRecorder) TestIdentityCleanupReport {
	t.Helper()
	var r TestIdentityCleanupReport
	switch rec.Code {
	case http.StatusOK:
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &r), rec.Body.String())
	case http.StatusConflict:
		var env struct {
			Error struct {
				Details struct {
					Reason string                    `json:"reason"`
					Report TestIdentityCleanupReport `json:"report"`
				} `json:"details"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env), rec.Body.String())
		require.Equal(t, testIdentityReasonPurgeIncomplete, env.Error.Details.Reason, rec.Body.String())
		r = env.Error.Details.Report
	default:
		t.Fatalf("cleanup: unexpected %d: %s", rec.Code, rec.Body.String())
	}
	return r
}

// fcRef is one target as type:id.
func fcRef(typ, id string) string { return typ + ":" + id }

// fcTargets returns the targets of r with status (all when ""), as type:id.
func fcTargets(r TestIdentityCleanupReport, status string) []string {
	out := []string{}
	for _, tg := range r.Targets {
		if status == "" || tg.Status == status {
			out = append(out, fcRef(tg.Type, tg.ID))
		}
	}
	sort.Strings(out)
	return out
}

func fcIdentities(r TestIdentityCleanupReport) map[string]string {
	out := map[string]string{}
	for _, id := range r.Identities {
		out[id.ID] = id.State
	}
	return out
}

func fcGroupExists(t *testing.T, s store.Store, id string) bool {
	t.Helper()
	_, err := s.GetGroup(context.Background(), id)
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	require.NoError(t, err)
	return true
}

func fcScheduleExists(t *testing.T, s store.Store, id string) bool {
	t.Helper()
	_, err := s.GetSchedule(context.Background(), id)
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	require.NoError(t, err)
	return true
}

func fcSorted(in ...string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// AC 3c-1: cleanup by issuer I removes the stamped resources of I's
// expired identity and of I's deleted identity, and the expired identity
// itself; I's live identity and its resources, and every resource of
// issuer J, are untouched. J's own cleanup reaches J's. An unscoped
// platform admin session reaches every issuer, and still never a live
// identity.
func TestFixtureCleanup_IssuerIsolation(t *testing.T) {
	e := newFPEnv(t, "fc-iso")
	e.srv.SetDispatcher(&purgeRecDispatcher{})

	// I's identity that will expire: a project, an agent, a schedule, a group.
	f1 := fcIssueShort(t, e.srv, e.issuerTok)
	p1 := tiCreateProject(t, e.srv, f1.AccessToken, "fc-iso-p1")
	fpAddProvider(t, e.store, p1)
	a1 := fpCreateAgent(t, e.srv, f1.AccessToken, p1, "fc-iso-a1")
	s1 := fpCreateSchedule(t, e.srv, f1.AccessToken, p1, fpScheduleBody("fc-iso-s1"))
	g1 := fpCreateGroup(t, e.srv, f1.AccessToken, CreateGroupRequest{Name: "fc-iso-g1"})
	// I's identity deleted by a plain delete: its group is left behind.
	f0 := tiIssue(t, e.srv, e.issuerTok, map[string]any{"role": "member"})
	g0 := fpCreateGroup(t, e.srv, f0.AccessToken, CreateGroupRequest{Name: "fc-iso-g0"})
	require.Equal(t, http.StatusNoContent, doRequestWithToken(t, e.srv, e.issuerTok, http.MethodDelete, "/api/v1/test-identities/"+f0.Identity.ID, nil).Code)
	// I's live identity (the env's own): an agent in its project.
	aLive := fpCreateAgent(t, e.srv, e.fx.AccessToken, e.project, "fc-iso-live")

	// Issuer J's identity that will expire.
	_, jTok := tiIssuer(t, e.srv, e.store, "fc-iso-j")
	fj := fcIssueShort(t, e.srv, jTok)
	pj := tiCreateProject(t, e.srv, fj.AccessToken, "fc-iso-pj")
	fpAddProvider(t, e.store, pj)
	aj := fpCreateAgent(t, e.srv, fj.AccessToken, pj, "fc-iso-aj")
	// Issuer K's identity that will expire (for the admin session).
	_, kTok := tiIssuer(t, e.srv, e.store, "fc-iso-k")
	fk := fcIssueShort(t, e.srv, kTok)
	gk := fpCreateGroup(t, e.srv, fk.AccessToken, CreateGroupRequest{Name: "fc-iso-gk"})

	fcShift(e.srv, 5*time.Minute)

	rec := doRequestWithToken(t, e.srv, e.issuerTok, http.MethodPost, fcCleanupPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	r := fcReport(t, rec)
	assert.True(t, r.Complete)
	assert.Equal(t, fixtureCleanupScopeIssuer, r.Scope)
	assert.NotEmpty(t, r.RunID)
	assert.Equal(t, map[string]string{f1.Identity.ID: fixtureStateExpired, f0.Identity.ID: fixtureStateAbsent}, fcIdentities(r))
	assert.Equal(t, fcSorted(fcRef("agent", a1), fcRef("schedule", s1), fcRef("group", g1), fcRef("project", p1),
		fcRef("test_identity", f1.Identity.ID), fcRef("group", g0)), fcTargets(r, fixtureCleanupDeleted))
	assert.Equal(t, fcTargets(r, ""), fcTargets(r, fixtureCleanupDeleted), "every target was deleted")
	assert.Equal(t, 1, r.LiveSkipped, "I's live identity is counted, not touched")
	assert.Equal(t, TestIdentityPurgeCounts{Agents: 1, Schedules: 1, Groups: 2, Projects: 1}, r.Deleted)
	assert.Equal(t, 1, r.IdentitiesDeleted)

	assert.False(t, fpAgentExists(t, e.store, a1))
	assert.False(t, fcScheduleExists(t, e.store, s1))
	assert.False(t, fcGroupExists(t, e.store, g1))
	assert.False(t, fcGroupExists(t, e.store, g0))
	assert.False(t, fpProjectExists(t, e.store, p1))
	assert.False(t, fpUserExists(t, e.store, f1.Identity.ID))
	// A live identity of the same issuer is untouched.
	assert.True(t, fpUserExists(t, e.store, e.fx.Identity.ID))
	assert.True(t, fpAgentExists(t, e.store, aLive))
	assert.True(t, fpProjectExists(t, e.store, e.project))
	// Every resource of issuer J and K is untouched.
	assert.True(t, fpUserExists(t, e.store, fj.Identity.ID))
	assert.True(t, fpAgentExists(t, e.store, aj))
	assert.True(t, fpProjectExists(t, e.store, pj))
	assert.True(t, fcGroupExists(t, e.store, gk))

	// A second run is a no-op.
	rec = doRequestWithToken(t, e.srv, e.issuerTok, http.MethodPost, fcCleanupPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	r = fcReport(t, rec)
	assert.Empty(t, r.Identities)
	assert.Empty(t, r.Targets)

	// J's own cleanup reaches J's expired identity.
	rec = doRequestWithToken(t, e.srv, jTok, http.MethodPost, fcCleanupPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, map[string]string{fj.Identity.ID: fixtureStateExpired}, fcIdentities(fcReport(t, rec)))
	assert.False(t, fpAgentExists(t, e.store, aj))
	assert.False(t, fpProjectExists(t, e.store, pj))
	assert.True(t, fcGroupExists(t, e.store, gk), "J never reaches K's")

	// An unscoped platform admin session reaches every issuer (K here),
	// and still never a live identity.
	admin := taUser(t, e.store, "fc-iso-admin", store.SystemRoleSuperAdmin)
	rec = doRequestAsUser(t, e.srv, admin, http.MethodPost, fcCleanupPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	r = fcReport(t, rec)
	assert.Equal(t, fixtureCleanupScopeAll, r.Scope)
	assert.Equal(t, map[string]string{fk.Identity.ID: fixtureStateExpired}, fcIdentities(r))
	assert.False(t, fcGroupExists(t, e.store, gk))
	assert.True(t, fpUserExists(t, e.store, e.fx.Identity.ID))
	assert.True(t, fpAgentExists(t, e.store, aLive))
}

// AC 3c-2 and 3c-5: a dry run writes nothing (no row changes, no audit
// row) and lists exactly the targets the real run then deletes. The real
// run writes one test_identity_cleanup row whose correlation ID every
// per-resource delete row shares, and every delete row (agent, sub-agent,
// schedule, group, project, identity) carries a non-empty provenance_basis
// naming the identity, its issuer, the run and the cleanup trigger.
func TestFixtureCleanup_DryRunMatchesRunAndAudit(t *testing.T) {
	e := newFPEnv(t, "fc-dry")
	e.srv.SetDispatcher(&purgeRecDispatcher{})
	ctx := context.Background()
	f1 := fcIssueShort(t, e.srv, e.issuerTok)
	p1 := tiCreateProject(t, e.srv, f1.AccessToken, "fc-dry-p1")
	fpAddProvider(t, e.store, p1)
	a1 := fpCreateAgent(t, e.srv, f1.AccessToken, p1, "fc-dry-a1")
	sub := fpCreateSubAgent(t, e.srv, a1, p1, "fc-dry-sub")
	s1 := fpCreateSchedule(t, e.srv, f1.AccessToken, p1, fpScheduleBody("fc-dry-s1"))
	g1 := fpCreateGroup(t, e.srv, f1.AccessToken, CreateGroupRequest{Name: "fc-dry-g1"})
	fcShift(e.srv, 5*time.Minute)

	allAudits := func() []*store.MutationAuditRecord {
		recs, _, err := e.store.ListMutationAudits(ctx, store.MutationAuditFilter{Limit: 100000})
		require.NoError(t, err)
		return recs
	}
	auditsBefore := len(allAudits())

	rec := doRequestWithToken(t, e.srv, e.issuerTok, http.MethodPost, fcCleanupPath+"?dryRun=true", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	dry := fcReport(t, rec)
	assert.True(t, dry.DryRun)
	assert.False(t, dry.Complete)
	assert.Empty(t, dry.RunID)
	want := fcSorted(fcRef("agent", a1), fcRef("agent", sub), fcRef("schedule", s1), fcRef("group", g1), fcRef("project", p1), fcRef("test_identity", f1.Identity.ID))
	assert.Equal(t, want, fcTargets(dry, fixtureCleanupTarget))
	assert.Equal(t, want, fcTargets(dry, ""))
	// No writes.
	assert.Len(t, allAudits(), auditsBefore, "a dry run writes no audit row")
	assert.True(t, fpAgentExists(t, e.store, a1))
	assert.True(t, fpAgentExists(t, e.store, sub))
	assert.True(t, fcScheduleExists(t, e.store, s1))
	assert.True(t, fcGroupExists(t, e.store, g1))
	assert.True(t, fpProjectExists(t, e.store, p1))
	assert.True(t, fpUserExists(t, e.store, f1.Identity.ID))

	rec = doRequestWithToken(t, e.srv, e.issuerTok, http.MethodPost, fcCleanupPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	run := fcReport(t, rec)
	require.True(t, run.Complete)
	assert.Equal(t, want, fcTargets(run, fixtureCleanupDeleted), "the real run deletes exactly the dry run's targets")
	assert.Equal(t, want, fcTargets(run, ""))

	// The run row: exactly one, with the counts and the run ID.
	rows, _, err := e.store.ListMutationAudits(ctx, store.MutationAuditFilter{CorrelationID: run.RunID, Limit: 1000})
	require.NoError(t, err)
	var runRows []*store.MutationAuditRecord
	byType := map[string][]*store.MutationAuditRecord{}
	for _, row := range rows {
		byType[row.MutationType] = append(byType[row.MutationType], row)
		if row.MutationType == testIdentityCleanupMutation {
			runRows = append(runRows, row)
		}
	}
	require.Len(t, runRows, 1, "one test_identity_cleanup row per run")
	runRow := runRows[0]
	assert.Equal(t, testIdentityCleanupTargetType, runRow.TargetType)
	assert.Equal(t, e.issuerID, runRow.TargetID)
	assert.Equal(t, fixturePurgeExecutorCleanup, runRow.ExecutorKind)
	var counts struct {
		Scope      string                  `json:"scope"`
		Identities int                     `json:"identities"`
		Deleted    TestIdentityPurgeCounts `json:"deleted"`
		Complete   bool                    `json:"complete"`
	}
	require.NoError(t, json.Unmarshal([]byte(runRow.AfterSummary), &counts))
	assert.Equal(t, fixtureCleanupScopeIssuer, counts.Scope)
	assert.Equal(t, 1, counts.Identities)
	assert.Equal(t, TestIdentityPurgeCounts{Agents: 2, Schedules: 1, Groups: 1, Projects: 1}, counts.Deleted)
	assert.True(t, counts.Complete)
	var runBasis fixtureCleanupBasis
	require.NoError(t, json.Unmarshal([]byte(runRow.ProvenanceBasis), &runBasis), runRow.ProvenanceBasis)
	assert.Equal(t, fixtureCleanupBasis{Basis: fixtureCleanupBasisName, Scope: fixtureCleanupScopeIssuer, IssuerID: e.issuerID,
		RunID: run.RunID, Trigger: testIdentityCleanupMutation}, runBasis)

	// Every delete row of the run carries the correlation ID and a
	// provenance_basis naming the identity, its issuer, the run and the
	// trigger; there is one per target.
	wantRows := map[string][]string{
		mutationTypeAgentHardDelete:     {a1, sub},
		mutationTypeScheduleDelete:      {s1},
		fixturePurgeGroupDeleteMutation: {g1},
		"project_delete":                {p1},
		testIdentityPurgeMutation:       {f1.Identity.ID},
	}
	for typ, ids := range wantRows {
		got := []string{}
		for _, row := range byType[typ] {
			got = append(got, row.TargetID)
			require.NotEmpty(t, row.ProvenanceBasis, "%s %s has no provenance_basis", typ, row.TargetID)
			var b fixturePurgeBasis
			require.NoError(t, json.Unmarshal([]byte(row.ProvenanceBasis), &b), row.ProvenanceBasis)
			assert.Equal(t, fixturePurgeBasis{RootID: f1.Identity.ID, IssuerID: e.issuerID, Basis: fixturePurgeBasisName,
				RunID: run.RunID, Trigger: testIdentityCleanupMutation}, b, "%s %s", typ, row.TargetID)
			assert.Equal(t, run.RunID, row.CorrelationID)
		}
		assert.ElementsMatch(t, ids, got, "%s rows of the run", typ)
	}
	require.Len(t, byType[testIdentityPurgeMutation], 1)
	assert.Equal(t, fixturePurgeExecutorCleanup, byType[testIdentityPurgeMutation][0].ExecutorKind)
}

// AC 3c-5: a failed audit write aborts that resource's delete. With the
// audit write of one agent and of the schedule failing, those two stay,
// with no audit row, are listed not_done, and keep the project and the
// identity; everything else is deleted. With the identity row's audit
// write failing, the identity stays. A run without the fault finishes.
func TestFixtureCleanup_FailedAuditAbortsDelete(t *testing.T) {
	e := newFPEnv(t, "fc-fault")
	e.srv.SetDispatcher(&purgeRecDispatcher{})
	f1 := fcIssueShort(t, e.srv, e.issuerTok)
	p1 := tiCreateProject(t, e.srv, f1.AccessToken, "fc-fault-p1")
	fpAddProvider(t, e.store, p1)
	aFail := fpCreateAgent(t, e.srv, f1.AccessToken, p1, "fc-fault-a")
	aOK := fpCreateAgent(t, e.srv, f1.AccessToken, p1, "fc-fault-b")
	sFail := fpCreateSchedule(t, e.srv, f1.AccessToken, p1, fpScheduleBody("fc-fault-s"))
	fcShift(e.srv, 5*time.Minute)

	failFor := map[string]bool{aFail: true, sFail: true}
	fixturePurgeAuditFault = func(r *store.MutationAuditRecord) error {
		if failFor[r.TargetID] {
			return errors.New("injected audit write fault")
		}
		return nil
	}
	t.Cleanup(func() { fixturePurgeAuditFault = nil })

	rec := doRequestWithToken(t, e.srv, e.issuerTok, http.MethodPost, fcCleanupPath, nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	r := fcReport(t, rec)
	assert.False(t, r.Complete)
	status := map[string]string{}
	for _, tg := range r.Targets {
		status[fcRef(tg.Type, tg.ID)] = tg.Status
	}
	assert.Equal(t, fixtureCleanupNotDone, status[fcRef("agent", aFail)])
	assert.Equal(t, fixtureCleanupNotDone, status[fcRef("schedule", sFail)])
	assert.Equal(t, fixtureCleanupDeleted, status[fcRef("agent", aOK)])
	assert.Equal(t, fixtureCleanupNotDone, status[fcRef("test_identity", f1.Identity.ID)])
	assert.True(t, fpAgentExists(t, e.store, aFail), "the agent whose audit write failed stays")
	assert.True(t, fcScheduleExists(t, e.store, sFail), "the schedule whose audit write failed stays")
	assert.False(t, fpAgentExists(t, e.store, aOK))
	assert.True(t, fpUserExists(t, e.store, f1.Identity.ID))
	assert.Empty(t, fpPurgeAudits(t, e.store, mutationTypeAgentHardDelete, aFail), "no audit row for the aborted delete")
	assert.Empty(t, fpPurgeAudits(t, e.store, mutationTypeScheduleDelete, sFail))

	// The identity row's audit write fails: everything else goes, the
	// identity stays.
	failFor = map[string]bool{f1.Identity.ID: true}
	for i := 0; i < 3 && (fpAgentExists(t, e.store, aFail) || fcScheduleExists(t, e.store, sFail) || fpProjectExists(t, e.store, p1)); i++ {
		rec = doRequestWithToken(t, e.srv, e.issuerTok, http.MethodPost, fcCleanupPath, nil)
		require.Contains(t, []int{http.StatusOK, http.StatusConflict}, rec.Code, rec.Body.String())
	}
	assert.False(t, fpAgentExists(t, e.store, aFail))
	assert.False(t, fcScheduleExists(t, e.store, sFail))
	assert.False(t, fpProjectExists(t, e.store, p1))
	assert.True(t, fpUserExists(t, e.store, f1.Identity.ID), "the identity whose audit write failed stays")
	assert.Empty(t, fpPurgeAudits(t, e.store, testIdentityPurgeMutation, f1.Identity.ID))

	// Without the fault the run finishes.
	failFor = map[string]bool{}
	rec = doRequestWithToken(t, e.srv, e.issuerTok, http.MethodPost, fcCleanupPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, fcReport(t, rec).Complete)
	assert.False(t, fpUserExists(t, e.store, f1.Identity.ID))
	assert.Len(t, fpPurgeAudits(t, e.store, testIdentityPurgeMutation, f1.Identity.ID), 1)
}

// AC 3c-1 with workload delegations (X2, test-ident-lead): a cleanup
// authenticated by delegation D1 reaches only the identities issued
// through D1, expired or deleted; D2's expired identity, the human
// issuer's own expired identity (same grantor) and D1's live identity are
// untouched. D2's cleanup then reaches only D2's, and the grantor's own
// session reaches its own and the D-issued leftovers. A delegation's dry
// run shows the same bound.
func TestFixtureCleanup_DelegationIsolation(t *testing.T) {
	e := newX2Env(t, true)
	e.srv.SetDispatcher(&purgeRecDispatcher{})
	fpAllowMembersToCreateGroups(t, e.s)
	grantor, _ := e.bindingGrantor(t, "fc-del-grantor")
	d1 := e.delegate(t, grantor, nil)
	d2 := e.delegate(t, grantor, nil)
	tok := func() string { return x2Token(t, x2TokenOpts{}) }
	mintWith := func(delegationID string, body map[string]any) TestIdentityTokenResponse {
		rec := e.workload(t, http.MethodPost, "/api/v1/test-identities", tok(), delegationID, body, nil)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		return tiDecode(t, rec)
	}
	short := map[string]any{"role": "member", "lifetimeSeconds": 60}
	withGroup := func(fx TestIdentityTokenResponse, name string) string {
		return fpCreateGroup(t, e.srv, fx.AccessToken, CreateGroupRequest{Name: name})
	}

	d1Expired := mintWith(d1.ID, short)
	d1ExpiredGroup := withGroup(d1Expired, "fc-del-d1-expired")
	d1Gone := mintWith(d1.ID, map[string]any{"role": "member"})
	d1GoneGroup := withGroup(d1Gone, "fc-del-d1-gone")
	require.Equal(t, http.StatusNoContent, e.workload(t, http.MethodDelete, "/api/v1/test-identities/"+d1Gone.Identity.ID, tok(), d1.ID, nil, nil).Code)
	d1Live := mintWith(d1.ID, map[string]any{"role": "member"})
	d1LiveGroup := withGroup(d1Live, "fc-del-d1-live")
	d2Expired := mintWith(d2.ID, short)
	d2ExpiredGroup := withGroup(d2Expired, "fc-del-d2-expired")
	d2Gone := mintWith(d2.ID, map[string]any{"role": "member"})
	d2GoneGroup := withGroup(d2Gone, "fc-del-d2-gone")
	require.Equal(t, http.StatusNoContent, e.workload(t, http.MethodDelete, "/api/v1/test-identities/"+d2Gone.Identity.ID, tok(), d2.ID, nil, nil).Code)
	rec := doRequestAsUser(t, e.srv, grantor, http.MethodPost, "/api/v1/test-identities", short)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	human := tiDecode(t, rec)
	humanGroup := withGroup(human, "fc-del-human")

	fcShift(e.srv, 5*time.Minute)

	// D1's dry run, then its run.
	rec = e.workload(t, http.MethodPost, fcCleanupPath+"?dryRun=true", tok(), d1.ID, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	dry := fcReport(t, rec)
	assert.Equal(t, fixtureCleanupScopeDelegation, dry.Scope)
	assert.Equal(t, map[string]string{d1Expired.Identity.ID: fixtureStateExpired, d1Gone.Identity.ID: fixtureStateAbsent}, fcIdentities(dry))

	rec = e.workload(t, http.MethodPost, fcCleanupPath, tok(), d1.ID, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	r := fcReport(t, rec)
	assert.True(t, r.Complete)
	assert.Equal(t, map[string]string{d1Expired.Identity.ID: fixtureStateExpired, d1Gone.Identity.ID: fixtureStateAbsent}, fcIdentities(r))
	assert.Equal(t, fcTargets(dry, ""), fcTargets(r, fixtureCleanupDeleted))
	assert.False(t, fcGroupExists(t, e.s, d1ExpiredGroup))
	assert.False(t, fcGroupExists(t, e.s, d1GoneGroup))
	assert.False(t, fpUserExists(t, e.s, d1Expired.Identity.ID))
	// Untouched: D1's live identity, D2's, the human issuer's.
	assert.True(t, fcGroupExists(t, e.s, d1LiveGroup))
	assert.True(t, fpUserExists(t, e.s, d1Live.Identity.ID))
	assert.True(t, fcGroupExists(t, e.s, d2ExpiredGroup))
	assert.True(t, fpUserExists(t, e.s, d2Expired.Identity.ID))
	assert.True(t, fcGroupExists(t, e.s, d2GoneGroup))
	assert.True(t, fcGroupExists(t, e.s, humanGroup))
	assert.True(t, fpUserExists(t, e.s, human.Identity.ID))
	// The run row names the delegation.
	rows, _, err := e.s.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: testIdentityCleanupMutation, CorrelationID: r.RunID})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	var b fixtureCleanupBasis
	require.NoError(t, json.Unmarshal([]byte(rows[0].ProvenanceBasis), &b))
	assert.Equal(t, fixtureCleanupScopeDelegation, b.Scope)
	assert.Equal(t, d1.ID, b.DelegationID)
	assert.Equal(t, grantor.ID, b.IssuerID)

	// Provenance under a delegation is scoped the same way (R1, R3): D1
	// proves its own live group and its own deleted one (by the deletion
	// record); D2's and the grantor's own resources, and D1's deleted one
	// asked by D2, get the neutral unproven answer.
	wprov := func(delegationID, res string) (TestIdentityProvenance, map[string]any) {
		rec := e.workload(t, http.MethodGet, fcProvenancePath+"?resource="+res, tok(), delegationID, nil, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var p TestIdentityProvenance
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
		var raw map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
		return p, raw
	}
	p, _ := wprov(d1.ID, "group:"+d1LiveGroup)
	assert.True(t, p.Proven)
	assert.False(t, p.Deleted)
	p, _ = wprov(d1.ID, "group:"+d1ExpiredGroup)
	assert.True(t, p.Proven)
	assert.True(t, p.Deleted)
	assert.Equal(t, r.RunID, p.DeletionRecord)
	for _, tc := range []struct{ delegation, res string }{
		{d2.ID, "group:" + d1ExpiredGroup},
		{d2.ID, "group:" + d1LiveGroup},
		{d1.ID, "group:" + d2ExpiredGroup},
		{d1.ID, "group:" + humanGroup},
	} {
		_, raw := wprov(tc.delegation, tc.res)
		fcUnprovenShape(t, raw, tc.res)
	}
	rec = doRequestAsUser(t, e.srv, grantor, http.MethodGet, fcProvenancePath+"?resource=group:"+d1LiveGroup, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"proven":true`, "the grantor's own session sees its delegations' stamps")

	// D2's cleanup reaches only D2's.
	rec = e.workload(t, http.MethodPost, fcCleanupPath, tok(), d2.ID, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, map[string]string{d2Expired.Identity.ID: fixtureStateExpired, d2Gone.Identity.ID: fixtureStateAbsent}, fcIdentities(fcReport(t, rec)))
	assert.False(t, fcGroupExists(t, e.s, d2ExpiredGroup))
	assert.False(t, fcGroupExists(t, e.s, d2GoneGroup))
	assert.True(t, fcGroupExists(t, e.s, humanGroup))
	assert.True(t, fcGroupExists(t, e.s, d1LiveGroup))

	// The grantor's own session reaches its own expired identity.
	rec = doRequestAsUser(t, e.srv, grantor, http.MethodPost, fcCleanupPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, map[string]string{human.Identity.ID: fixtureStateExpired}, fcIdentities(fcReport(t, rec)))
	assert.False(t, fcGroupExists(t, e.s, humanGroup))
	assert.True(t, fcGroupExists(t, e.s, d1LiveGroup), "a live identity is never touched")
}

// Admin-kind test identities (test_hub_admin, test_super_admin) never
// clean up and never read provenance: the route guard refuses both routes
// on their permission (test_identity.issue, deny for every admin kind)
// with the guard's structured 403, and the decision names the test-admin
// policy. Control: the human issuer's session gets 200 on both. The
// issuer's purge of the admin-kind identity writes provenance_basis on
// every grant row it removes (A3).
func TestFixtureCleanup_AdminKindCallersRefused(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	kinds, issuers := a3Kinds(t, srv, s, "fc-ak")
	target := kinds[store.UserKindTestHubAdmin]
	projectID := tiCreateProject(t, srv, target.AccessToken, "fc-ak-project")
	grantee := taUser(t, s, "fc-ak-grantee", store.SystemRoleHubMember)
	rec := doRequestWithToken(t, srv, target.AccessToken, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{RoleDefinitionID: a3ProjectRoleID(t, s, store.ProjectRoleAdmin), PrincipalType: "user", PrincipalID: grantee.ID})
	require.Less(t, rec.Code, 300, rec.Body.String())

	for kind, caller := range kinds {
		for _, req := range []struct{ method, path string }{
			{http.MethodPost, fcCleanupPath},
			{http.MethodPost, fcCleanupPath + "?dryRun=true"},
			{http.MethodGet, fcProvenancePath + "?resource=project:" + projectID},
		} {
			rec := doRequestWithToken(t, srv, caller.AccessToken, req.method, req.path, nil)
			require.Equal(t, http.StatusForbidden, rec.Code, "%s %s %s: %s", kind, req.method, req.path, rec.Body.String())
			var body struct {
				Error struct {
					Code    string            `json:"code"`
					Details map[string]string `json:"details"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
			assert.Equal(t, ErrCodeForbidden, body.Error.Code)
			assert.Equal(t, map[string]string{"resource_type": "test_identity", "denied_action": "issue"}, body.Error.Details, "%s: refused on the route's permission", kind)
		}
		d := srv.authzService.Decide(ctx, AuthzRequest{Principal: principalContextForIdentity(tsaIdentity(t, s, caller.Identity.ID)),
			Credential: credentialContextForIdentity(tsaIdentity(t, s, caller.Identity.ID)), Resource: Resource{Type: "test_identity"},
			Action: ActionIssue, Permission: permissionTestIdentityIssue, TargetEvidence: hubCollectionEvidence(permissionTestIdentityIssue)})
		assert.False(t, d.Allowed)
		assert.Equal(t, DeniedByTestAdminPolicy, d.DeniedBy, kind)
	}
	assert.True(t, fpProjectExists(t, s, projectID), "nothing was removed")

	// Control: the human issuer's session.
	issuer := issuers[store.UserKindTestHubAdmin]
	rec = doRequestAsUser(t, srv, issuer, http.MethodPost, fcCleanupPath+"?dryRun=true", nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, srv, issuer, http.MethodGet, fcProvenancePath+"?resource=project:"+projectID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var p TestIdentityProvenance
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	assert.True(t, p.Proven)
	assert.Equal(t, target.Identity.ID, p.FixtureRootID)

	// The issuer's purge removes the stamped grant with provenance_basis.
	row := a3Row(t, s, target.Identity.ID)
	rec = doRequestAsUser(t, srv, issuer, http.MethodDelete, "/api/v1/test-identities/"+target.Identity.ID+"?purge=true", nil)
	require.True(t, fpPurgeReport(t, rec).IdentityDeleted, rec.Body.String())
	grantRows, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testAdminGrantPurgeMutation, Limit: 100})
	require.NoError(t, err)
	require.NotEmpty(t, grantRows)
	for _, gr := range grantRows {
		var b fixturePurgeBasis
		require.NoError(t, json.Unmarshal([]byte(gr.ProvenanceBasis), &b), "grant row %s: %q", gr.TargetID, gr.ProvenanceBasis)
		assert.Equal(t, target.Identity.ID, b.RootID)
		assert.Equal(t, *row.IssuanceID, b.IssuanceID)
	}
	for _, typ := range []string{testIdentityPurgeMutation, "project_delete"} {
		for _, r := range fpPurgeAudits(t, s, typ, "") {
			if r.TargetID == target.Identity.ID || r.TargetID == projectID {
				assert.NotEmpty(t, r.ProvenanceBasis, "%s %s", typ, r.TargetID)
			}
		}
	}
}

// AC 3c-4: provenance on a real user's resource answers proven:false,
// reason "no fixture stamp", with no owner, creator or other field, and a
// missing resource gets the same answer; a stamped resource is proven with
// its stamp and its identity's state. 403 without test_identity.issue
// (a member, a test identity), 404 with the flag off; a malformed request
// is 400.
func TestFixtureProvenance_Route(t *testing.T) {
	e := newFPEnv(t, "fc-prov")
	broker := fpProjectBrokerID(t, e.store, e.project)
	realAgent := fpStampedAgentRow(t, e.store, e.project, broker, "fc-prov-real", e.human.ID, []string{e.human.ID}, store.FixtureProvenance{}, state.PhaseCreated)
	stamped := fpCreateAgent(t, e.srv, e.fx.AccessToken, e.project, "fc-prov-fx")
	groupID := fpCreateGroup(t, e.srv, e.fx.AccessToken, CreateGroupRequest{Name: "fc-prov-group"})
	scheduleID := fpCreateSchedule(t, e.srv, e.fx.AccessToken, e.project, fpScheduleBody("fc-prov-s"))

	get := func(token, query string) *httptest.ResponseRecorder {
		return doRequestWithToken(t, e.srv, token, http.MethodGet, fcProvenancePath+query, nil)
	}
	raw := func(rec *httptest.ResponseRecorder) map[string]any {
		var m map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &m), rec.Body.String())
		return m
	}

	// A real user's agent: unproven, with only resource, proven and reason.
	rec := get(e.issuerTok, "?resource=agent:"+realAgent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, map[string]any{"resource": "agent:" + realAgent.ID, "proven": false, "reason": "no fixture stamp"}, raw(rec))
	assert.NotContains(t, rec.Body.String(), e.human.ID, "no owner or creator data")
	// A missing agent and a missing project: the same answer.
	missing := generateID()
	for _, typ := range []string{"agent", "project", "schedule", "group"} {
		rec = get(e.issuerTok, "?resource="+typ+":"+missing)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, map[string]any{"resource": typ + ":" + missing, "proven": false, "reason": "no fixture stamp"}, raw(rec))
	}
	rec = get(e.issuerTok, "?resource=agent:not-a-uuid")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, false, raw(rec)["proven"])

	// Stamped resources: proven, with the stamp and the identity's state.
	for _, res := range []string{"agent:" + stamped, "project:" + e.project, "group:" + groupID, "schedule:" + scheduleID} {
		rec = get(e.issuerTok, "?resource="+res)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var p TestIdentityProvenance
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
		assert.Equal(t, TestIdentityProvenance{Resource: res, Proven: true, FixtureRootID: e.fx.Identity.ID, FixtureIssuerID: e.issuerID,
			FixtureState: fixtureStateLive, Reason: "fixture stamp"}, p, res)
	}
	// A deleted identity's leftover is still proven, its identity absent.
	gone := tiIssue(t, e.srv, e.issuerTok, map[string]any{"role": "member"})
	goneGroup := fpCreateGroup(t, e.srv, gone.AccessToken, CreateGroupRequest{Name: "fc-prov-gone"})
	require.Equal(t, http.StatusNoContent, doRequestWithToken(t, e.srv, e.issuerTok, http.MethodDelete, "/api/v1/test-identities/"+gone.Identity.ID, nil).Code)
	rec = get(e.issuerTok, "?resource=group:"+goneGroup)
	assert.Equal(t, map[string]any{"resource": "group:" + goneGroup, "proven": true, "fixtureRootId": gone.Identity.ID,
		"fixtureIssuerId": e.issuerID, "fixtureState": fixtureStateAbsent, "reason": "fixture stamp"}, raw(rec))
	// An expired identity's resource: the state follows the clock.
	fcShift(e.srv, 2*time.Hour)
	rec = get(e.issuerTok, "?resource=agent:"+stamped)
	assert.Equal(t, fixtureStateExpired, raw(rec)["fixtureState"])
	fcShift(e.srv, 0)

	// Malformed requests.
	for _, q := range []string{"", "?resource=", "?resource=agent", "?resource=agent:", "?resource=user:" + missing,
		"?resource=agent:a/b", "?resource=agent:" + stamped + "&resource=agent:" + stamped, "?resource=agent:" + stamped + "&owner=x"} {
		rec = get(e.issuerTok, q)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%q: %s", q, rec.Body.String())
	}

	// 403 without test_identity.issue: a hub member, and a test identity.
	rec = get(e.humanTok, "?resource=agent:"+stamped)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	fx2 := tiIssue(t, e.srv, e.issuerTok, map[string]any{"role": "member"})
	rec = get(fx2.AccessToken, "?resource=agent:"+stamped)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// 404 with the flag off, for every caller.
	off, offStore := newTestIdentityServer(t, false)
	_, offTok := tiIssuer(t, off, offStore, "fc-prov-off")
	for _, req := range []struct{ method, path string }{
		{http.MethodGet, fcProvenancePath + "?resource=agent:" + stamped},
		{http.MethodPost, fcCleanupPath},
		{http.MethodPost, fcCleanupPath + "?dryRun=true"},
	} {
		rec = doRequestWithToken(t, off, offTok, req.method, req.path, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s %s: %s", req.method, req.path, rec.Body.String())
	}
}

// The cleanup request names nothing: a body, any query parameter other
// than dryRun, or an invalid dryRun is 400 and removes nothing; a caller
// without test_identity.issue gets 403.
func TestFixtureCleanup_RequestCannotNameTargets(t *testing.T) {
	e := newFPEnv(t, "fc-req")
	e.srv.SetDispatcher(&purgeRecDispatcher{})
	f1 := fcIssueShort(t, e.srv, e.issuerTok)
	g1 := fpCreateGroup(t, e.srv, f1.AccessToken, CreateGroupRequest{Name: "fc-req-g1"})
	fcShift(e.srv, 5*time.Minute)

	for _, tc := range []struct {
		query string
		body  any
	}{
		{"", map[string]any{"ids": []string{g1}}},
		{"", map[string]any{}},
		{"?id=" + g1, nil},
		{"?dryRun=maybe", nil},
		{"?dryRun=true&dryRun=false", nil},
		{"?issuer=" + e.issuerID, nil},
	} {
		rec := doRequestWithToken(t, e.srv, e.issuerTok, http.MethodPost, fcCleanupPath+tc.query, tc.body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%q %v: %s", tc.query, tc.body, rec.Body.String())
	}
	assert.True(t, fcGroupExists(t, e.store, g1))

	rec := doRequestWithToken(t, e.srv, e.humanTok, http.MethodPost, fcCleanupPath, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doRequestWithToken(t, e.srv, e.fx.AccessToken, http.MethodPost, fcCleanupPath, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.True(t, fcGroupExists(t, e.store, g1))
}

// fcProvenance asks for res with token and decodes the answer and its raw
// key set.
func fcProvenance(t *testing.T, srv *Server, token, res string) (TestIdentityProvenance, map[string]any) {
	t.Helper()
	rec := doRequestWithToken(t, srv, token, http.MethodGet, fcProvenancePath+"?resource="+res, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var p TestIdentityProvenance
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p), rec.Body.String())
	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	return p, raw
}

// fcUnprovenShape asserts the neutral unproven answer: exactly resource,
// proven:false and the reason.
func fcUnprovenShape(t *testing.T, raw map[string]any, res string) {
	t.Helper()
	assert.Equal(t, map[string]any{"resource": res, "proven": false, "reason": "no fixture stamp"}, raw)
}

// R1 and R3 (review round 1): provenance is scoped like the cleanup.
// Another issuer's stamped resource gets the neutral unproven answer, with
// no stamp fields; the issuer's own is proven. A resource that is gone is
// proven by its deletion record when that record's provenance_basis is in
// scope, for a purge-deleted and a reaper-deleted resource alike, and is
// unproven for another issuer; a never-stamped resource and a missing one
// stay unproven.
func TestFixtureProvenance_ScopeAndDeletionRecords(t *testing.T) {
	e := newFPEnv(t, "fc-scope")
	e.srv.SetDispatcher(&purgeRecDispatcher{})
	ctx := context.Background()
	_, jTok := tiIssuer(t, e.srv, e.store, "fc-scope-j")
	mine := fpCreateAgent(t, e.srv, e.fx.AccessToken, e.project, "fc-scope-mine")
	fj := tiIssue(t, e.srv, jTok, map[string]any{"role": "member"})
	jGroup := fpCreateGroup(t, e.srv, fj.AccessToken, CreateGroupRequest{Name: "fc-scope-j"})

	// Live stamps: proven for their own issuer only.
	p, _ := fcProvenance(t, e.srv, e.issuerTok, "agent:"+mine)
	assert.True(t, p.Proven)
	assert.Equal(t, e.fx.Identity.ID, p.FixtureRootID)
	_, raw := fcProvenance(t, e.srv, jTok, "agent:"+mine)
	fcUnprovenShape(t, raw, "agent:"+mine)
	_, raw = fcProvenance(t, e.srv, e.issuerTok, "group:"+jGroup)
	fcUnprovenShape(t, raw, "group:"+jGroup)
	p, _ = fcProvenance(t, e.srv, jTok, "group:"+jGroup)
	assert.True(t, p.Proven)
	assert.Equal(t, fj.Identity.ID, p.FixtureRootID)

	// Purge-deleted: I purges F2; its agent is proven by its deletion
	// record for I, unproven for J.
	f2 := tiIssue(t, e.srv, e.issuerTok, map[string]any{"role": "member"})
	p2 := tiCreateProject(t, e.srv, f2.AccessToken, "fc-scope-p2")
	fpAddProvider(t, e.store, p2)
	purged := fpCreateAgent(t, e.srv, f2.AccessToken, p2, "fc-scope-purged")
	purgedSchedule := fpCreateSchedule(t, e.srv, f2.AccessToken, p2, fpScheduleBody("fc-scope-purged-s"))
	rec := tiPurge(t, e.srv, e.issuerTok, f2.Identity.ID, "", nil)
	require.True(t, fpPurgeReport(t, rec).Complete, rec.Body.String())
	require.False(t, fpAgentExists(t, e.store, purged))
	rows := fpPurgeAudits(t, e.store, mutationTypeAgentHardDelete, purged)
	require.Len(t, rows, 1)
	p, raw = fcProvenance(t, e.srv, e.issuerTok, "agent:"+purged)
	assert.Equal(t, TestIdentityProvenance{Resource: "agent:" + purged, Proven: true, FixtureRootID: f2.Identity.ID, FixtureIssuerID: e.issuerID,
		FixtureState: fixtureStateAbsent, Deleted: true, DeletionRecord: rows[0].CorrelationID, Reason: "deleted under fixture stamp"}, p)
	assert.NotEmpty(t, raw["deletionRecord"])
	_, raw = fcProvenance(t, e.srv, jTok, "agent:"+purged)
	fcUnprovenShape(t, raw, "agent:"+purged)
	// The schedule and the project the purge removed (O3): proven by
	// their own deletion records for I, unproven for J.
	require.False(t, fcScheduleExists(t, e.store, purgedSchedule))
	require.False(t, fpProjectExists(t, e.store, p2))
	for _, tc := range []struct{ typ, id, mutation string }{
		{"schedule", purgedSchedule, mutationTypeScheduleDelete},
		{"project", p2, "project_delete"},
	} {
		del := fpPurgeAudits(t, e.store, tc.mutation, tc.id)
		require.Len(t, del, 1, tc.typ)
		p, _ = fcProvenance(t, e.srv, e.issuerTok, tc.typ+":"+tc.id)
		assert.Equal(t, TestIdentityProvenance{Resource: tc.typ + ":" + tc.id, Proven: true, FixtureRootID: f2.Identity.ID, FixtureIssuerID: e.issuerID,
			FixtureState: fixtureStateAbsent, Deleted: true, DeletionRecord: del[0].CorrelationID, Reason: "deleted under fixture stamp"}, p, tc.typ)
		_, raw = fcProvenance(t, e.srv, jTok, tc.typ+":"+tc.id)
		fcUnprovenShape(t, raw, tc.typ+":"+tc.id)
	}

	// Reaper-deleted: F3 expires and the reaper purges it after the grace
	// period; its group is proven by its deletion record.
	f3 := fcIssueShort(t, e.srv, e.issuerTok)
	reaped := fpCreateGroup(t, e.srv, f3.AccessToken, CreateGroupRequest{Name: "fc-scope-reaped"})
	clock := f3.Identity.ExpiresAt.Add(testIdentityReaperGrace + time.Second)
	e.srv.testIdentities.now = func() time.Time { return clock }
	res, err := e.srv.reapTestIdentities(ctx, clock)
	require.NoError(t, err)
	require.Contains(t, res.Purged, f3.Identity.ID)
	require.False(t, fcGroupExists(t, e.store, reaped))
	groupRows := fpPurgeAudits(t, e.store, fixturePurgeGroupDeleteMutation, reaped)
	require.Len(t, groupRows, 1)
	p, _ = fcProvenance(t, e.srv, e.issuerTok, "group:"+reaped)
	assert.True(t, p.Proven)
	assert.True(t, p.Deleted)
	assert.Equal(t, groupRows[0].CorrelationID, p.DeletionRecord)
	assert.Equal(t, f3.Identity.ID, p.FixtureRootID)
	_, raw = fcProvenance(t, e.srv, jTok, "group:"+reaped)
	fcUnprovenShape(t, raw, "group:"+reaped)

	// Never stamped, and missing: unproven for everyone.
	broker := fpProjectBrokerID(t, e.store, e.project)
	never := fpStampedAgentRow(t, e.store, e.project, broker, "fc-scope-never", e.human.ID, []string{e.human.ID}, store.FixtureProvenance{}, state.PhaseCreated)
	_, raw = fcProvenance(t, e.srv, e.issuerTok, "agent:"+never.ID)
	fcUnprovenShape(t, raw, "agent:"+never.ID)
	missing := generateID()
	_, raw = fcProvenance(t, e.srv, e.issuerTok, "agent:"+missing)
	fcUnprovenShape(t, raw, "agent:"+missing)

	// An admin session sees every issuer's stamps and deletion records.
	admin := taUser(t, e.store, "fc-scope-admin", store.SystemRoleSuperAdmin)
	for _, res := range []string{"group:" + jGroup, "agent:" + purged} {
		rec := doRequestAsUser(t, e.srv, admin, http.MethodGet, fcProvenancePath+"?resource="+res, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var ap TestIdentityProvenance
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ap))
		assert.True(t, ap.Proven, res)
	}
}

// fcStoreFixtureWithGroup writes a test identity with the given ID and
// expiry for issuer, and one group stamped with it, through the store.
func fcStoreFixtureWithGroup(t *testing.T, s store.Store, id, issuer string, expiresAt time.Time) string {
	t.Helper()
	ctx := context.Background()
	u := &store.User{
		ID: id, Email: "test-identity-" + id[:8] + id[len(id)-4:] + "@" + store.TestFixtureEmailDomain, DisplayName: "Stored fixture",
		Role: store.UserRoleMember, Status: store.UserStatusActive, Kind: store.UserKindTestFixture,
		ExpiresAt: &expiresAt, IssuedBy: &issuer, Created: time.Now(),
	}
	require.NoError(t, s.CreateTestFixtureUser(ctx, u))
	gid := generateID()
	require.NoError(t, s.CreateGroup(ctx, &store.Group{ID: gid, Name: "fc-lim-" + gid[:8], Slug: "fc-lim-" + gid[:8], CreatedBy: id,
		Fixture: store.FixtureProvenance{RootID: id, IssuerID: issuer}}))
	return gid
}

// R2 (review round 1): the run limit counts only admitted, non-live
// identities. With more than fixtureCleanupRootLimit live identities of the
// issuer whose roots all sort before an expired one's, the run still
// reaches the expired identity and completes; live identities are counted,
// never touched. With the limit lowered to 1 and two expired identities,
// the first run is truncated after one and the second finishes.
func TestFixtureCleanup_LimitCountsOnlyAdmittedNonLive(t *testing.T) {
	e := newFPEnv(t, "fc-lim")
	e.srv.SetDispatcher(&purgeRecDispatcher{})
	now := time.Now()
	nLive := fixtureCleanupRootLimit + 5
	liveGroups := make([]string, 0, nLive)
	for i := 0; i < nLive; i++ {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		liveGroups = append(liveGroups, fcStoreFixtureWithGroup(t, e.store, id, e.issuerID, now.Add(time.Hour)))
	}
	lastExpired := fcStoreFixtureWithGroup(t, e.store, "ffffffff-ffff-4fff-bfff-ffffffffffff", e.issuerID, now.Add(-time.Minute))

	rec := doRequestWithToken(t, e.srv, e.issuerTok, http.MethodPost, fcCleanupPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	r := fcReport(t, rec)
	assert.True(t, r.Complete)
	assert.False(t, r.Truncated)
	assert.Equal(t, map[string]string{"ffffffff-ffff-4fff-bfff-ffffffffffff": fixtureStateExpired}, fcIdentities(r))
	assert.Equal(t, nLive+1, r.LiveSkipped, "the stored live identities and the env's own")
	assert.False(t, fcGroupExists(t, e.store, lastExpired))
	for _, g := range liveGroups {
		require.True(t, fcGroupExists(t, e.store, g))
	}

	// A lowered limit: two expired identities take two runs.
	prev := fixtureCleanupRootLimit
	fixtureCleanupRootLimit = 1
	t.Cleanup(func() { fixtureCleanupRootLimit = prev })
	ga := fcStoreFixtureWithGroup(t, e.store, "eeeeeeee-eeee-4eee-beee-eeeeeeeeeee1", e.issuerID, now.Add(-time.Minute))
	gb := fcStoreFixtureWithGroup(t, e.store, "eeeeeeee-eeee-4eee-beee-eeeeeeeeeee2", e.issuerID, now.Add(-time.Minute))
	rec = doRequestWithToken(t, e.srv, e.issuerTok, http.MethodPost, fcCleanupPath, nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	r = fcReport(t, rec)
	assert.True(t, r.Truncated)
	assert.Len(t, r.Identities, 1)
	assert.Equal(t, 1, boolCount(fcGroupExists(t, e.store, ga), fcGroupExists(t, e.store, gb)), "one of the two is cleaned")
	rec = doRequestWithToken(t, e.srv, e.issuerTok, http.MethodPost, fcCleanupPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	r = fcReport(t, rec)
	assert.True(t, r.Complete)
	assert.Len(t, r.Identities, 1)
	assert.False(t, fcGroupExists(t, e.store, ga))
	assert.False(t, fcGroupExists(t, e.store, gb))
}

func boolCount(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

// O1 (review round 1): an issuer's cleanup reaches its expired admin-kind
// identity through the A3 purge: the identity and every grant stamped with
// its issuance are removed, each grant row with provenance_basis naming
// the identity and the issuance, and the run's trigger.
func TestFixtureCleanup_AdminKindTargets(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	kinds, issuers := a3Kinds(t, srv, s, "fc-akt")
	target := kinds[store.UserKindTestHubAdmin]
	row := a3Row(t, s, target.Identity.ID)
	projectID := tiCreateProject(t, srv, target.AccessToken, "fc-akt-project")
	grantee := taUser(t, s, "fc-akt-grantee", store.SystemRoleHubMember)
	rec := doRequestWithToken(t, srv, target.AccessToken, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{RoleDefinitionID: a3ProjectRoleID(t, s, store.ProjectRoleAdmin), PrincipalType: "user", PrincipalID: grantee.ID})
	require.Less(t, rec.Code, 300, rec.Body.String())
	stamped, _, err := s.ListIssuanceStampedGrants(ctx, *row.IssuanceID, 0)
	require.NoError(t, err)
	require.NotEmpty(t, stamped)

	// Past the target's expiry.
	clock := row.ExpiresAt.Add(time.Second)
	srv.testIdentities.now = func() time.Time { return clock }

	rec = doRequestAsUser(t, srv, issuers[store.UserKindTestHubAdmin], http.MethodPost, fcCleanupPath, nil)
	require.Contains(t, []int{http.StatusOK, http.StatusConflict}, rec.Code, rec.Body.String())
	r := fcReport(t, rec)
	assert.Equal(t, fixtureStateExpired, fcIdentities(r)[target.Identity.ID])
	_, err = s.GetUser(ctx, target.Identity.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "the expired admin-kind identity is purged")
	after, _, err := s.ListIssuanceStampedGrants(ctx, *row.IssuanceID, 0)
	require.NoError(t, err)
	assert.Empty(t, after, "its stamped grants are removed")
	grantRows, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testAdminGrantPurgeMutation, CorrelationID: r.RunID, Limit: 100})
	require.NoError(t, err)
	require.Len(t, grantRows, len(stamped))
	for _, gr := range grantRows {
		var b fixturePurgeBasis
		require.NoError(t, json.Unmarshal([]byte(gr.ProvenanceBasis), &b), gr.ProvenanceBasis)
		assert.Equal(t, target.Identity.ID, b.RootID)
		assert.Equal(t, *row.IssuanceID, b.IssuanceID)
		assert.Equal(t, testIdentityCleanupMutation, b.Trigger)
	}
}

// O4 (review round 2): a workload delegation's cleanup never reaches an
// admin-kind identity, even an expired one its own grantor issued: neither
// the run nor its dry run lists it, and the identity and its stamped group
// stay. Control: the grantor's own session's dry run lists it.
func TestFixtureCleanup_DelegationCannotReachAdminKind(t *testing.T) {
	e := newX2Env(t, true)
	ctx := context.Background()
	grantor := e.grantor(t, "fc-dak-grantor")
	d := e.delegate(t, grantor, nil)

	rd, err := e.s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	exp := time.Now().Add(-time.Minute)
	issuer := grantor.ID
	issuance := generateID()
	admin := &store.User{
		ID:          generateID(),
		Email:       "test-admin-" + generateID()[:8] + "@" + store.TestHubAdminEmailDomain,
		DisplayName: "fc admin kind",
		Role:        store.TestAdminRole(store.UserKindTestHubAdmin),
		Status:      store.UserStatusActive,
		Kind:        store.UserKindTestHubAdmin,
		ExpiresAt:   &exp,
		IssuedBy:    &issuer,
		IssuanceID:  &issuance,
	}
	_, err = e.s.CreateTestAdminUser(ctx, admin, rd.ID)
	require.NoError(t, err)
	gid := generateID()
	require.NoError(t, e.s.CreateGroup(ctx, &store.Group{ID: gid, Name: "fc-dak-" + gid[:8], Slug: "fc-dak-" + gid[:8], CreatedBy: admin.ID,
		Fixture: store.FixtureProvenance{RootID: admin.ID, IssuerID: grantor.ID}}))

	for _, path := range []string{fcCleanupPath + "?dryRun=true", fcCleanupPath} {
		rec := e.workload(t, http.MethodPost, path, x2Token(t, x2TokenOpts{}), d.ID, nil, nil)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", path, rec.Body.String())
		r := fcReport(t, rec)
		assert.Equal(t, fixtureCleanupScopeDelegation, r.Scope)
		assert.Empty(t, r.Identities, path)
		assert.Empty(t, r.Targets, path)
	}
	_, err = e.s.GetUser(ctx, admin.ID)
	require.NoError(t, err, "the admin-kind identity stays")
	assert.True(t, fcGroupExists(t, e.s, gid), "its stamped group stays")
	// Provenance under the delegation: unproven (not its identity).
	rec := e.workload(t, http.MethodGet, fcProvenancePath+"?resource=group:"+gid, x2Token(t, x2TokenOpts{}), d.ID, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	fcUnprovenShape(t, raw, "group:"+gid)

	// Control: the grantor's own session reaches it (dry run).
	rec = doRequestAsUser(t, e.srv, grantor, http.MethodPost, fcCleanupPath+"?dryRun=true", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, map[string]string{admin.ID: fixtureStateExpired}, fcIdentities(fcReport(t, rec)))
}
