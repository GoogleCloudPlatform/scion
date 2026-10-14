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
	"log"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
)

// The graceful shutdown of 'server start' (ptone/scion#5054), in order:
//
//  1. Mark (pre-cancel): the hub and web servers mark themselves shutting
//     down (/readyz 503; new non-broker requests 503; hub dispatches and
//     the hub's own dispatch producers stopped; streams ended); plugin
//     exits become expected.
//  2. Conduit drain (pre-cancel): GoAway to every relay session, while
//     every listener is open (design §3.3).
//  3. cancel(): the servers start their shutdown.
//  4. Broker phase: the co-located Runtime Broker shuts down (bounded by
//     the broker-phase deadline) while the hub front still serves it, and
//     the hub front waits for it.
//  5. Hub front: the listener closes and in-flight requests drain
//     (bounded by the HTTP-drain deadline).
//  6. Hub background teardown (bounded by the teardown deadline), then
//     the audit writer close and the deferred closes.
//
// With server.hub.conduit.shutdown_grace (G) set, the phases end at fixed
// shares of the teardown span S (budget.span) after the shutdown began at
// T0: broker phase at T0+S/2, HTTP drain at T0+3S/4, teardown at T0+S.
// runServerStart stops waiting for the servers at T0+S and the process
// exits by T0+G-exitWatchdogMargin in every case. Unset, there is no
// budget: each phase keeps its own bound (broker 30s, HTTP drain 30s,
// teardown per-step bounds).

// shutdownClock is the time source of the graceful shutdown's deadlines
// (T0, the phase deadlines, the exit wait and the exit watchdog); tests
// replace it with a clock.Fake.
var shutdownClock clock.Clock = clock.Real()

// exitProcess ends the process when the shutdown cannot wait any longer
// (forced, or the budget used up); a test seam.
var exitProcess = os.Exit

// exitCodeUnfinishedShutdown is the exit status of a shutdown that ended
// before the servers had stopped (forced, or the budget used up).
const exitCodeUnfinishedShutdown = 1

// Phases of the pre-cancel steps. Steps run in phase order, then in
// registration order.
const (
	// shutdownPhaseMark: the servers mark themselves shutting down.
	shutdownPhaseMark = iota
	// shutdownPhaseDrain: the conduit relay sends GoAway to every session.
	shutdownPhaseDrain
)

// serverShutdownSequence is the single graceful shutdown of 'server
// start'. The first SIGINT or SIGTERM received on the server's
// subscription (installServerShutdownSignals), or the foreground broker's
// SIGTERM handler through serverShutdown, begins it: the pre-cancel steps
// run in phase order, then the server context is cancelled. A later
// signal on the server's subscription forces it (force). The zero value
// is not usable; use newServerShutdownSequence.
type serverShutdownSequence struct {
	cancel context.CancelFunc
	clk    clock.Clock

	mu      sync.Mutex
	begun   bool
	startAt time.Time
	steps   []shutdownStep

	beganCh   chan struct{} // closed when the shutdown begins
	forceOnce sync.Once
	forcedCh  chan struct{} // closed when a later signal forces it
}

type shutdownStep struct {
	phase int
	run   func()
}

func newServerShutdownSequence(cancel context.CancelFunc, clk clock.Clock) *serverShutdownSequence {
	return &serverShutdownSequence{
		cancel:   cancel,
		clk:      clk,
		beganCh:  make(chan struct{}),
		forcedCh: make(chan struct{}),
	}
}

// addStep registers a pre-cancel step in phase. It returns false, and the
// step never runs, when the shutdown has already begun.
func (q *serverShutdownSequence) addStep(phase int, run func()) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.begun {
		return false
	}
	q.steps = append(q.steps, shutdownStep{phase: phase, run: run})
	return true
}

// claim marks the shutdown begun (T0 = now) and returns true for the
// first caller only.
func (q *serverShutdownSequence) claim() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.begun {
		return false
	}
	q.begun = true
	q.startAt = q.clk.Now()
	close(q.beganCh)
	return true
}

// run is the claimed shutdown: the log line, the steps in phase order,
// then cancel. Once the shutdown is forced the remaining steps are
// skipped.
func (q *serverShutdownSequence) run(sig os.Signal) {
	log.Printf("Received signal %v, shutting down...", sig)
	q.mu.Lock()
	steps := append([]shutdownStep(nil), q.steps...)
	q.mu.Unlock()
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].phase < steps[j].phase })
	for _, s := range steps {
		if q.isForced() {
			break
		}
		s.run()
	}
	q.cancel()
}

// begin runs the shutdown if it has not begun yet. It is the run-once the
// foreground broker's SIGTERM handler calls (serverShutdown): a direct
// call never forces, so the one SIGTERM that reaches both that handler
// and the server's subscription begins the shutdown once.
func (q *serverShutdownSequence) begin(sig os.Signal) {
	if q.claim() {
		q.run(sig)
	}
}

// force forces a begun shutdown: cancel now, skip the remaining
// pre-cancel steps and stop waiting for the servers (runServerStart then
// exits). It is idempotent.
func (q *serverShutdownSequence) force(sig os.Signal) {
	q.forceOnce.Do(func() {
		log.Printf("Received signal %v again, forcing shutdown", sig)
		close(q.forcedCh)
		q.cancel()
	})
}

func (q *serverShutdownSequence) isForced() bool {
	select {
	case <-q.forcedCh:
		return true
	default:
		return false
	}
}

// began returns a channel closed when the shutdown begins.
func (q *serverShutdownSequence) began() <-chan struct{} { return q.beganCh }

// forced returns a channel closed when a later signal forces the shutdown.
func (q *serverShutdownSequence) forced() <-chan struct{} { return q.forcedCh }

// startedAt returns T0, when the shutdown began, and whether it has.
func (q *serverShutdownSequence) startedAt() (time.Time, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.startAt, q.begun
}

// pendingSelfInterrupts counts the SIGINTs this process sent itself
// (interruptSelf, from the foreground broker's SIGTERM handler) that the
// server's signal handler has not received yet. Such a SIGINT begins the
// shutdown when it is the first signal, but never forces one.
var pendingSelfInterrupts atomic.Int32

// consumeSelfInterrupt reports whether a SIGINT just received is one this
// process sent itself, and counts it as received.
func consumeSelfInterrupt() bool {
	for {
		n := pendingSelfInterrupts.Load()
		if n <= 0 {
			return false
		}
		if pendingSelfInterrupts.CompareAndSwap(n, n-1) {
			return true
		}
	}
}

// serverShutdownBudget is the whole-process shutdown budget:
// server.hub.conduit.shutdown_grace (G), "the time the platform allows
// between SIGTERM and process kill", when set. It has no default.
type serverShutdownBudget struct {
	grace time.Duration
	set   bool
}

// serverShutdownBudgetFor returns the shutdown budget of cfg
// (validateServerPreflight has already rejected a malformed value).
func serverShutdownBudgetFor(cfg *config.GlobalConfig) serverShutdownBudget {
	grace, set := conduitShutdownDrainSetting(cfg)
	return serverShutdownBudget{grace: grace, set: set}
}

// span is the teardown span S: G less relay.ShutdownGraceMargin (the
// margin the conduit drain keeps too), but at least G/2, so a grace of
// 4s or less still leaves the phases time and the exit a margin.
func (b serverShutdownBudget) span() time.Duration {
	return max(relay.ShutdownDrainCap(b.grace), b.grace/2)
}

// brokerPhaseEnd is the deadline of the broker phase: T0 + S/2.
func (b serverShutdownBudget) brokerPhaseEnd(t0 time.Time) time.Time {
	return t0.Add(b.span() / 2)
}

// httpDrainEnd is the deadline of the hub front's HTTP drain: T0 + 3S/4.
func (b serverShutdownBudget) httpDrainEnd(t0 time.Time) time.Time {
	return t0.Add(b.span() * 3 / 4)
}

// teardownEnd is the deadline of the hub background teardown and of
// runServerStart's wait for the servers: T0 + S.
func (b serverShutdownBudget) teardownEnd(t0 time.Time) time.Time {
	return t0.Add(b.span())
}

// exitWatchdogMargin is how long before T0+G the exit watchdog ends the
// process when the closes after the wait have not finished.
const exitWatchdogMargin = 250 * time.Millisecond

// exitBy is when the exit watchdog fires: T0 + G - exitWatchdogMargin,
// but never before the teardown deadline.
func (b serverShutdownBudget) exitBy(t0 time.Time) time.Time {
	return t0.Add(max(b.grace-exitWatchdogMargin, b.span()))
}

// exitCloseBound bounds the audit writer close when a forced shutdown
// stops waiting for the servers (no budget).
const exitCloseBound = time.Second

// clockContext returns a child of parent cancelled at deadline on clk.
func clockContext(parent context.Context, clk clock.Clock, deadline time.Time) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	t := clk.AfterFunc(deadline.Sub(clk.Now()), cancel)
	return ctx, func() {
		t.Stop()
		cancel()
	}
}

// serverShutdownPlan turns the budget into the contexts and signals of the
// shutdown's phases, on the sequence's clock.
type serverShutdownPlan struct {
	q      *serverShutdownSequence
	budget serverShutdownBudget
}

// t0 is when the shutdown began, or now when the servers stop without it
// (a server error cancelled the server context).
func (p serverShutdownPlan) t0() time.Time {
	if t, ok := p.q.startedAt(); ok {
		return t
	}
	return p.q.clk.Now()
}

// phaseContext returns a child of parent ending at end(T0) with the
// budget set, else parent itself.
func (p serverShutdownPlan) phaseContext(parent context.Context, end func(time.Time) time.Time) (context.Context, context.CancelFunc) {
	if !p.budget.set {
		return parent, func() {}
	}
	return clockContext(parent, p.q.clk, end(p.t0()))
}

// teardownContext is hub.ShutdownHooks.Context: the hub servers'
// shutdown runs under it (deadline T0 + S).
func (p serverShutdownPlan) teardownContext() (context.Context, context.CancelFunc) {
	return p.phaseContext(context.Background(), p.budget.teardownEnd)
}

// httpDrainContext is hub.ShutdownHooks.HTTPDrainContext (deadline
// T0 + 3S/4, within the teardown context).
func (p serverShutdownPlan) httpDrainContext(parent context.Context) (context.Context, context.CancelFunc) {
	return p.phaseContext(parent, p.budget.httpDrainEnd)
}

// brokerContext is the co-located broker's shutdown context (deadline
// T0 + S/2): runtimebroker.Server.SetShutdownContext and
// brokerhost.Config.ShutdownContext.
func (p serverShutdownPlan) brokerContext() (context.Context, context.CancelFunc) {
	return p.phaseContext(context.Background(), p.budget.brokerPhaseEnd)
}

// exitSignal returns a channel closed when runServerStart must stop
// waiting for the servers: when the shutdown is forced, or, with the
// budget set, at the teardown deadline. Nothing is closed after done is.
func (p serverShutdownPlan) exitSignal(done <-chan struct{}) <-chan struct{} {
	out := make(chan struct{})
	go func() {
		var deadline <-chan time.Time
		began := p.q.began()
		for {
			select {
			case <-p.q.forced():
				close(out)
				return
			case <-began:
				began = nil
				if p.budget.set {
					ch, stop := clock.After(p.q.clk, p.budget.teardownEnd(p.t0()).Sub(p.q.clk.Now()))
					defer stop()
					deadline = ch
				}
			case <-deadline:
				log.Printf("Shutdown budget (server.hub.conduit.shutdown_grace=%s) used up; exiting without waiting for the rest of the teardown", p.budget.grace)
				close(out)
				return
			case <-done:
				return
			}
		}
	}()
	return out
}

// exitCloseContext bounds the audit writer close after an early exit:
// until the exit watchdog with the budget set, else exitCloseBound.
func (p serverShutdownPlan) exitCloseContext() (context.Context, context.CancelFunc) {
	if p.budget.set {
		return clockContext(context.Background(), p.q.clk, p.budget.exitBy(p.t0()))
	}
	return clockContext(context.Background(), p.q.clk, p.q.clk.Now().Add(exitCloseBound))
}

// armExitWatchdog makes sure the process exits within the grace: with the
// budget set, once the shutdown begins, it ends the process at
// T0 + G - exitWatchdogMargin if runServerStart has not returned by then
// (done), whatever close is still running. It does nothing unset.
func (p serverShutdownPlan) armExitWatchdog(done <-chan struct{}) {
	if !p.budget.set {
		return
	}
	go func() {
		select {
		case <-p.q.began():
		case <-done:
			return
		}
		fire, stop := clock.After(p.q.clk, p.budget.exitBy(p.t0()).Sub(p.q.clk.Now()))
		defer stop()
		select {
		case <-fire:
			log.Printf("Shutdown grace (server.hub.conduit.shutdown_grace=%s) nearly over; exiting now", p.budget.grace)
			exitProcess(exitCodeUnfinishedShutdown)
		case <-done:
		}
	}()
}

// brokerStopWaitFallback bounds the hub front's wait for the co-located
// Runtime Broker when the shutdown has no budget: the broker's own
// shutdown bound plus a margin.
const brokerStopWaitFallback = runtimebroker.ShutdownDeadline + 5*time.Second

// colocatedBrokerStop tracks the in-process Runtime Broker for the hub
// front's shutdown wait: whether it was started and, if so, when every
// one of its goroutines has returned.
type colocatedBrokerStop struct {
	mu      sync.Mutex
	started bool
	stopped chan struct{}
	once    sync.Once
}

func newColocatedBrokerStop() *colocatedBrokerStop {
	return &colocatedBrokerStop{stopped: make(chan struct{})}
}

// markStarted records that the broker is being started.
func (b *colocatedBrokerStop) markStarted() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.started = true
}

// markStopped records that every broker goroutine has returned.
func (b *colocatedBrokerStop) markStopped() {
	b.once.Do(func() { close(b.stopped) })
}

// beforeHTTPDrain is the hub front's hub.ShutdownHooks.BeforeHTTPDrain.
// With a broker started it waits for the broker to stop, bounded by ctx
// (the teardown context), by the broker phase (phase, which with the
// budget set ends at T0 + S/2) and by fallback on clk; without one it
// returns at once. The broker reports to the hub through the hub's
// listener while it shuts down (in-flight start cleanup, control
// channel), so the hub closes its listener only after it.
func (b *colocatedBrokerStop) beforeHTTPDrain(clk clock.Clock, fallback time.Duration, phase func() (context.Context, context.CancelFunc)) func(context.Context) {
	return func(ctx context.Context) {
		b.mu.Lock()
		started := b.started
		b.mu.Unlock()
		if !started {
			return
		}
		pctx, cancel := phase()
		defer cancel()
		fire, stop := clock.After(clk, fallback)
		defer stop()
		select {
		case <-b.stopped:
		case <-ctx.Done():
			log.Printf("Shutdown deadline reached while waiting for the co-located Runtime Broker to stop")
		case <-pctx.Done():
			log.Printf("Broker phase of the shutdown over before the co-located Runtime Broker stopped; closing the hub listener")
		case <-fire:
			log.Printf("Co-located Runtime Broker still stopping after %s; closing the hub listener", fallback)
		}
	}
}
