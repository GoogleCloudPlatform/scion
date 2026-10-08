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
)

// Active authorization of user streams (design §3.5 "Admission vs. active
// authorization").
//
// The hub node that holds a user's leaf connection owns the authorization
// of the streams opened for it. Each such stream is tracked here for its
// lifetime and re-checked against the current authorization state:
//
//   - notify: a revocation event (permission removed, agent deleted, user
//     suspended or deleted, token revoked) re-checks the affected streams
//     at once. Events travel between nodes over the event publisher
//     (Postgres LISTEN/NOTIFY in a multi-node hub);
//   - resync: whenever this node's LISTEN connection is (re)established,
//     every tracked stream is re-checked, covering events missed while it
//     was down;
//   - sweep: every conduit.authz_recheck_interval (default 60s) every
//     tracked stream is re-checked. This is the guaranteed bound; notify
//     is the fast path.
//
// A check that fails closes the stream with 4401 authz_expired (4404
// target_not_found when the agent is gone). The trigger is recorded in the
// hub log and metric only; the client sees the same code and reason for
// every trigger. A check that cannot be evaluated (store or authorization
// lookup unavailable) neither closes nor renews the stream.

// Re-check triggers (the trigger attribute of the log line and metric).
const (
	conduitAuthzTriggerNotify = "notify"
	conduitAuthzTriggerResync = "resync"
	conduitAuthzTriggerSweep  = "sweep"
)

// Re-check outcomes (the outcome attribute of the log line and metric).
const (
	// conduitAuthzOutcomePassed: the check succeeded and no deadline
	// moved.
	conduitAuthzOutcomePassed = "passed"
	// conduitAuthzOutcomeClosed: permission no longer holds; the stream
	// was closed with 4401 authz_expired.
	conduitAuthzOutcomeClosed = "closed"
	// conduitAuthzOutcomeTargetGone: the agent no longer exists; the
	// stream was closed with 4404 target_not_found.
	conduitAuthzOutcomeTargetGone = "target_gone"
	// conduitAuthzOutcomeDeferred: the check could not be evaluated; the
	// stream was left as it was.
	conduitAuthzOutcomeDeferred = "deferred_unavailable"
)

// Close reason of a stream whose authorization no longer holds (§3.3.1).
const conduitReasonAuthzExpired = "authz_expired"

const (
	// defaultConduitAuthzRecheckInterval is the default sweep period
	// (conduit.authz_recheck_interval).
	defaultConduitAuthzRecheckInterval = 60 * time.Second
	// conduitAuthzCheckTimeout bounds one stream check. A check that
	// times out counts as unavailable.
	conduitAuthzCheckTimeout = 10 * time.Second
	// conduitAuthzCheckConcurrency bounds the checks one trigger runs at
	// once.
	conduitAuthzCheckConcurrency = 8
)

// conduitAuthzVerdict is the result of evaluating one stream's
// authorization.
type conduitAuthzVerdict int

const (
	// conduitAuthzAllowed: the principal still holds the permission.
	conduitAuthzAllowed conduitAuthzVerdict = iota
	// conduitAuthzDenied: the principal no longer holds the permission
	// (or is suspended or deleted).
	conduitAuthzDenied
	// conduitAuthzTargetGone: the agent no longer exists.
	conduitAuthzTargetGone
	// conduitAuthzUnavailable: the check could not be evaluated.
	conduitAuthzUnavailable
)

// conduitUserStream is one user-originated stream whose leaf this node
// holds.
type conduitUserStream struct {
	// Kind is the grant stream kind (grant.StreamKindTCP, ...PTY).
	Kind string
	// Identity is the principal that opened the stream.
	Identity Identity
	// UserID is the principal's user id.
	UserID string
	// AgentID and ProjectID name the agent the stream reaches.
	AgentID   string
	ProjectID string
	// Port is the agent-local port of a TCP stream.
	Port int
	// SessionID and StreamID identify the stream on its conduit session
	// (for logs).
	SessionID string
	StreamID  uint32
	// Admitted is when the stream was opened.
	Admitted time.Time
	// Close ends the stream on both legs with code and reason. It may be
	// called at most once and must not block on the tracker.
	Close func(code uint32, reason string)

	// mu serializes checks of this stream; closed is set once a check
	// closed it.
	mu     sync.Mutex
	closed bool
}

// conduitAuthzMatch selects the streams a revocation event affects. An
// empty field matches every stream; the zero value matches all.
type conduitAuthzMatch struct {
	UserID    string `json:"userId,omitempty"`
	ProjectID string `json:"projectId,omitempty"`
	AgentID   string `json:"agentId,omitempty"`
}

func (m conduitAuthzMatch) matches(st *conduitUserStream) bool {
	return (m.UserID == "" || m.UserID == st.UserID) &&
		(m.ProjectID == "" || m.ProjectID == st.ProjectID) &&
		(m.AgentID == "" || m.AgentID == st.AgentID)
}

// conduitStreamAuthzMetrics records one re-check.
type conduitStreamAuthzMetrics interface {
	RecordConduitStreamAuthz(trigger, outcome, kind string)
}

// conduitStreamAuthzConfig configures a conduitStreamAuthz.
type conduitStreamAuthzConfig struct {
	// Check evaluates a stream's authorization. Required. It must read
	// state that reflects every committed revocation (the primary store,
	// never a lagging replica).
	Check func(ctx context.Context, st *conduitUserStream) (conduitAuthzVerdict, string)
	// Clock drives the sweep (nil = real time).
	Clock clock.Clock
	// RecheckInterval is the sweep period (0 = 60s; negative disables
	// the sweep, for tests).
	RecheckInterval time.Duration
	// CheckTimeout bounds one check (0 = 10s).
	CheckTimeout time.Duration
	// Metrics records each re-check (nil = none).
	Metrics conduitStreamAuthzMetrics
	// Logger receives the re-check log lines (nil = slog.Default()).
	Logger *slog.Logger
}

// conduitStreamAuthz tracks the user streams this node owns and re-checks
// their authorization.
type conduitStreamAuthz struct {
	cfg conduitStreamAuthzConfig

	mu      sync.Mutex
	streams map[*conduitUserStream]struct{}
	sweep   clock.Timer
	ctx     context.Context
	stopped bool
}

func newConduitStreamAuthz(cfg conduitStreamAuthzConfig) *conduitStreamAuthz {
	if cfg.Clock == nil {
		cfg.Clock = clock.Real()
	}
	if cfg.RecheckInterval == 0 {
		cfg.RecheckInterval = defaultConduitAuthzRecheckInterval
	}
	if cfg.CheckTimeout <= 0 {
		cfg.CheckTimeout = conduitAuthzCheckTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &conduitStreamAuthz{cfg: cfg, streams: map[*conduitUserStream]struct{}{}, ctx: context.Background()}
}

// Start arms the periodic sweep; it runs until ctx ends or Stop.
func (a *conduitStreamAuthz) Start(ctx context.Context) {
	a.mu.Lock()
	a.ctx = ctx
	a.mu.Unlock()
	a.armSweep()
	go func() {
		<-ctx.Done()
		a.Stop()
	}()
}

// Stop disarms the sweep. Tracked streams are left as they are.
func (a *conduitStreamAuthz) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopped = true
	if a.sweep != nil {
		a.sweep.Stop()
		a.sweep = nil
	}
}

func (a *conduitStreamAuthz) armSweep() {
	if a.cfg.RecheckInterval < 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped {
		return
	}
	a.sweep = a.cfg.Clock.AfterFunc(a.cfg.RecheckInterval, func() {
		a.mu.Lock()
		ctx := a.ctx
		a.mu.Unlock()
		a.Recheck(ctx, conduitAuthzTriggerSweep, conduitAuthzMatch{})
		a.armSweep()
	})
}

// Track registers st until the returned function is called (when the
// stream ends for any reason).
func (a *conduitStreamAuthz) Track(st *conduitUserStream) (untrack func()) {
	a.mu.Lock()
	a.streams[st] = struct{}{}
	a.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			delete(a.streams, st)
			a.mu.Unlock()
		})
	}
}

// Len reports the number of tracked streams.
func (a *conduitStreamAuthz) Len() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.streams)
}

// Recheck re-checks every tracked stream that m selects and returns once
// all checks have finished.
func (a *conduitStreamAuthz) Recheck(ctx context.Context, trigger string, m conduitAuthzMatch) {
	a.mu.Lock()
	var batch []*conduitUserStream
	for st := range a.streams {
		if m.matches(st) {
			batch = append(batch, st)
		}
	}
	a.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	sem := make(chan struct{}, conduitAuthzCheckConcurrency)
	var wg sync.WaitGroup
	for _, st := range batch {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			a.check(ctx, trigger, st)
		}()
	}
	wg.Wait()
}

// check evaluates one stream and acts on the verdict.
func (a *conduitStreamAuthz) check(ctx context.Context, trigger string, st *conduitUserStream) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed {
		return
	}
	start := a.cfg.Clock.Now()
	cctx, cancel := context.WithTimeout(ctx, a.cfg.CheckTimeout)
	verdict, detail := a.cfg.Check(cctx, st)
	if verdict != conduitAuthzUnavailable && cctx.Err() != nil {
		// A verdict reached after the check's deadline is not trusted.
		verdict, detail = conduitAuthzUnavailable, "check timed out"
	}
	cancel()
	end := a.cfg.Clock.Now()

	var outcome string
	switch verdict {
	case conduitAuthzAllowed:
		outcome = conduitAuthzOutcomePassed
	case conduitAuthzDenied:
		outcome = conduitAuthzOutcomeClosed
		st.closed = true
		st.Close(conduit.CloseUnauthenticated, conduitReasonAuthzExpired)
	case conduitAuthzTargetGone:
		outcome = conduitAuthzOutcomeTargetGone
		st.closed = true
		st.Close(relay.CloseTargetNotFound, relay.ReasonTargetNotFound)
	default:
		outcome = conduitAuthzOutcomeDeferred
	}
	if st.closed {
		a.mu.Lock()
		delete(a.streams, st)
		a.mu.Unlock()
	}
	if a.cfg.Metrics != nil {
		a.cfg.Metrics.RecordConduitStreamAuthz(trigger, outcome, st.Kind)
	}
	level := slog.LevelInfo
	switch {
	case outcome == conduitAuthzOutcomeDeferred:
		level = slog.LevelWarn
	case outcome == conduitAuthzOutcomePassed && trigger == conduitAuthzTriggerSweep:
		level = slog.LevelDebug
	}
	a.cfg.Logger.Log(ctx, level, "conduit_stream_authz",
		"trigger", trigger,
		"outcome", outcome,
		"kind", st.Kind,
		"stream_id", st.StreamID,
		"session_id", st.SessionID,
		"agent_id", st.AgentID,
		"project_id", st.ProjectID,
		"principal_kind", string(principalContextForIdentity(st.Identity).Kind),
		"principal_id", st.UserID,
		"check_start", start,
		"check_end", end,
		"deadline_before", "",
		"deadline_after", "",
		"detail", detail,
	)
}

// OTelConduitStreamAuthzMetrics counts stream re-checks as
// scion.hub.conduit.stream_authz (attributes trigger, outcome, kind).
type OTelConduitStreamAuthzMetrics struct {
	total metric.Int64Counter
}

// NewOTelConduitStreamAuthzMetrics creates the counter.
func NewOTelConduitStreamAuthzMetrics(mp metric.MeterProvider) (*OTelConduitStreamAuthzMetrics, error) {
	if mp == nil {
		return nil, errors.New("otel conduit stream authz metrics: nil MeterProvider")
	}
	c, err := mp.Meter(instrumentationScope).Int64Counter("scion.hub.conduit.stream_authz",
		metric.WithUnit("{check}"))
	if err != nil {
		return nil, err
	}
	return &OTelConduitStreamAuthzMetrics{total: c}, nil
}

// RecordConduitStreamAuthz implements conduitStreamAuthzMetrics.
func (m *OTelConduitStreamAuthzMetrics) RecordConduitStreamAuthz(trigger, outcome, kind string) {
	m.total.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("trigger", trigger),
		attribute.String("outcome", outcome),
		attribute.String("kind", kind),
	))
}
