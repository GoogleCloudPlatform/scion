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

package ruleengine_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
)

// TestRev3Probe_StaleReplayDuringInlineEvalStaysTerminal (RV3-1):
// A stale-token replay (EvaluatePending or EvaluateGroup on the step-1 handle)
// that lands while Next is evaluating an Inline step makes the run terminal
// (invalid_completion). After Next re-locks, it must NOT record the inline
// outcome, must NOT advance nextPos, and must NOT overwrite chainStateDone:
// the run stays terminal with zero further I/O.
func TestRev3Probe_StaleReplayDuringInlineEvalStaysTerminal(t *testing.T) {
	for _, viaGroup := range []bool{false, true} {
		io := newCountingFakeIODialect("io3", time.Second)
		inl := newCountingFakeIODialect("inl3", time.Second)
		inl.performsIO = false
		started := make(chan struct{})
		release := make(chan struct{})
		inl.setHandler("slow", func(ctx context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
			close(started)
			<-release
			return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
		})
		eng, _ := newChainTestEngine(t, "probe", io, inl)
		c1 := mustCompile(t, eng, "probe", "io3", "a", ruleengine.KindFilter)
		c2 := mustCompile(t, eng, "probe", "inl3", "slow", ruleengine.KindFilter)
		c3 := mustCompile(t, eng, "probe", "io3", "b", ruleengine.KindFilter)
		dom := ruleengine.Domain{Ctx: context.Background()}
		run, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom},
			ruleengine.Chain{Elements: []ruleengine.ChainElement{
				{Index: 1, Compiled: c1, GroupKey: "g"},
				{Index: 2, Compiled: c2},
				{Index: 3, Compiled: c3},
			}},
			ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, p1, _, err := run.Next()
		if err != nil || p1 == nil {
			t.Fatalf("next1: %v %v", p1, err)
		}
		if err := eng.EvaluatePending(p1); err != nil {
			t.Fatal(err)
		}
		if _, err := run.Resume(); err != nil {
			t.Fatal(err)
		}

		type nr struct {
			done bool
			err  error
		}
		ch := make(chan nr, 1)
		go func() { _, _, d, e := run.Next(); ch <- nr{d, e} }()
		<-started

		var replayErr error
		if viaGroup {
			_, replayErr = eng.EvaluateGroup(p1, nil)
		} else {
			replayErr = eng.EvaluatePending(p1)
		}
		if replayErr == nil {
			t.Fatalf("viaGroup=%v: stale replay unexpectedly succeeded", viaGroup)
		}
		resMid, _ := run.Result()
		if !resMid.Stopped {
			t.Fatalf("viaGroup=%v: expected Stopped=true immediately after stale replay", viaGroup)
		}

		close(release)
		r2 := <-ch
		if !r2.done || r2.err != nil {
			t.Errorf("viaGroup=%v: in-flight inline Next() must return done=true, err=nil; got done=%v err=%v", viaGroup, r2.done, r2.err)
		}

		callsBefore := io.callCount()
		_, p3, done3, err3 := run.Next()
		if p3 != nil {
			_ = eng.EvaluatePending(p3)
		}
		res, _ := run.Result()

		if p3 != nil || !done3 || err3 != nil || io.callCount() != callsBefore {
			t.Errorf("viaGroup=%v: run resurrected after terminal invalid_completion: p3=%v done3=%v err3=%v ioCallsAfter=%d",
				viaGroup, p3 != nil, done3, err3, io.callCount()-callsBefore)
		}
		if !res.Stopped {
			t.Errorf("viaGroup=%v: expected Stopped=true", viaGroup)
		}
		if len(res.Steps) != 2 || res.Steps[1].Reason != ruleengine.ReasonInvalidCompletion {
			t.Errorf("viaGroup=%v: expected no step recorded after terminal invalid_completion; steps=%+v", viaGroup, res.Steps)
		}
		if !slices.Equal(res.Unexecuted, []int{2, 3}) {
			t.Errorf("viaGroup=%v: Unexecuted = %v, want [2 3]", viaGroup, res.Unexecuted)
		}
	}
}

// TestRev3Probe_StaleReplayInPendingCompleted (O3-1):
// A stale step-1 token replayed while step 2 is completed but unresumed
// (PendingCompleted) must record step 2's real outcome, advance nextPos so
// index 2 is not in Unexecuted, and record invalid_completion without
// double-counting Through.
func TestRev3Probe_StaleReplayInPendingCompleted(t *testing.T) {
	io := newCountingFakeIODialect("io4", time.Second)
	eng, _ := newChainTestEngine(t, "probe", io)
	c1 := mustCompile(t, eng, "probe", "io4", "a", ruleengine.KindFilter)
	c2 := mustCompile(t, eng, "probe", "io4", "b", ruleengine.KindFilter)
	c3 := mustCompile(t, eng, "probe", "io4", "c", ruleengine.KindFilter)
	dom := ruleengine.Domain{Ctx: context.Background()}
	run, err := eng.NewChainRun(ruleengine.ChainDomains{Inline: dom, IO: dom},
		ruleengine.Chain{Elements: []ruleengine.ChainElement{
			{Index: 1, Compiled: c1},
			{Index: 2, Compiled: c2},
			{Index: 3, Compiled: c3},
		}},
		ruleengine.Input{}, ruleengine.ChainLimits{}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, p1, _, _ := run.Next()
	if err := eng.EvaluatePending(p1); err != nil {
		t.Fatal(err)
	}
	if _, err := run.Resume(); err != nil {
		t.Fatal(err)
	}
	_, p2, _, _ := run.Next()
	if err := eng.EvaluatePending(p2); err != nil {
		t.Fatal(err)
	}
	if err := eng.EvaluatePending(p1); err == nil {
		t.Fatal("stale replay unexpectedly succeeded")
	}

	res, _ := run.Result()
	if !res.Stopped {
		t.Fatalf("expected Stopped=true")
	}
	for _, u := range res.Unexecuted {
		for _, s := range res.Steps {
			if s.Index == u {
				t.Errorf("index %d both recorded in Steps and listed Unexecuted (steps=%+v unexec=%v)", u, res.Steps, res.Unexecuted)
			}
		}
	}
	if !slices.Equal(res.Unexecuted, []int{3}) {
		t.Errorf("Unexecuted = %v, want [3]", res.Unexecuted)
	}
	if res.Through != 2 {
		t.Errorf("Through = %d, want 2 (must not double-count invalid_completion)", res.Through)
	}
	if len(res.Steps) != 3 || res.Steps[0].Index != 1 || res.Steps[1].Index != 2 || res.Steps[1].Class != ruleengine.StepClassPass || res.Steps[2].Reason != ruleengine.ReasonInvalidCompletion {
		t.Errorf("unexpected Steps = %+v, want [1:pass, 2:pass, 1:failed/invalid_completion]", res.Steps)
	}
}
