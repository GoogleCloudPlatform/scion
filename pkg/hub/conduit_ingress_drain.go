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

package hub

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Drain through the proxy, hub side (design §3.9 "Drain through the
// proxy", v2.21.58; contracts "Addition (6.2 (a3))"). In external relay
// mode the hub's one graceful shutdown stops proxying conduit dials before
// it marks any server (BeginConduitIngressShutdown: 503
// conduit_not_serving, cause ingress_terminating, before any byte to a
// relay). Its pre-cancel drain step then asks every live relay to send
// GoAway to the sessions this hub proxied, holds the hub's open proxied
// legs until they close, and at the hold deadline (computed by the
// shutdown) closes the legs left. The legs normally close at about the
// drain deadline sent, when the relays end the drained sessions; the hold
// deadline, at least 1s later, is the backstop. HTTP server shutdown does
// not close hijacked legs, so without the close they would end only at
// process exit. Every wait is bounded by the hold deadline; a failed call
// is logged and counted and never extends the step.

// conduitIngressCauseTerminating is the cause logged for conduit dials
// refused while the hub shuts down.
const conduitIngressCauseTerminating = "ingress_terminating"

// conduitIngressSendMargin is how much earlier than the hold deadline the
// drain deadline sent to the relays ends, so the sessions they drain have
// closed their legs before the hub closes what is left.
const conduitIngressSendMargin = time.Second

// conduitIngressMinDrainDeadline is the shortest drain deadline sent
// (design §3.3's 1ms floor).
const conduitIngressMinDrainDeadline = time.Millisecond

// conduitIngressDrainDeadline is the drain deadline sent to the relays at
// now: max(1ms, min(§3.3's min(drain deadline, shutdown_grace - 2s)
// measured from the signal, holdDeadline - now - 1s)). With
// shutdown_grace unset the first term is the session drain deadline (30s),
// so the value is in practice holdDeadline - now - 1s; with 1s or less
// left it is the 1ms floor. It is always sent, never left to the
// relay's default, so no relay promises a client more time than the hub
// keeps the legs open.
func conduitIngressDrainDeadline(now, signalAt, holdDeadline time.Time, grace time.Duration, graceSet bool) time.Duration {
	// Explicit comparisons, not the min and max builtins: a pkg/hub test
	// helper declares its own min(int, int), which shadows the builtin in
	// the package's test build.
	fromSignal := conduit.DefaultDrainDeadline
	if graceSet {
		if c := relay.ShutdownDrainCap(grace); c < fromSignal {
			fromSignal = c
		}
	}
	d := signalAt.Add(fromSignal).Sub(now)
	if h := holdDeadline.Sub(now) - conduitIngressSendMargin; h < d {
		d = h
	}
	if d < conduitIngressMinDrainDeadline {
		d = conduitIngressMinDrainDeadline
	}
	return d
}

// ConduitIngressDrainOptions are the inputs of the ingress drain step,
// all on the shutdown's clock.
type ConduitIngressDrainOptions struct {
	// Clock is the shutdown's clock (nil: the real clock).
	Clock clock.Clock
	// SignalAt is when the shutdown began (the signal).
	SignalAt time.Time
	// HoldDeadline is the drain's absolute deadline, computed by the
	// shutdown (design v2.21.58: min(step deadline, HTTP-drain deadline -
	// 1s), or the step deadline). The live-relay read, the fan-out and the
	// hold end by it; the legs still open then are closed and the step
	// returns.
	HoldDeadline time.Time
	// Grace is server.hub.conduit.shutdown_grace, and GraceSet whether it
	// is set.
	Grace    time.Duration
	GraceSet bool
}

// ConduitIngressProxyActive reports whether this hub proxies conduit dials
// to relays: external relay mode with proxy ingress serving. Only then
// does the shutdown need the ingress drain step; with the proxy off no leg
// or relay session came through this hub.
func (s *Server) ConduitIngressProxyActive() bool {
	rt := s.conduitExternal.Load()
	return rt != nil && rt.ingress == config.ConduitIngressProxy && rt.ingressOffCause == ""
}

// BeginConduitIngressShutdown stops proxying conduit dials, once. The
// shutdown runs it as a step before it marks any server (cmd
// shutdownPhaseIngressStop), so no dial is proxied after the hub's mark;
// the drain step runs it too, in case it did not run. It does nothing
// outside external relay mode.
func (s *Server) BeginConduitIngressShutdown() {
	rt := s.conduitExternal.Load()
	if rt == nil || !rt.terminating.CompareAndSwap(false, true) {
		return
	}
	slog.Info("Conduit ingress: shutting down; new conduit dials are not proxied", "cause", conduitIngressCauseTerminating, "ingress_instance", rt.instanceID)
}

// DrainConduitIngressForShutdown is the ingress drain step of the
// shutdown: it fans out drain to every live relay (draining ones included)
// with the drain deadline of conduitIngressDrainDeadline, then holds until
// the hub's last open proxied leg closes, and at o.HoldDeadline closes the
// legs left and returns. The live-relay read, the fan-out and the hold all
// end by o.HoldDeadline; if it has already passed, the step closes the
// legs and returns without a fan-out. Proxying has normally stopped before
// the mark phase; if not, it stops here first. It runs once; later calls,
// calls outside external mode and calls with the proxy off return at once.
func (s *Server) DrainConduitIngressForShutdown(o ConduitIngressDrainOptions) {
	rt := s.conduitExternal.Load()
	if rt == nil || !rt.drainStarted.CompareAndSwap(false, true) {
		return
	}
	s.BeginConduitIngressShutdown()
	if !s.ConduitIngressProxyActive() {
		// The ingress proxy was never on, so this hub proxied no session:
		// there is nothing to drain.
		slog.Info("Conduit ingress: the ingress proxy was off, so no relay is asked to drain", "cause", rt.ingressOffCause)
		return
	}
	clk := o.Clock
	if clk == nil {
		clk = clock.Real()
	}
	now := clk.Now()
	if !o.HoldDeadline.After(now) {
		// No time left to hold: close the legs now, without a fan-out.
		n := rt.legs.closeAll()
		slog.Warn("Conduit ingress: the drain hold deadline had passed; closed the proxied legs without a drain", "legs", n, "hold_deadline", o.HoldDeadline)
		return
	}
	ctx, cancel := conduitIngressStepContext(clk, o.HoldDeadline)
	defer cancel()
	deadline := conduitIngressDrainDeadline(now, o.SignalAt, o.HoldDeadline, o.Grace, o.GraceSet)
	slog.Info("Conduit ingress: draining the sessions this hub proxied",
		"ingress_instance", rt.instanceID, "hold_deadline", o.HoldDeadline, "drain_deadline", deadline)
	s.fanOutConduitIngressDrain(ctx, rt, deadline)

	select {
	case <-rt.legs.idle():
		slog.Info("Conduit ingress: every proxied leg closed")
	case <-ctx.Done():
		if n := rt.legs.closeAll(); n > 0 {
			slog.Warn("Conduit ingress: closed the proxied legs left at the end of the drain hold", "legs", n)
		}
	}
}

// conduitIngressStepContext returns a context cancelled at deadline on clk.
func conduitIngressStepContext(clk clock.Clock, deadline time.Time) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	d := deadline.Sub(clk.Now())
	if d <= 0 {
		cancel()
		return ctx, cancel
	}
	t := clk.AfterFunc(d, cancel)
	return ctx, func() {
		t.Stop()
		cancel()
	}
}

// fanOutConduitIngressDrain calls drain on every live relay as this hub's
// instance and returns when every call has answered or failed, or when ctx
// ends, whichever is first.
func (s *Server) fanOutConduitIngressDrain(ctx context.Context, rt *conduitExternalRuntime, deadline time.Duration) {
	relays, err := rt.registry.LiveRelays(ctx, rt.now())
	if err != nil {
		slog.Warn("Conduit ingress: listing live relays for the drain failed; the proxied legs are still held and closed at the hold deadline", "error", err)
		return
	}
	var wg sync.WaitGroup
	for _, ri := range relays {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := rt.peers.Drain(ctx, ri.InternalEndpoint, ri.InstanceID, ri.Generation, rt.instanceID, deadline)
			if err != nil {
				cause, status := relay.DrainFailedUnreachable, 0
				var de *relay.DrainError
				if errors.As(err, &de) {
					cause, status = de.Cause, de.Status
				}
				s.recordConduitIngressDrainFailed(cause)
				slog.Warn("Conduit ingress: drain call failed", "relay_instance_id", ri.InstanceID, "cause", cause, "status", status, "error", err)
				return
			}
			slog.Info("Conduit ingress: relay drained this hub's sessions", "relay_instance_id", ri.InstanceID, "goaway_sent", n)
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// conduitIngressLegs counts the hub's open proxied legs (a conduit dial
// being proxied, upgraded session included) and can close them. The zero
// value is ready to use.
type conduitIngressLegs struct {
	mu   sync.Mutex
	next uint64
	open map[uint64]context.CancelFunc
	// idleCh is closed when the last open leg ends; nil while nobody waits.
	idleCh chan struct{}
}

// add counts a leg; closeLeg closes it. It returns the id done takes.
func (l *conduitIngressLegs) add(closeLeg context.CancelFunc) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open == nil {
		l.open = map[uint64]context.CancelFunc{}
	}
	l.next++
	l.open[l.next] = closeLeg
	return l.next
}

// done ends the count of leg id.
func (l *conduitIngressLegs) done(id uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.open, id)
	if len(l.open) == 0 && l.idleCh != nil {
		close(l.idleCh)
		l.idleCh = nil
	}
}

// count returns the number of open legs.
func (l *conduitIngressLegs) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.open)
}

// idle returns a channel closed once no leg is open (already closed when
// none is).
func (l *conduitIngressLegs) idle() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.open) == 0 {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	if l.idleCh == nil {
		l.idleCh = make(chan struct{})
	}
	return l.idleCh
}

// closeAll closes every open leg and returns how many there were. Each
// leg's dial returns and ends its own count.
func (l *conduitIngressLegs) closeAll() int {
	l.mu.Lock()
	closers := make([]context.CancelFunc, 0, len(l.open))
	for _, c := range l.open {
		closers = append(closers, c)
	}
	l.mu.Unlock()
	for _, c := range closers {
		c()
	}
	return len(closers)
}

// OTelConduitIngressDrainMetrics counts failed drain calls of a
// terminating hub ingress as scion.hub.conduit.ingress_drain_failed
// (attribute cause: unreachable, timeout, refused or bad_response).
type OTelConduitIngressDrainMetrics struct {
	failed metric.Int64Counter
}

// NewOTelConduitIngressDrainMetrics creates the counter.
func NewOTelConduitIngressDrainMetrics(mp metric.MeterProvider) (*OTelConduitIngressDrainMetrics, error) {
	if mp == nil {
		return nil, errors.New("otel conduit ingress drain metrics: nil MeterProvider")
	}
	c, err := mp.Meter(instrumentationScope).Int64Counter("scion.hub.conduit.ingress_drain_failed",
		metric.WithUnit("{call}"))
	if err != nil {
		return nil, err
	}
	return &OTelConduitIngressDrainMetrics{failed: c}, nil
}

// SetConduitIngressDrainMetrics wires the
// scion.hub.conduit.ingress_drain_failed counter. Until it is set,
// failures are only logged.
func (s *Server) SetConduitIngressDrainMetrics(m *OTelConduitIngressDrainMetrics) {
	s.conduitIngressDrainMetrics.Store(m)
}

func (s *Server) recordConduitIngressDrainFailed(cause string) {
	if m := s.conduitIngressDrainMetrics.Load(); m != nil {
		m.failed.Add(context.Background(), 1, metric.WithAttributes(attribute.String("cause", cause)))
	}
}
