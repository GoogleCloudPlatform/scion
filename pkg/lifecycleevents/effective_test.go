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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	tc "github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// Tests for the broker effective-configuration echo in the snapshot, the
// digests and the per-event filter (WP1 slice 3 addendum v12 sections 4
// and 4a).

// echoFor is a slice-3 broker echo for run.
func echoFor(run string) *store.AgentEffectiveConfig {
	return &store.AgentEffectiveConfig{
		RunID: run, Template: "dev-resolved", TemplateHash: "sha256:cd", Harness: "claude",
		Model: "m-large-1-20261001", HarnessConfig: "claude-default", HarnessConfigRevision: "rev-1",
		Image: "img:1", Profile: "p", Runtime: "docker", BrokerID: "b1", BrokerBuild: "v1.2.3",
		Source: store.EffectiveConfigSourceSync,
	}
}

// mintRun records run on a with a run-mint write.
func (h *harness) mintRun(a *store.Agent, run string) {
	h.t.Helper()
	b := clone(a)
	a.RunID = run
	h.write(context.Background(), store.AgentWrite{Op: store.AgentWriteRunID, Before: b, After: clone(a),
		Hints: store.AgentWriteHints{Kind: store.WriteKindRunMint}})
}

// echoWrite stores e on a with SetAgentEffectiveConfig's write.
func (h *harness) echoWrite(a *store.Agent, e *store.AgentEffectiveConfig) {
	h.t.Helper()
	b := clone(a)
	a.EffectiveConfig = e
	h.write(context.Background(), store.AgentWrite{Op: store.AgentWriteEffectiveConfig, Before: b, After: clone(a)})
}

// startWrite is a start path's own running write (E(i)).
func (h *harness) startWrite(a *store.Agent) {
	h.t.Helper()
	b := clone(a)
	a.Phase = "running"
	h.write(context.Background(), store.AgentWrite{Op: store.AgentWriteStatus, Before: b, After: clone(a),
		Hints: store.AgentWriteHints{StartWrite: true}})
}

// effOf renders an event's effective fields ("-" when all are omitted,
// "nometa" for a non-snapshot event).
func effOf(e *tc.LifecycleEvent) string {
	m := e.Metadata
	if m == nil {
		return "nometa"
	}
	s := strings.Join([]string{m.EffectiveTemplate, m.EffectiveTemplateHash, m.EffectiveModel,
		m.EffectiveHarness, m.HarnessConfigRevision, m.EffectiveBrokerBuild}, "|")
	if s == "|||||" {
		return "-"
	}
	return s
}

// evAt returns the event at transition revision rev.
func evAt(t *testing.T, evs []*tc.LifecycleEvent, rev int64) *tc.LifecycleEvent {
	t.Helper()
	for _, e := range evs {
		if e.TransitionRevision == rev {
			return e
		}
	}
	t.Fatalf("no event at revision %d in %s", rev, seq(evs))
	return nil
}

const echoR1 = "dev-resolved|sha256:cd|m-large-1-20261001|claude|rev-1|v1.2.3"

// TestEffectiveEcho_ConfiguredOnDigestChange: a stored echo for the row's
// run gives configured with effective_* and launch_id = the run; an
// identical echo gives nothing; a changed echo for the same run gives a new
// configured; an image-only change moves no digest and gives nothing.
func TestEffectiveEcho_ConfiguredOnDigestChange(t *testing.T) {
	h := newHarness(t)
	a := baseAgent()
	h.create(a)
	h.mintRun(a, "R1")
	h.startWrite(a)
	h.echoWrite(a, echoFor("R1"))
	evs := h.events()
	if got := seq(evs); got != "created@1 started@2 configured@3" {
		t.Fatalf("events = %s", got)
	}
	started, configured := evAt(t, evs, 2), evAt(t, evs, 3)
	if effOf(started) != "nometa" {
		t.Errorf("started carries a metadata block: %s", effOf(started))
	}
	if configured.LaunchID != "R1" || effOf(configured) != echoR1 {
		t.Errorf("configured launch_id=%q effective=%s", configured.LaunchID, effOf(configured))
	}
	if configured.MetadataRevision != started.MetadataRevision+1 {
		t.Errorf("configured metadata_revision %d, want %d", configured.MetadataRevision, started.MetadataRevision+1)
	}
	if configured.Metadata.HarnessConfigSource != "hub-hydrated" {
		t.Errorf("harness_config_source keeps its applied source, got %q", configured.Metadata.HarnessConfigSource)
	}

	h.echoWrite(a, echoFor("R1")) // an identical retry
	img := echoFor("R1")
	img.Image, img.Profile, img.Runtime, img.HarnessConfig = "img:2", "p2", "k8s", "other"
	img.BrokerID, img.LaunchID, img.Source, img.ReportedAt = "b2", "al-1", store.EffectiveConfigSourceLaunchReport, t0
	h.echoWrite(a, img) // only fields outside the digest
	if got := seq(h.events()); got != "created@1 started@2 configured@3" {
		t.Fatalf("an identical echo or a non-digest field emitted: %s", got)
	}

	changed := echoFor("R1")
	changed.Model = "m-large-2"
	h.echoWrite(a, changed)
	evs = h.events()
	if got := seq(evs); got != "created@1 started@2 configured@3 configured@4" {
		t.Fatalf("a changed echo for the same run: %s", got)
	}
	if c := evAt(t, evs, 4); c.Metadata.EffectiveModel != "m-large-2" || c.MetadataRevision != configured.MetadataRevision+1 {
		t.Errorf("changed configured: model %q, metadata_revision %d", c.Metadata.EffectiveModel, c.MetadataRevision)
	}
}

// TestEffectiveEcho_RestartFixture is review r1 B2: R1 with its echo, a
// restart mints R2 (no metadata_changed at the mint), R2's first event
// before its echo omits effective_* at the current metadata revision, and
// R2's echo, identical in values, gives configured with effective_*.
func TestEffectiveEcho_RestartFixture(t *testing.T) {
	h := newHarness(t)
	a := baseAgent()
	h.create(a)
	h.mintRun(a, "R1")
	h.startWrite(a)
	h.echoWrite(a, echoFor("R1"))
	m := evAt(t, h.events(), 3).MetadataRevision

	h.write(context.Background(), store.AgentWrite{Op: store.AgentWriteRestartStopLeg, Before: clone(a), After: clone(a),
		Hints: store.AgentWriteHints{StopLegRunID: "R1"}})
	before := len(h.events())
	h.mintRun(a, "R2")
	if len(h.events()) != before {
		t.Fatalf("a run mint emitted: %s", seq(h.events()))
	}
	h.startWrite(a)
	h.echoWrite(a, echoFor("R2"))
	evs := h.events()
	if got := seq(evs); got != "created@1 started@2 configured@3 stopped@4(restart_stop_leg) started@5 configured@6" {
		t.Fatalf("events = %s", got)
	}
	// started carries no block; its metadata_revision is the current M.
	if s := evAt(t, evs, 5); s.LaunchID != "R2" || effOf(s) != "nometa" || s.MetadataRevision != m {
		t.Errorf("R2 started: launch_id %q, effective %s, metadata_revision %d (want R2, nometa, %d)", s.LaunchID, effOf(s), s.MetadataRevision, m)
	}
	if c := evAt(t, evs, 6); c.LaunchID != "R2" || effOf(c) != echoR1 || c.MetadataRevision != m+1 {
		t.Errorf("R2 configured: launch_id %q, effective %s, metadata_revision %d (want R2, %s, %d)", c.LaunchID, effOf(c), c.MetadataRevision, echoR1, m+1)
	}
}

// TestEffectiveEcho_PatchBetweenMintAndEcho: a label change between R2's
// mint and R2's echo is metadata_changed with a new revision, the new
// labels and no effective_* (R1's are never inherited).
func TestEffectiveEcho_PatchBetweenMintAndEcho(t *testing.T) {
	h := newHarness(t)
	a := baseAgent()
	h.create(a)
	h.mintRun(a, "R1")
	h.startWrite(a)
	h.echoWrite(a, echoFor("R1"))
	h.mintRun(a, "R2")
	b := clone(a)
	a.Labels["stage"] = "review"
	h.write(context.Background(), store.AgentWrite{Op: store.AgentWriteUpdate, Before: b, After: clone(a)})
	evs := h.events()
	mc := evs[len(evs)-1]
	if mc.Transition != tc.TransitionMetadataChanged {
		t.Fatalf("events = %s", seq(evs))
	}
	if effOf(mc) != "-" || mc.Metadata.Labels.Map()["stage"] != "review" || mc.LaunchID != "R2" {
		t.Errorf("metadata_changed: effective %s, labels %v, launch_id %q", effOf(mc), mc.Metadata.Labels.Map(), mc.LaunchID)
	}
	if mc.MetadataRevision != evAt(t, evs, 3).MetadataRevision+1 {
		t.Errorf("the label change must advance metadata_revision")
	}
}

// TestEffectiveEcho_DigestComponent is review r2 O2 and N3: with no echo
// the digests are exactly the slice-2 ones; the echo's run is part of both
// digests; fields outside the six emitted ones do not enter them.
func TestEffectiveEcho_DigestComponent(t *testing.T) {
	cr := Creator{Type: "user", ID: "u1", RequestedTemplate: "dev"}
	a := baseAgent()
	rs, err := newRowState(a, cr)
	if err != nil {
		t.Fatal(err)
	}
	m := Snapshot(a, cr)
	d, _ := MetadataDigest(m)
	cd, _ := ConfigDigest(m)
	if rs.digest != d || rs.configDigest != cd {
		t.Fatal("no echo must leave both digests as the plain snapshot digests")
	}

	digests := func(e *store.AgentEffectiveConfig) (string, string) {
		b := clone(a)
		b.EffectiveConfig = e
		rs, err := newRowState(b, cr)
		if err != nil {
			t.Fatal(err)
		}
		return rs.digest, rs.configDigest
	}
	d1, c1 := digests(echoFor("R1"))
	d2, c2 := digests(echoFor("R2"))
	if d1 == d || c1 == cd {
		t.Error("an echo must move both digests")
	}
	if d1 == d2 || c1 == c2 {
		t.Error("the same values for another run must move both digests")
	}
	other := echoFor("R1")
	other.Image, other.Profile, other.Runtime, other.HarnessConfig, other.HarnessConfigSource = "img:9", "p9", "k8s", "x", "y"
	other.BrokerID, other.LaunchID, other.Source, other.ReportedAt = "b9", "al-9", "launch_report", t0
	if d3, c3 := digests(other); d3 != d1 || c3 != c1 {
		t.Error("fields outside the six emitted ones and the run must not enter the digests")
	}
	for name, mutate := range map[string]func(e *store.AgentEffectiveConfig){
		"template":         func(e *store.AgentEffectiveConfig) { e.Template = "x" },
		"template hash":    func(e *store.AgentEffectiveConfig) { e.TemplateHash = "x" },
		"model":            func(e *store.AgentEffectiveConfig) { e.Model = "x" },
		"harness":          func(e *store.AgentEffectiveConfig) { e.Harness = "x" },
		"harness revision": func(e *store.AgentEffectiveConfig) { e.HarnessConfigRevision = "x" },
		"broker build":     func(e *store.AgentEffectiveConfig) { e.BrokerBuild = "x" },
	} {
		e := echoFor("R1")
		mutate(e)
		if d4, c4 := digests(e); d4 == d1 || c4 == c1 {
			t.Errorf("%s must enter both digests", name)
		}
	}
}

// TestEffectiveEcho_DeriveRowOtherNoEvidence is review r1 N4: the
// effective-config write is K=Other with no evidence, so a running row
// with no open segment and the same run before and after gives configured
// only, never started.
func TestEffectiveEcho_DeriveRowOtherNoEvidence(t *testing.T) {
	f := Facts{
		Op: store.AgentWriteEffectiveConfig, Kind: store.WriteKindOther,
		HasBefore: true, BeforeLive: true, HasAfter: true,
		BeforePhase: "running", AfterPhase: "running", BeforeRunID: "R1", AfterRunID: "R1",
		BeforeGeneration: 1, AfterGeneration: 1,
		ConfigResolution: true,
		BeforeDigest:     "d0", AfterDigest: "d1", BeforeConfigDigest: "c0", AfterConfigDigest: "c1",
		Cursor: CursorFacts{Exists: true, FingerprintMatches: true, Digest: "d0"},
		Now:    t0,
	}
	writeFacts(context.Background(), store.AgentWrite{Op: store.AgentWriteEffectiveConfig}, &f)
	if !f.ConfigResolution || f.Evidence != EvidenceNone {
		t.Fatalf("the op must be a config resolution with no evidence: %+v", f)
	}
	var got []string
	for _, p := range Derive(f).Events {
		got = append(got, p.Transition)
	}
	if strings.Join(got, " ") != tc.TransitionConfigured {
		t.Errorf("events = %v, want configured only", got)
	}
}

// TestEffectiveEcho_EchoWriteSupersedesOldRun is N6 at the observer: when
// R1's segment is still open (a queued restart minted R2 without a stop
// leg), R2's echo write first closes R1 as stopped(superseded) with R1's
// identity, then emits configured for R2 with R2's effective_*.
func TestEffectiveEcho_EchoWriteSupersedesOldRun(t *testing.T) {
	h := newHarness(t)
	a := baseAgent()
	h.create(a)
	h.mintRun(a, "R1")
	h.startWrite(a)
	h.mintRun(a, "R2") // run writes do not close the open segment
	if cur, _ := h.db.cursor(agentA); cur.OpenRun != "R1" {
		t.Fatalf("fixture: R1's segment must be open, cursor open run %q", cur.OpenRun)
	}
	h.echoWrite(a, echoFor("R2"))
	evs := h.events()
	if got := seq(evs); got != "created@1 started@2 stopped@3 configured@4" {
		t.Fatalf("events = %s", got)
	}
	if s := evAt(t, evs, 3); s.StopKind != tc.StopKindSuperseded || s.LaunchID != "R1" || effOf(s) != "nometa" {
		t.Errorf("superseded: stop_kind %q launch_id %q effective %s", s.StopKind, s.LaunchID, effOf(s))
	}
	if c := evAt(t, evs, 4); c.LaunchID != "R2" || effOf(c) != echoR1 {
		t.Errorf("configured: launch_id %q effective %s", c.LaunchID, effOf(c))
	}
}

// TestEffectiveEcho_OldSegmentClosure (token-analyst-2 02:12): a deleted
// closing R1's open segment while the row holds R2 with R2's echo takes
// launch_id R1 from the cursor and so omits effective_*; it keeps the
// metadata revision it was given (the current one) and filters only its
// own copy of the block, which is never digested.
func TestEffectiveEcho_OldSegmentClosure(t *testing.T) {
	a := baseAgent()
	a.RunID, a.Phase, a.DeletedAt = "R2", "stopped", t0
	a.EffectiveConfig = echoFor("R2")
	md := Snapshot(a, Creator{Type: "user", ID: "u1", RequestedTemplate: "dev"})
	cursor := &store.LifecycleCursor{AgentID: agentA, OpenRun: "R1", OpenRunGeneration: "1", OpenRunMetadataRevision: 2}
	ev, err := buildEvent(eventInput{
		Planned:            Planned{Transition: tc.TransitionDeleted, Row: RowAfter, DeleteMode: tc.DeleteModeSoft, ClosesOpenRun: true},
		Row:                a,
		TransitionRevision: 5, MetadataRevision: 3, Metadata: &md, Cursor: cursor,
		EventID: "00000000-0000-4000-8000-000000000001", HubID: "hub-a", ProducerBuild: "v", EventTS: t0, ObservedTS: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ev.LaunchID != "R1" || effOf(ev) != "-" {
		t.Errorf("deleted closing R1: launch_id %q effective %s", ev.LaunchID, effOf(ev))
	}
	if ev.MetadataRevision != 3 || ev.SegmentMetadataRevision == nil || *ev.SegmentMetadataRevision != 2 {
		t.Errorf("metadata_revision %d segment %v, want 3 and R1's frozen 2", ev.MetadataRevision, ev.SegmentMetadataRevision)
	}
	if md.EffectiveModel == "" {
		t.Error("the filter must not change the shared snapshot")
	}
}

// TestEffectiveEcho_BaseRunEventsOmit: an event with no launch_id (the
// reincarnation base-run events, OmitLaunchID) never carries effective_*,
// and an event tagged NextGeneration with launch_id R2 keeps R2's echo.
func TestEffectiveEcho_BaseRunEventsOmit(t *testing.T) {
	a := baseAgent()
	a.RunID = "R2"
	a.EffectiveConfig = echoFor("R2")
	md := Snapshot(a, Creator{Type: "user", ID: "u1", RequestedTemplate: "dev"})
	build := func(p Planned) *tc.LifecycleEvent {
		ev, err := buildEvent(eventInput{Planned: p, Row: a, TransitionRevision: 3, MetadataRevision: 2,
			Metadata: &md, EventID: "00000000-0000-4000-8000-000000000001", HubID: "hub-a",
			ProducerBuild: "v", EventTS: t0, ObservedTS: t0})
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}
	base := build(Planned{Transition: tc.TransitionConfigured, Row: RowAfter, NextGeneration: true, OmitLaunchID: true})
	if base.LaunchID != "" || effOf(base) != "-" || base.IncarnationID != "2" {
		t.Errorf("base-run configured: launch_id %q, effective %s, incarnation %q", base.LaunchID, effOf(base), base.IncarnationID)
	}
	next := build(Planned{Transition: tc.TransitionConfigured, Row: RowAfter, NextGeneration: true})
	if next.LaunchID != "R2" || effOf(next) != echoR1 || next.IncarnationID != "2" {
		t.Errorf("R2's configured: launch_id %q, effective %s, incarnation %q", next.LaunchID, effOf(next), next.IncarnationID)
	}
	if md.EffectiveModel == "" {
		t.Error("the filter must work on the event's copy, not the shared snapshot")
	}
}

// TestEffectiveEcho_RequestedTemplateProvenance is addendum v12 section 4a:
// request template A, the broker echoes B and the dispatch result rewrites
// agents.template to B. requested_template stays A (from the cursor);
// effective_template is B; the row's template change alone emits nothing.
func TestEffectiveEcho_RequestedTemplateProvenance(t *testing.T) {
	h := newHarness(t)
	a := baseAgent()
	a.Template = "A"
	h.create(a)
	h.mintRun(a, "R1")
	e := echoFor("R1")
	e.Template = "B"
	h.echoWrite(a, e)
	b := clone(a)
	a.Phase, a.Template = "running", "B"
	h.write(WithDispatchResult(context.Background(), NewDispatchResult("R1", "R1", true)),
		store.AgentWrite{Op: store.AgentWriteUpdate, Before: b, After: clone(a)})
	evs := h.events()
	if got := seq(evs); got != "created@1 configured@2 started@3" {
		t.Fatalf("events = %s", got)
	}
	for _, ev := range evs {
		if ev.Metadata != nil && ev.Metadata.RequestedTemplate != "A" {
			t.Errorf("%s requested_template %q, want A", ev.Transition, ev.Metadata.RequestedTemplate)
		}
	}
	if got := evAt(t, evs, 2).Metadata.EffectiveTemplate; got != "B" {
		t.Errorf("configured effective_template %q, want B", got)
	}
	if evAt(t, evs, 3).MetadataRevision != evAt(t, evs, 2).MetadataRevision {
		t.Error("the row's template rewrite must not move the metadata revision")
	}
}

// TestEffectiveEcho_LegacyCursorOmitsRequestedTemplate is review r1 N3: an
// agent first seen without a cursor (pre-slice-3, resynced(no_cursor)) has
// requested_template omitted, never agents.template; a cursor created
// before slice 3 (no recorded template) omits it too.
func TestEffectiveEcho_LegacyCursorOmitsRequestedTemplate(t *testing.T) {
	h := newHarness(t)
	a := baseAgent()
	b := clone(a)
	a.Labels["stage"] = "review"
	h.write(context.Background(), store.AgentWrite{Op: store.AgentWriteUpdate, Before: b, After: clone(a)})
	evs := h.events()
	if len(evs) == 0 || evs[0].Transition != tc.TransitionResynced {
		t.Fatalf("events = %s", seq(evs))
	}
	for _, ev := range evs {
		if ev.Metadata != nil && ev.Metadata.RequestedTemplate != "" {
			t.Errorf("%s requested_template %q, want omitted", ev.Transition, ev.Metadata.RequestedTemplate)
		}
	}
	if c, _ := h.db.cursor(agentA); c.RequestedTemplate != "" {
		t.Errorf("a cursor created after the create must not record a template, got %q", c.RequestedTemplate)
	}
}

const examplesSlice3Path = "testdata/lifecycle-examples-slice3.jsonl"

// TestLifecycleExamplesSlice3 produces the slice 3 example events through
// the observer and Marshal (goldens with effective fields): a sync create
// whose echo gives configured with effective_* before the E(iv) started, a
// restart whose R2 started omits effective_* until R2's echo, and an async
// launch whose succeeded report gives started and then the echo
// configured. Regenerate with -update-examples.
func TestLifecycleExamplesSlice3(t *testing.T) {
	h := newHarness(t)
	at := func(s int) time.Time { return t0.Add(-time.Minute + time.Duration(s)*time.Second) }
	ctx := context.Background()
	write := func(ctx context.Context, op store.AgentWriteOp, a *store.Agent, mutate func(), sec int, hints store.AgentWriteHints) {
		b := clone(a)
		mutate()
		h.write(ctx, store.AgentWrite{Op: op, Before: b, After: clone(a), Now: at(sec), Hints: hints})
	}

	// a1: sync create with the echo, then a restart.
	a := baseAgent()
	h.write(WithCreateContext(ctx, CreateContext{CreatorType: "user", CreatorID: "u1"}),
		store.AgentWrite{Op: store.AgentWriteCreate, After: clone(a), Now: at(0)})
	write(ctx, store.AgentWriteRunID, a, func() { a.RunID = "run-1" }, 1, store.AgentWriteHints{Kind: store.WriteKindRunMint})
	write(ctx, store.AgentWriteEffectiveConfig, a, func() { a.EffectiveConfig = echoFor("run-1") }, 2, store.AgentWriteHints{})
	write(WithDispatchResult(ctx, NewDispatchResult("run-1", "run-1", true)), store.AgentWriteUpdate, a,
		func() { a.Phase, a.Template = "running", "dev-resolved" }, 2, store.AgentWriteHints{})
	h.write(ctx, store.AgentWrite{Op: store.AgentWriteRestartStopLeg, Before: clone(a), After: clone(a), Now: at(3),
		Hints: store.AgentWriteHints{StopLegRunID: "run-1"}})
	write(ctx, store.AgentWriteRunID, a, func() { a.RunID = "run-2" }, 3, store.AgentWriteHints{Kind: store.WriteKindRunMint})
	write(ctx, store.AgentWriteStatus, a, func() {}, 4, store.AgentWriteHints{StartWrite: true})
	write(ctx, store.AgentWriteEffectiveConfig, a, func() { a.EffectiveConfig = echoFor("run-2") }, 4, store.AgentWriteHints{})

	// a2: an accepted async launch, its succeeded report, then the echo.
	c := baseAgent()
	c.ID, c.Slug, c.Name, c.RunID = "00000000-0000-4000-8000-0000000000a2", "a2", "a2", "run-7"
	h.write(WithCreateContext(ctx, CreateContext{CreatorType: "user", CreatorID: "u1"}),
		store.AgentWrite{Op: store.AgentWriteCreate, After: clone(c), Now: at(10)})
	write(ctx, store.AgentWriteLaunch, c, func() { c.LaunchID, c.LaunchState, c.Phase = "al-1", store.LaunchStateActive, "provisioning" }, 11,
		store.AgentWriteHints{LaunchAccepted: true, LaunchOwner: "bi-1"})
	write(ctx, store.AgentWriteLaunch, c, func() { c.LaunchState, c.Phase = store.LaunchStateEnded, "running" }, 12,
		store.AgentWriteHints{LaunchReport: true})
	write(ctx, store.AgentWriteEffectiveConfig, c, func() {
		e := echoFor("run-7")
		e.LaunchID, e.Source = "al-1", store.EffectiveConfigSourceLaunchReport
		c.EffectiveConfig = e
	}, 12, store.AgentWriteHints{})

	if want := "created@1 configured@2 started@3 stopped@4(restart_stop_leg) started@5 configured@6 " +
		"created@1 launch_accepted@2 started@3 configured@4"; seq(h.events()) != want {
		t.Fatalf("example sequence changed:\n got %s\nwant %s", seq(h.events()), want)
	}
	for _, e := range h.events() {
		if e.Transition == tc.TransitionConfigured && effOf(e) == "-" {
			t.Errorf("%s configured@%d carries no effective_*", e.AgentID, e.TransitionRevision)
		}
	}

	var buf bytes.Buffer
	for _, row := range h.db.rows() {
		buf.Write(row.Payload)
		buf.WriteByte('\n')
	}
	if *updateExamples {
		if err := os.WriteFile(filepath.FromSlash(examplesSlice3Path), buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(filepath.FromSlash(examplesSlice3Path))
	if err != nil {
		t.Fatalf("%v (run with -update-examples to create it)", err)
	}
	if !bytes.Equal(want, buf.Bytes()) {
		t.Errorf("examples differ from %s; rerun with -update-examples if the change is intended:\n%s", examplesSlice3Path, buf.String())
	}
}
