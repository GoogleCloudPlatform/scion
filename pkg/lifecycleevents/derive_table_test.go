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

// This file holds the slice 2 expectation table of WP1 design 2.4a: the
// phase rules as data (phaseRules, first match wins), the evidence
// classification (evidenceClass) and the overlays for the run axis, hints
// and deletion. TestDeriveCoversEveryCase checks Derive against them over
// every combination of phase pair x run axis x K x H x D x E (r3/r4 tele-em,
// r7 N3). TestDerivePhaseMatrix renders the explicit 10x10 matrix from the
// same rules, so the design's matrix is generated, not hand-maintained.

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	tc "github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// fullPlanString renders a plan with every field the rules decide.
func fullPlanString(p Plan) string {
	var parts []string
	if p.SkipRevision {
		parts = append(parts, "gap")
	}
	for _, e := range p.Events {
		var attrs []string
		for _, a := range []string{e.StopKind, e.Cause, e.EndReason, e.ErrorCode, e.DeleteMode} {
			if a != "" {
				attrs = append(attrs, a)
			}
		}
		if e.Transition == tc.TransitionLaunchFailed && !e.OmitLaunchID {
			attrs = append(attrs, "lid")
		}
		if e.ClosesOpenRun {
			attrs = append(attrs, "closes")
		}
		s := e.Transition
		if len(attrs) > 0 {
			s += "(" + strings.Join(attrs, ",") + ")"
		}
		parts = append(parts, s)
	}
	if p.Ambiguous != "" {
		parts = append(parts, "ambiguous:"+p.Ambiguous)
	}
	if p.UnconfirmedStart {
		parts = append(parts, "unconfirmed")
	}
	if p.DeleteCursor {
		parts = append(parts, "rmcursor")
	}
	return strings.Join(parts, " ")
}

// Phase groups as sets, for the declarative rules.
type gset uint8

const (
	gPre gset = 1 << iota
	gRun
	gWind
	gRest
	gErr
	gHalt
	gAny = gPre | gRun | gWind | gRest | gErr | gHalt
)

func gsetOf(phase string) gset {
	switch groupOf(phase) {
	case groupPre:
		return gPre
	case groupRun:
		return gRun
	case groupWind:
		return gWind
	case groupRest:
		return gRest
	case groupErr:
		return gErr
	case groupHalt:
		return gHalt
	}
	return 0
}

// tri is a three-valued condition.
type tri int

const (
	anyT tri = iota
	yes
	no
)

func (t tri) ok(v bool) bool { return t == anyT || (t == yes) == v }

// rcond is one phase rule's condition, all fields ANDed.
type rcond struct {
	ap, bp gset // 0 = any
	same   bool // bp == ap (the phase strings)
	s      tri  // S: this run's segment is open
	e      tri  // E: run-correlated evidence for the current run
	legacy tri  // a run-less legacy observation (report or dispatch result)
	// stopOrE: K = stop_record or E.
	stopOrE tri
	// cancel: K = stop_record or deletion_write.
	cancel tri
}

// phaseRules is design 2.4a step 4 (v6.15 plus the slice 2 rulings Q8-Q10)
// as data. Want strings: {ap} is the after phase, {lid} is ",lid" when the
// write correlates the attempt to the row's run (Q10 revised) and "" otherwise,
// {rb} is ",delete_rollback" on a deletion write.
var phaseRules = []struct {
	id   string
	c    rcond
	want string
}{
	{"P1a", rcond{ap: gRun, s: no, e: yes}, "started"},
	{"P1b", rcond{ap: gRun, bp: gAny &^ gRun, s: no, e: no, legacy: yes}, "started(run_uncorrelated)"},
	{"P1", rcond{same: true}, ""},
	{"P2", rcond{ap: gRun}, ""},
	{"P3", rcond{ap: gHalt}, "halted"},
	{"P4", rcond{ap: gErr, bp: gPre | gWind, s: no}, "launch_failed(launch_error{lid})"},
	{"P7a", rcond{ap: gRest | gErr, bp: gRun, s: no, stopOrE: yes}, "stopped({ap},start_unobserved)"},
	{"P5", rcond{ap: gErr, bp: gRun | gWind | gPre, s: yes}, "stopped(error)"},
	{"P5b", rcond{ap: gErr, bp: gRun, s: no}, "ambiguous:" + AmbiguousErrorNoSegment},
	{"P6", rcond{ap: gErr}, ""},
	{"P7", rcond{ap: gRest, bp: gRun | gWind | gPre, s: yes}, "stopped({ap}{rb})"},
	{"P8", rcond{ap: gRest, bp: gPre | gWind, s: no, cancel: yes}, "launch_failed(cancelled)"},
	{"P9", rcond{ap: gRest, bp: gPre | gWind | gRun, s: no}, "ambiguous:" + AmbiguousRestNoSegment},
	{"P10", rcond{ap: gRest}, ""},
	{"P11", rcond{ap: gWind}, ""},
	{"P12", rcond{ap: gPre}, ""},
}

// evidenceClass is how each evidence kind of the E axis classifies, for an
// after phase in RUN or not (design 2.4a E(i)-(iv), r7 N1/N2, Q10).
var evidenceClass = map[Evidence]struct{ eRun, eOther, legacy, corr bool }{
	EvidenceNone:                {},
	EvidenceStartWrite:          {eRun: true, corr: true},
	EvidenceReportRun:           {eRun: true, eOther: true, corr: true},
	EvidenceLaunchReport:        {eRun: true, eOther: true, corr: true},
	EvidenceDispatchResult:      {eRun: true, corr: true},
	EvidenceDispatchStale:       {},
	EvidenceDispatchNoBrokerRun: {legacy: true},
	EvidenceDispatchNotRunning:  {corr: true},
	EvidenceReportNoRun:         {legacy: true},
}

// expectPhase is the step 4 expectation from phaseRules.
func expectPhase(bpPhase, apPhase string, s bool, k store.AgentWriteKind, ev Evidence) string {
	ap, bp := gsetOf(apPhase), gsetOf(bpPhase)
	cls := evidenceClass[ev]
	e := cls.eOther
	if ap == gRun {
		e = cls.eRun
	}
	stop, del := k == store.WriteKindStopRecord, k == store.WriteKindDeletion
	for _, r := range phaseRules {
		c := r.c
		if (c.ap != 0 && c.ap&ap == 0) || (c.bp != 0 && c.bp&bp == 0) || (c.same && bpPhase != apPhase) ||
			!c.s.ok(s) || !c.e.ok(e) || !c.legacy.ok(cls.legacy) || !c.stopOrE.ok(stop || e) || !c.cancel.ok(stop || del) {
			continue
		}
		w := strings.ReplaceAll(r.want, "{ap}", apPhase)
		lid := ""
		if cls.corr {
			lid = ",lid"
		}
		w = strings.ReplaceAll(w, "{lid}", lid)
		rb := ""
		if del {
			rb = ",delete_rollback"
		}
		return strings.ReplaceAll(w, "{rb}", rb)
	}
	return "NO RULE"
}

var allPhases = []string{
	string(state.PhaseCreated), string(state.PhaseProvisioning), string(state.PhaseCloning), string(state.PhaseStarting),
	string(state.PhaseRunning), string(state.PhaseSuspended), string(state.PhaseStopping), string(state.PhaseStopped),
	string(state.PhaseError), string(state.PhaseHalted),
}

// Run axis (design 2.4a "Exhaustive test").
type runAxis int

const (
	runNoOpen runAxis = iota
	runOpenSame
	runOpenOther
	runMint
	runRevert
	runAdoptRebind
	runAdoptNoOpen
)

var runAxes = []runAxis{runNoOpen, runOpenSame, runOpenOther, runMint, runRevert, runAdoptRebind, runAdoptNoOpen}

// Hint axis.
type hintAxis int

const (
	hNone hintAxis = iota
	hLaunchEnd
	hCreateRolledBack
	hCreateFailed
	hRestartFailed
	hRestore
	hRestoreUnconfirmed
)

var hintAxes = []hintAxis{hNone, hLaunchEnd, hCreateRolledBack, hCreateFailed, hRestartFailed, hRestore, hRestoreUnconfirmed}

// Deletion axis.
type delAxis int

const (
	dNo delAxis = iota
	dLive
	dAlreadyDeleted
)

var (
	kindAxes = []store.AgentWriteKind{store.WriteKindOther, store.WriteKindStatusHeartbeat, store.WriteKindStopRecord, store.WriteKindDeletion}
	evAxes   = []Evidence{EvidenceNone, EvidenceStartWrite, EvidenceReportRun, EvidenceLaunchReport, EvidenceDispatchResult,
		EvidenceDispatchStale, EvidenceDispatchNoBrokerRun, EvidenceDispatchNotRunning, EvidenceReportNoRun}
	tableNow = time.Date(2026, 10, 22, 1, 2, 3, 0, time.UTC)
)

// caseFacts builds the Facts of one enumerated case.
func caseFacts(bp, ap string, ra runAxis, k store.AgentWriteKind, h hintAxis, d delAxis, ev Evidence) Facts {
	f := Facts{
		Op: store.AgentWriteStatus, Kind: k, HasBefore: true, BeforeLive: true, HasAfter: true,
		BeforePhase: bp, AfterPhase: ap, BeforeRunID: "R1", AfterRunID: "R1",
		BeforeGeneration: 1, AfterGeneration: 1, Evidence: ev, Now: tableNow,
		BeforeDigest: "d", AfterDigest: "d", BeforeConfigDigest: "c", AfterConfigDigest: "c",
		Cursor: CursorFacts{Exists: true, FingerprintMatches: true, Digest: "d"},
	}
	open := func(run string) {
		f.Cursor.OpenRun = run
		f.Cursor.Run = store.LifecycleRunState{OpenRun: run, OpenRunGeneration: "1", OpenRunMetadataRevision: 1}
	}
	switch ra {
	case runOpenSame:
		open("R1")
	case runOpenOther:
		open("R0")
	case runMint:
		open("R1")
		f.Kind, f.AfterRunID = store.WriteKindRunMint, "R2"
	case runRevert:
		open("R1")
		f.Kind, f.BeforeRunID = store.WriteKindRunRevert, "R2"
	case runAdoptRebind:
		open("R1")
		f.Kind, f.AfterRunID, f.AdoptionSource = store.WriteKindRunAdopt, "L1", store.AdoptionSourceCAS
	case runAdoptNoOpen:
		f.Kind, f.AfterRunID, f.AdoptionSource = store.WriteKindRunAdopt, "L1", store.AdoptionSourceCAS
	}
	switch h {
	case hLaunchEnd:
		f.Hint = Hint{Kind: HintLaunchEnd, EndReason: tc.EndReasonTimedOut, ErrorCode: "launch_timeout"}
	case hCreateRolledBack:
		f.Cause = Cause{Kind: CauseCreateRolledBack, Stage: "dispatch"}
	case hCreateFailed:
		f.Cause = Cause{Kind: CauseCreateFailed, Stage: "provision"}
	case hRestartFailed:
		f.Hint = Hint{Kind: HintRestartFailed}
	case hRestore:
		f.Hint = Hint{Kind: HintRestore}
	case hRestoreUnconfirmed:
		f.Hint = Hint{Kind: HintRestoreUnconfirmed}
	}
	switch d {
	case dLive:
		f.Op, f.HasAfter = store.AgentWriteHardDelete, false
	case dAlreadyDeleted:
		f.Op, f.HasAfter, f.BeforeLive = store.AgentWriteHardDelete, false, false
		f.Cursor.LastDeleted = true
	}
	return f
}

// expectCase is the expectation for one enumerated case: the deletion,
// run-ID and hint overlays on top of phaseRules.
func expectCase(bp, ap string, ra runAxis, k store.AgentWriteKind, h hintAxis, d delAxis, ev Evidence) string {
	openRun := ra == runOpenSame || ra == runOpenOther
	switch d {
	case dLive:
		// Step 1: [launch_failed(<create cause>)] + deleted, closing any
		// open segment.
		var parts []string
		switch h {
		case hCreateRolledBack:
			parts = append(parts, "launch_failed(create_rolled_back,dispatch,lid)")
		case hCreateFailed:
			parts = append(parts, "launch_failed(create_failed,provision,lid)")
		}
		del := "deleted(hard)"
		if h == hCreateRolledBack {
			del = "deleted(create_rolled_back)"
		}
		if openRun || ra == runMint || ra == runRevert || ra == runAdoptRebind {
			del = strings.TrimSuffix(del, ")") + ",closes)"
		}
		return strings.Join(append(parts, del, "rmcursor"), " ")
	case dAlreadyDeleted:
		return "rmcursor"
	}
	switch ra {
	case runMint, runRevert, runAdoptRebind, runAdoptNoOpen:
		return "" // step 0: run-ID writes never emit
	}
	var parts []string
	s := ra == runOpenSame
	if ra == runOpenOther {
		parts = append(parts, "stopped(superseded,closes)")
	}
	hint := ""
	switch h {
	case hLaunchEnd:
		hint = "launch_failed(timed_out,launch_timeout,lid)"
	case hCreateFailed:
		hint = "launch_failed(create_failed,provision,lid)"
	case hRestartFailed:
		hint = "launch_failed(restart_failed)"
	case hRestoreUnconfirmed:
		hint = "unconfirmed"
	case hRestore:
		if gsetOf(bp) == gPre && gsetOf(ap)&(gRest|gErr|gHalt) != 0 {
			hint = "launch_failed(dispatch_failed)"
		}
	}
	if hint == "" {
		hint = expectPhase(bp, ap, s, k, ev)
	}
	if hint != "" {
		parts = append(parts, hint)
	}
	return strings.Join(parts, " ")
}

// TestDeriveCoversEveryCase is the exhaustive pairs x run identity test
// (design 2.4a). Every case must have exactly one expectation (phaseRules
// is total: "NO RULE" fails), and Derive must match it.
func TestDeriveCoversEveryCase(t *testing.T) {
	n, fails := 0, 0
	for _, bp := range allPhases {
		for _, ap := range allPhases {
			for _, ra := range runAxes {
				for _, k := range kindAxes {
					if ra >= runMint && k != store.WriteKindOther {
						continue // the run-ID write is its own kind
					}
					for _, h := range hintAxes {
						for _, d := range []delAxis{dNo, dLive, dAlreadyDeleted} {
							if d != dNo && ra >= runMint {
								continue
							}
							for _, ev := range evAxes {
								n++
								want := expectCase(bp, ap, ra, k, h, d, ev)
								f := caseFacts(bp, ap, ra, k, h, d, ev)
								p := Derive(f)
								got := fullPlanString(p)
								if strings.Contains(want, "NO RULE") || got != want {
									fails++
									if fails <= 40 {
										t.Errorf("%s->%s run=%d K=%q H=%d D=%d E=%d: got %q, want %q", bp, ap, ra, k, h, d, ev, got, want)
									}
								}
								checkRunInvariants(t, f, p)
							}
						}
					}
				}
			}
		}
	}
	if fails > 0 {
		t.Errorf("%d of %d cases differ", fails, n)
	}
	t.Logf("checked %d cases", n)
}

// checkRunInvariants checks the run-state invariants of one plan: every
// closure carries the frozen segment identity (ClosesOpenRun), a started
// opens the committed row's run, and the cursor never names an open run
// after a closure without a new started.
func checkRunInvariants(t *testing.T, f Facts, p Plan) {
	t.Helper()
	opened, closed := false, false
	for _, e := range p.Events {
		switch {
		case e.Transition == tc.TransitionStopped && e.StopKind == tc.StopKindSuperseded:
			closed = true
			if !e.ClosesOpenRun {
				t.Errorf("%+v: superseded without the frozen segment identity", f)
			}
		case e.Transition == tc.TransitionStarted:
			opened = true
		}
	}
	if opened && p.Run.OpenRun != f.AfterRunID {
		t.Errorf("%+v: started but open_run = %q", f, p.Run.OpenRun)
	}
	if closed && !opened && p.Run.OpenRun != "" && !isRunWrite(f.Kind) {
		t.Errorf("%+v: a closure left open_run = %q", f, p.Run.OpenRun)
	}
}

// TestDerivePhaseMatrix renders the design's explicit 10x10 matrix (no D,
// no hint, no run change, K = other/stop_record/deletion_write, E none or
// correlated) from phaseRules, and checks that every cell has a rule. Run
// with -v to print it.
func TestDerivePhaseMatrix(t *testing.T) {
	var b strings.Builder
	b.WriteString("| before \\ after | " + strings.Join(allPhases, " | ") + " |\n|---|")
	b.WriteString(strings.Repeat("---|", len(allPhases)) + "\n")
	for _, bp := range allPhases {
		b.WriteString("| **" + bp + "** |")
		for _, ap := range allPhases {
			variants := map[string][]string{}
			for _, v := range []struct {
				name string
				s    bool
				k    store.AgentWriteKind
				ev   Evidence
			}{
				{"S=no", false, store.WriteKindOther, EvidenceNone},
				{"S=no,stop_record", false, store.WriteKindStopRecord, EvidenceNone},
				{"S=no,deletion", false, store.WriteKindDeletion, EvidenceNone},
				{"S=no,E", false, store.WriteKindOther, EvidenceReportRun},
				{"S=no,legacy", false, store.WriteKindOther, EvidenceReportNoRun},
				{"S=yes", true, store.WriteKindOther, EvidenceNone},
				{"S=yes,deletion", true, store.WriteKindDeletion, EvidenceNone},
			} {
				w := expectPhase(bp, ap, v.s, v.k, v.ev)
				if w == "NO RULE" {
					t.Errorf("no rule for %s->%s %s", bp, ap, v.name)
				}
				if w == "" {
					w = "—"
				}
				variants[w] = append(variants[w], v.name)
			}
			keys := make([]string, 0, len(variants))
			for k := range variants {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var cell []string
			for _, k := range keys {
				if len(keys) == 1 {
					cell = append(cell, k)
					break
				}
				cell = append(cell, fmt.Sprintf("%s: %s", strings.Join(variants[k], "/"), k))
			}
			b.WriteString(" " + strings.Join(cell, "; ") + " |")
		}
		b.WriteString("\n")
	}
	t.Log("\n" + b.String())
}
