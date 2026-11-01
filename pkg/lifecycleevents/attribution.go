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

// Persistent run attribution (WP1 attribution design v2.8). Derive decides
// the transitions of a write from the row pair; this layer decides, from
// the cursor's attribution state, whether each event may name the run it
// would name. A run the producer cannot prove for the event's generation
// is never named: agent-state events then name the attributed identity
// (on a generation match) or omit launch_id, run-class events and closures
// of untrusted segments omit it. The state is internal; the emitted
// envelope and its fields are unchanged.
//
// Per write (design section 4):
//  1. PrepareAttribution normalises the stored state (PRE) before Derive,
//     and discards a malformed segment identity (3.3, P3).
//  2. Derive runs unchanged.
//  3. Attribute computes the committed state (POST, 4.2), the push-time
//     trigger and closure trust (3.3), selects each event's identity by
//     row and role (4.3), and adds the gap of a discard (3.3, R4-6).
//  4. The observer finalises the segment marker once the frozen revision
//     of a segment opened by the write is known (FinalizeAttribution).

import (
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetryemit"
)

// attrState is a parsed attribution state.
type attrState struct {
	state            string // store.AttributionClear or store.AttributionHeld
	lastRun, lastGen string
	// attrRun/attrGen: the attributed identity in held ("" run:
	// UNATTRIBUTED); the undo identity in clear.
	attrRun, attrGen string
	quar             []string // newest first
	mode             string   // store.QuarantineComplete or store.QuarantineUnknown
	witness          string
}

func (s attrState) held() bool { return s.state == store.AttributionHeld }

func (s attrState) inQuar(run string) bool {
	if run == "" {
		return false
	}
	for _, q := range s.quar {
		if q == run {
			return true
		}
	}
	return false
}

// push adds run to the front of the quarantine list, deduplicated (3.4
// Entry). An empty run is never pushed (r2 N3a). A ninth distinct entry
// makes the history UNKNOWN for good (sticky).
func (s *attrState) push(run string) {
	if run == "" {
		return
	}
	out := []string{run}
	for _, q := range s.quar {
		if q != run {
			out = append(out, q)
		}
	}
	if len(out) > store.MaxPreviousRunIDs {
		out = out[:store.MaxPreviousRunIDs]
		s.mode = store.QuarantineUnknown
	}
	s.quar = out
}

func (s attrState) clone() attrState {
	s.quar = append([]string(nil), s.quar...)
	return s
}

// AttributionPre is the normalised attribution of the cursor before a
// write (design 4.1), produced by PrepareAttribution.
type AttributionPre struct {
	// Create: the write creates the agent; there is no PRE.
	Create bool
	// S is the normalised state.
	s attrState
	// Normalized: S differs from what the cursor stores; the write always
	// bumps the cursor (4.5).
	Normalized bool
	// Observing: S was normalised by an observing row (a mismatch or a
	// return to a quarantined run of a cursor that was observing): its
	// push of the before run fires the push-time trigger. A
	// history-unknown normalisation (missing, LEGACY, stale, CORRUPT)
	// does not (3.3, R6-1).
	Observing bool
	// SegUntrusted: the cursor's open segment (after any discard) is
	// untrusted: an exact-match marker, or a CORRUPT cursor.
	SegUntrusted bool
	// StaleReason is the lifecycle_attribution_stale reason, "" if none.
	StaleReason string
	// SegmentDiscarded: a malformed segment identity was discarded (3.3,
	// P3); Attribute adds one resync gap.
	SegmentDiscarded bool
	// cursorBase is the cursor's reincarnation base run before the write.
	cursorBase string
}

// AttributionOutcome is what the observer writes for a write.
type AttributionOutcome struct {
	// Has: the write has a committed state to write (it has an after row).
	Has bool
	// State is the committed attribution, without the segment marker,
	// which FinalizeAttribution sets.
	State store.LifecycleAttribution
	// Normalized and StaleReason come from PRE.
	Normalized  bool
	StaleReason string
	// closureUntrusted is the trust of the cursor's PRE segment after the
	// push-time trigger: the explicit per-write input closures use (R6-3).
	closureUntrusted bool
	post             attrState
	runChanged       bool
	rebind           bool
}

func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func parseQuar(s string) ([]string, bool) {
	if s == "" {
		return nil, true
	}
	parts := strings.Split(s, ",")
	seen := map[string]bool{}
	var out []string
	ok := len(parts) <= store.MaxPreviousRunIDs
	for _, p := range parts {
		if p == "" || seen[p] {
			ok = false
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	if len(out) > store.MaxPreviousRunIDs {
		out = out[:store.MaxPreviousRunIDs]
	}
	return out, ok
}

// segmentMalformed reports a segment identity no valid closure can be
// built for (3.3, P3): an open run with an empty or non-decimal generation
// or a metadata revision below 1.
func segmentMalformed(r store.LifecycleRunState) bool {
	return r.OpenRun != "" && (!isDecimal(r.OpenRunGeneration) || r.OpenRunMetadataRevision < 1)
}

// attributionCorrupt reports a CORRUPT stored state (design 4.1). The
// malformed segment identity is handled separately (discarded).
func attributionCorrupt(a store.LifecycleAttribution, c *store.LifecycleCursor) bool {
	// LEGACY (attribution_state "") runs through the same checks as every
	// other state (design 4.1; implementation review r1 RQ-1, contract
	// review TA-1): only the state enum and the last_run/last_generation
	// clause are scoped to non-LEGACY cursors, and a LEGACY cursor with a
	// stamp is CORRUPT. A genuine pre-upgrade cursor (every column at its
	// default, NULL stamp) passes and reads LEGACY.
	legacy := a.State == store.AttributionLegacy
	if legacy && c.AttributionStamp != nil {
		return true
	}
	if !legacy && a.State != store.AttributionClear && a.State != store.AttributionHeld {
		return true
	}
	if a.State == store.AttributionHeld && a.AttributedRun != "" && a.AttributedGeneration == "" {
		return true
	}
	for _, g := range []string{a.AttributedGeneration, a.LastGeneration} {
		if g != "" && !isDecimal(g) {
			return true
		}
	}
	if !legacy && a.LastRun != "" && a.LastGeneration == "" {
		return true
	}
	quar, ok := parseQuar(a.QuarantinedRuns)
	if !ok {
		return true
	}
	if a.State == store.AttributionClear && (a.AttributedRun != "" || a.AttributedGeneration != "") && len(quar) == 0 {
		return true
	}
	if a.QuarantineMode != store.QuarantineUnknown && a.QuarantineMode != store.QuarantineComplete {
		return true
	}
	markerSet := a.UntrustedOpenRun != "" || a.UntrustedOpenRunGeneration != "" || a.UntrustedOpenRunRev != 0
	if markerSet && (a.UntrustedOpenRun == "" || !isDecimal(a.UntrustedOpenRunGeneration) || a.UntrustedOpenRunRev < 1) {
		return true
	}
	if a.ReincarnationMintRun != "" && c.ReincarnationBaseRunID == "" {
		return true
	}
	return false
}

// markerMatches reports an exact-match untrusted marker on the segment.
func markerMatches(a store.LifecycleAttribution, r store.LifecycleRunState) bool {
	return r.OpenRun != "" && a.UntrustedOpenRun == r.OpenRun &&
		a.UntrustedOpenRunGeneration == r.OpenRunGeneration && a.UntrustedOpenRunRev == r.OpenRunMetadataRevision
}

// PrepareAttribution normalises the cursor's attribution before Derive
// (design 4.1) and discards a malformed segment identity from f.Cursor
// (3.3, P3), so no Derive branch plans a closure that cannot be built.
// c is the locked cursor (nil when the agent has none, or for a create).
func PrepareAttribution(f *Facts, c *store.LifecycleCursor) AttributionPre {
	if f.Op == store.AgentWriteCreate {
		return AttributionPre{Create: true}
	}
	pre := AttributionPre{}
	bRun, bGen := f.BeforeRunID, itoa(f.BeforeGeneration)
	unknown := func(reason string) {
		var quar []string
		if c != nil {
			quar, _ = parseQuar(c.Attribution.QuarantinedRuns)
		}
		pre.s = attrState{state: store.AttributionHeld, lastRun: bRun, lastGen: bGen, quar: quar, mode: store.QuarantineUnknown}
		pre.Normalized = true
		pre.StaleReason = reason
	}
	if c == nil {
		unknown(telemetryemit.AttributionStaleMissing)
		pre.s.push(bRun)
		return pre
	}
	pre.cursorBase = c.ReincarnationBaseRunID
	seg := f.Cursor.Run
	if seg.OpenRun == "" {
		seg.OpenRun = f.Cursor.OpenRun
	}
	if segmentMalformed(seg) {
		// Discard with closeSegment's semantics, in Run and in the slice 1
		// mirror (R4-5), before any Derive branch reads it.
		f.Cursor.Run.OpenRun, f.Cursor.Run.OpenRunGeneration = "", ""
		f.Cursor.Run.OpenRunMetadataRevision, f.Cursor.Run.PendingRunChangeAt = 0, nil
		f.Cursor.OpenRun = ""
		pre.SegmentDiscarded = true
		seg = f.Cursor.Run
	}
	a := c.Attribution
	switch {
	case pre.SegmentDiscarded || attributionCorrupt(a, c):
		unknown(telemetryemit.AttributionStaleCorrupt)
		if pre.SegmentDiscarded {
			pre.StaleReason = telemetryemit.AttributionStaleDiscarded
		}
		// A valid segment on a CORRUPT cursor is untrusted (3.3 order 2).
		pre.SegUntrusted = seg.OpenRun != ""
		pre.s.push(bRun)
		return pre
	case a.State == store.AttributionLegacy:
		unknown(telemetryemit.AttributionStaleLegacy)
		pre.SegUntrusted = markerMatches(a, seg)
		pre.s.push(bRun)
		return pre
	case c.AttributionStamp == nil || !c.AttributionStamp.Equal(c.UpdatedAt):
		unknown(telemetryemit.AttributionStaleStamp)
		pre.SegUntrusted = markerMatches(a, seg)
		pre.s.push(bRun)
		return pre
	}
	quar, _ := parseQuar(a.QuarantinedRuns)
	s := attrState{
		state: a.State, lastRun: a.LastRun, lastGen: a.LastGeneration,
		attrRun: a.AttributedRun, attrGen: a.AttributedGeneration,
		quar: quar, mode: a.QuarantineMode, witness: a.ReincarnationMintRun,
	}
	pre.SegUntrusted = markerMatches(a, seg)
	observing := func(keepAttr bool) {
		ns := s.clone()
		ns.state = store.AttributionHeld
		if !keepAttr {
			ns.attrRun, ns.attrGen = "", ""
			ns.lastRun = bRun
		}
		ns.lastGen = bGen
		ns.push(bRun)
		pre.s = ns
		pre.Normalized = true
		pre.Observing = true
	}
	switch {
	case s.inQuar(bRun) && bRun != s.lastRun:
		observing(false)
	case s.state == store.AttributionClear && (bRun != s.lastRun || bGen != s.lastGen):
		observing(false)
	case s.held() && bRun != s.lastRun:
		observing(false)
	case s.held() && bGen != s.lastGen:
		observing(true)
	default:
		// The `otherwise` row: no push, not a trigger source (R6-1).
		pre.s = s
	}
	return pre
}

// Attribute applies the attribution to plan (design 4.2-4.3) and returns
// the committed state. The plan's events gain AttrOmitLaunch/AttrLaunch,
// DropAdoption, and the discard gap.
func Attribute(f Facts, pre AttributionPre, plan *Plan) AttributionOutcome {
	out := AttributionOutcome{Normalized: pre.Normalized, StaleReason: pre.StaleReason}
	aRun, aGen := f.AfterRunID, itoa(f.AfterGeneration)
	bRun, bGen := f.BeforeRunID, itoa(f.BeforeGeneration)
	if pre.Create {
		if f.HasAfter {
			out.Has = true
			out.post = attrState{state: store.AttributionClear, lastRun: aRun, lastGen: aGen, mode: store.QuarantineComplete}
			out.State = out.post.store()
		}
		return out
	}

	s := pre.s.clone()
	// The trigger sources: runs this write pushes or holds by a firing
	// source (3.3; R5-1 exclusion form; R6-1 keyed on the source).
	sources := map[string]bool{}
	if pre.Observing && bRun != "" {
		sources[bRun] = true
	}
	seg := f.Cursor.Run // PRE segment, after any discard (R6-3)

	var post attrState
	hasPost := f.HasAfter
	runChanged := f.HasAfter && aRun != bRun
	deletion := f.HasAfter && f.BeforeLive && f.AfterDeleted
	isMint := f.Kind == store.WriteKindRunMint
	if hasPost {
		switch {
		case isMint && aRun != "" && aRun != s.lastRun && aRun != bRun && !s.inQuar(aRun) && !containsRun(f.BeforePreviousRunIDs, aRun):
			// Rule 2: an establishing fresh mint.
			post = s.clone()
			post.state = store.AttributionClear
			post.lastRun, post.lastGen = aRun, aGen
			if s.held() {
				post.attrRun, post.attrGen = s.attrRun, s.attrGen // the undo
			} else {
				post.attrRun, post.attrGen = "", ""
			}
			if pre.cursorBase != "" && f.AfterReincarnationInFlight {
				post.witness = aRun
			}
		case isMint && aRun == bRun:
			// Rule 2F, same value: a no-op.
			post = s.clone()
			post.lastRun, post.lastGen = aRun, aGen
		case isMint:
			// Rule 2F: a non-fresh mint fails closed in either mode.
			post = held(s, aRun, aGen, "", "")
			post.witness = ""
			sources[aRun] = true
		case deletion && runChanged:
			// Rule 3: the soft finish writes another run.
			ar, ag := attributedFor(f, pre, s, seg)
			post = held(s, aRun, aGen, ar, ag)
			sources[aRun] = true
		case runChanged && s.inQuar(aRun):
			// Rule 4: a return to a quarantined run. The undo applies only
			// to an observed revert to the newest entry (3.4).
			ar, ag := "", ""
			if s.state == store.AttributionClear && f.Kind == store.WriteKindRunRevert && len(s.quar) > 0 && s.quar[0] == aRun {
				ar, ag = s.attrRun, s.attrGen
			}
			post = held(s, aRun, aGen, ar, ag)
			sources[aRun] = true
		case runChanged && s.held():
			// Rule 5.
			post = held(s, aRun, aGen, "", "")
			sources[aRun] = true
		case runChanged && s.mode == store.QuarantineComplete:
			// Rule 6: the existing WP1 flows.
			post = s.clone()
			post.lastRun, post.lastGen = aRun, aGen
			post.attrRun, post.attrGen = "", ""
		case runChanged:
			// Rule 6U: UNKNOWN history, any non-establishing change.
			post = held(s, aRun, aGen, "", "")
			sources[aRun] = true
		case aGen != bGen:
			// Rule 7: a same-run generation change.
			if s.state == store.AttributionClear && pre.cursorBase != "" && s.witness != "" && aRun == s.witness &&
				f.AfterGeneration == f.BeforeGeneration+1 {
				post = s.clone()
				post.lastRun, post.lastGen = aRun, aGen
			} else {
				ar, ag := "", ""
				if s.held() {
					ar, ag = s.attrRun, s.attrGen
				}
				post = held(s, aRun, aGen, ar, ag)
				sources[aRun] = true
			}
		default:
			// Rule 8.
			post = s.clone()
			post.lastRun, post.lastGen = aRun, aGen
		}
		if post.held() {
			post.push(post.lastRun)
		}
		if plan.Run.ReincarnationBaseRunID == "" {
			post.witness = ""
		}
	}

	closureUntrusted := pre.SegUntrusted || (seg.OpenRun != "" && sources[seg.OpenRun])
	out.closureUntrusted = closureUntrusted
	out.runChanged = runChanged
	out.rebind = f.Kind == store.WriteKindRunAdopt && seg.OpenRun != "" && seg.OpenRun == bRun && runChanged &&
		plan.Run.OpenRun == aRun && !plan.FreezeSegment

	// Event selection (4.3).
	for i := range plan.Events {
		p := &plan.Events[i]
		switch {
		case p.ClosesOpenRun || p.ClosesSegment:
			// Closures follow segment trust, never the attribution.
			if closureUntrusted {
				p.AttrOmitLaunch = true
			}
		case p.OmitLaunchID:
		case p.Row == RowBefore:
			selectIdentity(p, pre.s, f.BeforeGeneration)
		case hasPost:
			selectIdentity(p, post, f.AfterGeneration)
		}
		if p.CarriesAdoption {
			from := f.Cursor.Run.PendingAdoptedFrom
			if p.AttrOmitLaunch || s.inQuar(from) || (hasPost && post.inQuar(from)) {
				p.DropAdoption = true
			}
		}
	}

	// The discard gap (3.3, R4-6, R5-3): one RowBefore resync gap without
	// launch_id, unless the write already has one; never on a create.
	if pre.SegmentDiscarded && f.HasBefore && !hasGap(plan.Events) {
		gap := Planned{Transition: telemetrycontract.TransitionResynced, Row: RowBefore,
			Cause: telemetrycontract.CauseUnobservedChange, Gap: true, OmitLaunchID: true}
		plan.Events = append([]Planned{gap}, plan.Events...)
		plan.SkipRevision = true
	}

	if hasPost {
		out.Has = true
		out.post = post
		out.State = post.store()
	}
	return out
}

// FinalizeAttribution sets the segment marker of the committed state
// (3.3 marker writes, 4.2 segment marker) once the run state is final:
// run carries the frozen revision of a segment the write opened.
func FinalizeAttribution(f Facts, plan Plan, run store.LifecycleRunState, out *AttributionOutcome) {
	if !out.Has {
		return
	}
	pre := f.Cursor.Run
	mark := false
	switch {
	case run.OpenRun == "":
	case plan.FreezeSegment:
		mark = out.post.held()
	case run.OpenRun == pre.OpenRun && run.OpenRunGeneration == pre.OpenRunGeneration &&
		run.OpenRunMetadataRevision == pre.OpenRunMetadataRevision:
		mark = out.closureUntrusted
	case out.rebind:
		mark = out.post.held() || out.post.inQuar(run.OpenRun) || out.closureUntrusted
	default:
		mark = out.post.held()
	}
	if mark {
		out.State.UntrustedOpenRun = run.OpenRun
		out.State.UntrustedOpenRunGeneration = run.OpenRunGeneration
		out.State.UntrustedOpenRunRev = run.OpenRunMetadataRevision
	}
}

func held(s attrState, run, gen, attrRun, attrGen string) attrState {
	p := s.clone()
	p.state = store.AttributionHeld
	p.lastRun, p.lastGen = run, gen
	p.attrRun, p.attrGen = attrRun, attrGen
	return p
}

// attributedFor is the identity a run-changing deletion stores for later
// agent-state events (design 4.4), chosen separately from the closure's.
func attributedFor(f Facts, pre AttributionPre, s attrState, seg store.LifecycleRunState) (string, string) {
	bRun, bGen := f.BeforeRunID, itoa(f.BeforeGeneration)
	switch {
	case s.held():
		return s.attrRun, s.attrGen // composition
	case bRun == "":
		return "", ""
	case pre.cursorBase != "" && bRun != pre.cursorBase &&
		(f.BeforeReincarnationInFlight || f.AfterReincarnationInFlight) && f.AfterGeneration == f.BeforeGeneration:
		return "", "" // a reincarnation in flight (R3, N10)
	case seg.OpenRun == bRun:
		if !pre.SegUntrusted && seg.OpenRunGeneration == bGen {
			return bRun, bGen
		}
		return "", ""
	}
	return bRun, bGen
}

// agentStateClass is the class whose events may name the attribution.
func agentStateClass(transition string) bool {
	switch transition {
	case telemetrycontract.TransitionDeleted, telemetrycontract.TransitionRestored,
		telemetrycontract.TransitionMetadataChanged, telemetrycontract.TransitionResynced:
		return true
	}
	return false
}

// selectIdentity applies ident (design 4.3) to a non-closure event.
func selectIdentity(p *Planned, s attrState, rowGen int) {
	if !s.held() {
		return
	}
	gen := rowGen
	if p.NextGeneration {
		gen++
	}
	if agentStateClass(p.Transition) && s.attrRun != "" && s.attrGen == itoa(gen) {
		p.AttrLaunch = s.attrRun
		return
	}
	p.AttrOmitLaunch = true
}

func hasGap(events []Planned) bool {
	for _, p := range events {
		if p.Transition == telemetrycontract.TransitionResynced && p.Gap {
			return true
		}
	}
	return false
}

func containsRun(runs []string, run string) bool {
	for _, r := range runs {
		if r == run {
			return true
		}
	}
	return false
}

func (s attrState) store() store.LifecycleAttribution {
	return store.LifecycleAttribution{
		State:                s.state,
		LastRun:              s.lastRun,
		LastGeneration:       s.lastGen,
		AttributedRun:        s.attrRun,
		AttributedGeneration: s.attrGen,
		QuarantinedRuns:      strings.Join(s.quar, ","),
		QuarantineMode:       s.mode,
		ReincarnationMintRun: s.witness,
	}
}
