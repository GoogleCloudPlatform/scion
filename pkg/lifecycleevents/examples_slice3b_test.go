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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	tc "github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

const examplesSlice3bPath = "testdata/lifecycle-examples-slice3b.jsonl"

// TestLifecycleExamplesSlice3b produces the slice 3b producer examples
// through the observer and Marshal (WP1 slice 3b design v9 section 7, the
// per-caller producer fixture for WP4): an async launch observed running
// whose completed report settles Rm -> Rb (adoption_source cas, carried by
// the echo's configured), and a heartbeat stranded-run adoption Rm -> Rb
// (adoption_source heartbeat_adopt, carried by the stop). Regenerate with
// -update-examples.
func TestLifecycleExamplesSlice3b(t *testing.T) {
	h := newHarness(t)
	at := func(s int) time.Time { return t0.Add(-time.Minute + time.Duration(s)*time.Second) }
	ctx := context.Background()
	write := func(a *store.Agent, op store.AgentWriteOp, mutate func(), sec int, hints store.AgentWriteHints) {
		b := clone(a)
		mutate()
		h.write(ctx, store.AgentWrite{Op: op, Before: b, After: clone(a), Now: at(sec), Hints: hints})
	}
	create := func(a *store.Agent, sec int) {
		h.write(WithCreateContext(ctx, CreateContext{CreatorType: "user", CreatorID: "u1"}),
			store.AgentWrite{Op: store.AgentWriteCreate, After: clone(a), Now: at(sec)})
	}

	// a1: async launch on run-m, observed running (the launch ends
	// running_observed), then the completed report's settle adopts run-b
	// (cas) and the echo for run-b follows.
	a := baseAgent()
	create(a, 0)
	write(a, store.AgentWriteRunID, func() { a.RunID = "run-m" }, 1, store.AgentWriteHints{Kind: store.WriteKindRunMint})
	write(a, store.AgentWriteLaunch, func() {
		a.LaunchID, a.LaunchRunID, a.LaunchState, a.Phase = "al-1", "run-m", store.LaunchStateActive, "provisioning"
	}, 2, store.AgentWriteHints{LaunchAccepted: true, LaunchOwner: "bi-1"})
	write(a, store.AgentWriteStatus, func() {
		a.Phase, a.LaunchState, a.LaunchEndReason = "running", store.LaunchStateEnded, store.LaunchEndReasonRunningObserved
	}, 3, store.AgentWriteHints{StartWrite: true})
	write(a, store.AgentWriteRunID, func() { a.RunID = "run-b" }, 4,
		store.AgentWriteHints{Kind: store.WriteKindRunAdopt, AdoptionSource: store.AdoptionSourceCAS})
	write(a, store.AgentWriteEffectiveConfig, func() {
		e := echoFor("run-b")
		e.LaunchID, e.Source = "al-1", store.EffectiveConfigSourceLaunchReport
		a.EffectiveConfig = e
	}, 4, store.AgentWriteHints{})

	// a2: running on run-m; the heartbeat adoption moves the row to the
	// run its entry holds (heartbeat_adopt); the stop carries it.
	c := baseAgent()
	c.ID, c.Slug, c.Name = "00000000-0000-4000-8000-0000000000a2", "a2", "a2"
	create(c, 10)
	write(c, store.AgentWriteRunID, func() { c.RunID = "run-m" }, 11, store.AgentWriteHints{Kind: store.WriteKindRunMint})
	write(c, store.AgentWriteStatus, func() { c.Phase = "running" }, 12, store.AgentWriteHints{StartWrite: true})
	write(c, store.AgentWriteRunID, func() { c.RunID = "run-b" }, 13,
		store.AgentWriteHints{Kind: store.WriteKindRunAdopt, AdoptionSource: store.AdoptionSourceHeartbeat})
	write(c, store.AgentWriteStatus, func() { c.Phase = "stopped" }, 14, store.AgentWriteHints{Kind: store.WriteKindStopRecord})

	if want := "created@1 launch_accepted@2 started@3 configured@4 created@1 started@2 stopped@3"; seq(h.events()) != want {
		t.Fatalf("example sequence changed:\n got %s\nwant %s", seq(h.events()), want)
	}
	want := map[string]string{a.ID: store.AdoptionSourceCAS, c.ID: store.AdoptionSourceHeartbeat}
	carried := map[string]int{}
	for _, e := range h.events() {
		if e.AdoptedFromLaunchID == "" {
			continue
		}
		carried[e.AgentID]++
		if e.AdoptedFromLaunchID != "run-m" || e.LaunchID != "run-b" || e.AdoptionSource != want[e.AgentID] {
			t.Errorf("%s %s@%d: adopted_from %q launch_id %q source %q, want run-m run-b %q",
				e.AgentID, e.Transition, e.TransitionRevision, e.AdoptedFromLaunchID, e.LaunchID, e.AdoptionSource, want[e.AgentID])
		}
		if e.AgentID == a.ID && e.Transition != tc.TransitionConfigured {
			t.Errorf("a1: the provenance rides on %s, want configured", e.Transition)
		}
	}
	for id := range want {
		if carried[id] != 1 {
			t.Errorf("%s: %d events carry the adoption, want exactly 1", id, carried[id])
		}
	}

	var buf bytes.Buffer
	for _, row := range h.db.rows() {
		buf.Write(row.Payload)
		buf.WriteByte('\n')
	}
	if *updateExamples {
		if err := os.WriteFile(filepath.FromSlash(examplesSlice3bPath), buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(filepath.FromSlash(examplesSlice3bPath))
	if err != nil {
		t.Fatalf("%v (run with -update-examples to create it)", err)
	}
	if !bytes.Equal(got, buf.Bytes()) {
		t.Errorf("examples differ from %s; rerun with -update-examples if the change is intended:\n%s", examplesSlice3bPath, buf.String())
	}
}
