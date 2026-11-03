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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	tc "github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// finalLaunch is the launch_id the builder gives p (closure override,
// then the attribution).
func finalLaunch(f Facts, p Planned) string {
	l := ""
	if !p.OmitLaunchID {
		if p.Row == RowBefore {
			l = f.BeforeRunID
		} else {
			l = f.AfterRunID
		}
	}
	if p.ClosesOpenRun {
		l = f.Cursor.Run.OpenRun
	}
	if p.AttrOmitLaunch {
		return ""
	}
	if p.AttrLaunch != "" {
		return p.AttrLaunch
	}
	return l
}

// attrCase is one point of the attribution axes (design 10.2).
type attrCase struct {
	state       string // clear, clear-run-mismatch, clear-gen-mismatch, clear-undo, held-attr, held-unattr, held-run-mismatch, held-gen-mismatch, legacy, stale, corrupt, missing
	segment     string // none, trusted, untrusted, malformed
	segOnBefore bool   // the open segment's run is the before run (else the after run)
	mode        string // complete, unknown
	witness     string // none, after, other
	write       string // mint-fresh, mint-same, mint-prev, mint-quar, revert-quar, revert-new, adopt-quar, adopt-new, rebind, soft-finish, soft-same, bump, bump-stop, status-stop, update, hard-delete
	targetQuar  bool
	inFlight    bool
}

func (c attrCase) String() string {
	return fmt.Sprintf("%s/%s(onBefore=%v)/%s/w=%s/%s/q=%v/fl=%v", c.state, c.segment, c.segOnBefore, c.mode, c.witness, c.write, c.targetQuar, c.inFlight)
}

// build turns the case into a cursor, facts and the expectation inputs.
func (c attrCase) build() (*store.LifecycleCursor, Facts, bool) {
	now := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	cur := &store.LifecycleCursor{AgentID: agentA, TransitionRevision: 5, MetadataRevision: 2, UpdatedAt: now, LastFingerprint: "fp"}
	stamp := now
	cur.AttributionStamp = &stamp
	bRun, bGen := "R1", 1
	a := &cur.Attribution
	a.QuarantineMode = store.QuarantineComplete
	if c.mode == "unknown" {
		a.QuarantineMode = store.QuarantineUnknown
	}
	a.QuarantinedRuns = "Q1"
	switch c.state {
	case "clear":
		a.State, a.LastRun, a.LastGeneration = store.AttributionClear, bRun, "1"
	case "clear-run-mismatch":
		a.State, a.LastRun, a.LastGeneration = store.AttributionClear, "R0", "1"
	case "clear-gen-mismatch":
		a.State, a.LastRun, a.LastGeneration = store.AttributionClear, bRun, "0"
	case "clear-undo":
		a.State, a.LastRun, a.LastGeneration = store.AttributionClear, bRun, "1"
		a.AttributedRun, a.AttributedGeneration = "A1", "1"
	case "held-attr":
		a.State, a.LastRun, a.LastGeneration = store.AttributionHeld, bRun, "1"
		a.AttributedRun, a.AttributedGeneration = "A1", "1"
		a.QuarantinedRuns = "R1,Q1"
	case "held-unattr":
		a.State, a.LastRun, a.LastGeneration = store.AttributionHeld, bRun, "1"
		a.QuarantinedRuns = "R1,Q1"
	case "held-run-mismatch":
		a.State, a.LastRun, a.LastGeneration = store.AttributionHeld, "R0", "1"
		a.AttributedRun, a.AttributedGeneration = "A1", "1"
		a.QuarantinedRuns = "R0,Q1"
	case "held-gen-mismatch":
		a.State, a.LastRun, a.LastGeneration = store.AttributionHeld, bRun, "0"
		a.AttributedRun, a.AttributedGeneration = "A1", "1"
		a.QuarantinedRuns = "R1,Q1"
	case "legacy":
		*a = store.LifecycleAttribution{}
		cur.AttributionStamp = nil
	case "stale":
		a.State, a.LastRun, a.LastGeneration = store.AttributionClear, bRun, "1"
		later := now.Add(time.Second)
		cur.UpdatedAt = later
	case "legacy-partial-marker", "legacy-bad-mode":
		// LEGACY (state "", NULL stamp) carrying a CORRUPT field, set after
		// the segment below (RQ-1, TA-1).
		*a = store.LifecycleAttribution{}
		cur.AttributionStamp = nil
	case "corrupt":
		a.State, a.LastRun, a.LastGeneration = "bogus", bRun, "1"
	case "missing":
		cur = nil
	}
	aRun, aGen := bRun, bGen
	f := Facts{HasBefore: true, BeforeLive: true, HasAfter: true, BeforePhase: "stopped", AfterPhase: "stopped",
		BeforeGeneration: bGen, AfterGeneration: bGen, BeforeDigest: "d", AfterDigest: "d",
		BeforeConfigDigest: "c", AfterConfigDigest: "c", Op: store.AgentWriteUpdate}
	target := "N1"
	if c.targetQuar {
		target = "Q1"
	}
	switch c.write {
	case "mint-fresh":
		f.Op, f.Kind, aRun = store.AgentWriteRunID, store.WriteKindRunMint, target
	case "mint-same":
		f.Op, f.Kind = store.AgentWriteRunID, store.WriteKindRunMint
	case "mint-prev":
		f.Op, f.Kind, aRun = store.AgentWriteRunID, store.WriteKindRunMint, "P1"
		f.BeforePreviousRunIDs = []string{"P1"}
	case "revert-quar", "revert-new":
		f.Op, f.Kind, aRun = store.AgentWriteRunID, store.WriteKindRunRevert, target
	case "adopt-quar", "adopt-new", "rebind":
		f.Op, f.Kind, aRun, f.AdoptionSource = store.AgentWriteRunID, store.WriteKindRunAdopt, target, store.AdoptionSourceCAS
	case "soft-finish":
		f.Op, f.Kind, aRun, f.AfterDeleted = store.AgentWriteSoftDelete, store.WriteKindDeletion, target, true
	case "soft-same":
		f.Op, f.Kind, f.AfterDeleted = store.AgentWriteSoftDelete, store.WriteKindDeletion, true
	case "bump":
		aGen = bGen + 1
	case "bump-stop":
		aGen = bGen + 1
		f.BeforePhase, f.AfterPhase = "running", "stopped"
	case "status-stop":
		f.Op, f.Kind = store.AgentWriteStatus, store.WriteKindStopRecord
		f.BeforePhase, f.AfterPhase = "running", "stopped"
	case "update":
		f.AfterDigest = "d2"
	case "hard-delete":
		f.Op, f.HasAfter = store.AgentWriteHardDelete, false
	}
	if c.write == "revert-new" || c.write == "adopt-new" {
		aRun = "N2"
	}
	f.BeforeRunID, f.AfterRunID, f.AfterGeneration = bRun, aRun, aGen
	if !f.HasAfter {
		f.AfterRunID, f.AfterGeneration = "", 0
	}
	if cur != nil {
		if c.inFlight {
			cur.ReincarnationBaseRunID = "BASE"
			f.BeforeReincarnationInFlight, f.AfterReincarnationInFlight = true, true
		}
		switch c.witness {
		case "after":
			a.ReincarnationMintRun = aRun
		case "other":
			a.ReincarnationMintRun = "W9"
		}
		if c.witness != "none" && cur.ReincarnationBaseRunID == "" {
			cur.ReincarnationBaseRunID = "BASE"
		}
		segRun := bRun
		if !c.segOnBefore && c.write != "rebind" {
			segRun = aRun
		}
		switch c.segment {
		case "trusted":
			cur.OpenRun, cur.OpenRunGeneration, cur.OpenRunMetadataRevision = segRun, "1", 2
		case "untrusted":
			cur.OpenRun, cur.OpenRunGeneration, cur.OpenRunMetadataRevision = segRun, "1", 2
			a.UntrustedOpenRun, a.UntrustedOpenRunGeneration, a.UntrustedOpenRunRev = segRun, "1", 2
		case "malformed":
			cur.OpenRun, cur.OpenRunGeneration, cur.OpenRunMetadataRevision = segRun, "", 2
		}
		switch c.state {
		case "legacy-partial-marker":
			a.UntrustedOpenRun, a.UntrustedOpenRunGeneration, a.UntrustedOpenRunRev = segRun, "", 0
		case "legacy-bad-mode":
			a.QuarantineMode = "weird"
		}
		f.Cursor = CursorFacts{Exists: true, OpenRun: cur.OpenRun, Run: cur.RunState(), FingerprintMatches: true, Digest: "d"}
	}
	historyUnknown := c.state == "legacy" || c.state == "stale" || c.corruptLike() || c.state == "missing" || c.segment == "malformed"
	return cur, f, historyUnknown
}

// TestAttributionAxesExhaustive composes PrepareAttribution, Derive and
// Attribute over the attribution axes (design 10.2) and checks the
// invariants on every point.
func TestAttributionAxesExhaustive(t *testing.T) {
	states := []string{"clear", "clear-run-mismatch", "clear-gen-mismatch", "clear-undo", "held-attr", "held-unattr",
		"held-run-mismatch", "held-gen-mismatch", "legacy", "stale", "corrupt", "legacy-partial-marker", "legacy-bad-mode", "missing"}
	segments := []string{"none", "trusted", "untrusted", "malformed"}
	writes := []string{"mint-fresh", "mint-same", "mint-prev", "revert-quar", "revert-new", "adopt-quar", "adopt-new", "rebind",
		"soft-finish", "soft-same", "bump", "bump-stop", "status-stop", "update", "hard-delete"}
	n := 0
	for _, st := range states {
		for _, sg := range segments {
			for _, onBefore := range []bool{true, false} {
				for _, mode := range []string{"complete", "unknown"} {
					for _, w := range []string{"none", "after", "other"} {
						for _, wr := range writes {
							for _, q := range []bool{false, true} {
								for _, fl := range []bool{false, true} {
									c := attrCase{state: st, segment: sg, segOnBefore: onBefore, mode: mode, witness: w, write: wr, targetQuar: q, inFlight: fl}
									checkAttrCase(t, c)
									n++
								}
							}
						}
					}
				}
			}
		}
	}
	t.Logf("%d attribution cases", n)
}

func checkAttrCase(t *testing.T, c attrCase) {
	t.Helper()
	cur, f, historyUnknown := c.build()
	pre := PrepareAttribution(&f, cur)
	plan := Derive(f)
	out := Attribute(f, pre, &plan)
	run := plan.Run
	if plan.FreezeSegment {
		run.OpenRunMetadataRevision = 3
	}
	FinalizeAttribution(f, plan, run, &out)
	fail := func(format string, args ...any) {
		t.Helper()
		t.Errorf("%s: %s; events %v", c, fmt.Sprintf(format, args...), plannedString(f, plan))
	}

	// The malformed segment never reaches Derive or the builder, and the
	// discard shows as exactly one gap (P3, R4-6).
	if c.segment == "malformed" && cur != nil {
		if f.Cursor.Run.OpenRun != "" || f.Cursor.OpenRun != "" {
			fail("malformed segment not discarded")
		}
		gaps := 0
		for _, p := range plan.Events {
			if p.ClosesOpenRun {
				fail("closure planned for a discarded segment")
			}
			if p.Transition == tc.TransitionResynced && p.Gap {
				gaps++
			}
		}
		if gaps != 1 {
			fail("%d gaps for a discard, want 1", gaps)
		}
	}

	quar := map[string]bool{}
	for _, q := range strings.Split(out.State.QuarantinedRuns, ",") {
		if q != "" {
			quar[q] = true
		}
	}
	preHeldRun := ""
	if pre.s.held() {
		preHeldRun = pre.s.lastRun
	}
	postHeldRun := ""
	if out.Has && out.State.State == store.AttributionHeld {
		postHeldRun = out.State.LastRun
	}
	// Every valid segment on a CORRUPT cursor (LEGACY included) is
	// untrusted (design 3.3 order step 2; OP-1, RQ-1).
	preSegUntrusted := cur != nil && (c.segment == "untrusted" || (c.corruptLike() && c.segment == "trusted"))
	for _, p := range plan.Events {
		l := finalLaunch(f, p)
		if l == "" {
			continue
		}
		closure := p.ClosesOpenRun || p.ClosesSegment
		// (2) An untrusted segment is never named.
		if closure && preSegUntrusted {
			fail("%s names an untrusted segment %s", p.Transition, l)
		}
		// (1) Nothing names a held or quarantined run, except the R4
		// closure of a valid named segment at a history-unknown
		// normalisation, and the attributed identity.
		if p.AttrLaunch != "" {
			// (5) An attributed name requires generation equality.
			gen := f.AfterGeneration
			if p.Row == RowBefore {
				gen = f.BeforeGeneration
			}
			if p.NextGeneration {
				gen++
			}
			s := pre.s
			if p.Row == RowAfter && out.Has {
				s = out.post
			}
			if s.attrGen != itoa(gen) {
				fail("%s names %s with generation %d, attributed %s", p.Transition, l, gen, s.attrGen)
			}
			continue
		}
		// The R4 class: a valid named segment without a marker on a
		// history-unknown normalisation, or persisted in a stored held
		// state (only reachable from one, T11s'), closes with its frozen
		// pair unless this write pushes or holds that run (R5-1, R6-1).
		segRun := cur0Seg(c)
		// Design 10.2: the exception is LEGACY or stale only, never CORRUPT
		// or missing (OP-1).
		r4 := closure && c.segment == "trusted" && !firingSources(c, f)[segRun] &&
			(c.state == "legacy" || c.state == "stale" || strings.HasPrefix(c.state, "held"))
		if !r4 && (l == preHeldRun && p.Row == RowBefore || l == postHeldRun && p.Row == RowAfter) {
			fail("%s names the held run %s", p.Transition, l)
		}
		if !r4 && closure && quar[l] {
			fail("closure %s names quarantined %s", p.Transition, l)
		}
	}
	if !out.Has {
		return
	}
	post := out.State
	isMint := f.Kind == store.WriteKindRunMint
	// (3) Only an observed fresh mint to a non-empty, different,
	// non-quarantined run clears a held PRE.
	if pre.s.held() && post.State == store.AttributionClear {
		ok := isMint && f.AfterRunID != "" && f.AfterRunID != f.BeforeRunID && !pre.s.inQuar(f.AfterRunID) &&
			!containsRun(f.BeforePreviousRunIDs, f.AfterRunID)
		if !ok {
			fail("held PRE cleared by %s", c.write)
		}
	}
	// (9) A non-fresh mint never yields clear, except the same-value no-op.
	if isMint && f.AfterRunID != f.BeforeRunID && post.State == store.AttributionClear {
		if containsRun(f.BeforePreviousRunIDs, f.AfterRunID) || pre.s.inQuar(f.AfterRunID) {
			fail("non-fresh mint yields clear")
		}
	}
	// (7) Under UNKNOWN no non-mint run change yields clear.
	if pre.s.mode == store.QuarantineUnknown && !isMint && f.AfterRunID != f.BeforeRunID && post.State == store.AttributionClear {
		fail("non-mint run change clear under UNKNOWN")
	}
	// (4) Composition: a run-changing deletion from a held PRE keeps the
	// attribution.
	if c.write == "soft-finish" && pre.s.held() && (post.AttributedRun != pre.s.attrRun || post.AttributedGeneration != pre.s.attrGen) {
		fail("composition lost: %s/%s -> %s/%s", pre.s.attrRun, pre.s.attrGen, post.AttributedRun, post.AttributedGeneration)
	}
	// (6) A reincarnation in flight stores UNATTRIBUTED (from a clear PRE).
	if c.write == "soft-finish" && c.inFlight && !pre.s.held() && post.AttributedRun != "" {
		fail("in-flight soft finish stored %s", post.AttributedRun)
	}
	// (8) A generation bump stays clear only with a witness equal to the run.
	if (c.write == "bump" || c.write == "bump-stop") && post.State == store.AttributionClear && c.witness != "after" {
		fail("unwitnessed bump stays clear")
	}
	// Every held POST lists its non-empty run.
	if post.State == store.AttributionHeld && post.LastRun != "" && !quar[post.LastRun] {
		fail("held run %s not quarantined", post.LastRun)
	}
	// History-unknown normalisation is sticky UNKNOWN.
	if historyUnknown && post.QuarantineMode != store.QuarantineUnknown {
		fail("history-unknown normalisation left mode %q", post.QuarantineMode)
	}
}

func plannedString(f Facts, plan Plan) string {
	var parts []string
	for _, p := range plan.Events {
		parts = append(parts, fmt.Sprintf("%s[%s]omit=%v", p.Transition, finalLaunch(f, p), p.AttrOmitLaunch))
	}
	return strings.Join(parts, " ")
}

// TestAttribution_T25HintClosureUntrusted (r1 R6): a launch_failed hint
// closing an untrusted segment omits launch_id.
func TestAttribution_T25HintClosureUntrusted(t *testing.T) {
	cur := &store.LifecycleCursor{AgentID: agentA, TransitionRevision: 3, UpdatedAt: t0, OpenRun: "X1", OpenRunGeneration: "1", OpenRunMetadataRevision: 2}
	cur.AttributionStamp = &cur.UpdatedAt
	// A clear state with an exact-match marker (a segment opened while
	// held, then a clear state): only the closure rule can omit here, so a
	// closure misrouted to the identity rule would name X1 (M20).
	cur.Attribution = store.LifecycleAttribution{State: store.AttributionClear, LastRun: "X1", LastGeneration: "1",
		QuarantineMode:   store.QuarantineComplete,
		UntrustedOpenRun: "X1", UntrustedOpenRunGeneration: "1", UntrustedOpenRunRev: 2}
	f := Facts{Op: store.AgentWriteLaunch, HasBefore: true, BeforeLive: true, HasAfter: true,
		BeforePhase: "running", AfterPhase: "error", BeforeRunID: "X1", AfterRunID: "X1", BeforeGeneration: 1, AfterGeneration: 1,
		Hint:   Hint{Kind: HintLaunchEnd, EndReason: store.LaunchEndReasonFailed, ErrorCode: "launch_error"},
		Cursor: CursorFacts{Exists: true, FingerprintMatches: true, OpenRun: "X1", Run: cur.RunState()}}
	pre := PrepareAttribution(&f, cur)
	plan := Derive(f)
	Attribute(f, pre, &plan)
	found := false
	for _, p := range plan.Events {
		if p.Transition == tc.TransitionLaunchFailed && p.ClosesSegment {
			found = true
			if !p.AttrOmitLaunch {
				t.Errorf("launch_failed closing the untrusted segment names it: %+v", p)
			}
		}
	}
	if !found {
		t.Fatalf("no launch_failed(ClosesSegment) planned: %s", plannedString(f, plan))
	}
}

// TestAttribution_DropAdoptionForQuarantinedSource: an event carrying an
// adoption whose adopted-from run is quarantined drops the trio, even
// when it names its own run.
func TestAttribution_DropAdoptionForQuarantinedSource(t *testing.T) {
	plan := Plan{Events: []Planned{{Transition: tc.TransitionStopped, Row: RowAfter, CarriesAdoption: true}}}
	f := Facts{HasBefore: true, HasAfter: true, BeforeLive: true, BeforeRunID: "Y1", AfterRunID: "Y1", BeforeGeneration: 1, AfterGeneration: 1,
		Cursor: CursorFacts{Exists: true, Run: store.LifecycleRunState{PendingAdoptedFrom: "Q1"}}}
	pre := AttributionPre{s: attrState{state: store.AttributionClear, lastRun: "Y1", lastGen: "1", quar: []string{"Q1"}, mode: store.QuarantineComplete}}
	Attribute(f, pre, &plan)
	if !plan.Events[0].DropAdoption || plan.Events[0].AttrOmitLaunch {
		t.Errorf("want the trio dropped and the launch kept: %+v", plan.Events[0])
	}
}

// TestEffectiveEcho_FilteredOnFinalLaunch (review R2 build level, M13): the
// effective echo is filtered against the FINAL launch_id: a trusted
// closure of R1 with R1's echo keeps it; an attributed R1 name on a row
// that holds B1 keeps R1's echo; an omitted launch_id clears all six.
func TestEffectiveEcho_FilteredOnFinalLaunch(t *testing.T) {
	a := baseAgent()
	a.RunID, a.Phase, a.DeletedAt = "B1", "stopped", t0
	a.EffectiveConfig = echoFor("R1")
	md := Snapshot(a, Creator{Type: "user", ID: "u1", RequestedTemplate: "dev"})
	build := func(p Planned, cursor *store.LifecycleCursor) *tc.LifecycleEvent {
		t.Helper()
		ev, err := buildEvent(eventInput{
			Planned: p, Row: a, TransitionRevision: 5, MetadataRevision: 3, Metadata: &md, Cursor: cursor,
			EventID: "00000000-0000-4000-8000-000000000001", HubID: "hub-a", ProducerBuild: "v", EventTS: t0, ObservedTS: t0,
		})
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}
	seg := &store.LifecycleCursor{AgentID: agentA, OpenRun: "R1", OpenRunGeneration: "1", OpenRunMetadataRevision: 2}
	closing := build(Planned{Transition: tc.TransitionDeleted, Row: RowAfter, DeleteMode: tc.DeleteModeSoft, ClosesOpenRun: true}, seg)
	if closing.LaunchID != "R1" || effOf(closing) != echoR1 {
		t.Errorf("trusted R1 closure: launch_id %q effective %s, want R1 and %s", closing.LaunchID, effOf(closing), echoR1)
	}
	named := build(Planned{Transition: tc.TransitionDeleted, Row: RowAfter, DeleteMode: tc.DeleteModeSoft, AttrLaunch: "R1"}, &store.LifecycleCursor{AgentID: agentA})
	if named.LaunchID != "R1" || effOf(named) != echoR1 {
		t.Errorf("attributed R1 on a B1 row: launch_id %q effective %s, want R1 and %s", named.LaunchID, effOf(named), echoR1)
	}
	omitted := build(Planned{Transition: tc.TransitionDeleted, Row: RowAfter, DeleteMode: tc.DeleteModeSoft, ClosesOpenRun: true, AttrOmitLaunch: true}, seg)
	if omitted.LaunchID != "" || effOf(omitted) != "-" || omitted.SegmentMetadataRevision == nil || omitted.IncarnationID != "1" {
		t.Errorf("untrusted closure: launch_id %q effective %s seg %v inc %q, want none, -, the segment fields", omitted.LaunchID, effOf(omitted), omitted.SegmentMetadataRevision, omitted.IncarnationID)
	}
	plain := build(Planned{Transition: tc.TransitionDeleted, Row: RowAfter, DeleteMode: tc.DeleteModeSoft}, &store.LifecycleCursor{AgentID: agentA})
	if plain.LaunchID != "B1" || effOf(plain) != "-" {
		t.Errorf("row run B1 with R1's echo: launch_id %q effective %s, want B1 and none", plain.LaunchID, effOf(plain))
	}
}

// cur0Seg is the open segment's run of the case.
func cur0Seg(c attrCase) string {
	if c.segOnBefore || c.write == "rebind" {
		return "R1"
	}
	_, f, _ := c.build()
	return f.AfterRunID
}

// firingSources computes, independently of Attribute, the runs a write
// pushes or holds by a firing source (design 3.3 exclusion form): the
// observing normalisation rows push the before run; every run change
// except an establishing mint and rule 6 (clear, complete, target not
// quarantined) pushes or holds the after run; an unwitnessed same-run
// generation change pushes it.
func firingSources(c attrCase, f Facts) map[string]bool {
	src := map[string]bool{}
	switch c.state {
	case "clear-run-mismatch", "clear-gen-mismatch", "held-run-mismatch", "held-gen-mismatch":
		src[f.BeforeRunID] = true
	}
	if !f.HasAfter {
		return src
	}
	held := strings.HasPrefix(c.state, "held") || c.state == "clear-run-mismatch" || c.state == "clear-gen-mismatch" ||
		c.state == "legacy" || c.state == "stale" || c.corruptLike() || c.state == "missing" || c.segment == "malformed"
	quar := c.targetQuar
	switch {
	case f.AfterRunID != f.BeforeRunID:
		establishing := c.write == "mint-fresh" && !quar
		rule6 := !held && c.mode == "complete" && !quar && c.write != "soft-finish" && f.Kind != store.WriteKindRunMint
		if !establishing && !rule6 {
			src[f.AfterRunID] = true
		}
	case f.AfterGeneration != f.BeforeGeneration:
		witnessed := !held && c.witness == "after" && f.AfterGeneration == f.BeforeGeneration+1
		if !witnessed {
			src[f.AfterRunID] = true
		}
	}
	return src
}

// corruptLike reports a state that is CORRUPT under design 4.1.
func (c attrCase) corruptLike() bool {
	return c.state == "corrupt" || c.state == "legacy-partial-marker" || c.state == "legacy-bad-mode"
}
