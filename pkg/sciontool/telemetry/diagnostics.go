package telemetry

import (
	"sync/atomic"
	"time"
)

// DeliverySnapshot is a bounded, local diagnostic view. Counters count records
// or metric points, except Attempts and Failed, which count export calls.
type DeliverySnapshot struct {
	Accepted, Filtered, Rejected, Queued, Delivered, Dropped int64
	Attempts, Failed                                         int64
	LastSuccess                                              time.Time
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
	accepted, filtered, rejected, queued, delivered, dropped atomic.Int64
	attempts, failed, lastSuccess                            atomic.Int64
}

func (d *signalDiagnostics) snapshot() DeliverySnapshot {
	result := DeliverySnapshot{
		Accepted: d.accepted.Load(), Filtered: d.filtered.Load(), Rejected: d.rejected.Load(),
		Queued: d.queued.Load(), Delivered: d.delivered.Load(), Dropped: d.dropped.Load(),
		Attempts: d.attempts.Load(), Failed: d.failed.Load(),
	}
	if timestamp := d.lastSuccess.Load(); timestamp != 0 {
		result.LastSuccess = time.Unix(0, timestamp)
	}
	return result
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
