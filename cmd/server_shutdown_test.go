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

package cmd

import (
	"context"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The shutdown tests drive every deadline with a clock.Fake (R7); real
// time only bounds how long a test waits for a goroutine to react.
const shutdownTestWait = 5 * time.Second

var shutdownTestT0 = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// newFakeShutdown returns a sequence on a fake clock and its plan for the
// given grace (set when grace > 0).
func newFakeShutdown(t *testing.T, grace time.Duration, cancel context.CancelFunc) (*clock.Fake, *serverShutdownSequence, serverShutdownPlan) {
	t.Helper()
	captureStdLog(t)
	clk := clock.NewFake(shutdownTestT0)
	q := newServerShutdownSequence(cancel, clk)
	return clk, q, serverShutdownPlan{q: q, budget: serverShutdownBudget{grace: grace, set: grace > 0}}
}

// stubExitProcess replaces exitProcess with a recorder of the fake clock
// time of each call.
func stubExitProcess(t *testing.T, clk *clock.Fake) <-chan time.Time {
	t.Helper()
	calls := make(chan time.Time, 4)
	prev := exitProcess
	exitProcess = func(code int) {
		assert.Equal(t, exitCodeUnfinishedShutdown, code)
		calls <- clk.Now()
	}
	t.Cleanup(func() { exitProcess = prev })
	return calls
}

func isDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// TestServerShutdownBudget_Phases: the budget comes from shutdown_grace
// only (no default), and its phases end at S/2, 3S/4 and S after T0, with
// S = G - 2s but at least G/2 (C3), and the exit watchdog at G - 250ms
// (never before S).
func TestServerShutdownBudget_Phases(t *testing.T) {
	cfg := &config.GlobalConfig{}
	assert.False(t, serverShutdownBudgetFor(cfg).set, "unset shutdown_grace gives no budget")
	cfg.Hub.Conduit.ShutdownGrace = "10s"
	assert.Equal(t, serverShutdownBudget{grace: 10 * time.Second, set: true}, serverShutdownBudgetFor(cfg))

	for _, tc := range []struct {
		grace                       time.Duration
		span, broker, drain, exitAt time.Duration
	}{
		{10 * time.Second, 8 * time.Second, 4 * time.Second, 6 * time.Second, 9750 * time.Millisecond},
		{20 * time.Second, 18 * time.Second, 9 * time.Second, 13500 * time.Millisecond, 19750 * time.Millisecond},
		{3 * time.Second, 1500 * time.Millisecond, 750 * time.Millisecond, 1125 * time.Millisecond, 2750 * time.Millisecond},
		{time.Second, 500 * time.Millisecond, 250 * time.Millisecond, 375 * time.Millisecond, 750 * time.Millisecond},
		{200 * time.Millisecond, 100 * time.Millisecond, 50 * time.Millisecond, 75 * time.Millisecond, 100 * time.Millisecond},
	} {
		b := serverShutdownBudget{grace: tc.grace, set: true}
		assert.Equal(t, tc.span, b.span(), "span for %s", tc.grace)
		assert.Equal(t, shutdownTestT0.Add(tc.broker), b.brokerPhaseEnd(shutdownTestT0), "broker phase for %s", tc.grace)
		assert.Equal(t, shutdownTestT0.Add(tc.drain), b.httpDrainEnd(shutdownTestT0), "HTTP drain for %s", tc.grace)
		assert.Equal(t, shutdownTestT0.Add(tc.span), b.teardownEnd(shutdownTestT0), "teardown for %s", tc.grace)
		assert.Equal(t, shutdownTestT0.Add(tc.exitAt), b.exitBy(shutdownTestT0), "exit watchdog for %s", tc.grace)
		assert.True(t, tc.exitAt <= tc.grace, "the process exits within the grace (%s)", tc.grace)
	}
}

// TestServerShutdownPlan_PhaseContexts: with the budget set, the phase
// contexts end at their deadlines measured from T0 (when the shutdown
// began, not when the context was asked for); unset, they never end on
// their own.
func TestServerShutdownPlan_PhaseContexts(t *testing.T) {
	t.Run("budget set", func(t *testing.T) {
		clk, q, p := newFakeShutdown(t, 10*time.Second, func() {})
		q.begin(syscall.SIGTERM) // T0
		clk.Advance(time.Second) // the servers ask for their contexts later

		broker, cancelBroker := p.brokerContext()
		defer cancelBroker()
		teardown, cancelTeardown := p.teardownContext()
		defer cancelTeardown()
		drain, cancelDrain := p.httpDrainContext(teardown)
		defer cancelDrain()

		clk.Advance(2999 * time.Millisecond) // T0 + 3.999s
		assert.False(t, isDone(broker), "broker phase ended early")
		clk.Advance(time.Millisecond) // T0 + 4s = S/2
		assert.True(t, isDone(broker), "broker phase did not end at T0+S/2")
		assert.False(t, isDone(drain))
		clk.Advance(2 * time.Second) // T0 + 6s = 3S/4
		assert.True(t, isDone(drain), "HTTP drain did not end at T0+3S/4")
		assert.False(t, isDone(teardown))
		clk.Advance(2 * time.Second) // T0 + 8s = S
		assert.True(t, isDone(teardown), "teardown did not end at T0+S")
	})
	t.Run("no shutdown begun: T0 is now", func(t *testing.T) {
		clk, _, p := newFakeShutdown(t, 10*time.Second, func() {})
		clk.Advance(time.Minute)
		teardown, cancel := p.teardownContext()
		defer cancel()
		clk.Advance(7999 * time.Millisecond)
		assert.False(t, isDone(teardown))
		clk.Advance(time.Millisecond)
		assert.True(t, isDone(teardown))
	})
	t.Run("unset", func(t *testing.T) {
		clk, q, p := newFakeShutdown(t, 0, func() {})
		q.begin(syscall.SIGTERM)
		broker, c1 := p.brokerContext()
		defer c1()
		teardown, c2 := p.teardownContext()
		defer c2()
		drain, c3 := p.httpDrainContext(teardown)
		defer c3()
		clk.Advance(time.Hour)
		assert.False(t, isDone(broker))
		assert.False(t, isDone(teardown))
		assert.False(t, isDone(drain))
		assert.Zero(t, clk.Pending(), "no timer armed without a budget")
	})
}

// TestServerShutdownPlan_ExitSignal: runServerStart stops waiting for the
// servers at T0+S with the budget set, on a force in any case, and never
// on its own without a budget.
func TestServerShutdownPlan_ExitSignal(t *testing.T) {
	t.Run("teardown deadline", func(t *testing.T) {
		clk, q, p := newFakeShutdown(t, 10*time.Second, func() {})
		done := make(chan struct{})
		defer close(done)
		exit := p.exitSignal(done)
		q.begin(syscall.SIGTERM)
		require.True(t, clk.WaitForTimer(shutdownTestWait, shutdownTestT0.Add(8*time.Second)))
		clk.Advance(7999 * time.Millisecond)
		select {
		case <-exit:
			t.Fatal("exit signalled before T0+S")
		default:
		}
		clk.Advance(time.Millisecond)
		select {
		case <-exit:
		case <-time.After(shutdownTestWait):
			t.Fatal("no exit signal at T0+S")
		}
	})
	t.Run("forced", func(t *testing.T) {
		_, q, p := newFakeShutdown(t, 0, func() {})
		done := make(chan struct{})
		defer close(done)
		exit := p.exitSignal(done)
		q.begin(syscall.SIGTERM)
		q.force(syscall.SIGINT)
		select {
		case <-exit:
		case <-time.After(shutdownTestWait):
			t.Fatal("no exit signal on force")
		}
	})
	t.Run("unset never fires", func(t *testing.T) {
		clk, q, p := newFakeShutdown(t, 0, func() {})
		done := make(chan struct{})
		exit := p.exitSignal(done)
		q.begin(syscall.SIGTERM)
		clk.Advance(time.Hour)
		close(done)
		select {
		case <-exit:
			t.Fatal("exit signalled without a budget or a force")
		case <-time.After(50 * time.Millisecond):
		}
	})
}

// TestShutdownExitSequence_WithinGrace (R2, ptone/scion#5105 timing): with
// shutdown_grace = 10s, a server wait that never returns and a log flush
// that blocks, the whole exit sequence ends the process (exitProcess) by
// T0 + G, without returning into the deferred store close: the wait stops
// at T0+S, the audit writer closes, then the logs are flushed (code review
// Q1), and the flush is cut at T0+G-250ms (the watchdog backs it:
// TestShutdownExitWatchdog).
func TestShutdownExitSequence_WithinGrace(t *testing.T) {
	grace := 10 * time.Second
	serverCtx, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()
	clk, q, p := newFakeShutdown(t, grace, serverCancel)
	exits := stubExitProcess(t, clk)
	lifetime, stop := context.WithCancel(context.Background())
	defer stop()
	// The watchdog, which fires at the same instant, is pinned by
	// TestShutdownExitWatchdog; it is not armed here, so exactly one exit
	// (awaitServerExit's) is observed.

	var auditClosed atomic.Bool
	flushEntered, releaseFlush := make(chan struct{}), make(chan struct{})
	defer close(releaseFlush)
	returned := make(chan error, 1)
	go func() {
		returned <- awaitServerExit(serverCtx, make(chan error), serverCancel,
			func() { select {} }, // the servers never finish
			func(ctx context.Context) error {
				auditClosed.Store(true)
				return nil
			},
			p.exitSignal(lifetime.Done()), p.exitCloseContext,
			func() { // a log flush that blocks
				assert.True(t, auditClosed.Load(), "the audit writer closes before the log flush")
				close(flushEntered)
				<-releaseFlush
			})
	}()

	q.begin(syscall.SIGTERM) // T0; cancels the server context
	require.True(t, clk.WaitForTimer(shutdownTestWait, shutdownTestT0.Add(8*time.Second)), "exit wait not armed")
	clk.Advance(8 * time.Second) // T0 + S: stop waiting
	select {
	case <-flushEntered:
	case <-time.After(shutdownTestWait):
		t.Fatal("the log flush did not start at T0+S")
	}
	require.True(t, clk.WaitForTimer(shutdownTestWait, shutdownTestT0.Add(9750*time.Millisecond)), "close bound not armed")
	clk.Advance(1750 * time.Millisecond) // T0 + G - 250ms

	select {
	case at := <-exits:
		assert.False(t, at.After(shutdownTestT0.Add(grace)), "process exit at %s, after T0+G", at.Sub(shutdownTestT0))
		assert.Equal(t, shutdownTestT0.Add(9750*time.Millisecond), at)
	case <-time.After(shutdownTestWait):
		t.Fatal("the process did not exit")
	}
	select {
	case <-returned:
	case <-time.After(shutdownTestWait):
		t.Fatal("awaitServerExit did not return after the (stubbed) exit")
	}
}

// TestShutdownExitWatchdog: with the budget set, the watchdog ends the
// process at T0 + G - 250ms when runServerStart has not returned (a close
// after the wait still running); it does not fire once runServerStart
// returned, and is not armed without a budget.
func TestShutdownExitWatchdog(t *testing.T) {
	t.Run("fires", func(t *testing.T) {
		clk, q, p := newFakeShutdown(t, 10*time.Second, func() {})
		exits := stubExitProcess(t, clk)
		done := make(chan struct{})
		defer close(done)
		p.armExitWatchdog(done)
		q.begin(syscall.SIGTERM)
		require.True(t, clk.WaitForTimer(shutdownTestWait, shutdownTestT0.Add(9750*time.Millisecond)), "watchdog timer not armed at T0+G-250ms")
		clk.Advance(9749 * time.Millisecond)
		select {
		case <-exits:
			t.Fatal("watchdog fired early")
		default:
		}
		clk.Advance(time.Millisecond)
		select {
		case at := <-exits:
			assert.Equal(t, shutdownTestT0.Add(9750*time.Millisecond), at)
		case <-time.After(shutdownTestWait):
			t.Fatal("watchdog did not fire")
		}
	})
	t.Run("returned first", func(t *testing.T) {
		clk, q, p := newFakeShutdown(t, 10*time.Second, func() {})
		exits := stubExitProcess(t, clk)
		done := make(chan struct{})
		p.armExitWatchdog(done)
		q.begin(syscall.SIGTERM)
		require.True(t, clk.WaitForTimer(shutdownTestWait, shutdownTestT0.Add(9750*time.Millisecond)))
		close(done)
		require.True(t, clk.WaitFor(shutdownTestWait, func(n int) bool { return n == 0 }), "watchdog timer not released")
		clk.Advance(time.Minute)
		select {
		case <-exits:
			t.Fatal("watchdog fired after runServerStart returned")
		case <-time.After(50 * time.Millisecond):
		}
	})
	t.Run("unset", func(t *testing.T) {
		clk, q, p := newFakeShutdown(t, 0, func() {})
		exits := stubExitProcess(t, clk)
		done := make(chan struct{})
		defer close(done)
		p.armExitWatchdog(done)
		q.begin(syscall.SIGTERM)
		clk.Advance(time.Hour)
		select {
		case <-exits:
			t.Fatal("watchdog armed without a budget")
		case <-time.After(50 * time.Millisecond):
		}
	})
}

// TestAwaitServerExit_ForcedExitsWithoutWaiting (R2/R6): a forced
// shutdown stops waiting for the servers, closes the audit writer within
// exitCloseBound and ends the process; nothing returns into the deferred
// closes while the servers still run.
func TestAwaitServerExit_ForcedExitsWithoutWaiting(t *testing.T) {
	serverCtx, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()
	clk, q, p := newFakeShutdown(t, 0, serverCancel)
	exits := stubExitProcess(t, clk)
	lifetime, stop := context.WithCancel(context.Background())
	defer stop()

	closeCtxEnded := make(chan struct{})
	returned := make(chan error, 1)
	go func() {
		returned <- awaitServerExit(serverCtx, make(chan error), serverCancel,
			func() { select {} },
			func(ctx context.Context) error {
				<-ctx.Done()
				close(closeCtxEnded)
				return nil
			},
			p.exitSignal(lifetime.Done()), p.exitCloseContext, nil)
	}()
	q.begin(syscall.SIGTERM)
	q.force(syscall.SIGINT)
	require.True(t, clk.WaitFor(shutdownTestWait, func(n int) bool { return n == 1 }), "close bound not armed")
	clk.Advance(exitCloseBound)
	select {
	case at := <-exits:
		assert.Equal(t, shutdownTestT0.Add(exitCloseBound), at, "the forced exit waits for the close only up to its bound")
	case <-time.After(shutdownTestWait):
		t.Fatal("the forced shutdown did not exit")
	}
	<-returned
	select {
	case <-closeCtxEnded:
	case <-time.After(shutdownTestWait):
		t.Fatal("the audit close's context did not end at the bound")
	}
}

// TestColocatedBrokerStop_BeforeHTTPDrain (R1, R3): the hub front waits
// for the co-located broker before it closes its listener, but no longer
// than the broker phase (T0+S/2 with shutdown_grace 10s), so the HTTP
// drain and the teardown keep their time even when a broker start never
// finishes; without a broker it does not wait at all.
func TestColocatedBrokerStop_BeforeHTTPDrain(t *testing.T) {
	t.Run("no broker", func(t *testing.T) {
		clk, q, p := newFakeShutdown(t, 10*time.Second, func() {})
		q.begin(syscall.SIGTERM)
		b := newColocatedBrokerStop()
		b.beforeHTTPDrain(clk, brokerStopWaitFallback, p.brokerContext)(context.Background())
	})
	t.Run("waits for the broker", func(t *testing.T) {
		clk, q, p := newFakeShutdown(t, 10*time.Second, func() {})
		q.begin(syscall.SIGTERM)
		b := newColocatedBrokerStop()
		b.markStarted()
		returned := make(chan struct{})
		go func() {
			b.beforeHTTPDrain(clk, brokerStopWaitFallback, p.brokerContext)(context.Background())
			close(returned)
		}()
		select {
		case <-returned:
			t.Fatal("returned before the broker stopped")
		case <-time.After(50 * time.Millisecond):
		}
		b.markStopped()
		select {
		case <-returned:
		case <-time.After(shutdownTestWait):
			t.Fatal("did not return once the broker stopped")
		}
	})
	t.Run("broker never stops: bounded by the broker phase", func(t *testing.T) {
		clk, q, p := newFakeShutdown(t, 10*time.Second, func() {})
		q.begin(syscall.SIGTERM)
		b := newColocatedBrokerStop()
		b.markStarted()
		returned := make(chan time.Time, 1)
		go func() {
			b.beforeHTTPDrain(clk, brokerStopWaitFallback, p.brokerContext)(context.Background())
			returned <- clk.Now()
		}()
		require.True(t, clk.WaitForTimer(shutdownTestWait, shutdownTestT0.Add(4*time.Second)))
		clk.Advance(4 * time.Second)
		select {
		case at := <-returned:
			assert.Equal(t, shutdownTestT0.Add(4*time.Second), at)
			assert.True(t, at.Before(p.budget.httpDrainEnd(shutdownTestT0)), "the HTTP drain keeps time")
		case <-time.After(shutdownTestWait):
			t.Fatal("the wait outlived the broker phase")
		}
	})
	t.Run("no budget: bounded by the fallback", func(t *testing.T) {
		clk, q, p := newFakeShutdown(t, 0, func() {})
		q.begin(syscall.SIGTERM)
		b := newColocatedBrokerStop()
		b.markStarted()
		returned := make(chan time.Time, 1)
		go func() {
			b.beforeHTTPDrain(clk, brokerStopWaitFallback, p.brokerContext)(context.Background())
			returned <- clk.Now()
		}()
		require.True(t, clk.WaitForTimer(shutdownTestWait, shutdownTestT0.Add(brokerStopWaitFallback)))
		clk.Advance(brokerStopWaitFallback)
		select {
		case at := <-returned:
			assert.Equal(t, shutdownTestT0.Add(brokerStopWaitFallback), at)
		case <-time.After(shutdownTestWait):
			t.Fatal("the wait outlived the fallback")
		}
	})
}

// TestOnceCleanups: the log cleanups run once, last first, whether the
// early exit or the deferred call runs them first (code review Q1).
func TestOnceCleanups(t *testing.T) {
	var order []int
	flush := onceCleanups([]func(){func() { order = append(order, 1) }, func() { order = append(order, 2) }})
	flush()
	flush()
	assert.Equal(t, []int{2, 1}, order, "cleanups run once, in defer order")
}
