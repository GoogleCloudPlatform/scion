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

package durablequeue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Default drainer timing and queue bounds (stage1-design §6.1).
const (
	DefaultPollInterval        = 2 * time.Second
	DefaultLeaseDuration       = 30 * time.Second
	DefaultHandlerTimeout      = 10 * time.Second
	DefaultBatchSize           = 20
	DefaultMaxRows             = 100_000
	DefaultMaxPermanent        = 5
	DefaultMaxAge              = 72 * time.Hour
	DefaultMaintenanceInterval = time.Minute
	DefaultShutdownBudget      = 5 * time.Second

	backoffBase        = time.Second
	backoffMax         = time.Minute * 5
	blockedLogInterval = time.Minute
	maxOwnerPrefixLen  = 200
)

// Drop reasons reported to OnDrop and Metrics.Dropped.
const (
	DropReasonAge      = "age"
	DropReasonOverflow = "overflow"
	DropReasonPoison   = "poison"
)

// Result is what a Handler reports after processing one claimed Item.
type Result struct {
	Done      bool      // delete the item
	RetryAt   time.Time // zero -> exponential backoff from Attempts (1 s ... 5 min)
	Permanent bool      // counts toward the poison bound; never retried once reached
	Err       error     // last_error (truncated to 256 bytes, content-free by contract)
}

// Handler processes one leased Item. Handlers must be idempotent: a lost lease
// can cause the same Item (with identical Payload bytes) to be handled again.
type Handler func(ctx context.Context, item Item, lease Lease) Result

// Lease lets a long-running Handler extend its claim on an Item. Renew extends
// lease_until; the drainer also auto-renews every LeaseDuration/3 while the
// handler runs.
type Lease interface {
	Renew(ctx context.Context) (held bool, err error)
}

// Metrics observes per-kind drainer activity and queue gauges.
type Metrics interface {
	Claimed(kind string, n int)
	Done(kind string)
	Retried(kind string)
	Permanent(kind string)
	Dropped(kind, reason string, n int)
	ObserveQueue(kind string, pending, leased int64, oldestAge time.Duration)
}

// NopMetrics is a Metrics implementation that discards all observations.
type NopMetrics struct{}

func (NopMetrics) Claimed(string, int)                              {}
func (NopMetrics) Done(string)                                      {}
func (NopMetrics) Retried(string)                                   {}
func (NopMetrics) Permanent(string)                                 {}
func (NopMetrics) Dropped(string, string, int)                      {}
func (NopMetrics) ObserveQueue(string, int64, int64, time.Duration) {}

// DrainerConfig configures a per-kind Drainer. Zero durations and sizes take
// the package defaults above.
type DrainerConfig struct {
	Store   store.DurableQueueStore
	Queue   Queue // optional: registers this drainer to receive Queue.Wake(Kind)
	Kind    string
	Handler Handler
	Owner   string      // replica lease identity; default random UUID
	Enabled func() bool // checked per pass (experiment gate); nil -> always

	PollInterval   time.Duration // default 2 s
	LeaseDuration  time.Duration // default 30 s
	HandlerTimeout time.Duration // default 10 s; must be < LeaseDuration

	BatchSize    int           // default 20
	MaxRows      int           // default 100 000
	MaxPermanent int           // default 5
	MaxAge       time.Duration // default 72 h

	// OnDrop is invoked when maintenance trims items by age, overflow, or
	// poison bounds. Reported Items carry metadata only (Seq, Kind, ProjectID,
	// DedupKey, CreatedAt); Payload is nil.
	OnDrop  func(ctx context.Context, dropped []Item, reason string)
	Metrics Metrics
	Logger  *slog.Logger

	// Optional operational overrides (match lifecycleevents.DrainerConfig).
	Now                 func() time.Time
	MaintenanceInterval time.Duration // default 1 min
	ShutdownBudget      time.Duration // default 5 s
}

// DrainResult summarizes one DrainOnce / poll pass.
type DrainResult struct {
	Claimed   int
	Done      int
	Retried   int
	Permanent int
	Blocked   bool
	Dropped   int
	Remaining int64 // -1 when stats were not sampled on this pass
}

// Drainer claims and dispatches items of a single Kind from a DurableQueueStore.
type Drainer struct {
	cfg         DrainerConfig
	ownerPrefix string
	wake        chan struct{}
	unregister  func()

	mu             sync.Mutex
	blocked        bool
	nextProbe      time.Time
	probeBackoff   time.Duration
	lastBlockedLog time.Time
}

// NewDrainer validates cfg and constructs a Drainer.
func NewDrainer(cfg DrainerConfig) (*Drainer, error) {
	if cfg.Store == nil {
		return nil, errors.New("durablequeue: drainer requires a Store")
	}
	if err := store.ValidateDurableQueueKind(cfg.Kind); err != nil {
		return nil, err
	}
	if cfg.Handler == nil {
		return nil, errors.New("durablequeue: drainer requires a Handler")
	}
	if cfg.Owner == "" {
		cfg.Owner = uuid.NewString()
	}
	if cfg.Metrics == nil {
		cfg.Metrics = NopMetrics{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
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
	setDur(&cfg.HandlerTimeout, DefaultHandlerTimeout)
	setDur(&cfg.MaxAge, DefaultMaxAge)
	setDur(&cfg.MaintenanceInterval, DefaultMaintenanceInterval)
	setDur(&cfg.ShutdownBudget, DefaultShutdownBudget)
	setInt(&cfg.BatchSize, DefaultBatchSize)
	setInt(&cfg.MaxRows, DefaultMaxRows)
	setInt(&cfg.MaxPermanent, DefaultMaxPermanent)

	if cfg.HandlerTimeout >= cfg.LeaseDuration {
		return nil, fmt.Errorf("durablequeue: handler timeout %s must be shorter than lease duration %s", cfg.HandlerTimeout, cfg.LeaseDuration)
	}

	d := &Drainer{
		cfg:         cfg,
		ownerPrefix: truncateOwnerPrefix(cfg.Owner),
		wake:        make(chan struct{}, 1),
	}
	if reg, ok := cfg.Queue.(drainerRegistrar); ok {
		d.unregister = reg.registerWake(cfg.Kind, d.Wake)
	}
	return d, nil
}

// Kind returns the queue item kind served by d.
func (d *Drainer) Kind() string { return d.cfg.Kind }

// Owner returns the lease owner identity used by d.
func (d *Drainer) Owner() string { return d.cfg.Owner }

// Wake nudges the drainer to run a pass immediately without blocking.
func (d *Drainer) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Blocked reports whether the drainer is currently in blocked-sink probe mode.
func (d *Drainer) Blocked() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.blocked
}

// Run polls every PollInterval and on Wake until ctx is cancelled, running
// trim + gauge maintenance every MaintenanceInterval (or immediately on the
// next pass when a bounded trim pass reports additional candidates remaining).
// On ctx cancellation it executes one final pass bounded by ShutdownBudget
// before returning.
func (d *Drainer) Run(ctx context.Context) {
	if d.unregister != nil {
		defer d.unregister()
	}
	ticker := time.NewTicker(d.cfg.PollInterval)
	defer ticker.Stop()

	var nextMaintenance time.Time // maintain on the first pass
	for {
		if ctx.Err() != nil {
			d.runShutdownDrain(ctx)
			return
		}
		now := d.cfg.Now()
		maintain := !now.Before(nextMaintenance)
		_, trimMore, err := d.drainPass(ctx, maintain)
		if err != nil && ctx.Err() == nil {
			d.cfg.Logger.Warn("durable queue drain pass failed", "kind", d.cfg.Kind, "error", err)
		}
		if maintain {
			if trimMore && err == nil {
				// Per-pass trim cap was reached; keep maintenance due and nudge
				// Wake so trimming continues on the next pass without holding
				// one giant transaction or starving claims/shutdown.
				nextMaintenance = time.Time{}
				d.Wake()
			} else {
				nextMaintenance = now.Add(d.cfg.MaintenanceInterval)
			}
		}
		select {
		case <-ctx.Done():
			d.runShutdownDrain(ctx)
			return
		case <-ticker.C:
		case <-d.wake:
		}
	}
}

func (d *Drainer) runShutdownDrain(ctx context.Context) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.cfg.ShutdownBudget)
	defer cancel()
	if _, err := d.DrainOnce(fctx); err != nil {
		d.cfg.Logger.Info("durable queue final drain incomplete; items remain queued", "kind", d.cfg.Kind, "error", err)
	}
}

// DrainOnce executes a single full pass (trim, claim + handle one batch, and
// stats sampling). When Enabled is non-nil and returns false, DrainOnce is a
// no-op. DrainOnce must not be called concurrently with Run on the same
// Drainer.
func (d *Drainer) DrainOnce(ctx context.Context) (DrainResult, error) {
	res, _, err := d.drainPass(ctx, true)
	return res, err
}

func (d *Drainer) nextClaimOwner() string {
	return d.ownerPrefix + ":" + uuid.NewString()
}

func (d *Drainer) drainPass(ctx context.Context, maintain bool) (DrainResult, bool, error) {
	var res DrainResult
	if d.cfg.Enabled != nil && !d.cfg.Enabled() {
		res.Remaining = -1
		return res, false, nil
	}

	var trimMore bool
	now := d.cfg.Now().UTC()
	if maintain {
		dropped, more, err := d.trimAndReport(ctx, now)
		if err != nil {
			return res, false, fmt.Errorf("trim: %w", err)
		}
		res.Dropped += dropped
		trimMore = more
	}

	hitPoisonBound := false
	if allowed, probe := d.claimAllowed(now); allowed {
		batch := d.cfg.BatchSize
		if probe {
			batch = 1
		}
		// Stamp each claim batch with a unique nonce suffix so lease CAS
		// checks identify this specific claim rather than only the replica
		// name (protecting against duplicate Owner strings or stale passes).
		claimOwner := d.nextClaimOwner()
		rows, err := d.cfg.Store.ClaimDurableItems(ctx, d.cfg.Kind, claimOwner, now, now.Add(d.cfg.LeaseDuration), batch)
		if err != nil {
			return res, trimMore, fmt.Errorf("claim: %w", err)
		}
		res.Claimed = len(rows)
		if len(rows) > 0 {
			d.cfg.Metrics.Claimed(d.cfg.Kind, len(rows))
		}
		for i, row := range rows {
			if ctx.Err() != nil {
				d.releaseRows(rows[i:], claimOwner)
				break
			}
			out, poisoned := d.handleRow(ctx, row, claimOwner)
			if poisoned {
				hitPoisonBound = true
			}
			switch out {
			case rowOutcomeDone:
				res.Done++
			case rowOutcomeRetried:
				res.Retried++
			case rowOutcomePermanent:
				res.Permanent++
			case rowOutcomeBlocked:
				res.Blocked = true
				d.releaseRows(rows[i+1:], claimOwner)
			case rowOutcomeSkipped:
			}
			if out == rowOutcomeBlocked {
				break
			}
		}
	}

	// If an item reached MaxPermanent on this pass, trim poison immediately so
	// OnDrop fires promptly and the item is never left pending until the next
	// maintenance window.
	if hitPoisonBound && ctx.Err() == nil {
		dropped, more, err := d.trimAndReport(ctx, d.cfg.Now().UTC())
		if err != nil {
			return res, trimMore, fmt.Errorf("poison trim: %w", err)
		}
		res.Dropped += dropped
		if more {
			trimMore = true
		}
	}

	if !maintain {
		res.Remaining = -1
		return res, trimMore, nil
	}
	stats, err := d.cfg.Store.DurableQueueStats(ctx, d.cfg.Kind, "")
	if err != nil {
		return res, trimMore, fmt.Errorf("stats: %w", err)
	}
	res.Remaining = stats.Pending
	var oldestAge time.Duration
	if stats.Oldest != nil {
		if age := d.cfg.Now().UTC().Sub(*stats.Oldest); age > 0 {
			oldestAge = age
		}
	}
	d.cfg.Metrics.ObserveQueue(d.cfg.Kind, stats.Pending, stats.Leased, oldestAge)
	return res, trimMore, nil
}

type rowOutcome int

const (
	rowOutcomeSkipped rowOutcome = iota
	rowOutcomeDone
	rowOutcomeRetried
	rowOutcomePermanent
	rowOutcomeBlocked
)

func (d *Drainer) handleRow(ctx context.Context, row store.DurableQueueRow, claimOwner string) (outcome rowOutcome, hitPoisonBound bool) {
	lease := &itemLease{d: d, seq: row.Seq, owner: claimOwner}
	held, err := lease.Renew(ctx)
	if err != nil || !held {
		return rowOutcomeSkipped, false
	}

	hctx, hcancel := context.WithTimeout(ctx, d.cfg.HandlerTimeout)
	rctx, stopRenew := context.WithCancel(hctx)
	renewDone := make(chan struct{})
	renewInterval := d.cfg.LeaseDuration / 3
	go func() {
		defer close(renewDone)
		if renewInterval <= 0 {
			return
		}
		ticker := time.NewTicker(renewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-rctx.Done():
				return
			case <-ticker.C:
				ok, rerr := lease.Renew(rctx)
				if rerr == nil && !ok {
					// Lease was lost to another replica; cancel the handler context.
					hcancel()
					return
				}
			}
		}
	}()

	res := d.invokeHandler(hctx, rowToItem(row), lease)
	stopRenew()
	<-renewDone
	hcancel()

	// Use a short uncancelled context if the caller cancelled while the handler
	// was finishing, so completion or lease release still reaches the store.
	sctx := ctx
	var scancel context.CancelFunc
	if sctx.Err() != nil {
		sctx, scancel = context.WithTimeout(context.Background(), time.Second)
		defer scancel()
	}

	if res.Done {
		completed, cerr := d.cfg.Store.CompleteDurableItem(sctx, row.Seq, claimOwner)
		if cerr != nil {
			d.cfg.Logger.Warn("durable queue item completed by handler but delete failed; will re-handle",
				"kind", d.cfg.Kind, "seq", row.Seq, "error", cerr)
			return rowOutcomeSkipped, false
		}
		if !completed {
			// Lease was lost while handling; the new owner owns the row.
			return rowOutcomeSkipped, false
		}
		d.unblock()
		d.cfg.Metrics.Done(d.cfg.Kind)
		return rowOutcomeDone, false
	}

	if !res.Permanent && errors.Is(res.Err, ErrSinkBlocked) {
		d.block(res.Err)
		lastErr := row.LastError
		if res.Err != nil {
			lastErr = sanitizeError(res.Err)
		}
		if _, ferr := d.cfg.Store.FailDurableItem(sctx, row.Seq, claimOwner, store.DurableQueueFailure{
			CountAttempt:  false,
			Permanent:     false,
			NextAttemptAt: row.NextAttemptAt,
			LastError:     lastErr,
		}); ferr != nil {
			d.cfg.Logger.Warn("durable queue blocked-item release failed", "kind", d.cfg.Kind, "seq", row.Seq, "error", ferr)
		}
		return rowOutcomeBlocked, false
	}

	now := d.cfg.Now().UTC()
	retryAt := res.RetryAt.UTC()
	if res.RetryAt.IsZero() {
		retryAt = now.Add(Backoff(row.Attempts))
	}
	poisoned := res.Permanent && d.cfg.MaxPermanent > 0 && row.PermanentFailures+1 >= d.cfg.MaxPermanent
	if poisoned {
		horizon := now.Add(max(d.cfg.MaxAge, 24*time.Hour) + time.Hour)
		if retryAt.Before(horizon) {
			retryAt = horizon
		}
	}

	failedHeld, ferr := d.cfg.Store.FailDurableItem(sctx, row.Seq, claimOwner, store.DurableQueueFailure{
		CountAttempt:  true,
		Permanent:     res.Permanent,
		NextAttemptAt: retryAt,
		LastError:     sanitizeError(res.Err),
	})
	if ferr != nil {
		d.cfg.Logger.Warn("durable queue item failure not recorded", "kind", d.cfg.Kind, "seq", row.Seq, "error", ferr)
		return rowOutcomeSkipped, false
	}
	if !failedHeld {
		// Lease was lost while handling; skip Retried/Permanent metrics,
		// unblock transition, and poison trim flag.
		return rowOutcomeSkipped, false
	}

	// Non-blocked outcome on a still-held lease means the sink is reachable.
	d.unblock()

	if res.Permanent {
		d.cfg.Metrics.Permanent(d.cfg.Kind)
		return rowOutcomePermanent, poisoned
	}
	d.cfg.Metrics.Retried(d.cfg.Kind)
	return rowOutcomeRetried, false
}

func (d *Drainer) invokeHandler(ctx context.Context, item Item, lease Lease) (res Result) {
	defer func() {
		if r := recover(); r != nil {
			d.cfg.Logger.Error("durable queue handler panicked", "kind", d.cfg.Kind, "seq", item.Seq, "panic", r)
			res = Result{
				Permanent: true,
				Err:       errors.New("durablequeue: handler panic"),
			}
		}
	}()
	return d.cfg.Handler(ctx, item, lease)
}

func (d *Drainer) releaseRows(rows []store.DurableQueueRow, claimOwner string) {
	for _, r := range rows {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, _ = d.cfg.Store.FailDurableItem(ctx, r.Seq, claimOwner, store.DurableQueueFailure{
			CountAttempt:  false,
			NextAttemptAt: r.NextAttemptAt,
			LastError:     r.LastError,
		})
		cancel()
	}
}

func (d *Drainer) claimAllowed(now time.Time) (allowed, probe bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.blocked {
		return true, false
	}
	return !now.Before(d.nextProbe), true
}

func (d *Drainer) block(err error) {
	d.mu.Lock()
	now := d.cfg.Now().UTC()
	if !d.blocked {
		d.probeBackoff = backoffBase
	} else {
		d.probeBackoff = min(d.probeBackoff*2, backoffMax)
	}
	d.blocked = true
	d.nextProbe = now.Add(d.probeBackoff)
	logNow := d.lastBlockedLog.IsZero() || now.Sub(d.lastBlockedLog) >= blockedLogInterval
	if logNow {
		d.lastBlockedLog = now
	}
	d.mu.Unlock()

	if logNow {
		d.cfg.Logger.Warn("durable queue sink blocked; probing with backoff",
			"kind", d.cfg.Kind, "backoff", d.probeBackoff, "error", sanitizeError(err))
	}
}

func (d *Drainer) unblock() {
	d.mu.Lock()
	was := d.blocked
	d.blocked = false
	d.nextProbe = time.Time{}
	d.probeBackoff = 0
	d.lastBlockedLog = time.Time{}
	d.mu.Unlock()

	if was {
		d.cfg.Logger.Info("durable queue sink unblocked", "kind", d.cfg.Kind)
	}
}

func (d *Drainer) trimAndReport(ctx context.Context, now time.Time) (int, bool, error) {
	trim, err := d.cfg.Store.TrimDurableQueue(ctx, d.cfg.Kind, now, d.cfg.MaxAge, d.cfg.MaxRows, d.cfg.MaxPermanent)
	if err != nil {
		return 0, false, err
	}
	total := 0
	for _, group := range []struct {
		reason string
		rows   []store.DurableQueueRow
	}{
		{DropReasonAge, trim.Age},
		{DropReasonOverflow, trim.Overflow},
		{DropReasonPoison, trim.Poison},
	} {
		if len(group.rows) == 0 {
			continue
		}
		items := make([]Item, len(group.rows))
		minSeq, maxSeq := group.rows[0].Seq, group.rows[0].Seq
		for i, r := range group.rows {
			items[i] = rowToItem(r)
			minSeq = min(minSeq, r.Seq)
			maxSeq = max(maxSeq, r.Seq)
		}
		total += len(items)
		d.cfg.Metrics.Dropped(d.cfg.Kind, group.reason, len(items))
		d.cfg.Logger.Warn("durable queue items dropped",
			"kind", d.cfg.Kind,
			"reason", group.reason,
			"count", len(items),
			"seq_min", minSeq,
			"seq_max", maxSeq,
		)
		if d.cfg.OnDrop != nil {
			d.invokeOnDrop(ctx, items, group.reason)
		}
	}
	return total, trim.More, nil
}

func (d *Drainer) invokeOnDrop(ctx context.Context, dropped []Item, reason string) {
	defer func() {
		if r := recover(); r != nil {
			d.cfg.Logger.Error("durable queue OnDrop callback panicked",
				"kind", d.cfg.Kind, "reason", reason, "count", len(dropped), "panic", r)
		}
	}()
	d.cfg.OnDrop(ctx, dropped, reason)
}

type itemLease struct {
	d     *Drainer
	seq   int64
	owner string
	mu    sync.Mutex
	lost  bool
}

func (l *itemLease) Renew(ctx context.Context) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lost {
		return false, nil
	}
	until := l.d.cfg.Now().UTC().Add(l.d.cfg.LeaseDuration)
	held, err := l.d.cfg.Store.RenewDurableLease(ctx, l.seq, l.owner, until)
	if err == nil && !held {
		l.lost = true
	}
	return held, err
}

func truncateOwnerPrefix(s string) string {
	if len(s) <= maxOwnerPrefixLen {
		return s
	}
	s = s[:maxOwnerPrefixLen]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// Backoff returns the exponential retry delay for prior attempt count attempts:
// 1 s doubling per prior attempt, capped at 5 min.
func Backoff(attempts int) time.Duration {
	if attempts <= 0 {
		return backoffBase
	}
	b := backoffBase
	for i := 0; i < attempts && b < backoffMax; i++ {
		b *= 2
	}
	return min(b, backoffMax)
}

func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) <= store.MaxDurableQueueLastErrorBytes {
		return s
	}
	s = s[:store.MaxDurableQueueLastErrorBytes]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
