/*
Copyright 2026 The Scion Authors.
*/

package telemetry

// analytics_drainer.go runs in the long-lived sciontool pipeline and moves
// agent.usage events from the outbox to Cloud Logging (WP2 design section
// 2): it pins the hub ID from the hub's public /healthz, finalizes staging
// lines into ready lines, sends them in order with bounded backoff, and
// expires the occurrence index. It runs only when the GCP exporter is
// active; otherwise analytics is disabled (AnalyticsDisabledNoGCPExporter).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// AnalyticsDisabledNoGCPExporter is the analytics_disabled_reason when no
// GCP exporter is configured (generic OTLP or cloud disabled). Analytics
// events are then neither written nor sent.
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
	outbox  *AnalyticsOutbox
	send    AnalyticsSendFunc
	resolve HubIDResolver

	// hubSink, when non-nil, drains finalized usage batches and periodic empty
	// meter-health heartbeats to the Scion Hub. When both send (GCP) and
	// hubSink are set (dual-run fan-out), they track independent cursors
	// ("cursor" and hubCursorName) and independent backoffs so a failure in
	// one sink never blocks or dead-letters the other.
	hubSink           *HubUsageSink
	batchSend         AnalyticsBatchSendFunc
	hubCursorName     string
	heartbeatInterval time.Duration
	lastHubContact    time.Time

	// hubProbe, when set on a GCP-only drainer whose initial probe was
	// negative, periodically re-probes the Hub and activates dual-run when
	// capabilities turn positive without disrupting the GCP drain.
	hubProbe         HubUsageProbeFunc
	probeInterval    time.Duration
	lastProbe        time.Time
	supportedSignals func() []string
	onHubActivated   func(HubUsageSinkConfig, *HubUsageSink)

	interval time.Duration
	now      func() time.Time

	mu            sync.Mutex
	backoff       time.Duration
	retryAfter    time.Time
	hubBackoff    time.Duration
	hubRetryAfter time.Time
	hubRevoked    bool
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

// NewAnalyticsDrainer builds a drainer for outbox. With a nil resolve the
// drainer never verifies a hub, so it finalizes nothing (provisional lines
// are held); already-final ready lines still drain.
func NewAnalyticsDrainer(outbox *AnalyticsOutbox, send AnalyticsSendFunc, resolve HubIDResolver) *AnalyticsDrainer {
	outbox.SetPrimaryCursor("cursor")
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

// SetHubProbe configures periodic capability re-probing on a drainer whose
// initial probe was negative. When probe returns a non-nil config, the Hub
// sink is activated in dual-run mode (with cursor.hub) alongside any existing
// GCP drain without interrupting GCP delivery.
func (d *AnalyticsDrainer) SetHubProbe(probe HubUsageProbeFunc, interval time.Duration, supportedSignals func() []string, onActivated func(HubUsageSinkConfig, *HubUsageSink)) {
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
	d.onHubActivated = onActivated
	d.mu.Unlock()
}

// NewHubAnalyticsDrainer builds a drainer that ships finalized usage batches
// and periodic empty meter-health heartbeats to the Scion Hub via sink.
func NewHubAnalyticsDrainer(outbox *AnalyticsOutbox, sink *HubUsageSink, resolve HubIDResolver) *AnalyticsDrainer {
	if sink == nil {
		return nil
	}
	return newHubAnalyticsDrainerWithSend(outbox, sink, sink.SendBatch, resolve)
}

func newHubAnalyticsDrainerWithSend(outbox *AnalyticsOutbox, sink *HubUsageSink, batchSend AnalyticsBatchSendFunc, resolve HubIDResolver) *AnalyticsDrainer {
	outbox.SetPrimaryCursor(HubAnalyticsCursorName)
	bounded := func(ctx context.Context, lines [][]byte) (BatchSendOutcome, error) {
		sctx, cancel := context.WithTimeout(ctx, analyticsSendTimeout)
		defer cancel()
		return batchSend(sctx, lines)
	}
	return &AnalyticsDrainer{
		outbox:            outbox,
		hubSink:           sink,
		batchSend:         bounded,
		hubCursorName:     HubAnalyticsCursorName,
		resolve:           resolve,
		interval:          sink.cfg.drainInterval(),
		heartbeatInterval: sink.cfg.heartbeatInterval(),
		now:               time.Now,
	}
}

// NewDualAnalyticsDrainer builds a drainer that fans out every finalized event
// to BOTH the GCP Cloud Logging sink (gcpSend, primary "cursor") and the Hub
// usage sink (sink, independent "cursor.hub") with isolated retry backoffs so
// neither sink can block or dead-letter the other.
func NewDualAnalyticsDrainer(outbox *AnalyticsOutbox, gcpSend AnalyticsSendFunc, sink *HubUsageSink, resolve HubIDResolver) *AnalyticsDrainer {
	if sink == nil {
		return NewAnalyticsDrainer(outbox, gcpSend, resolve)
	}
	if gcpSend == nil {
		return NewHubAnalyticsDrainer(outbox, sink, resolve)
	}
	return newDualAnalyticsDrainerWithSend(outbox, gcpSend, sink, sink.SendBatch, resolve)
}

func newDualAnalyticsDrainerWithSend(outbox *AnalyticsOutbox, gcpSend AnalyticsSendFunc, sink *HubUsageSink, hubBatchSend AnalyticsBatchSendFunc, resolve HubIDResolver) *AnalyticsDrainer {
	outbox.EnableSecondaryCursor(HubAnalyticsCursorName)
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
		outbox:            outbox,
		send:              boundedGCP,
		hubSink:           sink,
		batchSend:         boundedHub,
		hubCursorName:     HubAnalyticsCursorName,
		resolve:           resolve,
		interval:          interval,
		heartbeatInterval: sink.cfg.heartbeatInterval(),
		now:               time.Now,
	}
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
	if !d.resolved && d.resolve != nil && !d.now().Before(d.resolveAfter) {
		rctx, cancel := context.WithTimeout(ctx, analyticsHubIDTimeout)
		id, err := d.resolve(rctx)
		cancel()
		pinned := ""
		if err == nil {
			pinned, err = d.outbox.PinHubID(id)
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
				err = d.outbox.recordRelocation(id)
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
	if verified := d.VerifiedHubID(); verified != "" {
		// Re-checked under the outbox lock (another process may have
		// recorded a relocation since this one verified).
		if _, err := d.outbox.finalize(ctx, verified); err != nil {
			switch {
			case errors.Is(err, ErrAnalyticsOutboxRelocated), errors.Is(err, ErrAnalyticsHubNotVerified), ctx.Err() != nil:
				log.Debug("Analytics drainer: finalize: %v", err)
			default:
				// Unexpected: finalize (rotation or commit) failed (counted
				// in FinalizeFailures); warn at most every
				// analyticsBackoffMax.
				d.outbox.finalizeFailures.Add(1)
				if now := d.now(); d.lastFinalizeWarn.IsZero() || now.Sub(d.lastFinalizeWarn) >= analyticsBackoffMax {
					d.lastFinalizeWarn = now
					log.Warn("Analytics drainer: finalize failed; provisional usage stays held: %v", err)
				}
			}
		}
	}

	// Periodic re-probe while Hub sink is inactive (O1). Run before the GCP
	// drain so that if capabilities turn positive on this tick, cursor.hub is
	// registered before GCP deletes the ready files it drains.
	if !final && d.hubSink == nil && ctx.Err() == nil {
		d.mu.Lock()
		probe := d.hubProbe
		revoked := d.hubRevoked
		pInterval := d.probeInterval
		due := probe != nil && !revoked && (d.lastProbe.IsZero() || d.now().Sub(d.lastProbe) >= pInterval)
		if due {
			d.lastProbe = d.now()
		}
		supSigs := d.supportedSignals
		onAct := d.onHubActivated
		d.mu.Unlock()
		if due {
			pctx, cancel := context.WithTimeout(ctx, analyticsHubIDTimeout)
			cfg, perr := probe(pctx)
			cancel()
			if perr == nil && cfg != nil && cfg.Post != nil {
				c := *cfg
				if c.SupportedSignals == nil && supSigs != nil {
					c.SupportedSignals = supSigs
				}
				sink := NewHubUsageSink(d.outbox, c)
				if sink != nil {
					if d.send != nil {
						d.outbox.EnableSecondaryCursor(HubAnalyticsCursorName)
					} else {
						d.outbox.SetPrimaryCursor(HubAnalyticsCursorName)
					}
					d.hubSink = sink
					d.batchSend = func(bctx context.Context, lines [][]byte) (BatchSendOutcome, error) {
						sctx, scancel := context.WithTimeout(bctx, analyticsSendTimeout)
						defer scancel()
						return sink.SendBatch(sctx, lines)
					}
					d.hubCursorName = HubAnalyticsCursorName
					d.heartbeatInterval = sink.cfg.heartbeatInterval()
					if onAct != nil {
						onAct(c, sink)
					}
					log.Info("Analytics drainer: Hub usage ingest activated after re-probe")
				}
			}
		}
	}

	sent := 0
	if d.send != nil {
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

	if d.hubSink != nil {
		d.mu.Lock()
		revoked := d.hubRevoked
		hubWaiting := d.now().Before(d.hubRetryAfter)
		d.mu.Unlock()
		if !revoked {
			var hubDrainErr error
			hubSent := 0
			hubCursor := d.hubCursorName
			if hubCursor == "" {
				hubCursor = HubAnalyticsCursorName
			}
			revokeHub := func() {
				log.Warn("Analytics drainer: Hub usage ingest returned 404 (capability revoked); disabling Hub sink")
				d.mu.Lock()
				d.hubRevoked = true
				d.hubBackoff, d.hubRetryAfter = 0, time.Time{}
				d.mu.Unlock()
				if d.send != nil {
					d.outbox.DisableSecondaryCursor(hubCursor)
				}
			}
			if !hubWaiting {
				hubSent, hubDrainErr = d.outbox.DrainBatchCursor(ctx, hubCursor, d.hubSink.cfg.maxBatchEvents(), d.hubSink.EffectiveMaxBatchBytes(), d.batchSend)
				sent += hubSent
				if errors.Is(hubDrainErr, ErrUsageIngestRevoked) {
					revokeHub()
					revoked = true
				} else {
					d.mu.Lock()
					if hubDrainErr != nil && ctx.Err() == nil {
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
							d.lastHubContact = d.now()
						}
					}
					d.mu.Unlock()
				}
			}

			if !revoked && ctx.Err() == nil {
				if final {
					finalState := telemetrycontract.MeterFinalStateFinal
					if hubDrainErr != nil {
						finalState = telemetrycontract.MeterFinalStateIncomplete
					} else if pending, perr := d.outbox.pendingBytesForCursor(hubCursor); perr != nil || pending > 0 {
						finalState = telemetrycontract.MeterFinalStateIncomplete
					}
					hctx, cancel := context.WithTimeout(ctx, analyticsSendTimeout)
					if herr := d.hubSink.SendHeartbeat(hctx, finalState); herr != nil {
						log.Debug("Analytics drainer: final heartbeat failed: %v", herr)
					}
					cancel()
				} else if !hubWaiting && hubDrainErr == nil && d.heartbeatInterval > 0 {
					d.mu.Lock()
					due := d.lastHubContact.IsZero() || d.now().Sub(d.lastHubContact) >= d.heartbeatInterval
					d.mu.Unlock()
					if due {
						hctx, cancel := context.WithTimeout(ctx, analyticsSendTimeout)
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
		}
	}

	if !final && ctx.Err() == nil && d.now().Sub(d.lastExpiry) >= analyticsIndexExpiryEvery {
		if _, err := d.outbox.ExpireIndex(); err == nil {
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

// Backoff reports the current retry backoff (zero when healthy). When both
// GCP and Hub sinks are active, it returns the larger active backoff.
func (d *AnalyticsDrainer) Backoff() time.Duration {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.hubBackoff > d.backoff {
		return d.hubBackoff
	}
	return d.backoff
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
	drainer        *AnalyticsDrainer
	disabledReason string
	// send counts drainer send retries for WP3 diagnostics (health.go).
	send *countingSend

	// outboxDir and owner locate the outbox under the AGENT home and name
	// its owner (Pipeline.SetAnalyticsOutbox); resolver overrides the hub
	// ID resolver (tests).
	outboxDir        string
	owner            *OutboxOwner
	resolver         HubIDResolver
	hubSinkCfg       *HubUsageSinkConfig
	hubProbe         HubUsageProbeFunc
	hubProbeInterval time.Duration
	supportedSignals func() []string
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
	p.analytics.outboxDir = AnalyticsOutboxDirFor(agentHome)
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
	dir := a.outboxDir
	if dir == "" {
		// Not set by init (tests, or a pipeline run as the agent user):
		// this process's own home.
		home, _ := os.UserHomeDir()
		dir = AnalyticsOutboxDirFor(home)
	}
	outbox, err := openAnalyticsOutboxTolerant(dir, a.owner)
	if err != nil {
		// No outbox: nothing can be delivered, but every accepted
		// increment still leaves evidence (append_failed in dir when it
		// exists, else the per-process invalid counter).
		a.disabledReason = "outbox_unavailable"
		a.native = newFailedUsageAnalytics(dir, a.owner, err, telemetrycontract.UsageSourceNative)
		log.Error("Usage analytics disabled: %v", err)
		return
	}
	resolver := a.resolver
	if resolver == nil && a.hubSinkCfg != nil && a.hubSinkCfg.HubID != "" {
		hubID := a.hubSinkCfg.HubID
		resolver = func(context.Context) (string, error) { return hubID, nil }
	}
	if resolver == nil {
		resolver = NewHealthzHubIDResolver(analyticsHubEndpoint(), nil)
	}
	a.native = NewUsageAnalytics(outbox, telemetrycontract.UsageSourceNative)
	switch {
	case a.hubSinkCfg != nil && gcp != nil:
		// Dual-run fan-out (design §6 B2 / §9.3): keep container-direct Cloud
		// Logging active alongside the Hub sink using independent cursors.
		cfg := *a.hubSinkCfg
		if cfg.SupportedSignals == nil && a.supportedSignals != nil {
			cfg.SupportedSignals = a.supportedSignals
		}
		sink := NewHubUsageSink(outbox, cfg)
		a.native.runID = cfg.RunID
		a.native.onRecord = sink.NoteActivity
		a.send = newCountingSend(gcp.SendAnalytics)
		hubSend := newCountingBatchSend(sink.SendBatch)
		a.drainer = newDualAnalyticsDrainerWithSend(outbox, a.send.Send, sink, hubSend.SendBatch, resolver)
	case a.hubSinkCfg != nil:
		cfg := *a.hubSinkCfg
		if cfg.SupportedSignals == nil && a.supportedSignals != nil {
			cfg.SupportedSignals = a.supportedSignals
		}
		sink := NewHubUsageSink(outbox, cfg)
		a.native.runID = cfg.RunID
		a.native.onRecord = sink.NoteActivity
		a.send = newCountingBatchSend(sink.SendBatch)
		a.drainer = newHubAnalyticsDrainerWithSend(outbox, sink, a.send.SendBatch, resolver)
	default:
		a.send = newCountingSend(gcp.SendAnalytics)
		a.drainer = NewAnalyticsDrainer(outbox, a.send.Send, resolver)
		if a.hubProbe != nil {
			native := a.native
			a.drainer.SetHubProbe(a.hubProbe, a.probeInterval(), a.supportedSignals, func(cfg HubUsageSinkConfig, sink *HubUsageSink) {
				if native != nil {
					native.setRunAndActivity(cfg.RunID, sink.NoteActivity)
				}
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

// AnalyticsLedger returns the analytics outbox's on-disk ledger snapshot
// and in-process counters; ok is false when analytics is disabled.
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
