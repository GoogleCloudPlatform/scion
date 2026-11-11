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
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

var updateExamples = flag.Bool("update-examples", false, "rewrite testdata/lifecycle-examples-slice2.jsonl, -slice3.jsonl and -slice3b.jsonl")

const examplesPath = "testdata/lifecycle-examples-slice2.jsonl"

// TestLifecycleExamplesSlice2 produces the slice 2 example events through
// the observer and Marshal (design 3.1: byte-level goldens come only from
// Marshal; review r7 N5): a sync create whose broker answer named the run
// and said running (E(iv) started), a restart that closes R1 at the stop
// leg, a queued restart closed as superseded with run_changed_at, an
// authoritative stop of an unobserved start, a legacy dispatch result
// (started(run_uncorrelated)), an accepted async launch and a reaper
// deadline. The file is the WP4 example set; regenerate it with
// -update-examples.
func TestLifecycleExamplesSlice2(t *testing.T) {
	h := newHarness(t)
	// Commit times precede the observer's observed_ts clock (t0 onward), as
	// in production, where observed_ts is taken after the write (review r2 N2).
	at := func(s int) time.Time { return t0.Add(-time.Minute + time.Duration(s)*time.Second) }
	ctx := context.Background()
	// create commits a create at at(0), before every later write.
	create := func(a *store.Agent) {
		cctx := WithCreateContext(ctx, CreateContext{CreatorType: "user", CreatorID: "u1"})
		h.write(cctx, store.AgentWrite{Op: store.AgentWriteCreate, After: clone(a), Now: at(0)})
	}

	// a1: sync create, E(iv), label change, HTTP restart, queued restart,
	// stop of the unobserved start.
	a := baseAgent()
	create(a)
	r := func(run string, sec int) {
		b := clone(a)
		a.RunID = run
		h.write(ctx, store.AgentWrite{Op: store.AgentWriteRunID, Before: b, After: clone(a), Now: at(sec),
			Hints: store.AgentWriteHints{Kind: store.WriteKindRunMint}})
	}
	r("run-1", 1)
	b := clone(a)
	a.Phase = "running"
	a.AppliedConfig.HarnessConfigSource = "broker-resolved"
	h.write(WithDispatchResult(ctx, DispatchResult{RunID: "run-1", BrokerEvidence: true}),
		store.AgentWrite{Op: store.AgentWriteUpdate, Before: b, After: clone(a), Now: at(2)})
	b = clone(a)
	a.Labels["stage"] = "review"
	h.write(ctx, store.AgentWrite{Op: store.AgentWriteUpdate, Before: b, After: clone(a), Now: at(3)})
	h.write(ctx, store.AgentWrite{Op: store.AgentWriteRestartStopLeg, Before: clone(a), After: clone(a), Now: at(4),
		Hints: store.AgentWriteHints{StopLegRunID: "run-1"}})
	r("run-2", 4)
	h.write(ctx, store.AgentWrite{Op: store.AgentWriteStatus, Before: clone(a), After: clone(a), Now: at(5),
		Hints: store.AgentWriteHints{StartWrite: true}})
	r("run-3", 5) // a queued restart: run-ID writes only
	h.write(ctx, store.AgentWrite{Op: store.AgentWriteStatus, Before: clone(a), After: clone(a), Now: at(6),
		Hints: store.AgentWriteHints{AgentReport: true, ReportRunID: "run-3", Kind: store.WriteKindStatusHeartbeat}})
	r("run-4", 6)
	b = clone(a)
	a.Phase = "stopped"
	h.write(ctx, store.AgentWrite{Op: store.AgentWriteStatus, Before: b, After: clone(a), Now: at(7),
		Hints: store.AgentWriteHints{Kind: store.WriteKindStopRecord}})

	// a2: a legacy broker answered running without naming the run.
	l := baseAgent()
	l.ID, l.Slug, l.Name = "00000000-0000-4000-8000-0000000000a2", "a2", "a2"
	create(l)
	lb := clone(l)
	l.RunID = "run-9"
	h.write(ctx, store.AgentWrite{Op: store.AgentWriteRunID, Before: lb, After: clone(l), Now: at(8),
		Hints: store.AgentWriteHints{Kind: store.WriteKindRunMint}})
	lb = clone(l)
	l.Phase = "running"
	h.write(WithDispatchResult(ctx, DispatchResult{RunID: "run-9", NoBrokerRun: true}),
		store.AgentWrite{Op: store.AgentWriteUpdate, Before: lb, After: clone(l), Now: at(9)})

	// a3: an accepted async launch reaped at its deadline.
	c := baseAgent()
	c.ID, c.Slug, c.Name, c.RunID = "00000000-0000-4000-8000-0000000000a3", "a3", "a3", "run-7"
	create(c)
	cb := clone(c)
	c.LaunchID, c.LaunchState, c.Phase = "al-1", store.LaunchStateActive, "provisioning"
	h.write(ctx, store.AgentWrite{Op: store.AgentWriteLaunch, Before: cb, After: clone(c), Now: at(10),
		Hints: store.AgentWriteHints{LaunchAccepted: true, LaunchOwner: "bi-1"}})
	cb = clone(c)
	c.LaunchState, c.Phase = store.LaunchStateEnded, "error"
	h.write(ctx, store.AgentWrite{Op: store.AgentWriteLaunch, Before: cb, After: clone(c), Now: at(11),
		Hints: store.AgentWriteHints{LaunchEnd: &store.LaunchEndHint{Reason: store.LaunchEndReasonTimedOut, Code: store.LaunchErrorLaunchTimeout}}})

	// Per agent, event_ts never goes backwards with the revision, and
	// observed_ts is never earlier than event_ts (review r2 N2,
	// r2-delta Required).
	lastTS := map[string]time.Time{}
	for _, e := range h.events() {
		ets, err := time.Parse(time.RFC3339Nano, e.EventTS)
		if err != nil {
			t.Fatal(err)
		}
		ots, err := time.Parse(time.RFC3339Nano, e.ObservedTS)
		if err != nil {
			t.Fatal(err)
		}
		if ots.Before(ets) {
			t.Errorf("%s rev %d: observed_ts %s before event_ts %s", e.AgentID, e.TransitionRevision, e.ObservedTS, e.EventTS)
		}
		if prev, ok := lastTS[e.AgentID]; ok && ets.Before(prev) {
			t.Errorf("%s rev %d: event_ts %s runs backwards (previous %s)", e.AgentID, e.TransitionRevision, e.EventTS, prev.Format(time.RFC3339Nano))
		}
		lastTS[e.AgentID] = ets
	}

	var buf bytes.Buffer
	for _, row := range h.db.rows() {
		buf.Write(row.Payload)
		buf.WriteByte('\n')
	}
	if want := "created@1 configured@2 started@3 metadata_changed@4 stopped@5(restart_stop_leg) started@6 stopped@7 started@8 stopped@9 stopped@10(start_unobserved) " +
		"created@1 started@2(run_uncorrelated) created@1 launch_accepted@2 launch_failed@3"; seq(h.events()) != want {
		t.Fatalf("example sequence changed:\n got %s\nwant %s", seq(h.events()), want)
	}
	if *updateExamples {
		if err := os.WriteFile(filepath.FromSlash(examplesPath), buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(filepath.FromSlash(examplesPath))
	if err != nil {
		t.Fatalf("%v (run with -update-examples to create it)", err)
	}
	if !bytes.Equal(want, buf.Bytes()) {
		t.Errorf("examples differ from %s; rerun with -update-examples if the change is intended:\n%s", examplesPath, buf.String())
	}
}
