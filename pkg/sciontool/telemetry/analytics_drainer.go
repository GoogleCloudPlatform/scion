/*
Copyright 2026 The Scion Authors.
*/

package telemetry

// analytics_drainer.go runs in the long-lived sciontool pipeline and moves
// agent.usage events from the outbox to Cloud Logging (WP2 design section
// 2) and/or Scion Hub usage ingest: it pins the hub ID from the hub's public
// /healthz, finalizes staging lines into ready lines, sends them in order with
// bounded backoff, and expires the occurrence index. It runs when the GCP
// exporter and/or Hub usage ingest is active; otherwise analytics is disabled
// (AnalyticsDisabledNoGCPExporter).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// AnalyticsDisabledNoGCPExporter is the analytics_disabled_reason when neither
// a GCP exporter nor Hub usage ingest is active (e.g., generic OTLP or cloud
// disabled without Hub usage ingest). Analytics events are then neither
// written nor sent.
const AnalyticsDisabledNoGCPExporter = "no_gcp_exporter"

const (
	analyticsDrainInterval    = 5 * time.Second
	analyticsBackoffMin       = time.Second
	analyticsBackoffMax       = 5 * time.Minute
	analyticsIndexExpiryEvery = 10 * time.Minute
	analyticsHubIDTimeout     = 5 * time.Second
	// analyticsRelocationConfirm debounces a hub ID that differs from the
	// pin: it is recorded as a relocation only when a resolution at least
	// this long after the first sighting agrees (false-positive
	// mitigation for transient misroutes, not an identity proof).
	analyticsRelocationConfirm = 5 * time.Minute
	// analyticsSendTimeout bounds one send so a stalled RPC cannot hold
	// the drain loop until Stop.
	analyticsSendTimeout = 30 * time.Second
	// analyticsStopBudget caps the final drain at shutdown; it also never
	// takes more than half of the caller's remaining budget, so the
	// exporter flush that follows keeps its share.
	analyticsStopBudget = 5 * time.Second
	// Dual-run final-drain budget split at shutdown. The Hub usage ledger is
	// the authoritative operational store (quota accounting, dashboards, Hub
	// OTel re-emission) while container-direct GCP Cloud Logging export is
	// secondary. During dual-run shutdown, the Hub sink runs first and gets
	// first claim on 70% of the remaining stop budget, leaving 30% guaranteed
	// for GCP if the Hub stalls; any unused time from a fast Hub drain flows
	// immediately to GCP via the outer stop context.
	analyticsHubStopShareNumerator   = 7
	analyticsHubStopShareDenominator = 10
	// Within the Hub's final-drain budget, reserve a bounded slice for the
	// terminal meter-health heartbeat (final_state=final|incomplete) so a slow
	// batch drain cannot starve the final heartbeat that closes meter_health.
	analyticsHubFinalHeartbeatShareDivisor = 4
	analyticsHubFinalHeartbeatMinBudget    = 250 * time.Millisecond
	analyticsHubFinalHeartbeatMaxBudget    = 1 * time.Second
	// analyticsHubProbeInterval is the periodic re-probe cadence while the Hub
	// usage capabilities probe is negative (O1).
	analyticsHubProbeInterval = 60 * time.Second
)

// HubUsageProbeFunc probes the Hub's usage capabilities endpoint and returns a
// HubUsageSinkConfig when usage ingest is enabled, (nil, nil) when disabled or
// unsupported, or a non-nil error on transient failure.
type HubUsageProbeFunc func(ctx context.Context) (*HubUsageSinkConfig, error)

// HubIDResolver returns the hub's own ID, or an error if it cannot be
// determined now.
type HubIDResolver func(ctx context.Context) (string, error)

// NewHealthzHubIDResolver resolves the hub ID from the hub's public
// GET <endpoint>/healthz: the top-level hub_id of a hub server, or hub.hub_id
// of a combined web server's composite response. It never sends
// credentials.
func NewHealthzHubIDResolver(endpoint string, client *http.Client) HubIDResolver {
	if client == nil {
		client = &http.Client{Timeout: analyticsHubIDTimeout}
	}
	return func(ctx context.Context) (string, error) {
		if endpoint == "" {
			return "", errors.New("hub endpoint is not configured")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/healthz", nil)
		if err != nil {
			return "", err
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return "", err
		}
		// A degraded/unhealthy hub still reports its identity, so the
		// status code is not required to be 200 as long as the body names
		// the hub.
		var health struct {
			HubID string `json:"hub_id"`
			Hub   *struct {
				HubID string `json:"hub_id"`
			} `json:"hub"`
		}
		if err := json.Unmarshal(body, &health); err != nil {
			return "", fmt.Errorf("hub /healthz returned HTTP %d without a JSON body", resp.StatusCode)
		}
		id := health.HubID
		if id == "" && health.Hub != nil {
			id = health.Hub.HubID
		}
		if strings.TrimSpace(id) == "" {
			return "", fmt.Errorf("hub /healthz (HTTP %d) does not report hub_id", resp.StatusCode)
		}
		return strings.TrimSpace(id), nil
	}
}

// AnalyticsDrainer periodically finalizes and sends outbox events.
type AnalyticsDrainer struct {
	// outbox is the GCP analytics outbox (nil in Hub-only mode).
	outbox  *AnalyticsOutbox
	send    AnalyticsSendFunc
	resolve HubIDResolver

	// hubOutbox and hubSink, when non-nil, drain finalized usage batches and
	// periodic empty meter-health heartbeats to the Scion Hub from an
	// independent WAL directory (analytics-outbox-hub). When both GCP (outbox)
	// and Hub (hubOutbox) are active (dual-run), each sink has its own cap,
	// cursor, dead-letter file, health counters, and retry backoff so a
	// failure or overflow in one sink can never affect the other.
	hubOutbox         *AnalyticsOutbox
	hubSink           *HubUsageSink
	batchSend         AnalyticsBatchSendFunc
	heartbeatInterval time.Duration
	lastHubContact    time.Time

	// hubProbe, when set on a GCP-only drainer whose initial probe was
	// negative, periodically re-probes the Hub and opens the sibling Hub
	// outbox when capabilities turn positive without disrupting the GCP drain.
	hubProbe         HubUsageProbeFunc
	probeInterval    time.Duration
	lastProbe        time.Time
	hubOutboxDir     string
	hubOutboxOwner   *OutboxOwner
	supportedSignals func() []string
	schemaDrift      func() bool
	onHubActivated   func(HubUsageSinkConfig, *AnalyticsOutbox, *HubUsageSink)
	onHubRevoked     func()

	interval time.Duration
	now      func() time.Time

	mu            sync.Mutex
	backoff       time.Duration
	retryAfter    time.Time
	hubBackoff    time.Duration
	hubRetryAfter time.Time
	hubRevoked    bool
	lastHubDrain  time.Time
	lastExpiry    time.Time

	// resolveBackoff/resolveAfter pace hub ID resolution while unresolved
	// (drainer goroutine only).
	resolveBackoff time.Duration
	resolveAfter   time.Time
	// resolved is set once this process resolved the hub ID and passed it
	// to PinHubID (drainer goroutine only). hubMu guards verifiedHub (the
	// resolution, when it equals the pin: this process's finalize
	// authorization) and observedHub (the resolution, whatever the pin).
	resolved bool
	// candidate/candidateSince track an unconfirmed mismatch: a resolution
	// differing from the pin is recorded as a relocation only when a later
	// resolution, at least analyticsRelocationConfirm after the first,
	// agrees; a resolution equal to the pin in between clears it.
	candidate      string
	candidateSince time.Time
	// lastFinalizeWarn rate-limits the unexpected-finalize-failure warning
	// (drainer goroutine only).
	lastFinalizeWarn time.Time
	hubMu            sync.Mutex
	verifiedHub      string
	observedHub      string

	cancel context.CancelFunc
	done   chan struct{}
}

// NewAnalyticsDrainer builds a GCP-only drainer for outbox. With a nil resolve
// the drainer never verifies a hub, so it finalizes nothing (provisional lines
// are held); already-final ready lines still drain.
func NewAnalyticsDrainer(outbox *AnalyticsOutbox, send AnalyticsSendFunc, resolve HubIDResolver) *AnalyticsDrainer {
	bounded := func(ctx context.Context, line []byte) error {
		sctx, cancel := context.WithTimeout(ctx, analyticsSendTimeout)
		defer cancel()
		return send(sctx, line)
	}
	return &AnalyticsDrainer{
		outbox:   outbox,
		send:     bounded,
		resolve:  resolve,
		interval: analyticsDrainInterval,
		now:      time.Now,
	}
}

// SetHubProbe configures periodic capability re-probing on a GCP-only drainer
// whose initial probe was negative. When probe returns a non-nil config, the
// drainer opens the sibling Hub outbox, marks it active on disk, and activates
// dual-run without interrupting GCP delivery.
func (d *AnalyticsDrainer) SetHubProbe(probe HubUsageProbeFunc, interval time.Duration, supportedSignals func() []string, schemaDrift func() bool, onActivated func(HubUsageSinkConfig, *AnalyticsOutbox, *HubUsageSink)) {
	if d == nil || probe == nil {
		return
	}
	if interval <= 0 {
		interval = analyticsHubProbeInterval
	}
	d.mu.Lock()
	d.hubProbe = probe
	d.probeInterval = interval
	d.supportedSignals = supportedSignals
	d.schemaDrift = schemaDrift
	d.onHubActivated = onActivated
	d.mu.Unlock()
}

// NewHubAnalyticsDrainer builds a Hub-only drainer that ships finalized usage
// batches and periodic empty meter-health heartbeats to the Scion Hub from
// hubOutbox via sink.
func NewHubAnalyticsDrainer(hubOutbox *AnalyticsOutbox, sink *HubUsageSink, resolve HubIDResolver) *AnalyticsDrainer {
	if sink == nil {
		return nil
	}
	if hubOutbox == nil {
		hubOutbox = sink.outbox
	}
	return newHubAnalyticsDrainerWithSend(hubOutbox, sink, sink.SendBatch, resolve)
}

func newHubAnalyticsDrainerWithSend(hubOutbox *AnalyticsOutbox, sink *HubUsageSink, batchSend AnalyticsBatchSendFunc, resolve HubIDResolver) *AnalyticsDrainer {
	hubOutbox.markHubActive()
	bounded := func(ctx context.Context, lines [][]byte) (BatchSendOutcome, error) {
		sctx, cancel := context.WithTimeout(ctx, analyticsSendTimeout)
		defer cancel()
		return batchSend(sctx, lines)
	}
	return &AnalyticsDrainer{
		hubOutbox:         hubOutbox,
		hubSink:           sink,
		batchSend:         bounded,
		resolve:           resolve,
		interval:          sink.cfg.drainInterval(),
		heartbeatInterval: sink.cfg.heartbeatInterval(),
		now:               time.Now,
	}
}

// NewDualAnalyticsDrainer builds a drainer that drains both the GCP Cloud
// Logging outbox (gcpOutbox, gcpSend) and the independent Hub usage outbox
// (sink.outbox, sink) with completely isolated caps, cursors, dead-letter
// files, health counters, and retry backoffs.
func NewDualAnalyticsDrainer(gcpOutbox *AnalyticsOutbox, gcpSend AnalyticsSendFunc, sink *HubUsageSink, resolve HubIDResolver) *AnalyticsDrainer {
	if sink == nil {
		return NewAnalyticsDrainer(gcpOutbox, gcpSend, resolve)
	}
	if gcpSend == nil || gcpOutbox == nil {
		return NewHubAnalyticsDrainer(sink.outbox, sink, resolve)
	}
	return newDualAnalyticsDrainerWithSend(gcpOutbox, gcpSend, sink.outbox, sink, sink.SendBatch, resolve)
}

func newDualAnalyticsDrainerWithSend(gcpOutbox *AnalyticsOutbox, gcpSend AnalyticsSendFunc, hubOutbox *AnalyticsOutbox, sink *HubUsageSink, hubBatchSend AnalyticsBatchSendFunc, resolve HubIDResolver) *AnalyticsDrainer {
	hubOutbox.markHubActive()
	boundedGCP := func(ctx context.Context, line []byte) error {
		sctx, cancel := context.WithTimeout(ctx, analyticsSendTimeout)
		defer cancel()
		return gcpSend(sctx, line)
	}
	boundedHub := func(ctx context.Context, lines [][]byte) (BatchSendOutcome, error) {
		sctx, cancel := context.WithTimeout(ctx, analyticsSendTimeout)
		defer cancel()
		return hubBatchSend(sctx, lines)
	}
	interval := analyticsDrainInterval
	if di := sink.cfg.drainInterval(); di > 0 && di < interval {
		interval = di
	}
	return &AnalyticsDrainer{
		outbox:            gcpOutbox,
		send:              boundedGCP,
		hubOutbox:         hubOutbox,
		hubSink:           sink,
		batchSend:         boundedHub,
		resolve:           resolve,
		interval:          interval,
		heartbeatInterval: sink.cfg.heartbeatInterval(),
		now:               time.Now,
	}
}

// HubOutbox returns the Hub usage outbox managed by this drainer, or nil when
// the Hub sink is not active.
func (d *AnalyticsDrainer) HubOutbox() *AnalyticsOutbox {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.hubOutbox
}

func (d *AnalyticsDrainer) primaryOutbox() *AnalyticsOutbox {
	if d.outbox != nil {
		return d.outbox
	}
	return d.HubOutbox()
}

// ObservedHubID returns the hub ID this process resolved, whatever the
// outbox pin says, or "" before the first successful resolution. It is
// process-fixed: the drainer resolves once per process and never replaces
// the value (a later hub change in a live process is not a supported path
// and is not re-resolved). It is published only after PinHubID returned,
// i.e. after the pin comparison and any relocated/hub_mismatch write
// completed under the outbox lock, so a LedgerSnapshot taken after a
// non-empty read sees the relocation marker if one was written. It is the
// attribution source for per-process events (telemetry.health).
func (d *AnalyticsDrainer) ObservedHubID() string {
	if d == nil {
		return ""
	}
	d.hubMu.Lock()
	defer d.hubMu.Unlock()
	return d.observedHub
}

// VerifiedHubID returns this process's usage finalize authorization: the
// resolved hub ID when it equals the outbox pin, else "" (unresolved, or
// relocated: it is never set on a mismatch). Published under the same rule
// as ObservedHubID. finalize still re-checks the persisted relocation
// marker under the outbox lock.
func (d *AnalyticsDrainer) VerifiedHubID() string {
	if d == nil {
		return ""
	}
	d.hubMu.Lock()
	defer d.hubMu.Unlock()
	return d.verifiedHub
}

// Start runs the drain loop in a goroutine until Stop.
func (d *AnalyticsDrainer) Start(ctx context.Context) {
	if d == nil {
		return
	}
	ctx, d.cancel = context.WithCancel(ctx)
	d.done = make(chan struct{})
	go func() {
		defer close(d.done)
		ticker := time.NewTicker(d.interval)
		defer ticker.Stop()
		for {
			d.RunOnce(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// Stop ends the loop, then makes one final bounded drain attempt within
// ctx (graceful stop). Events still pending stay in the outbox.
func (d *AnalyticsDrainer) Stop(ctx context.Context) {
	if d == nil {
		return
	}
	if d.cancel != nil {
		d.cancel()
		<-d.done
	}
	d.mu.Lock()
	d.retryAfter = time.Time{} // the final attempt ignores backoff
	d.hubRetryAfter = time.Time{}
	d.mu.Unlock()
	d.runOnce(ctx, true)
}

// RunOnce performs one drain step: resolve the hub ID once per process
// (pinning it, verifying it against the pin, or recording a relocation),
// finalize if verified,
// send (unless backing off), and occasionally expire the index. It returns
// the number of lines sent.
func (d *AnalyticsDrainer) RunOnce(ctx context.Context) int {
	if d == nil {
		return 0
	}
	return d.runOnce(ctx, false)
}

// runOnce is RunOnce; final (the stop pass) skips index expiry so the pass
// stays within the stop budget.
func (d *AnalyticsDrainer) runOnce(ctx context.Context, final bool) int {
	if ctx.Err() != nil {
		return 0
	}
	// Resolve once per process, even when a pin is on disk: provisional
	// lines are finalized only under a hub this process verified, and a
	// home now under a different hub is recorded as relocated once a
	// later resolution confirms it (see analyticsRelocationConfirm).
	primary := d.primaryOutbox()
	if !d.resolved && d.resolve != nil && primary != nil && !d.now().Before(d.resolveAfter) {
		rctx, cancel := context.WithTimeout(ctx, analyticsHubIDTimeout)
		id, err := d.resolve(rctx)
		cancel()
		pinned := ""
		if err == nil {
			pinned, err = primary.PinHubID(id)
		}
		confirmed := err == nil && pinned == id
		if err == nil && pinned != id {
			switch {
			case d.candidate != id:
				// First sighting (or a different hub): wait for agreement.
				d.candidate, d.candidateSince = id, d.now()
				d.resolveAfter = d.candidateSince.Add(analyticsRelocationConfirm)
				log.Debug("Analytics drainer: hub %q differs from the pinned hub %q; confirming", id, pinned)
			case d.now().Before(d.candidateSince.Add(analyticsRelocationConfirm)):
				d.resolveAfter = d.candidateSince.Add(analyticsRelocationConfirm)
			default:
				err = primary.recordRelocation(id)
				confirmed = err == nil
			}
		}
		switch {
		case err != nil:
			// Back off like sends do (1 s .. 5 min), so an agent whose hub
			// never reports hub_id does not poll /healthz every interval.
			if d.resolveBackoff == 0 {
				d.resolveBackoff = analyticsBackoffMin
			} else if d.resolveBackoff *= 2; d.resolveBackoff > analyticsBackoffMax {
				d.resolveBackoff = analyticsBackoffMax
			}
			d.resolveAfter = d.now().Add(d.resolveBackoff)
			log.Debug("Analytics drainer: hub ID not resolved yet (retry in %s): %v", d.resolveBackoff, err)
		case !confirmed:
			// Unconfirmed mismatch: nothing published, finalize held; the
			// next resolution is scheduled above.
		default:
			d.resolveBackoff, d.resolveAfter = 0, time.Time{}
			d.candidate, d.candidateSince = "", time.Time{}
			d.resolved = true
			if pinned == id {
				if hubOutbox := d.HubOutbox(); hubOutbox != nil && hubOutbox != primary {
					_, _ = hubOutbox.PinHubID(id)
				}
			}
			d.hubMu.Lock()
			d.observedHub = id
			if pinned == id {
				d.verifiedHub = id
			}
			d.hubMu.Unlock()
			if pinned != id {
				log.Warn("Analytics outbox relocated: pinned hub %q, current hub %q; new usage is refused and provisional lines are held", pinned, id)
			}
		}
	}
	finalizeOutbox := func(o *AnalyticsOutbox, verified string) {
		if o == nil || verified == "" {
			return
		}
		// Re-checked under the outbox lock (another process may have
		// recorded a relocation since this one verified).
		if _, err := o.finalize(ctx, verified); err != nil {
			switch {
			case errors.Is(err, ErrAnalyticsOutboxRelocated), errors.Is(err, ErrAnalyticsHubNotVerified), ctx.Err() != nil:
				log.Debug("Analytics drainer: finalize: %v", err)
			default:
				// Unexpected: finalize (rotation or commit) failed (counted
				// in FinalizeFailures); warn at most every
				// analyticsBackoffMax.
				o.finalizeFailures.Add(1)
				if now := d.now(); d.lastFinalizeWarn.IsZero() || now.Sub(d.lastFinalizeWarn) >= analyticsBackoffMax {
					d.lastFinalizeWarn = now
					log.Warn("Analytics drainer: finalize failed; provisional usage stays held: %v", err)
				}
			}
		}
	}
	if verified := d.VerifiedHubID(); verified != "" {
		finalizeOutbox(d.outbox, verified)
		if hubOutbox := d.HubOutbox(); hubOutbox != nil && !d.HubRevoked() {
			finalizeOutbox(hubOutbox, verified)
		}
	}

	// Periodic re-probe while Hub sink is inactive in dual-run mode (O1).
	// When capabilities turn positive, open the sibling Hub outbox, mark it
	// active on disk, and attach it to the native producer without disrupting
	// the GCP drain.
	if !final && d.hubSink == nil && d.send != nil && ctx.Err() == nil {
		d.mu.Lock()
		probe := d.hubProbe
		revoked := d.hubRevoked
		pInterval := d.probeInterval
		due := probe != nil && !revoked && (d.lastProbe.IsZero() || d.now().Sub(d.lastProbe) >= pInterval)
		if due {
			d.lastProbe = d.now()
		}
		supSigs := d.supportedSignals
		driftFn := d.schemaDrift
		onAct := d.onHubActivated
		hubDir := d.hubOutboxDir
		hubOwner := d.hubOutboxOwner
		d.mu.Unlock()
		if due {
			pctx, cancel := context.WithTimeout(ctx, analyticsHubIDTimeout)
			cfg, perr := probe(pctx)
			cancel()
			if perr == nil && cfg != nil && cfg.Post != nil {
				if hubDir == "" && d.outbox != nil {
					hubDir = filepath.Join(filepath.Dir(d.outbox.Dir()), HubAnalyticsOutboxDirName)
				}
				hubOutbox, oerr := openAnalyticsOutboxTolerant(hubDir, hubOwner)
				if oerr != nil {
					log.Warn("Analytics drainer: opening Hub outbox %q after re-probe failed: %v", hubDir, oerr)
				} else {
					c := *cfg
					if c.SupportedSignals == nil && supSigs != nil {
						c.SupportedSignals = supSigs
					}
					if c.SchemaDriftDetected == nil && driftFn != nil {
						c.SchemaDriftDetected = driftFn
					}
					sink := NewHubUsageSink(hubOutbox, c)
					if sink != nil {
						hubOutbox.markHubActive()
						if verified := d.VerifiedHubID(); verified != "" {
							_, _ = hubOutbox.PinHubID(verified)
							finalizeOutbox(hubOutbox, verified)
						}
						d.mu.Lock()
						d.hubOutbox = hubOutbox
						d.hubSink = sink
						d.mu.Unlock()
						d.batchSend = func(bctx context.Context, lines [][]byte) (BatchSendOutcome, error) {
							sctx, scancel := context.WithTimeout(bctx, analyticsSendTimeout)
							defer scancel()
							return sink.SendBatch(sctx, lines)
						}
						d.heartbeatInterval = sink.cfg.heartbeatInterval()
						if onAct != nil {
							onAct(c, hubOutbox, sink)
						}
						log.Info("Analytics drainer: Hub usage ingest activated after re-probe")
					}
				}
			}
		}
	}

	sent := 0
	// Drain the Hub sink before the GCP Cloud Logging sink. The Hub is the
	// authoritative usage ledger; GCP export is secondary. During dual-run
	// shutdown (final == true with both sinks active), the Hub sink gets first
	// claim on analyticsHubStopShareNumerator/analyticsHubStopShareDenominator
	// (70%) of the remaining stop budget, leaving 30% guaranteed for GCP if
	// the Hub stalls, while any unused Hub time flows to GCP via ctx.
	if hubOutbox := d.HubOutbox(); hubOutbox != nil && d.hubSink != nil {
		d.mu.Lock()
		revoked := d.hubRevoked
		hubWaiting := d.now().Before(d.hubRetryAfter)
		onRevoked := d.onHubRevoked
		d.mu.Unlock()
		if !revoked {
			hubCtx := ctx
			var hubCancel context.CancelFunc
			var hubBudget time.Duration
			if final {
				dualRun := d.outbox != nil && d.send != nil
				hubBudget = hubFinalPhaseBudget(ctx, dualRun)
				if dualRun {
					hubCtx, hubCancel = context.WithTimeout(ctx, hubBudget)
				}
			}

			var hubDrainErr error
			hubSent := 0
			revokeHub := func() {
				log.Warn("Analytics drainer: Hub usage ingest returned 404 (capability revoked); disabling Hub sink")
				hubOutbox.clearHubActive()
				if onRevoked != nil {
					onRevoked()
				}
				d.mu.Lock()
				d.hubRevoked = true
				d.hubBackoff, d.hubRetryAfter = 0, time.Time{}
				d.mu.Unlock()
			}
			if !hubWaiting {
				drainCtx := hubCtx
				var drainCancel context.CancelFunc
				if final && hubBudget > 0 {
					hbReserve := hubFinalHeartbeatReserve(hubBudget)
					drainBudget := hubBudget - hbReserve
					if drainBudget <= 0 {
						drainBudget = hubBudget / 2
					}
					drainCtx, drainCancel = context.WithTimeout(hubCtx, drainBudget)
				}
				hubSent, hubDrainErr = hubOutbox.DrainBatch(drainCtx, d.hubSink.cfg.maxBatchEvents(), d.hubSink.EffectiveMaxBatchBytes(), d.batchSend)
				if drainCancel != nil {
					drainCancel()
				}
				sent += hubSent
				if errors.Is(hubDrainErr, ErrUsageIngestRevoked) {
					revokeHub()
					revoked = true
				} else {
					d.mu.Lock()
					if hubDrainErr != nil && drainCtx.Err() == nil && ctx.Err() == nil {
						if d.hubBackoff == 0 {
							d.hubBackoff = analyticsBackoffMin
						} else if d.hubBackoff *= 2; d.hubBackoff > analyticsBackoffMax {
							d.hubBackoff = analyticsBackoffMax
						}
						d.hubRetryAfter = d.now().Add(d.hubBackoff)
						log.Debug("Analytics drainer: hub send failed, retrying in %s: %v", d.hubBackoff, hubDrainErr)
					} else if hubDrainErr == nil {
						d.hubBackoff, d.hubRetryAfter = 0, time.Time{}
						if hubSent > 0 {
							now := d.now()
							d.lastHubContact = now
							d.lastHubDrain = now
						}
					}
					d.mu.Unlock()
				}
			}

			if !revoked && hubCtx.Err() == nil {
				if final {
					finalState := telemetrycontract.MeterFinalStateFinal
					if hubDrainErr != nil {
						finalState = telemetrycontract.MeterFinalStateIncomplete
					} else if pending, _, perr := hubOutbox.pendingBytes(); perr != nil || pending > 0 {
						finalState = telemetrycontract.MeterFinalStateIncomplete
					}
					hctx, cancel := context.WithTimeout(hubCtx, analyticsSendTimeout)
					if herr := d.hubSink.SendHeartbeat(hctx, finalState); herr != nil {
						log.Debug("Analytics drainer: final heartbeat failed: %v", herr)
					}
					cancel()
				} else if !hubWaiting && hubDrainErr == nil && d.heartbeatInterval > 0 {
					d.mu.Lock()
					due := d.lastHubContact.IsZero() || d.now().Sub(d.lastHubContact) >= d.heartbeatInterval
					d.mu.Unlock()
					if due {
						hctx, cancel := context.WithTimeout(hubCtx, analyticsSendTimeout)
						herr := d.hubSink.SendHeartbeat(hctx, "")
						cancel()
						if errors.Is(herr, ErrUsageIngestRevoked) {
							revokeHub()
						} else {
							d.mu.Lock()
							if herr != nil && ctx.Err() == nil {
								if d.hubBackoff == 0 {
									d.hubBackoff = analyticsBackoffMin
								} else if d.hubBackoff *= 2; d.hubBackoff > analyticsBackoffMax {
									d.hubBackoff = analyticsBackoffMax
								}
								d.hubRetryAfter = d.now().Add(d.hubBackoff)
								log.Debug("Analytics drainer: heartbeat failed, retrying in %s: %v", d.hubBackoff, herr)
							} else if herr == nil {
								d.hubBackoff, d.hubRetryAfter = 0, time.Time{}
								d.lastHubContact = d.now()
							}
							d.mu.Unlock()
						}
					}
				}
			}
			if hubCancel != nil {
				hubCancel()
			}
		}
	}

	if d.outbox != nil && d.send != nil && ctx.Err() == nil {
		d.mu.Lock()
		waiting := d.now().Before(d.retryAfter)
		d.mu.Unlock()
		if !waiting {
			n, err := d.outbox.Drain(ctx, d.send)
			sent += n
			d.mu.Lock()
			if err != nil && ctx.Err() == nil {
				if d.backoff == 0 {
					d.backoff = analyticsBackoffMin
				} else if d.backoff *= 2; d.backoff > analyticsBackoffMax {
					d.backoff = analyticsBackoffMax
				}
				d.retryAfter = d.now().Add(d.backoff)
				log.Debug("Analytics drainer: send failed, retrying in %s: %v", d.backoff, err)
			} else if err == nil {
				d.backoff, d.retryAfter = 0, time.Time{}
			}
			d.mu.Unlock()
		}
	}

	if !final && ctx.Err() == nil && d.now().Sub(d.lastExpiry) >= analyticsIndexExpiryEvery {
		expiredOK := true
		if d.outbox != nil {
			if _, err := d.outbox.ExpireIndex(); err != nil {
				expiredOK = false
			}
		}
		if hubOutbox := d.HubOutbox(); hubOutbox != nil {
			if _, err := hubOutbox.ExpireIndex(); err != nil {
				expiredOK = false
			}
		}
		if expiredOK {
			d.lastExpiry = d.now()
		}
	}
	return sent
}

// HubRevoked reports whether the Hub usage sink was disabled mid-run after
// receiving HTTP 404 (capability revoked).
func (d *AnalyticsDrainer) HubRevoked() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.hubRevoked
}

// Backoff reports the primary sink's current retry backoff (zero when healthy):
// GCP's backoff when GCP is active (GCP-only or dual-run), or the Hub sink's
// backoff in Hub-only mode.
func (d *AnalyticsDrainer) Backoff() time.Duration {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.send != nil {
		return d.backoff
	}
	return d.hubBackoff
}

// HubBackoff reports the Hub usage sink's current retry backoff (zero when healthy).
func (d *AnalyticsDrainer) HubBackoff() time.Duration {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.hubBackoff
}

// LastHubDrain reports the timestamp of the last successful non-empty batch
// drain to the Hub, or the zero time if no batch has drained yet.
func (d *AnalyticsDrainer) LastHubDrain() time.Time {
	if d == nil {
		return time.Time{}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastHubDrain
}

// HubState reports the Hub usage sink's current operational state ("active",
// "degraded", or "revoked"), or "" when the Hub sink is not configured.
func (d *AnalyticsDrainer) HubState() string {
	if d == nil {
		return ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.hubRevoked {
		return telemetrycontract.HubAnalyticsStateRevoked
	}
	if d.hubSink == nil {
		return ""
	}
	if d.hubBackoff > 0 {
		return telemetrycontract.HubAnalyticsStateDegraded
	}
	return telemetrycontract.HubAnalyticsStateActive
}

// hubFinalPhaseBudget returns the shutdown budget allocated to the Hub sink's
// final pass (batch drain + terminal heartbeat). In dual-run mode it takes
// 70% of the remaining ctx deadline (or 70% of analyticsStopBudget when ctx
// has no deadline); in Hub-only mode it uses the full remaining budget.
func hubFinalPhaseBudget(ctx context.Context, dualRun bool) time.Duration {
	remaining := analyticsStopBudget
	if dl, ok := ctx.Deadline(); ok {
		remaining = time.Until(dl)
	}
	if remaining <= 0 {
		return 0
	}
	if !dualRun {
		return remaining
	}
	return remaining * analyticsHubStopShareNumerator / analyticsHubStopShareDenominator
}

// hubFinalHeartbeatReserve returns the portion of hubBudget reserved for the
// terminal meter-health heartbeat so a slow batch drain cannot starve it.
func hubFinalHeartbeatReserve(hubBudget time.Duration) time.Duration {
	reserve := hubBudget / analyticsHubFinalHeartbeatShareDivisor
	if reserve < analyticsHubFinalHeartbeatMinBudget {
		reserve = analyticsHubFinalHeartbeatMinBudget
	}
	if reserve > analyticsHubFinalHeartbeatMaxBudget {
		reserve = analyticsHubFinalHeartbeatMaxBudget
	}
	return reserve
}

// analyticsHubEndpoint is the hub endpoint sciontool uses, from the agent
// environment.
func analyticsHubEndpoint() string {
	for _, key := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return ""
}

// analyticsState is the pipeline's analytics wiring: the outbox, the
// native producer handed to the UsageDeriver, and the drainer. Its zero
// value is "disabled"; only start enables it.
type analyticsState struct {
	native         *UsageAnalytics
	sqliteNative   *UsageAnalytics
	drainer        *AnalyticsDrainer
	disabledReason string
	// send counts drainer send retries for WP3 diagnostics (health.go).
	send            *countingSend
	hubOutboxFailed bool

	// agentHome, outboxDir, hubOutboxDir and owner locate the outboxes and
	// tailer state under the AGENT home and name their owner
	// (Pipeline.SetAnalyticsOutbox); resolver overrides the hub ID resolver
	// (tests).
	agentHome         string
	outboxDir         string
	hubOutboxDir      string
	owner             *OutboxOwner
	resolver          HubIDResolver
	hubSinkCfg        *HubUsageSinkConfig
	hubProbe          HubUsageProbeFunc
	hubProbeInterval  time.Duration
	supportedSignals  func() []string
	schemaDrift       func() bool
	onSuppressChanged func(bool)
	isSuppressed      func() bool
}

// SetHubUsageSink configures the pipeline's analytics outbox to drain usage
// batches and meter-health heartbeats to the Scion Hub. When set, the outbox
// is enabled even without a GCP exporter. Must be called before Start.
func (p *Pipeline) SetHubUsageSink(cfg HubUsageSinkConfig) {
	if p == nil || cfg.Post == nil {
		return
	}
	c := cfg
	p.analytics.hubSinkCfg = &c
}

// SetHubUsageProbe registers a periodic capability re-probe used in dual-run
// (when a GCP exporter is active) after a negative initial probe at init (O1).
// When a later re-probe succeeds, the Hub sink activates alongside GCP with
// cursor.hub without interrupting GCP delivery. Hub-only mode (no GCP exporter)
// never late-activates after a negative init probe so the Hub does not receive
// empty heartbeats for a run whose producers were not wired to the outbox.
// Must be called before Start.
func (p *Pipeline) SetHubUsageProbe(probe HubUsageProbeFunc) {
	if p == nil || probe == nil {
		return
	}
	p.analytics.hubProbe = probe
}

// SetHubUsageProbeInterval overrides the periodic Hub capability re-probe
// interval (defaults to 60s). Intended for tests.
func (p *Pipeline) SetHubUsageProbeInterval(d time.Duration) {
	if p == nil {
		return
	}
	p.analytics.hubProbeInterval = d
}

// SetAnalyticsOutbox points the pipeline's analytics outbox at the agent
// home and names the agent user that must own it. sciontool init calls it
// before Start: init runs as root with HOME=/root, while hook processes run
// as the agent user with HOME=<agent home>, so the outbox must be resolved
// from the agent home explicitly and its files chowned to the agent user.
func (p *Pipeline) SetAnalyticsOutbox(agentHome string, uid, gid int) {
	if p == nil {
		return
	}
	p.analytics.agentHome = agentHome
	p.analytics.outboxDir = AnalyticsOutboxDirFor(agentHome)
	p.analytics.hubOutboxDir = HubAnalyticsOutboxDirFor(agentHome)
	p.analytics.owner = &OutboxOwner{UID: uid, GID: gid}
}

// SetHubIDResolver configures the hub ID resolver used by the analytics
// drainer and health reporter. When unset, both fall back to
// NewHealthzHubIDResolver.
func (p *Pipeline) SetHubIDResolver(resolve HubIDResolver) {
	if p == nil {
		return
	}
	p.analytics.resolver = resolve
}

// AnalyticsOutboxDir returns the outbox directory the pipeline uses (for
// WP3 diagnostics), or "".
func (p *Pipeline) AnalyticsOutboxDir() string {
	if p == nil {
		return ""
	}
	return p.analytics.native.Dir() // nil-safe; the evidence dir when failed
}

func (a *analyticsState) resolvedDirs() (gcpDir, hubDir string) {
	gcpDir = a.outboxDir
	if gcpDir == "" {
		// Not set by init (tests, or a pipeline run as the agent user):
		// this process's own home.
		home, _ := os.UserHomeDir()
		gcpDir = AnalyticsOutboxDirFor(home)
	}
	hubDir = a.hubOutboxDir
	if hubDir == "" && gcpDir != "" {
		hubDir = filepath.Join(filepath.Dir(gcpDir), HubAnalyticsOutboxDirName)
	}
	return gcpDir, hubDir
}

// start enables analytics when either the Hub usage sink or the GCP exporter
// is active. Any failure leaves analytics disabled with a reason; it never
// fails the pipeline.
func (a *analyticsState) start(ctx context.Context, gcp *GCPExporter) {
	if a.drainer != nil {
		return
	}
	if a.hubSinkCfg == nil && gcp == nil {
		// Hub-only mode (no GCP exporter) does not late-activate after a
		// negative init probe: without producers wired at startup, heartbeats
		// would falsely advertise a healthy idle meter while usage is dropped.
		// Clear any stale active marker from an earlier run without creating
		// the Hub outbox directory.
		_, hubDir := a.resolvedDirs()
		ClearHubOutboxActive(hubDir)
		a.disabledReason = AnalyticsDisabledNoGCPExporter
		return
	}
	a.startDrainer(ctx, gcp)
}

func (a *analyticsState) probeInterval() time.Duration {
	if a.hubProbeInterval > 0 {
		return a.hubProbeInterval
	}
	return analyticsHubProbeInterval
}

func (a *analyticsState) startDrainer(ctx context.Context, gcp *GCPExporter) {
	onSuppressChanged := a.onSuppressChanged
	setSuppress := func(v bool) {
		if onSuppressChanged != nil {
			onSuppressChanged(v)
		}
	}
	gcpDir, hubDir := a.resolvedDirs()
	resolver := a.resolver
	if resolver == nil && a.hubSinkCfg != nil && a.hubSinkCfg.HubID != "" {
		hubID := a.hubSinkCfg.HubID
		resolver = func(context.Context) (string, error) { return hubID, nil }
	}
	if resolver == nil {
		resolver = NewHealthzHubIDResolver(analyticsHubEndpoint(), nil)
	}

	switch {
	case a.hubSinkCfg != nil && gcp != nil:
		// Dual-run (design §6 B2 / §11 P10.1): keep container-direct Cloud
		// Logging active alongside the Hub sink using two completely
		// independent outboxes (analytics-outbox and analytics-outbox-hub).
		gcpOutbox, err := openAnalyticsOutboxTolerant(gcpDir, a.owner)
		if err != nil {
			a.disabledReason = "outbox_unavailable"
			a.native = newFailedUsageAnalytics(gcpDir, a.owner, err, telemetrycontract.UsageSourceNative)
			log.Error("Usage analytics disabled: %v", err)
			return
		}
		a.send = newCountingSend(gcp.SendAnalytics)
		hubOutbox, hubErr := openAnalyticsOutboxTolerant(hubDir, a.owner)
		if hubErr != nil {
			a.hubOutboxFailed = true
			log.Error("Hub usage analytics outbox unavailable (continuing GCP-only): %v", hubErr)
			a.native = NewUsageAnalytics(gcpOutbox, telemetrycontract.UsageSourceNative)
			a.sqliteNative = NewUsageAnalytics(gcpOutbox, telemetrycontract.UsageSourceSQLiteNative)
			a.drainer = NewAnalyticsDrainer(gcpOutbox, a.send.Send, resolver)
		} else {
			a.hubOutboxFailed = false
			cfg := *a.hubSinkCfg
			if cfg.SupportedSignals == nil && a.supportedSignals != nil {
				cfg.SupportedSignals = a.supportedSignals
			}
			if cfg.SchemaDriftDetected == nil && a.schemaDrift != nil {
				cfg.SchemaDriftDetected = a.schemaDrift
			}
			applySuppress := func(v bool) {
				setSuppress(v)
				hubOutbox.setHubSuppress(v)
			}
			cfg.OnSuppressChanged = applySuppress
			if cfg.IsSuppressed == nil && a.isSuppressed != nil {
				cfg.IsSuppressed = a.isSuppressed
			}
			sink := NewHubUsageSink(hubOutbox, cfg)
			a.native = NewDualUsageAnalytics(gcpOutbox, hubOutbox, telemetrycontract.UsageSourceNative)
			a.native.setRunAndActivity(cfg.RunID, sink.NoteActivity)
			a.native.setDirectOTelSuppressedFn(a.isSuppressed)
			a.sqliteNative = NewDualUsageAnalytics(gcpOutbox, hubOutbox, telemetrycontract.UsageSourceSQLiteNative)
			a.sqliteNative.setRunAndActivity(cfg.RunID, sink.NoteActivity)
			a.sqliteNative.setDirectOTelSuppressedFn(a.isSuppressed)
			hubSend := newCountingBatchSend(sink.SendBatch)
			a.drainer = newDualAnalyticsDrainerWithSend(gcpOutbox, a.send.Send, hubOutbox, sink, hubSend.SendBatch, resolver)
			applySuppress(cfg.SuppressDirectOTel)
			native := a.native
			sqliteNative := a.sqliteNative
			a.drainer.onHubRevoked = func() {
				applySuppress(false)
				native.clearHubOutbox()
				sqliteNative.clearHubOutbox()
			}
		}
	case a.hubSinkCfg != nil:
		// Hub-only mode (no GCP exporter): only analytics-outbox-hub exists.
		hubOutbox, err := openAnalyticsOutboxTolerant(hubDir, a.owner)
		if err != nil {
			a.hubOutboxFailed = true
			a.disabledReason = "outbox_unavailable"
			a.native = newFailedUsageAnalytics(hubDir, a.owner, err, telemetrycontract.UsageSourceNative)
			log.Error("Usage analytics disabled: %v", err)
			return
		}
		a.hubOutboxFailed = false
		cfg := *a.hubSinkCfg
		if cfg.SupportedSignals == nil && a.supportedSignals != nil {
			cfg.SupportedSignals = a.supportedSignals
		}
		if cfg.SchemaDriftDetected == nil && a.schemaDrift != nil {
			cfg.SchemaDriftDetected = a.schemaDrift
		}
		applySuppress := func(v bool) {
			setSuppress(v)
			hubOutbox.setHubSuppress(v)
		}
		cfg.OnSuppressChanged = applySuppress
		if cfg.IsSuppressed == nil && a.isSuppressed != nil {
			cfg.IsSuppressed = a.isSuppressed
		}
		sink := NewHubUsageSink(hubOutbox, cfg)
		a.native = NewUsageAnalytics(hubOutbox, telemetrycontract.UsageSourceNative)
		a.native.setRunAndActivity(cfg.RunID, sink.NoteActivity)
		a.native.setDirectOTelSuppressedFn(a.isSuppressed)
		a.sqliteNative = NewUsageAnalytics(hubOutbox, telemetrycontract.UsageSourceSQLiteNative)
		a.sqliteNative.setRunAndActivity(cfg.RunID, sink.NoteActivity)
		a.sqliteNative.setDirectOTelSuppressedFn(a.isSuppressed)
		a.send = newCountingBatchSend(sink.SendBatch)
		a.drainer = newHubAnalyticsDrainerWithSend(hubOutbox, sink, a.send.SendBatch, resolver)
		applySuppress(cfg.SuppressDirectOTel)
		native := a.native
		sqliteNative := a.sqliteNative
		a.drainer.onHubRevoked = func() {
			applySuppress(false)
			native.detach()
			sqliteNative.detach()
		}
	default:
		// GCP-only mode (probe negative): never create analytics-outbox-hub;
		// clear any stale active marker from an earlier run.
		ClearHubOutboxActive(hubDir)
		gcpOutbox, err := openAnalyticsOutboxTolerant(gcpDir, a.owner)
		if err != nil {
			a.disabledReason = "outbox_unavailable"
			a.native = newFailedUsageAnalytics(gcpDir, a.owner, err, telemetrycontract.UsageSourceNative)
			log.Error("Usage analytics disabled: %v", err)
			return
		}
		a.native = NewUsageAnalytics(gcpOutbox, telemetrycontract.UsageSourceNative)
		a.native.setDirectOTelSuppressedFn(a.isSuppressed)
		a.sqliteNative = NewUsageAnalytics(gcpOutbox, telemetrycontract.UsageSourceSQLiteNative)
		a.sqliteNative.setDirectOTelSuppressedFn(a.isSuppressed)
		a.send = newCountingSend(gcp.SendAnalytics)
		a.drainer = NewAnalyticsDrainer(gcpOutbox, a.send.Send, resolver)
		a.drainer.hubOutboxDir = hubDir
		a.drainer.hubOutboxOwner = a.owner
		if a.hubProbe != nil {
			native := a.native
			sqliteNative := a.sqliteNative
			a.drainer.onHubRevoked = func() {
				setSuppress(false)
				native.clearHubOutbox()
				sqliteNative.clearHubOutbox()
			}
			a.drainer.SetHubProbe(a.hubProbe, a.probeInterval(), a.supportedSignals, a.schemaDrift, func(cfg HubUsageSinkConfig, hubOutbox *AnalyticsOutbox, sink *HubUsageSink) {
				applySuppress := func(v bool) {
					setSuppress(v)
					hubOutbox.setHubSuppress(v)
				}
				sink.cfg.OnSuppressChanged = applySuppress
				if sink.cfg.IsSuppressed == nil && a.isSuppressed != nil {
					sink.cfg.IsSuppressed = a.isSuppressed
				}
				if native != nil {
					native.setHubOutbox(hubOutbox, cfg.RunID, sink.NoteActivity)
				}
				if sqliteNative != nil {
					sqliteNative.setHubOutbox(hubOutbox, cfg.RunID, sink.NoteActivity)
				}
				applySuppress(cfg.SuppressDirectOTel)
			})
		}
	}
	a.drainer.Start(ctx)
	a.disabledReason = ""
}

// stop ends the drainer after one final drain bounded by its own sub-deadline:
// at most analyticsStopBudget and at most half of ctx's remaining time, so the
// exporter shutdown that follows keeps its share.
func (a *analyticsState) stop(ctx context.Context) {
	if a.drainer == nil {
		return
	}
	budget := analyticsStopBudget
	if dl, ok := ctx.Deadline(); ok {
		if half := time.Until(dl) / 2; half < budget {
			budget = half
		}
	}
	if budget < 0 {
		budget = 0
	}
	sctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	a.drainer.Stop(sctx)
	a.drainer = nil
}

// AnalyticsLedger returns the primary analytics outbox's on-disk ledger
// snapshot and in-process counters; ok is false when analytics is disabled.
func (p *Pipeline) AnalyticsLedger() (disk OutboxLedger, mem AnalyticsOutboxStats, ok bool, err error) {
	if p == nil || p.analytics.native == nil {
		return OutboxLedger{}, AnalyticsOutboxStats{}, false, nil
	}
	o := p.analytics.native.Outbox()
	if o == nil {
		return OutboxLedger{}, AnalyticsOutboxStats{}, false, nil
	}
	disk, err = o.LedgerSnapshot()
	return disk, o.MemoryStats(), true, err
}

// HubAnalyticsLedger returns the Hub usage outbox's on-disk ledger snapshot
// and in-process counters; ok is false when the Hub sink is not active.
func (p *Pipeline) HubAnalyticsLedger() (disk OutboxLedger, mem AnalyticsOutboxStats, ok bool, err error) {
	if p == nil || p.analytics.drainer == nil {
		return OutboxLedger{}, AnalyticsOutboxStats{}, false, nil
	}
	o := p.analytics.drainer.HubOutbox()
	if o == nil {
		return OutboxLedger{}, AnalyticsOutboxStats{}, false, nil
	}
	disk, err = o.LedgerSnapshot()
	return disk, o.MemoryStats(), true, err
}
