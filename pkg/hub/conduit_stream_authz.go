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
//     is the fast path;
//   - interval: each stream carries an authorization deadline, its
//     admission time plus conduit.stream_authz_max (user streams: default
//     8h). At the deadline the stream is re-checked: if the permission
//     still holds, the deadline moves forward by one interval and the
//     stream's target is sent a hub-originated AuthRefresh{stream_id}
//     renewal notice.
//
// A check that fails closes the stream with 4401 authz_expired (4404
// target_not_found when the agent is gone). The trigger is recorded in the
// hub log and metric only; the client sees the same code and reason for
// every trigger. A check that cannot be evaluated (store or authorization
// lookup unavailable) neither closes nor renews the stream, so the stream
// runs to its deadline; an interval check that cannot be evaluated closes
// it there with 4401 authz_expired. Only an interval check moves a
// deadline: notify, resync and sweep checks that pass leave it as it is.
//
// In Phases 2-5 this node, the one holding the leaf, is the only
// enforcement point of the deadline (design §3.5). Targets hold no
// deadline; the renewal notice is advisory.

// Re-check triggers (the trigger attribute of the log line and metric).
const (
	conduitAuthzTriggerNotify = "notify"
	conduitAuthzTriggerResync = "resync"
	conduitAuthzTriggerSweep  = "sweep"
	// conduitAuthzTriggerInterval: the stream reached its authorization
	// deadline.
	conduitAuthzTriggerInterval = "interval"
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
	// conduitAuthzOutcomeRenewed: an interval check succeeded; the
	// deadline moved forward by one interval.
	conduitAuthzOutcomeRenewed = "renewed"
	// conduitAuthzOutcomeExpired: an interval check could not be
	// evaluated; the stream reached its deadline unrenewed and was closed
	// with 4401 authz_expired.
	conduitAuthzOutcomeExpired = "expired_unavailable"
)

// Delivery of the renewal notice (the notice attribute of a renewed
// check's log line).
const (
	// conduitAuthzNoticeLocal: the notice was sent on the stream's
	// conduit session, held by this node.
	conduitAuthzNoticeLocal = "local"
	// conduitAuthzNoticeNotDelivered: no notice was sent (the target's
	// session is held by another node, or ended). The deadline moved all
	// the same: it is enforced here, not by the target.
	conduitAuthzNoticeNotDelivered = "not_delivered"
)

// Close reason of a stream whose authorization no longer holds (§3.3.1).
const conduitReasonAuthzExpired = "authz_expired"

const (
	// defaultConduitAuthzRecheckInterval is the default sweep period
	// (conduit.authz_recheck_interval).
	defaultConduitAuthzRecheckInterval = 60 * time.Second
	// defaultConduitUserStreamAuthzMax is the default authorization
	// interval of user streams (conduit.stream_authz_max.user).
	defaultConduitUserStreamAuthzMax = 8 * time.Hour
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
	// Renew sends the hub-originated AuthRefresh{stream_id} renewal
	// notice on the stream's conduit session (nil: the session is not
	// held by this node, and no notice is sent). It must not block on the
	// tracker.
	Renew func() error

	// mu serializes checks of this stream; closed is set once a check
	// closed it.
	mu     sync.Mutex
	closed bool

	// dmu guards the authorization deadline and its timer. It is never
	// held while calling out, so untracking (which may run inside Close)
	// does not wait on a check.
	dmu       sync.Mutex
	interval  time.Duration // 0: no deadline
	deadline  time.Time
	timer     clock.Timer
	untracked bool
}

// Deadline returns the stream's authorization deadline (zero when it has
// none).
func (st *conduitUserStream) Deadline() time.Time {
	st.dmu.Lock()
	defer st.dmu.Unlock()
	return st.deadline
}

// stopDeadline disarms the deadline timer for good.
func (st *conduitUserStream) stopDeadline() {
	st.dmu.Lock()
	defer st.dmu.Unlock()
	st.untracked = true
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
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
	// UserStreamAuthzMax is the authorization interval of user streams
	// (0 = 8h; negative disables the deadline, for tests).
	UserStreamAuthzMax time.Duration
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

	mu       sync.Mutex
	streams  map[*conduitUserStream]struct{}
	sweep    clock.Timer
	sweeping bool // a sweep is running
	ctx      context.Context
	stopped  bool
}

func newConduitStreamAuthz(cfg conduitStreamAuthzConfig) *conduitStreamAuthz {
	if cfg.Clock == nil {
		cfg.Clock = clock.Real()
	}
	if cfg.RecheckInterval == 0 {
		cfg.RecheckInterval = defaultConduitAuthzRecheckInterval
	}
	if cfg.UserStreamAuthzMax == 0 {
		cfg.UserStreamAuthzMax = defaultConduitUserStreamAuthzMax
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

// runContext is the context Start was given (Background before Start).
func (a *conduitStreamAuthz) runContext() context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ctx
}

// Stop disarms the sweep. Tracked streams are left as they are, and their
// deadlines stay armed: a deadline is a bound, enforced even while no
// other re-check runs. Once the run context has ended, an interval check
// runs with that ended context, so its verdict cannot be trusted
// (unavailable): the stream is never renewed, and is closed at its
// deadline with 4401 authz_expired.
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
	a.sweep = a.cfg.Clock.AfterFunc(a.cfg.RecheckInterval, a.runSweep)
}

// runSweep is one sweep tick. Ticks keep a fixed period: the next one is
// armed before this sweep runs, so a long sweep does not push the
// following ones back. A tick that finds the previous sweep still running
// is skipped (and logged); that sweep is already re-checking every stream.
func (a *conduitStreamAuthz) runSweep() {
	a.armSweep()
	a.mu.Lock()
	ctx := a.ctx
	busy := a.sweeping
	a.sweeping = true
	a.mu.Unlock()
	if busy {
		a.cfg.Logger.Warn("conduit_stream_authz_sweep_skipped",
			"reason", "previous sweep still running", "interval", a.cfg.RecheckInterval)
		return
	}
	start := a.cfg.Clock.Now()
	n := a.Len()
	a.Recheck(ctx, conduitAuthzTriggerSweep, conduitAuthzMatch{})
	took := a.cfg.Clock.Now().Sub(start)
	a.mu.Lock()
	a.sweeping = false
	a.mu.Unlock()
	level := slog.LevelDebug
	if took > a.cfg.RecheckInterval {
		level = slog.LevelWarn
	}
	a.cfg.Logger.Log(ctx, level, "conduit_stream_authz_sweep", "streams", n, "duration", took, "interval", a.cfg.RecheckInterval)
}

// Track registers st until the returned function is called (when the
// stream ends for any reason), and arms its authorization deadline:
// st.Admitted (now if unset) plus the user stream interval.
func (a *conduitStreamAuthz) Track(st *conduitUserStream) (untrack func()) {
	a.mu.Lock()
	a.streams[st] = struct{}{}
	a.mu.Unlock()
	if d := a.cfg.UserStreamAuthzMax; d > 0 {
		admitted := st.Admitted
		if admitted.IsZero() {
			admitted = a.cfg.Clock.Now()
		}
		st.dmu.Lock()
		st.interval = d
		st.deadline = admitted.Add(d)
		a.armDeadlineLocked(st)
		st.dmu.Unlock()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			st.stopDeadline()
			a.mu.Lock()
			delete(a.streams, st)
			a.mu.Unlock()
		})
	}
}

// armDeadlineLocked (re)arms st's timer for st.deadline. st.dmu is held.
func (a *conduitStreamAuthz) armDeadlineLocked(st *conduitUserStream) {
	if st.untracked {
		return
	}
	if st.timer != nil {
		st.timer.Stop()
	}
	st.timer = a.cfg.Clock.AfterFunc(st.deadline.Sub(a.cfg.Clock.Now()), func() {
		a.check(a.runContext(), conduitAuthzTriggerInterval, st)
	})
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

// check evaluates one stream and acts on the verdict. The decision and
// its state change (close, deadline move) happen under st.mu; the renewal
// notice, metric and log line follow once st.mu is released, so a slow
// notice send never holds up other checks of the stream.
func (a *conduitStreamAuthz) check(ctx context.Context, trigger string, st *conduitUserStream) {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return
	}
	if trigger == conduitAuthzTriggerInterval && !a.deadlineReached(st) {
		// A timer outlived the deadline it was armed for (the stream was
		// renewed or untracked meanwhile): there is nothing to enforce.
		st.mu.Unlock()
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

	interval := trigger == conduitAuthzTriggerInterval
	var outcome string
	switch {
	case interval && verdict == conduitAuthzAllowed:
		outcome = conduitAuthzOutcomeRenewed
		var renewed bool
		renewed, detail = a.renew(st, detail)
		if !renewed {
			outcome = conduitAuthzOutcomePassed
		}
	case interval && verdict == conduitAuthzUnavailable:
		// The stream reached its deadline unrenewed.
		outcome = conduitAuthzOutcomeExpired
		st.closed = true
		st.Close(conduit.CloseUnauthenticated, conduitReasonAuthzExpired)
	}
	switch {
	case outcome != "":
	case verdict == conduitAuthzAllowed:
		outcome = conduitAuthzOutcomePassed
	case verdict == conduitAuthzDenied:
		outcome = conduitAuthzOutcomeClosed
		st.closed = true
		st.Close(conduit.CloseUnauthenticated, conduitReasonAuthzExpired)
	case verdict == conduitAuthzTargetGone:
		outcome = conduitAuthzOutcomeTargetGone
		st.closed = true
		st.Close(relay.CloseTargetNotFound, relay.ReasonTargetNotFound)
	default:
		outcome = conduitAuthzOutcomeDeferred
	}
	if st.closed {
		st.stopDeadline()
		a.mu.Lock()
		delete(a.streams, st)
		a.mu.Unlock()
	}
	st.mu.Unlock()

	// The renewal notice is advisory (the deadline is enforced here), so
	// it is sent outside st.mu. A notice that reaches a stream closed
	// meanwhile is ignored by the target.
	var notice string
	if outcome == conduitAuthzOutcomeRenewed {
		notice = conduitAuthzNoticeNotDelivered
		if st.Renew != nil {
			if err := st.Renew(); err != nil {
				detail = joinDetail(detail, "renewal notice: "+err.Error())
			} else {
				notice = conduitAuthzNoticeLocal
			}
		}
	}
	if a.cfg.Metrics != nil {
		a.cfg.Metrics.RecordConduitStreamAuthz(trigger, outcome, st.Kind)
	}
	level := slog.LevelInfo
	switch {
	case outcome == conduitAuthzOutcomeDeferred, outcome == conduitAuthzOutcomeExpired:
		level = slog.LevelWarn
	case outcome == conduitAuthzOutcomePassed && trigger == conduitAuthzTriggerSweep:
		level = slog.LevelDebug
	}
	attrs := []any{
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
		"detail", detail,
	}
	if outcome == conduitAuthzOutcomeRenewed {
		attrs = append(attrs, "notice", notice, "deadline", st.Deadline())
	}
	a.cfg.Logger.Log(ctx, level, "conduit_stream_authz", attrs...)
}

// renew moves st's deadline forward by exactly one interval and re-arms
// its timer. It refuses (and reports why in the returned detail) when the
// deadline has not been reached, so a stale timer never extends a stream.
func (a *conduitStreamAuthz) renew(st *conduitUserStream, detail string) (bool, string) {
	st.dmu.Lock()
	defer st.dmu.Unlock()
	if st.interval <= 0 || st.untracked {
		return false, joinDetail(detail, "no deadline to renew")
	}
	if a.cfg.Clock.Now().Before(st.deadline) {
		return false, joinDetail(detail, "deadline not reached")
	}
	st.deadline = st.deadline.Add(st.interval)
	a.armDeadlineLocked(st)
	return true, detail
}

// deadlineReached reports whether st is tracked with a deadline that has
// been reached.
func (a *conduitStreamAuthz) deadlineReached(st *conduitUserStream) bool {
	st.dmu.Lock()
	defer st.dmu.Unlock()
	return st.interval > 0 && !st.untracked && !a.cfg.Clock.Now().Before(st.deadline)
}

func joinDetail(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
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
