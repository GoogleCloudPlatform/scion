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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetryemit"
)

// Outbox bounds and drainer timing (design 5.3). The bounds are constants,
// not operational settings, in slice 1.
const (
	MaxOutboxRows        = 100_000
	MaxOutboxAge         = 72 * time.Hour
	MaxPermanentFailures = 5

	DefaultPollInterval  = 2 * time.Second
	DefaultBatchSize     = 20
	DefaultLeaseDuration = 30 * time.Second
	DefaultEmitTimeout   = 10 * time.Second
	DefaultSweepInterval = time.Hour
	// DefaultMaintenanceInterval is the cadence of the trim and the gauge
	// sampling, slower than claiming (section 22 item 12, N5).
	DefaultMaintenanceInterval = time.Minute
	DefaultSweepBatch          = 500
	DefaultShutdownBudget      = 5 * time.Second
	backoffBase                = time.Second
	backoffMax                 = 5 * time.Minute
	blockedLogInterval         = time.Minute
	maxDropIDsPerLine          = 100
	maxDropLinesPerReason      = 10
	maxLastErrorBytes          = 256
)

// DrainerConfig configures a Drainer. Zero durations and sizes take the
// defaults above.
type DrainerConfig struct {
	Store    store.LifecycleOutboxStore
	Sink     telemetryemit.Sink
	Counters telemetryemit.LifecycleCounters
	Logger   *slog.Logger
	// Owner is this replica's lease identity; default a random UUID.
	Owner string
	// Now is the drainer clock; default time.Now.
	Now func() time.Time
	// Validate re-validates a stored payload before delivery; a failure is
	// a permanent failure of that payload. Default: decode and validate as
	// a telemetrycontract.LifecycleEvent.
	Validate func(payload []byte) error

	PollInterval  time.Duration
	BatchSize     int
	LeaseDuration time.Duration
	EmitTimeout   time.Duration
	SweepInterval time.Duration
	// MaintenanceInterval is how often Run trims the outbox and samples the
	// gauges; claiming runs every PollInterval.
	MaintenanceInterval time.Duration
	SweepBatch          int
	ShutdownBudget      time.Duration
	MaxRows             int
	MaxAge              time.Duration
	MaxPermanent        int

	// ExitWhenEmpty makes Run return once the outbox is empty: used when
	// the drainer runs only to deliver a backlog left from a time the
	// observer was installed (review r6 N4).
	ExitWhenEmpty bool
}

// Drainer delivers outbox rows to a sink after commit: lease a batch,
// re-validate, emit, delete on success, record the failure otherwise. A
// lost lease can only cause a duplicate delivery with the same bytes and
// event_id, never a different key.
type Drainer struct {
	cfg  DrainerConfig
	wake chan struct{}

	mu             sync.Mutex
	blocked        bool
	blockedReason  string
	nextProbe      time.Time
	probeBackoff   time.Duration
	lastBlockedLog time.Time
	deliveredMax   int64
}

// NewDrainer returns a Drainer.
func NewDrainer(cfg DrainerConfig) (*Drainer, error) {
	if cfg.Store == nil || cfg.Sink == nil {
		return nil, errors.New("lifecycleevents: drainer needs a store and a sink")
	}
	if cfg.Counters == nil {
		cfg.Counters = telemetryemit.NopCounters{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Owner == "" {
		cfg.Owner = uuid.NewString()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Validate == nil {
		cfg.Validate = func(p []byte) error {
			var ev telemetrycontract.LifecycleEvent
			return telemetrycontract.Unmarshal(p, &ev)
		}
	}
	setDur := func(d *time.Duration, def time.Duration) {
		if *d <= 0 {
			*d = def
		}
	}
	setInt := func(n *int, def int) {
		if *n <= 0 {
			*n = def
		}
	}
	setDur(&cfg.PollInterval, DefaultPollInterval)
	setDur(&cfg.LeaseDuration, DefaultLeaseDuration)
	setDur(&cfg.EmitTimeout, DefaultEmitTimeout)
	setDur(&cfg.SweepInterval, DefaultSweepInterval)
	setDur(&cfg.MaintenanceInterval, DefaultMaintenanceInterval)
	setDur(&cfg.ShutdownBudget, DefaultShutdownBudget)
	setDur(&cfg.MaxAge, MaxOutboxAge)
	setInt(&cfg.BatchSize, DefaultBatchSize)
	setInt(&cfg.SweepBatch, DefaultSweepBatch)
	setInt(&cfg.MaxRows, MaxOutboxRows)
	setInt(&cfg.MaxPermanent, MaxPermanentFailures)
	if cfg.EmitTimeout >= cfg.LeaseDuration {
		return nil, fmt.Errorf("lifecycleevents: emit timeout %s must be shorter than the lease %s", cfg.EmitTimeout, cfg.LeaseDuration)
	}
	return &Drainer{cfg: cfg, wake: make(chan struct{}, 1)}, nil
}

// Wake asks the drainer to run a pass now. It never blocks.
func (d *Drainer) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// DrainResult summarizes one pass.
type DrainResult struct {
	Claimed, Delivered, Failed int
	Dropped                    int
	Remaining                  int64
}

// Run sweeps orphan cursors, then drains every PollInterval and on Wake
// until ctx is done, sweeping again every SweepInterval. On exit it makes
// one bounded final pass (ShutdownBudget); whatever is left stays in the
// outbox for the next start or another replica.
func (d *Drainer) Run(ctx context.Context) {
	d.sweep(ctx)
	ticker := time.NewTicker(d.cfg.PollInterval)
	defer ticker.Stop()
	nextSweep := d.cfg.Now().Add(d.cfg.SweepInterval)
	var nextMaintenance time.Time // the first pass maintains
	for {
		// A backlog-only drainer needs the pending count each pass to know
		// when to exit; otherwise trim and stats run on the slower cadence.
		maintain := d.cfg.ExitWhenEmpty || !d.cfg.Now().Before(nextMaintenance)
		if maintain {
			nextMaintenance = d.cfg.Now().Add(d.cfg.MaintenanceInterval)
		}
		res, err := d.drainPass(ctx, maintain)
		if err != nil && ctx.Err() == nil {
			d.cfg.Logger.Warn("lifecycle outbox drain pass failed", "error", err)
		}
		if d.cfg.ExitWhenEmpty && err == nil && res.Remaining == 0 {
			d.cfg.Logger.Info("lifecycle outbox backlog delivered; drainer stopping")
			return
		}
		if now := d.cfg.Now(); !now.Before(nextSweep) {
			d.sweep(ctx)
			nextSweep = now.Add(d.cfg.SweepInterval)
		}
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.cfg.ShutdownBudget)
			if _, err := d.DrainOnce(fctx); err != nil {
				d.cfg.Logger.Info("lifecycle outbox final drain incomplete; rows stay for the next start", "error", err)
			}
			cancel()
			return
		case <-ticker.C:
		case <-d.wake:
		}
	}
}

// sweep removes orphan cursors in batches until none is left.
func (d *Drainer) sweep(ctx context.Context) {
	total := 0
	for ctx.Err() == nil {
		n, err := d.cfg.Store.SweepOrphanLifecycleCursors(ctx, d.cfg.SweepBatch)
		if err != nil {
			d.cfg.Logger.Warn("lifecycle orphan cursor sweep failed", "error", err)
			return
		}
		total += n
		if n == 0 {
			break
		}
	}
	if total > 0 {
		d.cfg.Logger.Info("lifecycle orphan cursors removed", "count", total)
	}
}

// DrainOnce runs one pass: trim by the bounds, then (unless the sink is
// blocked and not yet due for a probe) claim and deliver one batch, then
// sample the gauges.
func (d *Drainer) DrainOnce(ctx context.Context) (DrainResult, error) {
	return d.drainPass(ctx, true)
}

// drainPass is one pass; maintain adds the trim before the claim and the
// gauge sampling after it. Without maintain, Remaining is -1 (unknown).
func (d *Drainer) drainPass(ctx context.Context, maintain bool) (DrainResult, error) {
	var res DrainResult
	now := d.cfg.Now()
	if maintain {
		trim, err := d.cfg.Store.TrimLifecycleOutbox(ctx, now, d.cfg.MaxAge, d.cfg.MaxRows, d.cfg.MaxPermanent)
		if err != nil {
			return res, fmt.Errorf("trim: %w", err)
		}
		res.Dropped = d.reportDrops(trim)
	}

	if allowed, probe := d.claimAllowed(now); allowed {
		batch := d.cfg.BatchSize
		if probe {
			batch = 1 // a blocked sink is probed with one row
		}
		rows, err := d.cfg.Store.ClaimLifecycleOutbox(ctx, d.cfg.Owner, now, now.Add(d.cfg.LeaseDuration), batch)
		if err != nil {
			return res, fmt.Errorf("claim: %w", err)
		}
		res.Claimed = len(rows)
		for i, row := range rows {
			if ctx.Err() != nil {
				d.releaseRows(rows[i:])
				break
			}
			delivered, blocked := d.deliver(ctx, row)
			switch {
			case delivered:
				res.Delivered++
			case blocked:
				res.Failed++
				d.releaseRows(rows[i+1:])
			default:
				res.Failed++
			}
			if blocked {
				break
			}
		}
	}

	if !maintain {
		res.Remaining = -1
		return res, nil
	}
	stats, err := d.cfg.Store.LifecycleOutboxStats(ctx)
	if err != nil {
		return res, fmt.Errorf("stats: %w", err)
	}
	res.Remaining = stats.Pending
	g := telemetryemit.OutboxGauges{Pending: stats.Pending, SeqAllocatedMax: stats.MaxSeq}
	if stats.Oldest != nil {
		g.OldestAgeSeconds = int64(d.cfg.Now().Sub(*stats.Oldest) / time.Second)
	}
	d.mu.Lock()
	g.SeqDeliveredMax = d.deliveredMax
	d.mu.Unlock()
	d.cfg.Counters.Outbox(g)
	return res, nil
}

// claimAllowed is false while the sink is blocked and its next probe is
// not yet due; probe reports that the claim is a blocked-sink probe.
func (d *Drainer) claimAllowed(now time.Time) (allowed, probe bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.blocked {
		return true, false
	}
	return !now.Before(d.nextProbe), true
}

// deliver emits one leased row. It returns whether it was delivered and
// whether the sink is now blocked.
func (d *Drainer) deliver(ctx context.Context, row store.LifecycleOutboxRow) (delivered, blocked bool) {
	now := d.cfg.Now()
	ok, err := d.cfg.Store.RenewLifecycleOutboxLease(ctx, row.Seq, d.cfg.Owner, now.Add(d.cfg.LeaseDuration))
	if err != nil || !ok {
		// Another drainer owns it now (or the store failed): skip.
		return false, false
	}
	if verr := d.cfg.Validate(row.Payload); verr != nil {
		d.fail(ctx, row, telemetryemit.Permanent(telemetryemit.ReasonInvalidPayload, verr))
		return false, false
	}
	d.cfg.Counters.EmitAttempt()
	ectx, cancel := context.WithTimeout(ctx, d.cfg.EmitTimeout)
	err = d.cfg.Sink.Emit(ectx, row.Payload)
	cancel()
	if err != nil {
		class, _ := telemetryemit.Classify(err)
		d.fail(ctx, row, err)
		return false, class == telemetryemit.ClassBlocked
	}
	d.unblock()
	d.cfg.Counters.Delivered()
	d.mu.Lock()
	if row.Seq > d.deliveredMax {
		d.deliveredMax = row.Seq
	}
	d.mu.Unlock()
	if _, err := d.cfg.Store.CompleteLifecycleOutboxRow(ctx, row.Seq, d.cfg.Owner); err != nil {
		// The row stays and is delivered again with the same bytes.
		d.cfg.Logger.Warn("lifecycle outbox row delivered but not deleted; it will be re-delivered with the same event_id",
			"seq", row.Seq, "event_id", row.EventID, "error", err)
	}
	return true, false
}

// fail records a failed attempt per its class (design 5.3).
func (d *Drainer) fail(ctx context.Context, row store.LifecycleOutboxRow, err error) {
	class, reason := telemetryemit.Classify(err)
	d.cfg.Counters.EmitFailure(reason)
	now := d.cfg.Now()
	f := store.LifecycleOutboxFailure{
		CountAttempt:  class != telemetryemit.ClassBlocked,
		Permanent:     class == telemetryemit.ClassPermanent,
		NextAttemptAt: now.Add(backoff(row.Attempts)),
		LastError:     sanitizeError(err),
	}
	if class == telemetryemit.ClassBlocked {
		f.NextAttemptAt = row.NextAttemptAt
		d.block(reason, err)
	}
	if ferr := d.cfg.Store.FailLifecycleOutboxRow(ctx, row.Seq, d.cfg.Owner, f); ferr != nil {
		d.cfg.Logger.Warn("lifecycle outbox failure not recorded", "seq", row.Seq, "error", ferr)
	}
}

// releaseRows gives back leases on rows this pass will not attempt,
// without counting an attempt.
func (d *Drainer) releaseRows(rows []store.LifecycleOutboxRow) {
	for _, r := range rows {
		// A fresh context: a cancelled pass must still release its leases
		// (otherwise they expire after LeaseDuration, which is also safe).
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = d.cfg.Store.FailLifecycleOutboxRow(ctx, r.Seq, d.cfg.Owner, store.LifecycleOutboxFailure{NextAttemptAt: r.NextAttemptAt, LastError: r.LastError})
		cancel()
	}
}

// block marks the sink blocked: stop claiming, probe with backoff, set the
// gauge, and log at most once a minute.
func (d *Drainer) block(reason string, err error) {
	d.mu.Lock()
	now := d.cfg.Now()
	if !d.blocked {
		d.probeBackoff = backoffBase
	} else {
		d.probeBackoff = min(d.probeBackoff*2, backoffMax)
	}
	d.blocked = true
	d.blockedReason = reason
	d.nextProbe = now.Add(d.probeBackoff)
	logNow := d.lastBlockedLog.IsZero() || now.Sub(d.lastBlockedLog) >= blockedLogInterval
	if logNow {
		d.lastBlockedLog = now
	}
	d.mu.Unlock()
	d.cfg.Counters.SinkBlocked(reason, true)
	if logNow {
		d.cfg.Logger.Error("lifecycle analytics sink is blocked; rows are kept and retried",
			"reason", reason, "error", err)
	}
}

func (d *Drainer) unblock() {
	d.mu.Lock()
	was, reason := d.blocked, d.blockedReason
	d.blocked = false
	d.blockedReason = ""
	d.lastBlockedLog = time.Time{}
	d.mu.Unlock()
	if was {
		d.cfg.Counters.SinkBlocked(reason, false)
		d.cfg.Logger.Info("lifecycle analytics sink unblocked", "reason", reason)
	}
}

// Blocked reports whether the sink is currently treated as blocked.
func (d *Drainer) Blocked() (bool, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.blocked, d.blockedReason
}

// reportDrops counts and logs rows removed by the bounds; the WARN lines
// carry the event IDs (durable loss evidence) in lines of at most 100.
func (d *Drainer) reportDrops(t store.LifecycleOutboxTrim) int {
	total := 0
	for _, g := range []struct {
		reason string
		rows   []store.LifecycleOutboxDropped
	}{{"age", t.Age}, {"overflow", t.Overflow}, {"poison", t.Poison}} {
		if len(g.rows) == 0 {
			continue
		}
		total += len(g.rows)
		d.cfg.Counters.Dropped(g.reason, len(g.rows))
		minSeq, maxSeq := g.rows[0].Seq, g.rows[0].Seq
		agents := map[string]bool{}
		for _, r := range g.rows {
			minSeq, maxSeq = min(minSeq, r.Seq), max(maxSeq, r.Seq)
			agents[r.AgentID] = true
		}
		for line := 0; line*maxDropIDsPerLine < len(g.rows); line++ {
			if line == maxDropLinesPerReason {
				d.cfg.Logger.Warn("lifecycle outbox drop log rate-limited",
					"reason", g.reason, "omitted_event_ids", len(g.rows)-line*maxDropIDsPerLine)
				break
			}
			chunk := g.rows[line*maxDropIDsPerLine : min(len(g.rows), (line+1)*maxDropIDsPerLine)]
			ids := make([]string, len(chunk))
			revs := make([]string, len(chunk))
			for i, r := range chunk {
				ids[i] = r.EventID
				revs[i] = fmt.Sprintf("%s:%d", r.AgentID, r.TransitionRevision)
			}
			d.cfg.Logger.Warn("lifecycle outbox rows dropped",
				"reason", g.reason, "count", len(g.rows), "agents", len(agents),
				"seq_min", minSeq, "seq_max", maxSeq,
				"event_ids", strings.Join(ids, ","), "agent_revisions", strings.Join(revs, ","))
		}
	}
	return total
}

// backoff is 1 s doubling per prior attempt, capped at 5 min.
func backoff(attempts int) time.Duration {
	b := backoffBase
	for i := 0; i < attempts && b < backoffMax; i++ {
		b *= 2
	}
	return min(b, backoffMax)
}

// sanitizeError reduces an error to one bounded line.
func sanitizeError(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) > maxLastErrorBytes {
		s = s[:maxLastErrorBytes]
		for len(s) > 0 && !utf8ValidTail(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

// utf8ValidTail reports whether s does not end in a truncated UTF-8
// sequence.
func utf8ValidTail(s string) bool {
	return strings.ToValidUTF8(s, "�") == s
}
