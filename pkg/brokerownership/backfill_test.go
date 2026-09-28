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

package brokerownership_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/brokerownership"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// harnessSeq gives each test its own isolated in-memory database, mirroring
// pkg/hub's newTestStore convention.
var harnessSeq atomic.Int64

// harness wires a fresh Ent-backed store for a single test. Project and
// RuntimeBroker rows are created through the raw *ent.Client so tests can
// pin exact, deterministic Created timestamps (both fields are immutable
// and always default to time.Now() through the store.Store interface,
// which is unusable for boundary tests). Everything else — linking,
// reading CreatedBy back, running the backfill under test — goes through
// the store.Store interface, exactly as production code does.
type harness struct {
	t      *testing.T
	ctx    context.Context
	client *ent.Client
	store  store.Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dsn := fmt.Sprintf("file:brokerownershiptest%d?mode=memory&cache=shared", harnessSeq.Add(1))
	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	s := entadapter.NewCompositeStore(client)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { _ = s.Close() })
	return &harness{t: t, ctx: context.Background(), client: client, store: s}
}

// project creates a project with the given owner and Created timestamp,
// returning its ID. An empty createdBy models the co-located/global
// ownerless project (design note R1, §8 (a.3)).
func (h *harness) project(createdBy string, created time.Time) string {
	h.t.Helper()
	id := uuid.New()
	create := h.client.Project.Create().
		SetID(id).
		SetName("project-" + id.String()).
		SetSlug("project-" + id.String()).
		SetCreated(created).
		SetUpdated(created)
	if createdBy != "" {
		create.SetCreatedBy(createdBy)
	}
	_, err := create.Save(h.ctx)
	require.NoError(h.t, err)
	return id.String()
}

// broker creates a runtime broker with the given Created timestamp and
// labels, always with an empty CreatedBy (the ownerless starting state this
// package backfills), returning its ID and name.
func (h *harness) broker(created time.Time, labels map[string]string) (id, name string) {
	h.t.Helper()
	u := uuid.New()
	name = "broker-" + u.String()
	create := h.client.RuntimeBroker.Create().
		SetID(u).
		SetName(name).
		SetSlug(name).
		SetCreated(created).
		SetUpdated(created)
	if labels != nil {
		create.SetLabels(labels)
	}
	_, err := create.Save(h.ctx)
	require.NoError(h.t, err)
	return u.String(), name
}

// link adds brokerID/brokerName as a provider of projectID, optionally
// recording linkedBy (empty mirrors the deprecated embedded-broker path,
// which never records it).
func (h *harness) link(projectID, brokerID, brokerName, linkedBy string) {
	h.t.Helper()
	require.NoError(h.t, h.store.AddProjectProvider(h.ctx, &store.ProjectProvider{
		ProjectID:  projectID,
		BrokerID:   brokerID,
		BrokerName: brokerName,
		Status:     store.BrokerStatusOnline,
		LinkedBy:   linkedBy,
	}))
}

// createdByOf reads back a broker's current CreatedBy.
func (h *harness) createdByOf(brokerID string) string {
	h.t.Helper()
	b, err := h.store.GetRuntimeBroker(h.ctx, brokerID)
	require.NoError(h.t, err)
	return b.CreatedBy
}

func decisionFor(t *testing.T, result *brokerownership.Result, brokerID string) brokerownership.Decision {
	t.Helper()
	for _, d := range result.Decisions {
		if d.BrokerID == brokerID {
			return d
		}
	}
	t.Fatalf("no decision recorded for broker %s", brokerID)
	return brokerownership.Decision{}
}

// (a) Unique original linkage -> owner set.
func TestBackfill_UniqueOriginalLinkage_Attributes(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Second)

	p := h.project("user-a", base)
	b, name := h.broker(base.Add(500*time.Millisecond), nil) // within default 1s window
	h.link(p, b, name, "")                                   // deprecated-flow style: no linked_by

	result, err := brokerownership.Backfill(h.ctx, h.store, brokerownership.Config{})
	require.NoError(t, err)

	d := decisionFor(t, result, b)
	require.Equal(t, brokerownership.ReasonAttributed, d.Reason)
	require.Equal(t, "user-a", d.OwnerID)
	require.Equal(t, "user-a", h.createdByOf(b))
}

// (a.1) One-sided W-boundary pair: just inside W attributes, just outside
// does not.
func TestBackfill_WindowBoundary(t *testing.T) {
	t.Run("just inside W attributes", func(t *testing.T) {
		h := newHarness(t)
		base := time.Now().UTC().Truncate(time.Second)
		p := h.project("user-a", base)
		delta := time.Duration(brokerownership.DefaultWindowSeconds*float64(time.Second)) - 100*time.Millisecond
		b, name := h.broker(base.Add(delta), nil)
		h.link(p, b, name, "")

		result, err := brokerownership.Backfill(h.ctx, h.store, brokerownership.Config{})
		require.NoError(t, err)

		d := decisionFor(t, result, b)
		require.Equal(t, brokerownership.ReasonAttributed, d.Reason)
		require.Equal(t, "user-a", h.createdByOf(b))
	})

	t.Run("just outside W leaves ownerless", func(t *testing.T) {
		h := newHarness(t)
		base := time.Now().UTC().Truncate(time.Second)
		p := h.project("user-a", base)
		delta := time.Duration(brokerownership.DefaultWindowSeconds*float64(time.Second)) + 100*time.Millisecond
		b, name := h.broker(base.Add(delta), nil)
		h.link(p, b, name, "")

		result, err := brokerownership.Backfill(h.ctx, h.store, brokerownership.Config{})
		require.NoError(t, err)

		d := decisionFor(t, result, b)
		require.Equal(t, brokerownership.ReasonNoTimingMatch, d.Reason)
		require.Empty(t, h.createdByOf(b))
	})

	// The window is inclusive on both ends: [broker.Created - W,
	// broker.Created]. These two cases pin the exact boundary values
	// (delta == 0 and delta == W) as attributed, so a future change from
	// <= to < at either end is caught rather than only being exercised by
	// the "just inside"/"just outside" cases 100ms off either edge above.
	t.Run("delta exactly zero attributes", func(t *testing.T) {
		h := newHarness(t)
		base := time.Now().UTC().Truncate(time.Second)
		p := h.project("user-a", base)
		b, name := h.broker(base, nil) // broker.Created == project.Created, delta == 0
		h.link(p, b, name, "")

		result, err := brokerownership.Backfill(h.ctx, h.store, brokerownership.Config{})
		require.NoError(t, err)

		d := decisionFor(t, result, b)
		require.Equal(t, brokerownership.ReasonAttributed, d.Reason)
		require.Equal(t, "user-a", h.createdByOf(b))
	})

	t.Run("delta exactly W attributes", func(t *testing.T) {
		h := newHarness(t)
		base := time.Now().UTC().Truncate(time.Second)
		p := h.project("user-a", base)
		delta := time.Duration(brokerownership.DefaultWindowSeconds * float64(time.Second))
		b, name := h.broker(base.Add(delta), nil) // delta == W exactly
		h.link(p, b, name, "")

		result, err := brokerownership.Backfill(h.ctx, h.store, brokerownership.Config{})
		require.NoError(t, err)

		d := decisionFor(t, result, b)
		require.Equal(t, brokerownership.ReasonAttributed, d.Reason)
		require.Equal(t, "user-a", h.createdByOf(b))
	})
}

// (a.2) A negative delta (project "created after" the broker) can never be
// a real pair and must be excluded outright, regardless of magnitude.
func TestBackfill_NegativeDelta_NeverMatches(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Second)
	b, name := h.broker(base, nil)
	p := h.project("user-a", base.Add(1*time.Second)) // project created AFTER the broker
	h.link(p, b, name, "")

	result, err := brokerownership.Backfill(h.ctx, h.store, brokerownership.Config{})
	require.NoError(t, err)

	d := decisionFor(t, result, b)
	require.Equal(t, brokerownership.ReasonNoTimingMatch, d.Reason)
	require.Empty(t, h.createdByOf(b))
}

// (a.3) A project with an empty CreatedBy (models the co-located/global
// ownerless project+broker pair) must never be treated as a timing
// candidate, even when its Created falls inside the window and it links
// the broker.
func TestBackfill_EmptyCreatedByProject_NeverCandidate(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Second)
	p := h.project("", base) // ownerless project
	b, name := h.broker(base.Add(1*time.Second), nil)
	h.link(p, b, name, "")

	result, err := brokerownership.Backfill(h.ctx, h.store, brokerownership.Config{})
	require.NoError(t, err)

	d := decisionFor(t, result, b)
	require.Equal(t, brokerownership.ReasonNoTimingMatch, d.Reason)
	require.Empty(t, h.createdByOf(b))
}

// (b) Ambiguous / disagreeing linkage variants all leave the broker
// ownerless, each with a distinct reason code.
func TestBackfill_AmbiguousAndDisagreeingLinkage(t *testing.T) {
	t.Run("two projects within the window is ambiguous-timing", func(t *testing.T) {
		h := newHarness(t)
		base := time.Now().UTC().Truncate(time.Second)
		p1 := h.project("user-a", base)                           // delta from b: 900ms
		p2 := h.project("user-b", base.Add(500*time.Millisecond)) // delta from b: 400ms
		b, name := h.broker(base.Add(900*time.Millisecond), nil)  // both p1 and p2 land in [b-1s, b]
		h.link(p1, b, name, "")

		result, err := brokerownership.Backfill(h.ctx, h.store, brokerownership.Config{})
		require.NoError(t, err)

		d := decisionFor(t, result, b)
		require.Equal(t, brokerownership.ReasonAmbiguousTiming, d.Reason)
		require.Empty(t, h.createdByOf(b))
		_ = p2 // only its Created/CreatedBy matter; no link needed to create the ambiguity
	})

	t.Run("timing candidate does not currently link the broker is link-mismatch", func(t *testing.T) {
		h := newHarness(t)
		base := time.Now().UTC().Truncate(time.Second)
		origin := h.project("user-a", base)
		other := h.project("user-b", base.Add(10*time.Second)) // outside the window, irrelevant to timing
		b, name := h.broker(base.Add(500*time.Millisecond), nil)
		// The broker is currently linked only to a project that is NOT the
		// timing candidate — models the origin's link having moved away.
		h.link(other, b, name, "")
		_ = origin

		result, err := brokerownership.Backfill(h.ctx, h.store, brokerownership.Config{})
		require.NoError(t, err)

		d := decisionFor(t, result, b)
		require.Equal(t, brokerownership.ReasonLinkMismatch, d.Reason)
		require.Empty(t, h.createdByOf(b))
	})

	t.Run("broker currently linked by more than one project is multi-linked", func(t *testing.T) {
		h := newHarness(t)
		base := time.Now().UTC().Truncate(time.Second)
		origin := h.project("user-a", base)
		second := h.project("user-b", base.Add(20*time.Second)) // outside the window
		b, name := h.broker(base.Add(500*time.Millisecond), nil)
		h.link(origin, b, name, "")
		h.link(second, b, name, "")

		result, err := brokerownership.Backfill(h.ctx, h.store, brokerownership.Config{})
		require.NoError(t, err)

		d := decisionFor(t, result, b)
		require.Equal(t, brokerownership.ReasonMultiLinked, d.Reason)
		require.Empty(t, h.createdByOf(b))
	})

	t.Run("linked_by disagreeing with the candidate's CreatedBy is linked-by-mismatch", func(t *testing.T) {
		h := newHarness(t)
		base := time.Now().UTC().Truncate(time.Second)
		origin := h.project("user-a", base)
		b, name := h.broker(base.Add(500*time.Millisecond), nil)
		h.link(origin, b, name, "user-c") // someone other than user-a explicitly linked it

		result, err := brokerownership.Backfill(h.ctx, h.store, brokerownership.Config{})
		require.NoError(t, err)

		d := decisionFor(t, result, b)
		require.Equal(t, brokerownership.ReasonLinkedByMismatch, d.Reason)
		require.Empty(t, h.createdByOf(b))
	})

	// The linked-by check only ever REJECTS a linker that DISAGREES with the
	// candidate project's CreatedBy — it must not reject on the mere
	// presence of a non-empty linked_by. This is the positive case sibling
	// to the mismatch case above: linked_by equal to the candidate's own
	// CreatedBy is exactly what the legitimate explicit-link path
	// (addProjectProvider) produces, and must still attribute.
	t.Run("linked_by equal to the candidate's CreatedBy still attributes", func(t *testing.T) {
		h := newHarness(t)
		base := time.Now().UTC().Truncate(time.Second)
		origin := h.project("user-a", base)
		b, name := h.broker(base.Add(500*time.Millisecond), nil)
		h.link(origin, b, name, "user-a") // linked_by agrees with the project's own creator

		result, err := brokerownership.Backfill(h.ctx, h.store, brokerownership.Config{})
		require.NoError(t, err)

		d := decisionFor(t, result, b)
		require.Equal(t, brokerownership.ReasonAttributed, d.Reason)
		require.Equal(t, "user-a", d.OwnerID)
		require.Equal(t, "user-a", h.createdByOf(b))
	})
}

// (c) An embedded-role broker is untouched regardless of any other signal —
// here it has a perfect timing and linkage match, and must still be left
// alone.
func TestBackfill_EmbeddedRoleBroker_Untouched(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Second)
	p := h.project("user-a", base)
	b, name := h.broker(base.Add(1*time.Second), map[string]string{
		brokerownership.LabelBrokerRole: brokerownership.BrokerRoleEmbedded,
	})
	h.link(p, b, name, "")

	result, err := brokerownership.Backfill(h.ctx, h.store, brokerownership.Config{})
	require.NoError(t, err)

	d := decisionFor(t, result, b)
	require.Equal(t, brokerownership.ReasonExcludedEmbedded, d.Reason)
	require.Empty(t, h.createdByOf(b))
}

// (d) Idempotent re-run: a second pass over the same data makes no further
// writes and never revisits an already-attributed (or already-owned) row.
func TestBackfill_IdempotentReRun(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Second)
	p := h.project("user-a", base)
	b, name := h.broker(base.Add(1*time.Second), nil)
	h.link(p, b, name, "")

	// A broker that already has an owner from another source must never be
	// scanned or altered.
	preOwned, preOwnedName := h.broker(base.Add(1*time.Second), nil)
	require.NoError(t, h.client.RuntimeBroker.UpdateOneID(uuid.MustParse(preOwned)).SetCreatedBy("user-z").Exec(h.ctx))
	h.link(p, preOwned, preOwnedName, "")

	first, err := brokerownership.Backfill(h.ctx, h.store, brokerownership.Config{})
	require.NoError(t, err)
	require.Equal(t, brokerownership.ReasonAttributed, decisionFor(t, first, b).Reason)
	require.Equal(t, "user-a", h.createdByOf(b))
	require.Equal(t, "user-z", h.createdByOf(preOwned))
	for _, d := range first.Decisions {
		require.NotEqual(t, preOwned, d.BrokerID, "an already-owned broker must never be scanned")
	}

	second, err := brokerownership.Backfill(h.ctx, h.store, brokerownership.Config{})
	require.NoError(t, err)
	require.Zero(t, second.TotalScanned, "no ownerless brokers remain; the second pass must scan none")
	require.Equal(t, "user-a", h.createdByOf(b), "already-attributed broker must not be revisited")
	require.Equal(t, "user-z", h.createdByOf(preOwned))
}

// (e) Dry-run/execute parity on an unchanged fixture: the two passes must
// reach identical decisions, and DryRun must not write anything.
func TestBackfill_DryRunExecuteParity_UnchangedFixture(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Second)
	p := h.project("user-a", base)
	b, name := h.broker(base.Add(1*time.Second), nil)
	h.link(p, b, name, "")

	dry, err := brokerownership.Backfill(h.ctx, h.store, brokerownership.Config{DryRun: true})
	require.NoError(t, err)
	require.Equal(t, brokerownership.ReasonAttributed, decisionFor(t, dry, b).Reason)
	require.Empty(t, h.createdByOf(b), "dry run must not write")

	exec, err := brokerownership.Backfill(h.ctx, h.store, brokerownership.Config{})
	require.NoError(t, err)

	dryDecision := decisionFor(t, dry, b)
	execDecision := decisionFor(t, exec, b)
	require.Equal(t, dryDecision.Reason, execDecision.Reason)
	require.Equal(t, dryDecision.OwnerID, execDecision.OwnerID)
	require.Equal(t, "user-a", h.createdByOf(b))
}

// (f) Dry-run and execute are separate passes over current data, not one
// operation with a replayed decision: if linkage changes between the two,
// execute must follow the new state, not dry-run's earlier decision.
func TestBackfill_ExecuteReEvaluatesAgainstCurrentData(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Second)
	p := h.project("user-a", base)
	b, name := h.broker(base.Add(1*time.Second), nil)
	h.link(p, b, name, "")

	dry, err := brokerownership.Backfill(h.ctx, h.store, brokerownership.Config{DryRun: true})
	require.NoError(t, err)
	require.Equal(t, brokerownership.ReasonAttributed, decisionFor(t, dry, b).Reason)

	// Simulate a re-link landing between the dry-run boot and the execute
	// boot: a second, unrelated project also starts linking the broker.
	second := h.project("user-b", base.Add(30*time.Second))
	h.link(second, b, name, "")

	exec, err := brokerownership.Backfill(h.ctx, h.store, brokerownership.Config{})
	require.NoError(t, err)

	execDecision := decisionFor(t, exec, b)
	require.Equal(t, brokerownership.ReasonMultiLinked, execDecision.Reason,
		"execute must re-evaluate against current data, not replay dry-run's decision")
	require.Empty(t, h.createdByOf(b))
}

// TestBackfill_MultiPagePagination proves Backfill's own broker-listing loop
// pages through every ownerless broker, not just the first page. Brokers are
// listed newest-first (store.ListRuntimeBrokers), so before the
// keyset-pagination fix a small PageSize silently stopped after page one and
// the OLDEST brokers — exactly the legacy ownerless population this package
// exists to repair — were skipped permanently, with the migration marker
// still recorded as a completed pass.
func TestBackfill_MultiPagePagination(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Second)

	const total = 7
	const pageSize = 2 // several pages, with a remainder on the last one
	brokerIDs := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		// Spaced a minute apart so each broker's own project is comfortably
		// inside its window and comfortably outside every other broker's,
		// keeping attribution unambiguous per broker.
		t0 := base.Add(time.Duration(i) * time.Minute)
		p := h.project(fmt.Sprintf("user-%d", i), t0)
		b, name := h.broker(t0.Add(1*time.Second), nil)
		h.link(p, b, name, "")
		brokerIDs[b] = true
	}

	result, err := brokerownership.Backfill(h.ctx, h.store, brokerownership.Config{PageSize: pageSize})
	require.NoError(t, err)
	require.Equal(t, total, result.TotalScanned,
		"every ownerless broker must be scanned across pages, including the oldest")

	seen := make(map[string]bool, total)
	for _, d := range result.Decisions {
		require.False(t, seen[d.BrokerID], "broker decided twice across pages: %s", d.BrokerID)
		seen[d.BrokerID] = true
		require.Equal(t, brokerownership.ReasonAttributed, d.Reason)
	}
	for id := range brokerIDs {
		require.True(t, seen[id], "broker missing from a multi-page pass: %s", id)
	}
}

// TestBackfill_MultiPagePagination_CreatedTiebreak proves the (created, id)
// keyset tiebreaker prevents a page-boundary skip or duplicate when several
// ownerless brokers share the exact same Created timestamp. Ordering by
// Created alone, with no id tiebreaker, can nondeterministically split or
// duplicate such a group across a page boundary. This only checks that every
// broker is scanned exactly once — not what each is attributed to, since
// several projects all landing in the same tight window makes their
// attribution outcome ambiguous-timing by design (§4), which is orthogonal
// to what this test proves.
func TestBackfill_MultiPagePagination_CreatedTiebreak(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Second)

	const total = 4
	const pageSize = 1 // forces every broker onto its own page
	brokerIDs := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		b, _ := h.broker(base, nil) // identical Created for every broker
		brokerIDs[b] = true
	}

	result, err := brokerownership.Backfill(h.ctx, h.store, brokerownership.Config{PageSize: pageSize})
	require.NoError(t, err)
	require.Equal(t, total, result.TotalScanned,
		"every identically-timestamped broker must be scanned exactly once")

	seen := make(map[string]bool, total)
	for _, d := range result.Decisions {
		require.False(t, seen[d.BrokerID],
			"broker decided twice across pages at a Created-tiebreak boundary: %s", d.BrokerID)
		seen[d.BrokerID] = true
	}
	for id := range brokerIDs {
		require.True(t, seen[id], "broker missing from pagination at a Created-tiebreak boundary: %s", id)
	}
}

// TestBackfill_Deltas_SignedValuesAndInWindowFlags proves Result.Deltas
// records the exact signed delta and InWindow flag for both an in-window
// candidate and a near-miss just outside the window, so a mutant that drops
// Deltas entirely, or that flips a near-miss's sign, is caught.
func TestBackfill_Deltas_SignedValuesAndInWindowFlags(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Second)

	// In-window pair: delta = broker.Created - p.Created = 400ms.
	p := h.project("user-a", base)
	b, name := h.broker(base.Add(400*time.Millisecond), nil)
	h.link(p, b, name, "")

	// A second, unrelated project positioned so its delta from b is
	// 1100ms > W (1s): the nearest neighbor just outside the window on the
	// lower (older) side, so evaluate() must report it as a near-miss
	// (InWindow=false) with its true positive sign.
	farProject := h.project("user-far", base.Add(-700*time.Millisecond))

	// A third, unrelated project created ~300ms AFTER the broker: the
	// nearest neighbor just outside the window on the upper side. A
	// project created after the broker can never be a genuine pair
	// regardless of window width (§3), so this delta must come through
	// negative — never flipped to a positive magnitude — which is what
	// makes it "always an anomaly" worth flagging on its own.
	laterProject := h.project("user-later", base.Add(700*time.Millisecond))

	result, err := brokerownership.Backfill(h.ctx, h.store, brokerownership.Config{})
	require.NoError(t, err)

	var sawInWindow, sawLowerNearMiss, sawUpperNearMiss bool
	for _, d := range result.Deltas {
		if d.BrokerID != b {
			continue
		}
		switch d.ProjectID {
		case p:
			sawInWindow = true
			require.True(t, d.InWindow, "the attributed candidate must be recorded in-window")
			require.InDelta(t, 0.4, d.DeltaSeconds, 0.05, "in-window delta must be the signed broker-minus-project value")
		case farProject:
			sawLowerNearMiss = true
			require.False(t, d.InWindow, "a candidate outside W must be recorded as a near-miss, not in-window")
			require.InDelta(t, 1.1, d.DeltaSeconds, 0.05, "lower-side near-miss delta must keep its true (positive) sign and magnitude")
		case laterProject:
			sawUpperNearMiss = true
			require.False(t, d.InWindow, "a project created after the broker must be recorded as a near-miss, not in-window")
			require.InDelta(t, -0.3, d.DeltaSeconds, 0.05, "upper-side near-miss delta must come through negative, never flipped positive")
		}
	}
	require.True(t, sawInWindow, "expected an in-window delta observation for the attributed pair")
	require.True(t, sawLowerNearMiss, "expected a lower-side (positive) near-miss delta observation just outside the window")
	require.True(t, sawUpperNearMiss, "expected an upper-side (negative) near-miss delta observation for a project created after the broker")
}

// TestBackfill_ApplyRaceLoser_ReportsAlreadySet proves Backfill honors the
// applied bool returned by SetRuntimeBrokerCreatedByIfEmpty. The wrapped
// store below deterministically simulates a concurrent writer that set
// CreatedBy first (applied=false) without touching the underlying row, so
// this is exercised without depending on real goroutine timing. It asserts
// that when the write reports applied=false, Backfill records the outcome
// as ReasonAlreadySet and excludes it from both the attributed count and
// the residual, rather than counting it as an attribution it did not
// perform.
func TestBackfill_ApplyRaceLoser_ReportsAlreadySet(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Second)
	p := h.project("user-a", base)
	b, name := h.broker(base.Add(500*time.Millisecond), nil)
	h.link(p, b, name, "")

	wrapped := &raceLoserStore{Store: h.store, brokerID: b}

	result, err := brokerownership.Backfill(h.ctx, wrapped, brokerownership.Config{})
	require.NoError(t, err)

	d := decisionFor(t, result, b)
	require.Equal(t, brokerownership.ReasonAlreadySet, d.Reason,
		"a lost write race must be reported as already-set, never as this pass's attribution")
	require.Empty(t, d.OwnerID, "the loser must not report an owner it did not set")
	require.Equal(t, 1, result.Reasons[brokerownership.ReasonAlreadySet])
	require.Zero(t, result.Reasons[brokerownership.ReasonAttributed],
		"a lost race must not also be counted as attributed")

	// The real store was never actually written by this pass.
	require.Empty(t, h.createdByOf(b))
}

// raceLoserStore wraps a store.Store and makes SetRuntimeBrokerCreatedByIfEmpty
// report applied=false for one specific broker, without touching the
// underlying row — deterministically simulating a concurrent writer that got
// there first. Every other operation delegates to the wrapped store
// unchanged (Go interface embedding).
type raceLoserStore struct {
	store.Store
	brokerID string
}

func (s *raceLoserStore) SetRuntimeBrokerCreatedByIfEmpty(ctx context.Context, id, createdBy string) (bool, error) {
	if id == s.brokerID {
		return false, nil
	}
	return s.Store.SetRuntimeBrokerCreatedByIfEmpty(ctx, id, createdBy)
}
