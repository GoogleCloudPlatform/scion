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

package entadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/delegationadoption"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// adoptionWorld is a store seeded with legacy (unrecorded) delegation edges
// before Migrate runs, as on a database written before provenance was
// recorded.
type adoptionWorld struct {
	t       *testing.T
	ctx     context.Context
	cs      *CompositeStore
	project *store.Project
	logs    *bytes.Buffer
}

func newAdoptionWorld(t *testing.T) *adoptionWorld {
	t.Helper()
	ctx := context.Background()
	cs := NewCompositeStore(enttest.NewClient(t))
	t.Cleanup(func() { _ = cs.Close() })
	logs := &bytes.Buffer{}
	cs.adoptionLogger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	p := &store.Project{ID: uuid.NewString(), Name: "adopt", Slug: "adopt-" + uuid.NewString()[:8], Created: time.Now(), Updated: time.Now()}
	require.NoError(t, cs.CreateProject(ctx, p))
	return &adoptionWorld{t: t, ctx: ctx, cs: cs, project: p, logs: logs}
}

func (w *adoptionWorld) user() string {
	w.t.Helper()
	id := uuid.NewString()
	require.NoError(w.t, w.cs.CreateUser(w.ctx, &store.User{
		ID: id, Email: id + "@example.com", DisplayName: "u", Role: store.UserRoleMember,
		Status: store.UserStatusActive, Created: time.Now(),
	}))
	return id
}

// agent creates a live agent under creator (a user ID, or parent's ID when
// parent is non-nil) with an unrecorded edge of the same role.
func (w *adoptionWorld) agent(root string, parent *store.Agent, role string) *store.Agent {
	w.t.Helper()
	id := uuid.NewString()
	a := &store.Agent{
		ID: id, Slug: "a-" + id[:8], Name: "a-" + id[:8], ProjectID: w.project.ID, Phase: "running",
		OwnerID: root, AppliedConfig: &store.AgentAppliedConfig{AgentRole: role},
	}
	delegatorType, delegatorID := store.DelegationPrincipalUser, root
	if parent != nil {
		delegatorType, delegatorID = store.DelegationPrincipalAgent, parent.ID
		a.CreatedBy = parent.ID
		a.Ancestry = append(append([]string{}, parent.Ancestry...), parent.ID)
	} else {
		a.CreatedBy = root
		a.Ancestry = []string{root}
	}
	require.NoError(w.t, w.cs.CreateAgent(w.ctx, a))
	require.NoError(w.t, w.cs.CreateDelegationEdge(w.ctx, &store.DelegationEdge{
		DelegatorType: delegatorType, DelegatorID: delegatorID,
		DelegateType: store.DelegationPrincipalAgent, DelegateID: id,
		ScopeType: store.RoleScopeProject, ScopeID: w.project.ID, Role: role, Active: true,
	}))
	return a
}

func (w *adoptionWorld) backfillMarker() {
	w.t.Helper()
	_, err := w.cs.UpsertHubSetting(w.ctx, delegationEdgeBackfillMarkerSection,
		json.RawMessage(`{"schema_version":1,"completed":true}`), "migration", 0, "seeded")
	require.NoError(w.t, err)
}

func (w *adoptionWorld) migrate() {
	w.t.Helper()
	require.NoError(w.t, w.cs.Migrate(w.ctx))
}

func (w *adoptionWorld) activeEdge(agentID string) *store.DelegationEdge {
	w.t.Helper()
	edges, err := w.cs.GetDelegationEdgesForDelegate(w.ctx, store.DelegationPrincipalAgent, agentID)
	require.NoError(w.t, err)
	require.Len(w.t, edges, 1)
	return edges[0]
}

func (w *adoptionWorld) allEdges(agentID string) []*store.DelegationEdge {
	w.t.Helper()
	edges, err := w.cs.ListAllDelegationEdgesForDelegate(w.ctx, store.DelegationPrincipalAgent, agentID)
	require.NoError(w.t, err)
	return edges
}

func (w *adoptionWorld) records() []*store.DelegationAdoption {
	w.t.Helper()
	recs, _, err := w.cs.ListDelegationAdoptions(w.ctx, store.DelegationAdoptionFilter{})
	require.NoError(w.t, err)
	return recs
}

func (w *adoptionWorld) record(agentID string) *store.DelegationAdoption {
	w.t.Helper()
	var found *store.DelegationAdoption
	for _, r := range w.records() {
		if r.DelegateID == agentID {
			require.Nil(w.t, found, "one record per delegate")
			found = r
		}
	}
	require.NotNil(w.t, found, "no record for %s", agentID)
	return found
}

func (w *adoptionWorld) marker() *DelegationAdoptionHeader {
	w.t.Helper()
	s, err := w.cs.GetHubSetting(w.ctx, DelegationProvenanceAdoptionMarkerSection)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	require.NoError(w.t, err)
	var h DelegationAdoptionHeader
	require.NoError(w.t, json.Unmarshal(s.Value, &h))
	return &h
}

func (w *adoptionWorld) header() *DelegationAdoptionHeader {
	w.t.Helper()
	s, err := w.cs.GetHubSetting(w.ctx, DelegationProvenanceAdoptionCohortSection)
	require.NoError(w.t, err)
	var h DelegationAdoptionHeader
	require.NoError(w.t, json.Unmarshal(s.Value, &h))
	return &h
}

func compat(t *testing.T, role string, sa bool) []string {
	t.Helper()
	ids, ok := permissions.CompatibilityCeiling(permissions.CompatibilityPolicyV1, role, sa)
	require.True(t, ok)
	return ids
}

// assertAdopted checks the shape of an adopted edge against the original.
func assertAdopted(t *testing.T, w *adoptionWorld, a *store.Agent, original *store.DelegationEdge, ids []string) *store.DelegationEdge {
	t.Helper()
	e := w.activeEdge(a.ID)
	assert.NotEqual(t, original.ID, e.ID, "a new row, not an in-place update")
	assert.Equal(t, original.DelegatorType, e.DelegatorType)
	assert.Equal(t, original.DelegatorID, e.DelegatorID)
	assert.Equal(t, original.Role, e.Role)
	assert.Equal(t, original.ScopeID, e.ScopeID)
	assert.Equal(t, store.ProvenanceVersionV1, e.ProvenanceVersion)
	assert.Equal(t, store.SourceCredentialSystemMigration, e.SourceCredentialKind)
	assert.Equal(t, original.DelegatorType, e.SourcePrincipalKind)
	assert.Equal(t, original.DelegatorID, e.SourcePrincipalID)
	assert.Empty(t, e.SourceCredentialID, "no credential is invented")
	assert.Empty(t, e.SourceEventID)
	assert.Empty(t, e.SourceScheduleID)
	assert.Empty(t, e.InitiatorPrincipalKind, "no initiator on a boot adoption")
	assert.Empty(t, e.InitiatorPrincipalID)
	assert.Equal(t, store.EffectCeilingBounded, e.Kind)
	assert.Equal(t, permissions.CeilingVersionV1, e.Version)
	assert.Equal(t, ids, e.PermissionIDs)
	assert.Equal(t, string(permissions.BoundaryKindProject), e.BoundaryKind)
	assert.Equal(t, original.ScopeID, e.BoundaryProjectID)

	old, err := w.cs.GetDelegationEdge(w.ctx, original.ID)
	require.NoError(t, err)
	assert.False(t, old.Active, "the original row is kept, inactive")
	assert.Equal(t, store.EdgeDeactivationProvenanceAdopted, old.Cause)
	assert.NotNil(t, old.At)
	assert.Equal(t, store.AuthorityProvenance{}, old.AuthorityProvenance, "the original row is not rewritten")
	return e
}

// An existing edge-backfill marker does not suppress the adoption migration.
func TestProvenanceAdoptionRunsWhenBackfillMarkerExists(t *testing.T) {
	w := newAdoptionWorld(t)
	u := w.user()
	a := w.agent(u, nil, "full")
	b := w.agent(u, a, "baseline")
	ea, eb := w.activeEdge(a.ID), w.activeEdge(b.ID)
	w.backfillMarker()

	w.migrate()

	assertAdopted(t, w, a, ea, compat(t, "full", false))
	assertAdopted(t, w, b, eb, compat(t, "baseline", false))
	m := w.marker()
	require.NotNil(t, m)
	assert.True(t, m.Completed)
	assert.Equal(t, 1, m.PolicyVersion)
	assert.Equal(t, w.header().CohortID, m.CohortID)
	assert.Equal(t, 2, m.Counts["adopted"])
	ra := w.record(a.ID)
	assert.Equal(t, store.DelegationAdoptionAdopted, ra.Status)
	assert.Equal(t, store.DelegationAdoptionOriginBoot, ra.Origin)
	assert.Equal(t, ea.ID, ra.OriginalEdgeID)
	assert.Equal(t, w.activeEdge(a.ID).ID, ra.AdoptedEdgeID)
	assert.NotEmpty(t, ra.BeforeFingerprint)
	assert.Contains(t, ra.AfterSummary, `"provenance_version":1`)
	assert.Empty(t, ra.ActorKind)
	_, err := w.cs.GetHubSetting(w.ctx, delegationEdgeBackfillMarkerSection)
	assert.NoError(t, err, "the backfill marker is untouched")
}

func TestProvenanceAdoptionIsIdempotent(t *testing.T) {
	w := newAdoptionWorld(t)
	u := w.user()
	a := w.agent(u, nil, "full")
	w.backfillMarker()
	w.migrate()
	adopted := w.activeEdge(a.ID)
	recs := w.records()
	marker := w.marker()

	w.migrate()
	w.migrate()
	assert.Equal(t, adopted.ID, w.activeEdge(a.ID).ID)
	assert.Len(t, w.allEdges(a.ID), 2, "one original, one adopted; nothing more")
	assert.Equal(t, len(recs), len(w.records()))
	assert.Equal(t, marker.CohortID, w.marker().CohortID)
}

func TestProvenanceAdoptionResumesAfterCrashBeforeMarker(t *testing.T) {
	w := newAdoptionWorld(t)
	u := w.user()
	a := w.agent(u, nil, "full")
	b := w.agent(u, a, "full")
	c := w.agent(u, b, "full")
	w.backfillMarker()
	w.cs.adoptionHopHook = func(i int, _ *store.DelegationAdoption) error {
		if i == 1 {
			return errors.New("injected failure")
		}
		return nil
	}
	w.migrate()
	assert.Nil(t, w.marker(), "the marker stays unset after a failed hop")
	cohort := w.header().CohortID
	assert.Equal(t, store.DelegationAdoptionAdopted, w.record(a.ID).Status)
	assert.Equal(t, store.DelegationAdoptionPending, w.record(b.ID).Status)
	assert.Equal(t, store.DelegationAdoptionPending, w.record(c.ID).Status)
	assert.Equal(t, store.EffectCeilingUnrecorded, w.activeEdge(b.ID).Kind, "an unadopted hop keeps its unrecorded edge")

	w.cs.adoptionHopHook = nil
	w.migrate()
	assert.Equal(t, cohort, w.header().CohortID, "no new snapshot")
	for _, ag := range []*store.Agent{a, b, c} {
		assert.Equal(t, store.DelegationAdoptionAdopted, w.record(ag.ID).Status)
		assert.Equal(t, store.EffectCeilingBounded, w.activeEdge(ag.ID).Kind)
	}
	assert.Len(t, w.records(), 3)
	require.NotNil(t, w.marker())

	// Crash after every hop but before the marker: the next run writes it.
	_ = w.cs.DeleteHubSetting(w.ctx, DelegationProvenanceAdoptionMarkerSection)
	w.migrate()
	require.NotNil(t, w.marker())
	assert.Len(t, w.records(), 3)
}

func TestProvenanceAdoptionWriteFailureRollsBackHop(t *testing.T) {
	w := newAdoptionWorld(t)
	u := w.user()
	a := w.agent(u, nil, "full")
	ea := w.activeEdge(a.ID)
	w.backfillMarker()
	w.cs.adoptionTxHook = func(tx store.Store, _ *store.DelegationAdoption) error {
		// The deactivation and the insert have been written in tx.
		edges, err := tx.GetDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, a.ID)
		require.NoError(t, err)
		require.Len(t, edges, 1)
		require.Equal(t, store.EffectCeilingBounded, edges[0].Kind)
		return errors.New("injected write failure")
	}
	w.migrate()
	assert.Nil(t, w.marker())
	e := w.activeEdge(a.ID)
	assert.Equal(t, ea.ID, e.ID, "the original row is the active row")
	assert.Equal(t, store.EffectCeilingUnrecorded, e.Kind)
	assert.Len(t, w.allEdges(a.ID), 1, "no partial adoption")
	assert.Equal(t, store.DelegationAdoptionPending, w.record(a.ID).Status)
	assert.Contains(t, w.logs.String(), "hop write failed")

	w.cs.adoptionTxHook = nil
	w.migrate()
	assertAdopted(t, w, a, ea, compat(t, "full", false))
}

func TestProvenanceAdoptionMixedVersionCohort(t *testing.T) {
	w := newAdoptionWorld(t)
	u := w.user()
	a := w.agent(u, nil, "full")
	b := w.agent(u, a, "full")
	c := w.agent(u, b, "full")

	// a: recorded from a session.
	ea := w.activeEdge(a.ID)
	recordedA := *ea
	recordedA.ID = ""
	recordedA.AuthorityProvenance = store.AuthorityProvenance{ProvenanceVersion: 1, SourcePrincipalKind: "user", SourcePrincipalID: u, SourceCredentialKind: store.SourceCredentialSession}
	recordedA.EffectCeiling = store.EffectCeiling{Kind: store.EffectCeilingPrincipal}
	require.NoError(t, w.cs.DeactivateDelegationEdge(w.ctx, ea.ID))
	require.NoError(t, w.cs.CreateDelegationEdge(w.ctx, &recordedA))

	// b: adopted by an earlier operational repair, with a narrower ceiling.
	eb := w.activeEdge(b.ID)
	narrow := []string{"agent.create", "gcp_service_account.assign", "project.read"}
	repaired := delegationadoption.AdoptedEdge(eb, narrow, delegationadoption.Actor{PrincipalKind: "user", PrincipalID: u, CredentialKind: "session"})
	require.NoError(t, w.cs.DeactivateDelegationEdge(w.ctx, eb.ID))
	require.NoError(t, w.cs.CreateDelegationEdge(w.ctx, repaired))

	ec := w.activeEdge(c.ID)
	w.backfillMarker()
	w.migrate()

	assert.Equal(t, recordedA.ID, w.activeEdge(a.ID).ID, "a recorded hop is never rewritten")
	assert.Equal(t, repaired.ID, w.activeEdge(b.ID).ID, "a repair-adopted hop is left as is")
	rb := w.record(b.ID)
	assert.Equal(t, store.DelegationAdoptionRecognized, rb.Status)
	assert.Equal(t, eb.ID, rb.OriginalEdgeID)
	assert.Equal(t, repaired.ID, rb.AdoptedEdgeID)
	assertAdopted(t, w, c, ec, narrow)
	for _, r := range w.records() {
		assert.NotEqual(t, a.ID, r.DelegateID, "recorded hops get no record")
	}
}

func TestProvenanceAdoptionPreservesRepairAdoptedEdges(t *testing.T) {
	w := newAdoptionWorld(t)
	u := w.user()
	a := w.agent(u, nil, "full")
	ea := w.activeEdge(a.ID)
	ids := []string{"agent.create", "gcp_service_account.assign", "gcp_service_account.use", "project.read"}
	repaired := delegationadoption.AdoptedEdge(ea, ids, delegationadoption.Actor{PrincipalKind: "user", PrincipalID: u, CredentialKind: "session"})
	require.NoError(t, w.cs.DeactivateDelegationEdge(w.ctx, ea.ID))
	require.NoError(t, w.cs.CreateDelegationEdge(w.ctx, repaired))
	w.backfillMarker()
	w.migrate()
	e := w.activeEdge(a.ID)
	assert.Equal(t, repaired.ID, e.ID)
	assert.Equal(t, ids, e.PermissionIDs, "existing limits are not widened")
	assert.Equal(t, u, e.InitiatorPrincipalID)
	assert.Equal(t, store.DelegationAdoptionRecognized, w.record(a.ID).Status)
	require.NotNil(t, w.marker())
}

func TestProvenanceAdoptionConcurrentDeactivationSkipsHop(t *testing.T) {
	w := newAdoptionWorld(t)
	u := w.user()
	a := w.agent(u, nil, "full")
	ea := w.activeEdge(a.ID)
	w.backfillMarker()
	w.cs.adoptionHopHook = func(_ int, rec *store.DelegationAdoption) error {
		// Another replica deactivates the edge (agent deleted) between the
		// snapshot and the hop write.
		return w.cs.DeactivateDelegationEdge(w.ctx, rec.OriginalEdgeID)
	}
	w.migrate()
	r := w.record(a.ID)
	assert.Equal(t, store.DelegationAdoptionSkippedChanged, r.Status)
	assert.NotEmpty(t, r.Reason)
	edges := w.allEdges(a.ID)
	require.Len(t, edges, 1, "nothing inserted")
	assert.Equal(t, ea.ID, edges[0].ID)
	assert.False(t, edges[0].Active)
	require.NotNil(t, w.marker(), "a skipped hop is final")
}

func TestProvenanceAdoptionConcurrentAdoptionCreatesOneActiveEdge(t *testing.T) {
	w := newAdoptionWorld(t)
	u := w.user()
	a := w.agent(u, nil, "full")
	w.backfillMarker()
	var adminEdge string
	w.cs.adoptionHopHook = func(_ int, rec *store.DelegationAdoption) error {
		// An admin commit adopts the same hop first.
		return w.cs.WithTx(w.ctx, func(tx store.Store) error {
			plan, err := delegationadoption.Build(w.ctx, tx, delegationadoption.Scope{AgentIDs: []string{rec.DelegateID}})
			if err != nil {
				return err
			}
			res, err := delegationadoption.ApplyPlannedAdopt(w.ctx, tx, plan.Hop(rec.DelegateID), "admin-op", delegationadoption.Actor{PrincipalKind: "user", PrincipalID: u, CredentialKind: "session"})
			adminEdge = res.AdoptedEdgeID
			return err
		})
	}
	w.migrate()
	e := w.activeEdge(a.ID)
	assert.Equal(t, adminEdge, e.ID)
	assert.Len(t, w.allEdges(a.ID), 2)
	r := w.record(a.ID)
	assert.Equal(t, store.DelegationAdoptionRecognized, r.Status)
	assert.Equal(t, adminEdge, r.AdoptedEdgeID)
}

// A unique-index rejection of the adopted row is resolved without a second
// active edge.
func TestProvenanceAdoptionUniqueViolationResolves(t *testing.T) {
	w := newAdoptionWorld(t)
	u := w.user()
	a := w.agent(u, nil, "full")
	w.backfillMarker()
	w.cs.adoptionTxHook = func(_ store.Store, _ *store.DelegationAdoption) error {
		return store.ErrAlreadyExists
	}
	w.migrate()
	r := w.record(a.ID)
	assert.Equal(t, store.DelegationAdoptionSkippedChanged, r.Status)
	assert.Equal(t, string(delegationadoption.ReasonConcurrentAdoption), r.Reason)
	assert.Len(t, w.allEdges(a.ID), 1)
}

func TestProvenanceAdoptionIgnoresRowsWrittenAfterSnapshot(t *testing.T) {
	w := newAdoptionWorld(t)
	u := w.user()
	a := w.agent(u, nil, "full")
	w.backfillMarker()
	w.cs.adoptionHopHook = func(int, *store.DelegationAdoption) error { return errors.New("injected failure") }
	w.migrate()
	require.Nil(t, w.marker())

	// A row written after the snapshot (for example by an older replica).
	late := w.agent(u, nil, "full")
	w.cs.adoptionHopHook = nil
	w.logs.Reset()
	w.migrate()

	assert.Equal(t, store.EffectCeilingBounded, w.activeEdge(a.ID).Kind)
	assert.Equal(t, store.EffectCeilingUnrecorded, w.activeEdge(late.ID).Kind, "outside the snapshot")
	for _, r := range w.records() {
		assert.NotEqual(t, late.ID, r.DelegateID)
	}
	assert.Contains(t, w.logs.String(), "not_in_cohort=1")
	assert.Contains(t, w.logs.String(), "remain unrecorded")
}

// Edges seeded after the first Migrate on a fresh database are outside the
// (empty) snapshot and are never adopted automatically.
func TestProvenanceAdoptionFreshInstallSnapshotIsEmpty(t *testing.T) {
	w := newAdoptionWorld(t)
	w.migrate()
	require.NotNil(t, w.marker())
	assert.Empty(t, w.records())
	u := w.user()
	a := w.agent(u, nil, "full")
	w.migrate()
	assert.Equal(t, store.EffectCeilingUnrecorded, w.activeEdge(a.ID).Kind)
}

func TestProvenanceAdoptionLogsPendingSummary(t *testing.T) {
	w := newAdoptionWorld(t)
	u := w.user()
	w.agent(u, nil, "full")
	sched := w.agent(u, nil, "none") // role none: excluded
	w.backfillMarker()
	w.migrate()
	out := w.logs.String()
	assert.Contains(t, out, "delegation provenance adoption summary")
	assert.Contains(t, out, "adopted=1")
	assert.Contains(t, out, "excluded=1")
	assert.Contains(t, out, "1 hops on live agent chains remain unrecorded; review GET /api/v1/admin/delegation-adoption")
	r := w.record(sched.ID)
	assert.Equal(t, store.DelegationAdoptionExcluded, r.Status)
	assert.Equal(t, string(delegationadoption.ReasonRoleNone), r.Reason)
	assert.Equal(t, 1, w.marker().Counts["excluded"])
}

func TestProvenanceAdoptionUnknownMarkerLayoutIsComplete(t *testing.T) {
	w := newAdoptionWorld(t)
	u := w.user()
	a := w.agent(u, nil, "full")
	w.backfillMarker()
	_, err := w.cs.UpsertHubSetting(w.ctx, DelegationProvenanceAdoptionMarkerSection,
		json.RawMessage(`{"schema_version":9}`), "migration", 0, "seeded")
	require.NoError(t, err)
	w.migrate()
	assert.Equal(t, store.EffectCeilingUnrecorded, w.activeEdge(a.ID).Kind)
	assert.True(t, strings.Contains(w.logs.String(), "unknown layout"))
}

func TestDelegationEdgeGuardedDeactivateAndReactivate(t *testing.T) {
	w := newAdoptionWorld(t)
	u := w.user()
	a := w.agent(u, nil, "full")
	e := w.activeEdge(a.ID)

	stale := e.UpdatedAt.Add(-time.Second)
	ok, err := w.cs.DeactivateDelegationEdgeGuarded(w.ctx, e.ID, store.DelegationEdgeDeactivateGuard{Unrecorded: true, UpdatedAt: &stale}, store.EdgeDeactivationProvenanceAdopted, "op")
	require.NoError(t, err)
	assert.False(t, ok, "stale updated time")
	ok, err = w.cs.DeactivateDelegationEdgeGuarded(w.ctx, e.ID, store.DelegationEdgeDeactivateGuard{Recorded: true}, store.EdgeDeactivationProvenanceAdopted, "op")
	require.NoError(t, err)
	assert.False(t, ok, "not recorded")
	ok, err = w.cs.DeactivateDelegationEdgeGuarded(w.ctx, e.ID, store.DelegationEdgeDeactivateGuard{Unrecorded: true, UpdatedAt: &e.UpdatedAt}, store.EdgeDeactivationProvenanceAdopted, "op")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = w.cs.DeactivateDelegationEdgeGuarded(w.ctx, e.ID, store.DelegationEdgeDeactivateGuard{}, store.EdgeDeactivationProvenanceAdopted, "op")
	require.NoError(t, err)
	assert.False(t, ok, "already inactive")
	got, err := w.cs.GetDelegationEdge(w.ctx, e.ID)
	require.NoError(t, err)
	assert.Equal(t, "op", got.OpID)

	assert.ErrorIs(t, w.cs.ReactivateDelegationEdge(w.ctx, e.ID, store.EdgeDeactivationAdoptionReverted), store.ErrRevisionConflict, "wrong cause")
	other := *e
	other.ID = ""
	require.NoError(t, w.cs.CreateDelegationEdge(w.ctx, &other))
	assert.ErrorIs(t, w.cs.ReactivateDelegationEdge(w.ctx, e.ID, store.EdgeDeactivationProvenanceAdopted), store.ErrRevisionConflict, "another active edge")
	require.NoError(t, w.cs.DeactivateDelegationEdge(w.ctx, other.ID))
	require.NoError(t, w.cs.ReactivateDelegationEdge(w.ctx, e.ID, store.EdgeDeactivationProvenanceAdopted))
	got, err = w.cs.GetDelegationEdge(w.ctx, e.ID)
	require.NoError(t, err)
	assert.True(t, got.Active)
	assert.Equal(t, store.Deactivation{}, got.Deactivation)
	_, err = w.cs.GetDelegationEdge(w.ctx, uuid.NewString())
	assert.ErrorIs(t, err, store.ErrNotFound)
}
