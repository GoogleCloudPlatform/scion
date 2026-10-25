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

package lifecycleevents

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	tc "github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetryemit"
)

var t0 = time.Date(2026, 10, 22, 8, 0, 0, 123456789, time.UTC)

const (
	agentA   = "00000000-0000-4000-8000-0000000000a1"
	projectA = "00000000-0000-4000-8000-0000000000f1"
)

// harness wires an Observer to a memDB with deterministic IDs and clock.
type harness struct {
	t        *testing.T
	db       *memDB
	obs      *Observer
	counters *telemetryemit.MemoryCounters
	logs     *bytes.Buffer
	clock    time.Time
	ids      int
	mu       sync.Mutex
	wakes    int
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, db: newMemDB(), counters: telemetryemit.NewMemoryCounters(), logs: &bytes.Buffer{}, clock: t0}
	obs, err := NewObserver(ObserverConfig{
		HubID:         "hub-a",
		ProducerBuild: "v0.0.0-test",
		Counters:      h.counters,
		Logger:        slog.New(slog.NewTextHandler(h.logs, nil)),
		Now: func() time.Time {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.clock = h.clock.Add(time.Millisecond)
			return h.clock
		},
		NewEventID: func() string {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.ids++
			return fmt.Sprintf("00000000-0000-4000-8000-%012d", h.ids)
		},
		OnEnqueued: func() { h.mu.Lock(); h.wakes++; h.mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	h.obs = obs
	return h
}

func baseAgent() *store.Agent {
	return &store.Agent{
		ID: agentA, ProjectID: projectA, Slug: "a1", Name: "a1", Template: "dev",
		Phase: "created", Runtime: "docker", RuntimeBrokerID: "b1", Generation: 1,
		CreatedBy: "u-created-by",
		Labels:    map[string]string{"stage": "implement", "x/y.z-1": "a<b>&c", "empty": ""},
		AppliedConfig: &store.AgentAppliedConfig{
			TemplateID: "t1", TemplateHash: "sha256:ab", Model: "m-large-1", ModelSize: "large",
			Harness: "claude", HarnessConfig: "claude-default", HarnessConfigSource: "hub-hydrated", Profile: "p",
		},
		Ancestry: []string{"u1", "parent-agent"},
	}
}

func clone(a *store.Agent) *store.Agent {
	c := *a
	c.Labels = map[string]string{}
	for k, v := range a.Labels {
		c.Labels[k] = v
	}
	if a.AppliedConfig != nil {
		ac := *a.AppliedConfig
		c.AppliedConfig = &ac
	}
	if a.EffectiveConfig != nil {
		ec := *a.EffectiveConfig
		c.EffectiveConfig = &ec
	}
	return &c
}

func (h *harness) write(ctx context.Context, w store.AgentWrite) {
	h.t.Helper()
	if w.Now.IsZero() {
		w.Now = t0
	}
	if err := h.db.observe(ctx, h.obs, w); err != nil {
		h.t.Fatalf("observe %s: %v", w.Op, err)
	}
}

func (h *harness) create(a *store.Agent) {
	ctx := WithCreateContext(context.Background(), CreateContext{CreatorType: "user", CreatorID: "u1"})
	h.write(ctx, store.AgentWrite{Op: store.AgentWriteCreate, After: clone(a)})
}

// events decodes the outbox in seq order.
func (h *harness) events() []*tc.LifecycleEvent {
	h.t.Helper()
	var out []*tc.LifecycleEvent
	for _, r := range h.db.rows() {
		var ev tc.LifecycleEvent
		if err := tc.Unmarshal(r.Payload, &ev); err != nil {
			h.t.Fatalf("outbox row %d does not decode: %v", r.Seq, err)
		}
		if ev.EventID != r.EventID || ev.TransitionRevision != r.TransitionRevision || ev.AgentID != r.AgentID {
			h.t.Fatalf("outbox row columns disagree with payload: %+v", r)
		}
		out = append(out, &ev)
	}
	return out
}

// seq renders events as "transition@revision" with gaps visible.
func seq(evs []*tc.LifecycleEvent) string {
	parts := make([]string, len(evs))
	for i, e := range evs {
		s := fmt.Sprintf("%s@%d", e.Transition, e.TransitionRevision)
		if e.Cause != "" {
			s += "(" + e.Cause + ")"
		}
		if e.DeleteMode != "" {
			s += "(" + e.DeleteMode + ")"
		}
		parts[i] = s
	}
	return strings.Join(parts, " ")
}

func TestObserverCreated(t *testing.T) {
	h := newHarness(t)
	h.create(baseAgent())
	evs := h.events()
	if got := seq(evs); got != "created@1" {
		t.Fatalf("events = %s", got)
	}
	ev := evs[0]
	if ev.MetadataRevision != 1 || ev.ProducerNamespace != "scion-hub/hub-a" || ev.HubID != "hub-a" ||
		ev.ProjectID != projectA || ev.AgentID != agentA || ev.BrokerID != "b1" || ev.IncarnationID != "1" ||
		ev.LaunchID != "" || ev.ProducerBuild != "v0.0.0-test" {
		t.Errorf("envelope: %+v", ev.Envelope)
	}
	if ev.EventTS != tc.FormatTS(t0) {
		t.Errorf("event_ts = %s, want the write's time", ev.EventTS)
	}
	want := tc.LifecycleMetadata{
		Name: "a1", Slug: "a1", RequestedTemplate: "dev", RequestedTemplateID: "t1", RequestedTemplateHash: "sha256:ab",
		RequestedModel: "m-large-1", RequestedModelAlias: "large", Harness: "claude", HarnessConfig: "claude-default",
		HarnessConfigSource: "hub-hydrated", Profile: "p", Runtime: "docker",
		CreatorPrincipalType: "user", CreatorPrincipalID: "u1", ParentAgentID: "parent-agent",
		Labels: tc.LabelsFromMap(baseAgent().Labels),
	}
	if !reflect.DeepEqual(*ev.Metadata, want) {
		t.Errorf("metadata:\n got %+v\nwant %+v", *ev.Metadata, want)
	}
	c, ok := h.db.cursor(agentA)
	if !ok || c.TransitionRevision != 1 || c.MetadataRevision != 1 || c.CreatorPrincipalType != "user" ||
		c.CreatorPrincipalID != "u1" || c.LastDeleted || c.LastFingerprint == "" || c.ProjectID != projectA {
		t.Errorf("cursor after create: %+v", c)
	}
	if h.counters.Get(telemetryemit.MetricEventsEnqueued, "created") != 1 || h.wakes != 1 {
		t.Errorf("post-commit counters: %s wakes=%d", h.counters, h.wakes)
	}
}

func TestObserverCreateWithoutContextUsesCreatedBy(t *testing.T) {
	h := newHarness(t)
	h.write(context.Background(), store.AgentWrite{Op: store.AgentWriteCreate, After: baseAgent()})
	md := h.events()[0].Metadata
	if md.CreatorPrincipalID != "u-created-by" || md.CreatorPrincipalType != "" {
		t.Errorf("creator = %q/%q, want created_by with type unknown", md.CreatorPrincipalType, md.CreatorPrincipalID)
	}
}

// TestObserverSliceOneDoneWhenSequence is the unit form of the slice 1
// done-when test: create (observed), the dispatch result write
// (unobserved in slice 1), then a hard delete: created(1), gap at 2,
// resynced(unobserved_change, 3), deleted(4).
func TestObserverSliceOneDoneWhenSequence(t *testing.T) {
	h := newHarness(t)
	a := baseAgent()
	h.create(a)
	dispatched := clone(a)
	dispatched.Phase = "running"
	dispatched.RunID = "run-1"
	h.write(context.Background(), store.AgentWrite{Op: store.AgentWriteHardDelete, Before: dispatched})
	evs := h.events()
	if got, want := seq(evs), "created@1 resynced@3(unobserved_change) deleted@4(hard)"; got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
	if evs[1].Gap == nil || !*evs[1].Gap {
		t.Error("resynced(unobserved_change) must carry gap=true")
	}
	// run-1 was written unobserved: the cursor cannot prove it, so the
	// deleted omits launch_id (attribution design 6.2, D2 fail closed).
	if evs[2].LaunchID != "" || evs[2].Metadata == nil || evs[2].MetadataRevision != 1 {
		t.Errorf("deleted: launch_id=%q metadata_revision=%d, want no launch_id", evs[2].LaunchID, evs[2].MetadataRevision)
	}
	if _, ok := h.db.cursor(agentA); ok {
		t.Error("hard delete must remove the cursor")
	}
	if h.counters.Get(telemetryemit.MetricResyncs, tc.CauseUnobservedChange) != 1 {
		t.Errorf("resync counter: %s", h.counters)
	}
}

func TestObserverHardDeleteObservedNoResync(t *testing.T) {
	h := newHarness(t)
	a := baseAgent()
	h.create(a)
	h.write(context.Background(), store.AgentWrite{Op: store.AgentWriteHardDelete, Before: clone(a)})
	evs := h.events()
	if got := seq(evs); got != "created@1 deleted@2(hard)" {
		t.Fatalf("events = %s", got)
	}
	if !reflect.DeepEqual(evs[1].Metadata, evs[0].Metadata) || evs[1].MetadataRevision != 1 {
		t.Errorf("deleted must carry the last metadata unchanged:\n%+v\n%+v", evs[1].Metadata, evs[0].Metadata)
	}
}

func TestObserverSoftDeleteThenPurge(t *testing.T) {
	h := newHarness(t)
	a := baseAgent()
	h.create(a)
	deleted := clone(a)
	deleted.DeletedAt = t0.Add(time.Minute)
	h.write(context.Background(), store.AgentWrite{Op: store.AgentWriteSoftDelete, Before: clone(a), After: deleted})
	c, ok := h.db.cursor(agentA)
	if !ok || !c.LastDeleted || c.TransitionRevision != 2 {
		t.Fatalf("cursor after soft delete: %+v ok=%v", c, ok)
	}
	// Purge: already deleted and observed: nothing, cursor removed.
	h.write(context.Background(), store.AgentWrite{Op: store.AgentWritePurge, Before: clone(deleted)})
	if got := seq(h.events()); got != "created@1 deleted@2(soft)" {
		t.Fatalf("events = %s", got)
	}
	if _, ok := h.db.cursor(agentA); ok {
		t.Error("purge must remove the cursor")
	}
}

// TestObserverUnobservedSoftDeleteThenPurge is review r6 N2: the soft
// delete ran while the observer was off, then the row is purged.
func TestObserverUnobservedSoftDeleteThenPurge(t *testing.T) {
	for _, op := range []store.AgentWriteOp{store.AgentWritePurge, store.AgentWriteHardDelete, store.AgentWriteProjectDelete} {
		t.Run(string(op), func(t *testing.T) {
			h := newHarness(t)
			a := baseAgent()
			h.create(a)
			deleted := clone(a)
			deleted.DeletedAt = t0.Add(time.Minute)
			h.write(context.Background(), store.AgentWrite{Op: op, Before: deleted})
			mode := tc.DeleteModeHard
			switch op {
			case store.AgentWriteProjectDelete:
				mode = tc.DeleteModeProjectDeleted
			case store.AgentWritePurge:
				mode = tc.DeleteModePurged
			}
			if got, want := seq(h.events()), "created@1 resynced@3(unobserved_change) deleted@4("+mode+")"; got != want {
				t.Fatalf("events = %s, want %s", got, want)
			}
		})
	}
}

func TestObserverPreRolloutRows(t *testing.T) {
	t.Run("live hard delete without cursor", func(t *testing.T) {
		h := newHarness(t)
		h.write(context.Background(), store.AgentWrite{Op: store.AgentWriteHardDelete, Before: baseAgent()})
		evs := h.events()
		if got := seq(evs); got != "resynced@1(no_cursor) deleted@2(hard)" {
			t.Fatalf("events = %s", got)
		}
		if evs[0].Gap != nil || evs[0].Metadata.CreatorPrincipalID != "u-created-by" || evs[0].Metadata.CreatorPrincipalType != "" {
			t.Errorf("no_cursor resync: gap=%v creator=%+v", evs[0].Gap, evs[0].Metadata)
		}
	})
	t.Run("purge of deleted row without cursor", func(t *testing.T) {
		h := newHarness(t)
		a := baseAgent()
		a.DeletedAt = t0
		h.write(context.Background(), store.AgentWrite{Op: store.AgentWritePurge, Before: a})
		if evs := h.events(); len(evs) != 0 {
			t.Fatalf("events = %s, want none", seq(evs))
		}
	})
	t.Run("soft delete without cursor keeps a cursor", func(t *testing.T) {
		h := newHarness(t)
		a := baseAgent()
		d := clone(a)
		d.DeletedAt = t0
		h.write(context.Background(), store.AgentWrite{Op: store.AgentWriteSoftDelete, Before: a, After: d})
		if got := seq(h.events()); got != "resynced@1(no_cursor) deleted@2(soft)" {
			t.Fatalf("events = %s", got)
		}
		if c, ok := h.db.cursor(agentA); !ok || c.TransitionRevision != 2 || !c.LastDeleted || c.CreatorPrincipalID != "u-created-by" {
			t.Errorf("cursor: %+v", c)
		}
	})
}

func TestObserverRestoreUnobservedThenDelete(t *testing.T) {
	// Soft delete observed, restore unobserved (slice 1 does not observe
	// UpdateAgent), then a hard delete: the restore shows as a gap and
	// the deletion is emitted again with a new revision.
	h := newHarness(t)
	a := baseAgent()
	h.create(a)
	d := clone(a)
	d.DeletedAt = t0
	h.write(context.Background(), store.AgentWrite{Op: store.AgentWriteSoftDelete, Before: clone(a), After: d})
	h.write(context.Background(), store.AgentWrite{Op: store.AgentWriteHardDelete, Before: clone(a)})
	if got, want := seq(h.events()), "created@1 deleted@2(soft) resynced@4(unobserved_change) deleted@5(hard)"; got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
}

func TestObserverCreateRollback(t *testing.T) {
	h := newHarness(t)
	a := baseAgent()
	h.create(a)
	ctx := WithCause(context.Background(), CauseCreateRolledBack, "dispatch")
	h.write(ctx, store.AgentWrite{Op: store.AgentWriteHardDelete, Before: clone(a)})
	evs := h.events()
	if got := seq(evs); got != "created@1 launch_failed@2 deleted@3(create_rolled_back)" {
		t.Fatalf("events = %s", got)
	}
	if evs[1].EndReason != tc.EndReasonCreateRolledBack || evs[1].ErrorCode != "dispatch" || evs[1].Metadata != nil {
		t.Errorf("launch_failed: %+v", evs[1])
	}
}

func TestObserverMetadataRevisionFollowsDigest(t *testing.T) {
	h := newHarness(t)
	a := baseAgent()
	h.create(a)
	// An unobserved label change, then a soft delete.
	changed := clone(a)
	changed.Labels["stage"] = "review"
	deleted := clone(changed)
	deleted.DeletedAt = t0
	h.write(context.Background(), store.AgentWrite{Op: store.AgentWriteSoftDelete, Before: changed, After: deleted})
	evs := h.events()
	if got := seq(evs); got != "created@1 resynced@3(unobserved_change) deleted@4(soft)" {
		t.Fatalf("events = %s", got)
	}
	if evs[1].MetadataRevision != 2 || evs[2].MetadataRevision != 2 {
		t.Errorf("metadata revisions = %d, %d; want the resync to bump to 2 and deleted to keep it", evs[1].MetadataRevision, evs[2].MetadataRevision)
	}
	if evs[1].Metadata.Labels.Map()["stage"] != "review" {
		t.Errorf("resync snapshot must be the before row: %+v", evs[1].Metadata.Labels)
	}
	if c, _ := h.db.cursor(agentA); c.MetadataRevision != 2 {
		t.Errorf("cursor metadata revision = %d", c.MetadataRevision)
	}
}

func TestObserverBuildFailureMovesNothing(t *testing.T) {
	h := newHarness(t)
	a := baseAgent()
	h.create(a)
	bad := clone(a)
	bad.Labels["k"] = "bad\xff" // not valid UTF-8: the snapshot cannot be serialized
	h.write(context.Background(), store.AgentWrite{Op: store.AgentWriteHardDelete, Before: bad})
	if got := seq(h.events()); got != "created@1" {
		t.Fatalf("events = %s, want only created", got)
	}
	c, ok := h.db.cursor(agentA)
	if !ok || c.TransitionRevision != 1 {
		t.Fatalf("a build failure must not move the cursor: %+v ok=%v", c, ok)
	}
	if h.counters.Get(telemetryemit.MetricBuildFailures, "invalid_utf8") != 1 {
		t.Errorf("build failure not counted: %s", h.counters)
	}
	if !strings.Contains(h.logs.String(), "lifecycle event build failed") {
		t.Errorf("build failure not logged at ERROR: %s", h.logs)
	}
}

func TestObserverBuildFailureNextWriteResyncs(t *testing.T) {
	// A build failure on a soft delete leaves the cursor at the old
	// fingerprint; the next observed write sees the mismatch and resyncs.
	h := newHarness(t)
	a := baseAgent()
	h.create(a)
	bad := clone(a)
	bad.Labels["k"] = "bad\xff"
	badDeleted := clone(bad)
	badDeleted.DeletedAt = t0
	h.write(context.Background(), store.AgentWrite{Op: store.AgentWriteSoftDelete, Before: bad, After: badDeleted})
	good := clone(a)
	good.Labels["k"] = "fixed"
	good.DeletedAt = t0
	h.write(context.Background(), store.AgentWrite{Op: store.AgentWritePurge, Before: good})
	if got, want := seq(h.events()), "created@1 resynced@3(unobserved_change) deleted@4(purged)"; got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
}

// TestObserverDatabaseErrorRollsBack is crash injection (a): the observer
// fails before commit, so the write rolls back and no row exists.
func TestObserverDatabaseErrorRollsBack(t *testing.T) {
	h := newHarness(t)
	h.db.failInsert = errInjected
	err := h.db.observe(WithCreateContext(context.Background(), CreateContext{}), h.obs,
		store.AgentWrite{Op: store.AgentWriteCreate, After: baseAgent(), Now: t0})
	if !errors.Is(err, errInjected) {
		t.Fatalf("err = %v, want the injected failure", err)
	}
	if len(h.db.rows()) != 0 {
		t.Fatal("rolled-back write left an outbox row")
	}
	if _, ok := h.db.cursor(agentA); ok {
		t.Fatal("rolled-back write left a cursor")
	}
	if h.counters.Get(telemetryemit.MetricEventsEnqueued, "created") != 0 || h.wakes != 0 {
		t.Fatalf("a rolled-back write was counted: %s", h.counters)
	}
}

func TestObserverOpenRunClosureCarriesSegment(t *testing.T) {
	// Build-side enforcement of review r6 N6: a deleted that closes an
	// open segment carries segment_metadata_revision and the segment's
	// frozen run identity. Slice 1 never opens a segment; the cursor is
	// seeded directly.
	h := newHarness(t)
	a := baseAgent()
	h.create(a)
	h.db.stateMu.Lock()
	c := h.db.cursors[agentA]
	c.OpenRun, c.OpenRunGeneration, c.OpenRunMetadataRevision = "run-1", "1", 1
	// A coherent observed state: the cursor saw run-1 (attribution
	// last_run), so the segment is trusted and its closure named.
	c.Attribution.LastRun = "run-1"
	h.db.cursors[agentA] = c
	h.db.stateMu.Unlock()
	before := clone(a)
	before.RunID = "run-1"
	h.write(context.Background(), store.AgentWrite{Op: store.AgentWriteHardDelete, Before: before})
	evs := h.events()
	del := evs[len(evs)-1]
	if del.Transition != tc.TransitionDeleted || del.SegmentMetadataRevision == nil || *del.SegmentMetadataRevision != 1 ||
		del.LaunchID != "run-1" || del.IncarnationID != "1" {
		t.Fatalf("closing deleted: %+v", del)
	}

	// A cursor naming an open run without its frozen revision is a
	// malformed segment identity: no valid closure exists, so it is
	// discarded before Derive and the loss shows as one resync gap
	// (attribution design 3.3, P3 and R4-6), not a build failure.
	h2 := newHarness(t)
	h2.create(a)
	h2.db.stateMu.Lock()
	c2 := h2.db.cursors[agentA]
	c2.OpenRun = "run-1"
	h2.db.cursors[agentA] = c2
	h2.db.stateMu.Unlock()
	h2.write(context.Background(), store.AgentWrite{Op: store.AgentWriteHardDelete, Before: clone(a)})
	if got := seq(h2.events()); got != "created@1 resynced@3(unobserved_change) deleted@4(hard)" {
		t.Fatalf("events = %s, want the segment discarded with one gap", got)
	}
	if del := h2.events()[2]; del.SegmentMetadataRevision != nil || del.LaunchID != "" {
		t.Errorf("deleted after a discard carries segment fields: %+v", del)
	}
	if h2.counters.Get(telemetryemit.MetricBuildFailures, "segment_state") != 0 ||
		h2.counters.Get(telemetryemit.MetricAttributionStale, telemetryemit.AttributionStaleDiscarded) != 1 {
		t.Errorf("counters: %s", h2.counters)
	}
}

func TestObserverConcurrentWritersDenseRevisions(t *testing.T) {
	// Writers on one agent serialize on the row lock (memDB's lock): the
	// revisions are dense and follow commit order. Each writer here makes a
	// soft-delete-shaped write whose before row matches the cursor, so no
	// write resyncs.
	h := newHarness(t)
	a := baseAgent()
	h.create(a)
	var wg sync.WaitGroup
	var mu sync.Mutex
	state := clone(a)
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx := h.db.begin()
			mu.Lock()
			before := clone(state)
			after := clone(state)
			if before.DeletedAt.IsZero() {
				after.DeletedAt = t0
			} else {
				after.DeletedAt = time.Time{}
			}
			state = after
			mu.Unlock()
			op := store.AgentWriteSoftDelete
			if err := h.obs.ObserveAgentWrite(context.Background(), tx, store.AgentWrite{Op: op, Before: before, After: after, Now: t0}); err != nil {
				tx.rollback()
				errs <- err
				return
			}
			tx.commit()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	evs := h.events()
	for i, e := range evs {
		if e.Transition == tc.TransitionResynced {
			t.Fatalf("serialized writers must not resync: %s", seq(evs))
		}
		if e.TransitionRevision != int64(i+1) {
			t.Fatalf("revisions not dense: %s", seq(evs))
		}
		if i > 0 && e.TransitionRevision <= evs[i-1].TransitionRevision {
			t.Fatalf("revisions not increasing in commit order: %s", seq(evs))
		}
	}
	c, _ := h.db.cursor(agentA)
	if c.TransitionRevision != evs[len(evs)-1].TransitionRevision {
		t.Fatalf("cursor at %d, last event at %d", c.TransitionRevision, evs[len(evs)-1].TransitionRevision)
	}
}

func TestObserverStaleCursorOnCreateIsReplaced(t *testing.T) {
	h := newHarness(t)
	h.db.cursors[agentA] = store.LifecycleCursor{AgentID: agentA, TransitionRevision: 7, MetadataRevision: 3}
	h.create(baseAgent())
	if got := seq(h.events()); got != "created@1" {
		t.Fatalf("events = %s", got)
	}
	if c, _ := h.db.cursor(agentA); c.TransitionRevision != 1 || c.MetadataRevision != 1 {
		t.Fatalf("cursor = %+v, want a fresh one", c)
	}
}

func TestSnapshotDigestAndFingerprint(t *testing.T) {
	a := baseAgent()
	m1 := Snapshot(a, Creator{Type: "user", ID: "u1"})
	b := clone(a)
	b.Labels = map[string]string{}
	for _, k := range []string{"x/y.z-1", "empty", "stage"} { // different insertion order
		b.Labels[k] = a.Labels[k]
	}
	m2 := Snapshot(b, Creator{Type: "user", ID: "u1"})
	d1, err1 := MetadataDigest(m1)
	d2, err2 := MetadataDigest(m2)
	if err1 != nil || err2 != nil || d1 != d2 {
		t.Fatalf("digest not stable under map order: %v %v %s %s", err1, err2, d1, d2)
	}
	// The denormalized harness_config column is not a snapshot input.
	b.HarnessConfig = "rewritten-by-reconcile"
	if d, _ := MetadataDigest(Snapshot(b, Creator{Type: "user", ID: "u1"})); d != d1 {
		t.Error("harness_config column changed the digest")
	}
	// Fingerprint ignores the run ID but not the phase.
	fp := Fingerprint(a, d1)
	r := clone(a)
	r.RunID = "run-2"
	if Fingerprint(r, d1) != fp {
		t.Error("run_id changed the fingerprint")
	}
	p := clone(a)
	p.Phase = "running"
	if Fingerprint(p, d1) == fp {
		t.Error("phase did not change the fingerprint")
	}
	// deleted_at is compared at storage precision.
	x, y := clone(a), clone(a)
	x.DeletedAt = time.Date(2026, 1, 1, 0, 0, 0, 1_000_100, time.UTC)
	y.DeletedAt = time.Date(2026, 1, 1, 0, 0, 0, 1_000_999, time.UTC).In(time.FixedZone("x", 3600))
	if Fingerprint(x, d1) != Fingerprint(y, d1) {
		t.Error("fingerprint depends on sub-microsecond precision or zone")
	}
	// No ancestry beyond the root: no parent.
	solo := clone(a)
	solo.Ancestry = []string{"u1"}
	if Snapshot(solo, Creator{}).ParentAgentID != "" {
		t.Error("a root-only ancestry has no parent agent")
	}
	// No applied config: requested fields omitted, labels still [].
	bare := &store.Agent{ID: agentA, Name: "n"}
	m := Snapshot(bare, Creator{})
	if m.Labels == nil || len(m.Labels) != 0 || m.RequestedModel != "" {
		t.Errorf("bare snapshot: %+v", m)
	}
}

// TestObserverLabelRoundTrip carries the API's label shapes (printable
// ASCII punctuation, empty values) through the observer unchanged.
func TestObserverLabelRoundTrip(t *testing.T) {
	h := newHarness(t)
	a := baseAgent()
	a.Labels = map[string]string{`p/u.n-c_t`: `< > & " \ ' :`, "empty": "", "k": strings.Repeat("v", 63)}
	h.create(a)
	if got := h.events()[0].Metadata.Labels.Map(); !reflect.DeepEqual(got, a.Labels) {
		t.Fatalf("labels changed: %q", got)
	}
}

// TestObserverSoftDeleteOfRunningAgent is design r3 N3: a soft delete of a
// running agent emits only deleted (no stopped), and a zero-transition
// write then advances the fingerprint with no revision allocated (r2 B2).
func TestObserverSoftDeleteOfRunningAgent(t *testing.T) {
	h := newHarness(t)
	a := baseAgent()
	a.Phase = "running"
	a.RunID = "run-1"
	h.create(a)
	d := clone(a)
	d.DeletedAt = t0
	d.Phase = "stopped"
	h.write(context.Background(), store.AgentWrite{Op: store.AgentWriteSoftDelete, Before: clone(a), After: d})
	if got := seq(h.events()); got != "created@1 deleted@2(soft)" {
		t.Fatalf("events = %s", got)
	}
	// A soft finalize that rewrites deleted_at on the already-deleted row
	// derives nothing but keeps the fingerprint current.
	d2 := clone(d)
	d2.DeletedAt = t0.Add(time.Minute)
	h.write(context.Background(), store.AgentWrite{Op: store.AgentWriteSoftDelete, Before: clone(d), After: d2})
	c, _ := h.db.cursor(agentA)
	if c.TransitionRevision != 2 {
		t.Fatalf("zero-transition write allocated a revision: %+v", c)
	}
	// The cursor carries the requested template recorded at created.
	rs, err := newRowState(d2, Creator{Type: "user", ID: "u1", RequestedTemplate: a.Template})
	if err != nil {
		t.Fatal(err)
	}
	if want := Fingerprint(d2, rs.digest); c.LastFingerprint != want {
		t.Fatalf("zero-transition write did not advance the fingerprint to the committed row's")
	}
}
