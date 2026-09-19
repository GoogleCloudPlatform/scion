package telemetry

import (
	"sync/atomic"
	"time"
)

// DeliverySnapshot is a bounded, local diagnostic view. Counters count records
// or metric points, except Attempts and Failed, which count export calls.
type DeliverySnapshot struct {
	Accepted, Filtered, Rejected, Queued, Delivered, Dropped, Unconfirmed int64
	Permanent, Partial, AttemptLimit, AgeLimit, Canceled                  int64
	BackendRejected                                                       int64
	Attempts, Failed                                                      int64
	LastSuccess                                                           time.Time
}

type QueueDepth struct{ Bytes, Records, Entries int }

func (p *Pipeline) QueueDepth() QueueDepth {
	if p == nil {
		return QueueDepth{}
	}
	p.budget.mu.Lock()
	defer p.budget.mu.Unlock()
	return QueueDepth{p.budget.bytes, p.budget.records, p.budget.entries}
}

type signalDiagnostics struct {
	accepted, filtered, rejected, queued, delivered, dropped, unconfirmed atomic.Int64
	permanent, partial, attemptLimit, ageLimit, canceled                  atomic.Int64
	backendRejected                                                       atomic.Int64
	attempts, failed, lastSuccess                                         atomic.Int64
}

func (d *signalDiagnostics) snapshot() DeliverySnapshot {
	result := DeliverySnapshot{
		Accepted: d.accepted.Load(), Filtered: d.filtered.Load(), Rejected: d.rejected.Load(),
		Queued: d.queued.Load(), Delivered: d.delivered.Load(), Dropped: d.dropped.Load(), Unconfirmed: d.unconfirmed.Load(),
		Permanent: d.permanent.Load(), Partial: d.partial.Load(), AttemptLimit: d.attemptLimit.Load(), AgeLimit: d.ageLimit.Load(), Canceled: d.canceled.Load(),
		BackendRejected: d.backendRejected.Load(),
		Attempts:        d.attempts.Load(), Failed: d.failed.Load(),
	}
	if timestamp := d.lastSuccess.Load(); timestamp != 0 {
		result.LastSuccess = time.Unix(0, timestamp)
	}
	return result
}

type terminalReason uint8

const (
	terminalPermanent terminalReason = iota
	terminalPartial
	terminalAttemptLimit
	terminalAgeLimit
	terminalCanceled
)

func (d *signalDiagnostics) terminal(records int, reason terminalReason, knownRejected int64) {
	if knownRejected < 0 {
		knownRejected = 0
	}
	if knownRejected > int64(records) {
		knownRejected = int64(records)
	}
	d.unconfirmed.Add(int64(records))
	switch reason {
	case terminalPermanent:
		d.permanent.Add(int64(records))
	case terminalPartial:
		d.partial.Add(int64(records))
	case terminalAttemptLimit:
		d.attemptLimit.Add(int64(records))
	case terminalAgeLimit:
		d.ageLimit.Add(int64(records))
	case terminalCanceled:
		d.canceled.Add(int64(records))
	}
	d.backendRejected.Add(knownRejected)
}

func (d *signalDiagnostics) success(records int) {
	d.delivered.Add(int64(records))
	d.lastSuccess.Store(time.Now().UnixNano())
}

// Diagnostics returns fixed-cardinality local counters. It never includes
// request content or destination error strings.
func (p *Pipeline) Diagnostics() map[string]DeliverySnapshot {
	if p == nil {
		return nil
	}
	return map[string]DeliverySnapshot{
		"spans":   p.spanDiagnostics.snapshot(),
		"metrics": p.metricDiagnostics.snapshot(),
		"logs":    p.logDiagnostics.snapshot(),
	}
}
