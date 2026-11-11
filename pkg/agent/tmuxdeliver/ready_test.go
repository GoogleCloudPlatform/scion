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

package tmuxdeliver_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/tmuxdeliver"
)

const nonce = "0123456789abcdef0123456789abcdef"

func probeOut(session, now, initrun, initspec, ready, readymtime string) string {
	return fmt.Sprintf("session=%s\nnow=%s\ninitrun=%s\ninitspec=%s\nready=%s\nreadymtime=%s\n",
		session, now, initrun, initspec, ready, readymtime)
}

func TestProbeArgv_IsConstantShell(t *testing.T) {
	a, b := tmuxdeliver.ProbeArgv(), tmuxdeliver.ProbeArgv()
	if !reflect.DeepEqual(a, b) || len(a) != 3 || a[0] != "sh" || a[1] != "-c" {
		t.Fatalf("ProbeArgv = %q", a)
	}
}

func TestParseProbe(t *testing.T) {
	hookRun := "nonce=" + nonce + " mode=" + state.ReadyModeHook
	settleRun := "nonce=" + nonce + " mode=" + state.ReadyModeSettle
	cases := []struct {
		name       string
		out        string
		ready      bool
		mode       string
		reason     string
		fresh      bool
		deadline   time.Duration
		wantSettle time.Duration
	}{
		{name: "no session", out: probeOut("", "1000", hookRun, "", "", ""), mode: state.ReadyModeHook, reason: "tmux session not found", wantSettle: time.Second},
		{name: "hook ready, old marker", out: probeOut("900", "1000", hookRun, "", "nonce="+nonce, "990"), ready: true, mode: state.ReadyModeHook, wantSettle: time.Second},
		{name: "hook ready, fresh marker", out: probeOut("900", "1000", hookRun, "", "nonce="+nonce, "1000"), ready: true, mode: state.ReadyModeHook, fresh: true, wantSettle: time.Second},
		{name: "hook marker missing", out: probeOut("900", "1000", hookRun, "", "", ""), mode: state.ReadyModeHook, reason: "ready marker missing", wantSettle: time.Second},
		{name: "hook marker from another run", out: probeOut("900", "1000", hookRun, "", "nonce=ffffffffffffffffffffffffffffffff", "990"), mode: state.ReadyModeHook, reason: "ready marker from another run", wantSettle: time.Second},
		{name: "settle mode old session", out: probeOut("900", "1000", settleRun, "", "", ""), ready: true, mode: state.ReadyModeSettle, wantSettle: time.Second},
		{name: "settle mode young session", out: probeOut("995", "1000", settleRun, "", "", ""), mode: state.ReadyModeSettle, reason: "session younger than 8s", wantSettle: time.Second},
		{name: "no init-run file, old session", out: probeOut("900", "1000", "", "", "", ""), ready: true, mode: tmuxdeliver.ModeNoInitRun, wantSettle: time.Second},
		{name: "hook line 2 sets settle and deadline", out: probeOut("900", "1000", hookRun, "strategy=hook settle_ms=2000 deadline_ms=30000", "nonce="+nonce, "990"), ready: true, mode: state.ReadyModeHook, deadline: 30 * time.Second, wantSettle: 2 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tmuxdeliver.ParseProbe(tc.out, 8*time.Second, time.Second)
			if p.Ready != tc.ready || p.Mode != tc.mode || p.Reason != tc.reason || p.MarkerFresh != tc.fresh || p.Deadline != tc.deadline || p.Settle != tc.wantSettle {
				t.Errorf("ParseProbe = %+v", p)
			}
			if tc.mode != tmuxdeliver.ModeNoInitRun && p.Nonce != nonce {
				t.Errorf("nonce = %q, want %q", p.Nonce, nonce)
			}
		})
	}
}

// fakeClock drives Wait in tests: Sleep advances it instantly.
type fakeClock struct {
	t      time.Time
	sleeps []time.Duration
}

func (c *fakeClock) now() time.Time { return c.t }
func (c *fakeClock) sleep(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	c.sleeps = append(c.sleeps, d)
	c.t = c.t.Add(d)
	return true
}

// scriptRunner answers successive probes from a script.
type scriptRunner struct {
	outs []string
	errs []error
	n    int
}

func (s *scriptRunner) Run(_ context.Context, argv []string, stdin io.Reader) (string, error) {
	if !reflect.DeepEqual(argv, tmuxdeliver.ProbeArgv()) || stdin != nil {
		return "", errors.New("unexpected command")
	}
	i := s.n
	if i >= len(s.outs) {
		i = len(s.outs) - 1
	}
	s.n++
	var err error
	if i < len(s.errs) {
		err = s.errs[i]
	}
	return s.outs[i], err
}

func waitCfg(r tmuxdeliver.Runner, c *fakeClock, memo *tmuxdeliver.MemoStore) tmuxdeliver.WaitConfig {
	return tmuxdeliver.WaitConfig{
		Runner:  r,
		Timings: tmuxdeliver.Timings{PollInterval: 500 * time.Millisecond, Deadline: 3 * time.Second, Settle: time.Second, SettleFallback: 8 * time.Second},
		Memo:    memo,
		Key:     "container-1",
		Now:     c.now,
		Sleep:   c.sleep,
	}
}

var (
	hookRun    = "nonce=" + nonce + " mode=" + state.ReadyModeHook
	notReady   = probeOut("900", "1000", hookRun, "", "", "")
	readyOld   = probeOut("900", "1000", hookRun, "", "nonce="+nonce, "990")
	readyFresh = probeOut("900", "1000", hookRun, "", "nonce="+nonce, "1000")
)

func TestWait_ReadyFirstProbe(t *testing.T) {
	c := &fakeClock{t: time.Unix(1000, 0)}
	res := tmuxdeliver.Wait(context.Background(), waitCfg(&scriptRunner{outs: []string{readyOld}}, c, &tmuxdeliver.MemoStore{}))
	if res.Outcome != tmuxdeliver.WaitReady || len(c.sleeps) != 0 {
		t.Fatalf("Wait = %+v, sleeps %v", res, c.sleeps)
	}
}

func TestWait_FreshMarkerSettles(t *testing.T) {
	c := &fakeClock{t: time.Unix(1000, 0)}
	res := tmuxdeliver.Wait(context.Background(), waitCfg(&scriptRunner{outs: []string{readyFresh}}, c, &tmuxdeliver.MemoStore{}))
	if res.Outcome != tmuxdeliver.WaitSettled || !reflect.DeepEqual(c.sleeps, []time.Duration{time.Second}) {
		t.Fatalf("Wait = %+v, sleeps %v", res, c.sleeps)
	}
}

func TestWait_ReadyDuringWaitSettles(t *testing.T) {
	c := &fakeClock{t: time.Unix(1000, 0)}
	res := tmuxdeliver.Wait(context.Background(), waitCfg(&scriptRunner{outs: []string{notReady, readyOld}}, c, &tmuxdeliver.MemoStore{}))
	if res.Outcome != tmuxdeliver.WaitSettled {
		t.Fatalf("Wait = %+v", res)
	}
	if want := []time.Duration{500 * time.Millisecond, time.Second}; !reflect.DeepEqual(c.sleeps, want) {
		t.Errorf("sleeps = %v, want %v", c.sleeps, want)
	}
}

// TestWait_TimeoutThenMemo: the first wait times out (first timeout); a later
// wait for the same run returns WaitRunTimedOut without waiting.
func TestWait_TimeoutThenMemo(t *testing.T) {
	memo := &tmuxdeliver.MemoStore{}
	c := &fakeClock{t: time.Unix(1000, 0)}
	res := tmuxdeliver.Wait(context.Background(), waitCfg(&scriptRunner{outs: []string{notReady}}, c, memo))
	if res.Outcome != tmuxdeliver.WaitTimedOut || !res.FirstTimeout || res.Waited != 3*time.Second {
		t.Fatalf("first Wait = %+v", res)
	}
	c2 := &fakeClock{t: time.Unix(2000, 0)}
	res = tmuxdeliver.Wait(context.Background(), waitCfg(&scriptRunner{outs: []string{notReady}}, c2, memo))
	if res.Outcome != tmuxdeliver.WaitRunTimedOut || len(c2.sleeps) != 0 {
		t.Fatalf("second Wait = %+v, sleeps %v", res, c2.sleeps)
	}
	if memo.Len() != 1 {
		t.Errorf("memo size = %d, want 1", memo.Len())
	}
}

// TestWait_NilMemo works without a memo and warns on every timeout.
func TestWait_NilMemo(t *testing.T) {
	c := &fakeClock{t: time.Unix(1000, 0)}
	res := tmuxdeliver.Wait(context.Background(), waitCfg(&scriptRunner{outs: []string{notReady}}, c, nil))
	if res.Outcome != tmuxdeliver.WaitTimedOut || !res.FirstTimeout {
		t.Fatalf("Wait = %+v", res)
	}
}

func TestWait_TargetGone(t *testing.T) {
	c := &fakeClock{t: time.Unix(1000, 0)}
	gone := errors.New("no such container")
	cfg := waitCfg(&scriptRunner{outs: []string{""}, errs: []error{gone}}, c, nil)
	cfg.Gone = func(_ context.Context, err error) bool { return errors.Is(err, gone) }
	res := tmuxdeliver.Wait(context.Background(), cfg)
	if res.Outcome != tmuxdeliver.WaitTargetGone || !errors.Is(res.ProbeErr, gone) {
		t.Fatalf("Wait = %+v", res)
	}
}

func TestWait_Cancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &fakeClock{t: time.Unix(1000, 0)}
	res := tmuxdeliver.Wait(ctx, waitCfg(&scriptRunner{outs: []string{notReady}}, c, nil))
	if res.Outcome != tmuxdeliver.WaitCancelled {
		t.Fatalf("Wait = %+v", res)
	}
}

// TestWait_LineTwoDeadline: a hook line 2 deadline replaces the caller's.
func TestWait_LineTwoDeadline(t *testing.T) {
	out := probeOut("900", "1000", hookRun, "strategy=hook settle_ms=1000 deadline_ms=10000", "", "")
	c := &fakeClock{t: time.Unix(1000, 0)}
	res := tmuxdeliver.Wait(context.Background(), waitCfg(&scriptRunner{outs: []string{out}}, c, nil))
	if res.Outcome != tmuxdeliver.WaitTimedOut || res.Waited != 10*time.Second {
		t.Fatalf("Wait = %+v", res)
	}
}

// TestWait_RouteCaps: a line 2 asking for a 10 min deadline and a 60 s settle
// is held to the caps (90 s, 10 s); without caps (the exec path) line 2's
// values apply.
func TestWait_RouteCaps(t *testing.T) {
	notReadyLong := probeOut("900", "1000", hookRun, "strategy=hook settle_ms=60000 deadline_ms=600000", "", "")
	readyLong := probeOut("900", "1000", hookRun, "strategy=hook settle_ms=60000 deadline_ms=600000", "nonce="+nonce, "1000")

	c := &fakeClock{t: time.Unix(1000, 0)}
	cfg := waitCfg(&scriptRunner{outs: []string{notReadyLong}}, c, nil)
	cfg.MaxDeadline, cfg.MaxSettle = tmuxdeliver.MaxReadyTimeout, tmuxdeliver.MaxReadySettle
	res := tmuxdeliver.Wait(context.Background(), cfg)
	if res.Outcome != tmuxdeliver.WaitTimedOut || res.Waited != 90*time.Second {
		t.Fatalf("capped deadline: Wait = %+v, want a timeout after 90s", res)
	}

	c = &fakeClock{t: time.Unix(1000, 0)}
	cfg = waitCfg(&scriptRunner{outs: []string{readyLong}}, c, nil)
	cfg.MaxDeadline, cfg.MaxSettle = tmuxdeliver.MaxReadyTimeout, tmuxdeliver.MaxReadySettle
	res = tmuxdeliver.Wait(context.Background(), cfg)
	if res.Outcome != tmuxdeliver.WaitSettled || !reflect.DeepEqual(c.sleeps, []time.Duration{10 * time.Second}) {
		t.Fatalf("capped settle: Wait = %+v, sleeps %v, want one 10s settle", res, c.sleeps)
	}

	c = &fakeClock{t: time.Unix(1000, 0)}
	res = tmuxdeliver.Wait(context.Background(), waitCfg(&scriptRunner{outs: []string{notReadyLong}}, c, nil))
	if res.Outcome != tmuxdeliver.WaitTimedOut || res.Waited != 10*time.Minute {
		t.Fatalf("uncapped: Wait = %+v, want line 2's 10 min deadline", res)
	}
}

// TestMemoStore_NewNonceDropsOld: a new run for a key drops the key's older
// runs.
func TestMemoStore_NewNonceDropsOld(t *testing.T) {
	memo := &tmuxdeliver.MemoStore{}
	c := &fakeClock{t: time.Unix(1000, 0)}
	_ = tmuxdeliver.Wait(context.Background(), waitCfg(&scriptRunner{outs: []string{readyOld}}, c, memo))
	other := "ffffffffffffffffffffffffffffffff"
	newRun := probeOut("900", "1000", "nonce="+other+" mode="+state.ReadyModeHook, "", "nonce="+other, "990")
	_ = tmuxdeliver.Wait(context.Background(), waitCfg(&scriptRunner{outs: []string{newRun}}, c, memo))
	if memo.Len() != 1 {
		t.Fatalf("memo size = %d, want 1", memo.Len())
	}
	memo.Clear()
	if memo.Len() != 0 {
		t.Fatalf("memo size after Clear = %d", memo.Len())
	}
}
