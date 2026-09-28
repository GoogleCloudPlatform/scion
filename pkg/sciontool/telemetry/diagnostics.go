package telemetry

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

const diagnosticSnapshotInterval = 60 * time.Second

// DeliverySnapshot is a bounded, local diagnostic view. Counters count records
// or metric points, except Attempts and Failed, which count export calls.
type DeliverySnapshot struct {
	Accepted, Filtered, Rejected, Queued, Delivered, Dropped, Unconfirmed int64
	Permanent, Partial, AttemptLimit, AgeLimit, Canceled                  int64
	BackendRejected                                                       int64
	Attempts, Failed                                                      int64
	SDKErrors                                                             int64
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
	sdkErrors                                                             atomic.Int64
}

func (d *signalDiagnostics) snapshot() DeliverySnapshot {
	result := DeliverySnapshot{
		Accepted: d.accepted.Load(), Filtered: d.filtered.Load(), Rejected: d.rejected.Load(),
		Queued: d.queued.Load(), Delivered: d.delivered.Load(), Dropped: d.dropped.Load(), Unconfirmed: d.unconfirmed.Load(),
		Permanent: d.permanent.Load(), Partial: d.partial.Load(), AttemptLimit: d.attemptLimit.Load(), AgeLimit: d.ageLimit.Load(), Canceled: d.canceled.Load(),
		BackendRejected: d.backendRejected.Load(),
		Attempts:        d.attempts.Load(), Failed: d.failed.Load(),
		SDKErrors: d.sdkErrors.Load(),
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

// Snapshots use the local logger only. They are fixed-cardinality and do not
// traverse the telemetry exporter whose failure they describe.
func (p *Pipeline) logDeliverySnapshot(force bool) {
	if p == nil {
		return
	}
	p.diagnosticMu.Lock()
	defer p.diagnosticMu.Unlock()
	if !force && time.Since(p.diagnosticLast) < diagnosticSnapshotInterval {
		return
	}
	p.diagnosticLast = time.Now()
	depth := p.QueueDepth()
	diagnostics := p.Diagnostics()
	log.Info("Telemetry delivery snapshot state=%s queue_bytes=%d queue_records=%d queue_entries=%d spans=%+v metrics=%+v logs=%+v",
		p.DeliveryState(), depth.Bytes, depth.Records, depth.Entries,
		diagnostics["spans"], diagnostics["metrics"], diagnostics["logs"])
}

func (p *Pipeline) markDeliveryDegraded() {
	if p == nil {
		return
	}
	previous := p.deliveryState.Swap("degraded")
	p.logDeliverySnapshot(previous != "degraded")
}

func (p *Pipeline) startDiagnosticSnapshots(ctx context.Context) {
	localCtx, cancel := context.WithCancel(ctx)
	p.diagnosticCancel = cancel
	p.diagnosticDone = make(chan struct{})
	go func() {
		defer close(p.diagnosticDone)
		ticker := time.NewTicker(diagnosticSnapshotInterval)
		defer ticker.Stop()
		for {
			select {
			case <-localCtx.Done():
				return
			case <-ticker.C:
				p.logDeliverySnapshot(false)
			}
		}
	}()
}
