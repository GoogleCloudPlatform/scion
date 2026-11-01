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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	tc "github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

const examplesAttributionPath = "testdata/lifecycle-examples-attribution.jsonl"

// lcLaunch renders transition@rev(kind,cause,reason)[launch_id/incarnation].
func lcLaunch(evs []*tc.LifecycleEvent) string {
	parts := make([]string, len(evs))
	for i, e := range evs {
		s := fmt.Sprintf("%s@%d", e.Transition, e.TransitionRevision)
		var attrs []string
		for _, a := range []string{e.StopKind, e.Cause, e.EndReason, e.DeleteMode} {
			if a != "" {
				attrs = append(attrs, a)
			}
		}
		if len(attrs) > 0 {
			s += "(" + strings.Join(attrs, ",") + ")"
		}
		parts[i] = s + "[" + e.LaunchID + "/" + e.IncarnationID + "]"
	}
	return strings.Join(parts, " ")
}

// TestLifecycleExamplesAttribution produces the attribution design's D4
// presence goldens through the observer and Marshal (attribution design
// v2.8, section 10.3): every run-class transition emitted with launch_id
// absent while the row has a run, untrusted closures with their segment
// fields and no launch_id, agent-state events without launch_id (held
// unattributed and generation mismatch), an event whose adoption trio was
// dropped with its launch_id, and the malformed-segment discard's resync
// gap. Each passes Validate (Marshal validates). Regenerate with
// -update-examples.
func TestLifecycleExamplesAttribution(t *testing.T) {
	h := newHarness(t)
	at := func(s int) time.Time { return t0.Add(-time.Minute + time.Duration(s)*time.Second) }
	ctx := context.Background()
	write := func(c context.Context, a *store.Agent, op store.AgentWriteOp, mutate func(), sec int, hints store.AgentWriteHints) {
		b := clone(a)
		mutate()
		h.write(c, store.AgentWrite{Op: op, Before: b, After: clone(a), Now: at(sec), Hints: hints})
	}
	create := func(a *store.Agent, sec int) {
		h.write(WithCreateContext(ctx, CreateContext{CreatorType: "user", CreatorID: "u1"}),
			store.AgentWrite{Op: store.AgentWriteCreate, After: clone(a), Now: at(sec)})
	}
	mint := func(a *store.Agent, run string, sec int) {
		write(ctx, a, store.AgentWriteRunID, func() { a.RunID = run }, sec, store.AgentWriteHints{Kind: store.WriteKindRunMint})
	}
	report := func(a *store.Agent, phase string, sec int) {
		run := a.RunID
		write(ctx, a, store.AgentWriteStatus, func() { a.Phase = phase }, sec,
			store.AgentWriteHints{Kind: store.WriteKindStatusHeartbeat, AgentReport: true, ReportRunID: run})
	}
	stopRecord := func(a *store.Agent, sec int) {
		write(ctx, a, store.AgentWriteStatus, func() { a.Phase = "stopped" }, sec, store.AgentWriteHints{Kind: store.WriteKindStopRecord})
	}
	softFinishTo := func(a *store.Agent, run string, sec int) {
		write(ctx, a, store.AgentWriteSoftDelete, func() { a.RunID, a.Phase, a.DeletedAt = run, "stopped", at(sec) }, sec,
			store.AgentWriteHints{Kind: store.WriteKindDeletion})
	}
	restore := func(a *store.Agent, sec int) {
		write(ctx, a, store.AgentWriteUpdate, func() { a.DeletedAt = time.Time{} }, sec, store.AgentWriteHints{})
	}

	// a1: R1 ran and stopped; the soft finish recorded B1; restore (names
	// R1). Then B1 runs unproven: started, stopped(error), halted,
	// stopped(restart_stop_leg), launch_failed closing its segment,
	// stopped(superseded) at a fresh mint, and stopped(stopped), all with
	// launch_id absent; a hard delete closes the last untrusted segment.
	a := baseAgent()
	create(a, 0)
	mint(a, "R1", 1)
	write(ctx, a, store.AgentWriteStatus, func() { a.Phase = "running" }, 2, store.AgentWriteHints{StartWrite: true})
	stopRecord(a, 3)
	softFinishTo(a, "B1", 4)
	restore(a, 5)
	report(a, "running", 6)
	report(a, "error", 7)
	report(a, "running", 8)
	report(a, "halted", 9)
	report(a, "running", 10)
	h.write(ctx, store.AgentWrite{Op: store.AgentWriteRestartStopLeg, Before: clone(a), After: clone(a), Now: at(11),
		Hints: store.AgentWriteHints{StopLegRunID: "B1"}})
	report(a, "running", 12)
	write(ctx, a, store.AgentWriteLaunch, func() { a.Phase = "error" }, 13,
		store.AgentWriteHints{LaunchEnd: &store.LaunchEndHint{Reason: store.LaunchEndReasonFailed, Code: store.LaunchErrorLaunchTimeout}})
	report(a, "running", 14)
	mint(a, "B2", 15) // B2 is not fresh against the row's runs? it is fresh: establishes
	write(ctx, a, store.AgentWriteStatus, func() {}, 16, store.AgentWriteHints{StartWrite: true})
	stopRecord(a, 17)

	// a2: a row with no run soft-finished to B1 (explicit UNATTRIBUTED):
	// deleted, restored, metadata_changed, configured, launch_accepted,
	// reincarnated, stopped(start_unobserved) without launch_id; then an
	// untrusted segment closed by a hard delete.
	b := baseAgent()
	b.ID, b.Slug, b.Name = "00000000-0000-4000-8000-0000000000a2", "a2", "a2"
	create(b, 20)
	softFinishTo(b, "B1", 21)
	restore(b, 22)
	write(ctx, b, store.AgentWriteUpdate, func() { b.Labels["stage"] = "held" }, 23, store.AgentWriteHints{})
	write(WithConfigResolution(ctx), b, store.AgentWriteUpdate, func() { b.AppliedConfig.Model = "m-held" }, 24, store.AgentWriteHints{})
	write(ctx, b, store.AgentWriteLaunch, func() {
		b.LaunchID, b.LaunchState, b.Phase = "al-9", store.LaunchStateActive, "provisioning"
	}, 25, store.AgentWriteHints{LaunchAccepted: true, LaunchOwner: "bi-9"})
	write(ctx, b, store.AgentWriteUpdate, func() { b.Generation = 2; b.LaunchState = store.LaunchStateEnded }, 26, store.AgentWriteHints{})
	write(ctx, b, store.AgentWriteUpdate, func() { b.Phase = "running" }, 27, store.AgentWriteHints{})
	stopRecord(b, 28)
	report(b, "running", 29)
	h.write(ctx, store.AgentWrite{Op: store.AgentWriteHardDelete, Before: clone(b), Now: at(30)})

	// a3: held with the attribution R1/1, then a generation change: the
	// agent-state events after it omit (generation mismatch), incl. a
	// no-segment deleted.
	c := baseAgent()
	c.ID, c.Slug, c.Name = "00000000-0000-4000-8000-0000000000a3", "a3", "a3"
	create(c, 40)
	mint(c, "R1", 41)
	write(ctx, c, store.AgentWriteStatus, func() { c.Phase = "running" }, 42, store.AgentWriteHints{StartWrite: true})
	stopRecord(c, 43)
	softFinishTo(c, "B3", 44)
	restore(c, 45)
	write(ctx, c, store.AgentWriteUpdate, func() { c.Generation = 2 }, 46, store.AgentWriteHints{})
	write(ctx, c, store.AgentWriteUpdate, func() { c.Labels["stage"] = "mismatch" }, 47, store.AgentWriteHints{})
	write(ctx, c, store.AgentWriteSoftDelete, func() { c.DeletedAt = at(48) }, 48, store.AgentWriteHints{Kind: store.WriteKindDeletion})

	// a4: a clear rebinding (adoption pending) of R2's segment to Y1, then
	// an unwitnessed generation bump pushes Y1: the closure omits launch_id
	// and its adoption trio is dropped with it.
	d := baseAgent()
	d.ID, d.Slug, d.Name = "00000000-0000-4000-8000-0000000000a4", "a4", "a4"
	create(d, 50)
	mint(d, "R2", 51)
	write(ctx, d, store.AgentWriteStatus, func() { d.Phase = "running" }, 52, store.AgentWriteHints{StartWrite: true})
	write(ctx, d, store.AgentWriteRunID, func() { d.RunID = "Y1" }, 53,
		store.AgentWriteHints{Kind: store.WriteKindRunAdopt, AdoptionSource: store.AdoptionSourceCAS})
	write(ctx, d, store.AgentWriteUpdate, func() { d.Generation = 2 }, 54, store.AgentWriteHints{})
	stopRecord(d, 55)

	// a5: a malformed segment identity (revision 0) is discarded with one
	// resync gap without launch_id (P3, R4-6).
	e := baseAgent()
	e.ID, e.Slug, e.Name = "00000000-0000-4000-8000-0000000000a5", "a5", "a5"
	create(e, 60)
	mint(e, "R5", 61)
	write(ctx, e, store.AgentWriteStatus, func() { e.Phase = "running" }, 62, store.AgentWriteHints{StartWrite: true})
	h.db.stateMu.Lock()
	cur := h.db.cursors[e.ID]
	cur.OpenRunMetadataRevision = 0
	h.db.cursors[e.ID] = cur
	h.db.stateMu.Unlock()
	stopRecord(e, 63)

	evs := h.events()
	byAgent := map[string][]*tc.LifecycleEvent{}
	for _, ev := range evs {
		byAgent[ev.AgentID] = append(byAgent[ev.AgentID], ev)
	}
	want := map[string]string{
		a.ID: "created@1[/1] started@2[R1/1] stopped@3(stopped)[R1/1] deleted@4(soft)[R1/1] restored@5[R1/1] " +
			"started@6[/1] stopped@7(error)[/1] started@8[/1] halted@9[/1] started@10[/1] stopped@11(stopped,restart_stop_leg)[/1] " +
			"started@12[/1] launch_failed@13(failed)[/1] started@14[/1] stopped@15(superseded)[/1] started@16[B2/1] stopped@17(stopped)[B2/1]",
		b.ID: "created@1[/1] deleted@2(soft)[/1] restored@3[/1] metadata_changed@4[/1] configured@5[/1] launch_accepted@6[/1] " +
			"reincarnated@7[/2] stopped@8(stopped,start_unobserved)[/2] started@9[/2] deleted@10(hard)[/2]",
		c.ID: "created@1[/1] started@2[R1/1] stopped@3(stopped)[R1/1] deleted@4(soft)[R1/1] restored@5[R1/1] " +
			"reincarnated@6[/2] metadata_changed@7[/2] deleted@8(soft)[/2]",
		d.ID: "created@1[/1] started@2[R2/1] reincarnated@3[/2] stopped@4(stopped)[/1]",
		e.ID: "created@1[/1] started@2[R5/1] resynced@4(unobserved_change)[/1] stopped@5(stopped,start_unobserved)[/1]",
	}
	for id, w := range want {
		if got := lcLaunch(byAgent[id]); got != w {
			t.Errorf("%s:\n got %s\nwant %s", id, got, w)
		}
	}
	for _, ev := range byAgent[d.ID] {
		if ev.AdoptedFromLaunchID != "" {
			t.Errorf("a4 %s@%d carries an adoption trio without launch_id", ev.Transition, ev.TransitionRevision)
		}
	}

	var buf bytes.Buffer
	for _, row := range h.db.rows() {
		buf.Write(row.Payload)
		buf.WriteByte('\n')
	}
	if *updateExamples {
		if err := os.WriteFile(filepath.FromSlash(examplesAttributionPath), buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(filepath.FromSlash(examplesAttributionPath))
	if err != nil {
		t.Fatalf("%v (run with -update-examples to create it)", err)
	}
	if !bytes.Equal(got, buf.Bytes()) {
		t.Errorf("examples differ from %s; rerun with -update-examples if the change is intended:\n%s", examplesAttributionPath, buf.String())
	}
}
