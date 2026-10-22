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
	// analyticsSendTimeout bounds one send so a stalled RPC cannot hold
	// the drain loop until Stop.
	analyticsSendTimeout = 30 * time.Second
	// analyticsStopBudget caps the final drain at shutdown; it also never
	// takes more than half of the caller's remaining budget, so the
	// exporter flush that follows keeps its share.
	analyticsStopBudget = 5 * time.Second
)

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

	interval time.Duration
	now      func() time.Time

	mu         sync.Mutex
	backoff    time.Duration
	retryAfter time.Time
	lastExpiry time.Time

	// resolveBackoff/resolveAfter pace hub ID resolution while unresolved
	// (drainer goroutine only).
	resolveBackoff time.Duration
	resolveAfter   time.Time

	cancel context.CancelFunc
	done   chan struct{}
}

// NewAnalyticsDrainer builds a drainer for outbox. resolve may be nil when
// the hub ID is already pinned.
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
	d.mu.Unlock()
	d.runOnce(ctx, true)
}

// RunOnce performs one drain step: pin the hub ID if needed, finalize,
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
	if d.outbox.PinnedHubID() == "" && d.resolve != nil && !d.now().Before(d.resolveAfter) {
		rctx, cancel := context.WithTimeout(ctx, analyticsHubIDTimeout)
		id, err := d.resolve(rctx)
		cancel()
		if err == nil {
			_, err = d.outbox.PinHubID(id)
		}
		if err != nil {
			// Back off like sends do (1 s .. 5 min), so an agent whose hub
			// never reports hub_id does not poll /healthz every interval.
			if d.resolveBackoff == 0 {
				d.resolveBackoff = analyticsBackoffMin
			} else if d.resolveBackoff *= 2; d.resolveBackoff > analyticsBackoffMax {
				d.resolveBackoff = analyticsBackoffMax
			}
			d.resolveAfter = d.now().Add(d.resolveBackoff)
			log.Debug("Analytics drainer: hub ID not resolved yet (retry in %s): %v", d.resolveBackoff, err)
		} else {
			d.resolveBackoff, d.resolveAfter = 0, time.Time{}
		}
	}
	if d.outbox.PinnedHubID() != "" {
		if _, err := d.outbox.FinalizeContext(ctx); err != nil {
			log.Debug("Analytics drainer: finalize: %v", err)
		}
	}

	sent := 0
	d.mu.Lock()
	waiting := d.now().Before(d.retryAfter)
	d.mu.Unlock()
	if !waiting {
		n, err := d.outbox.Drain(ctx, d.send)
		sent = n
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

	if !final && ctx.Err() == nil && d.now().Sub(d.lastExpiry) >= analyticsIndexExpiryEvery {
		if _, err := d.outbox.ExpireIndex(); err == nil {
			d.lastExpiry = d.now()
		}
	}
	return sent
}

// Backoff reports the current retry backoff (zero when healthy).
func (d *AnalyticsDrainer) Backoff() time.Duration {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
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

	// outboxDir and owner locate the outbox under the AGENT home and name
	// its owner (Pipeline.SetAnalyticsOutbox); resolver overrides the hub
	// ID resolver (tests).
	outboxDir string
	owner     *OutboxOwner
	resolver  HubIDResolver
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

// AnalyticsOutboxDir returns the outbox directory the pipeline uses (for
// WP3 diagnostics), or "".
func (p *Pipeline) AnalyticsOutboxDir() string {
	if p == nil {
		return ""
	}
	return p.analytics.native.Dir() // nil-safe; the evidence dir when failed
}

// start enables analytics when the GCP exporter is active. Any failure
// leaves analytics disabled with a reason; it never fails the pipeline.
func (a *analyticsState) start(ctx context.Context, gcp *GCPExporter) {
	if a.drainer != nil {
		return
	}
	if gcp == nil {
		a.disabledReason = AnalyticsDisabledNoGCPExporter
		return
	}
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
	if resolver == nil {
		resolver = NewHealthzHubIDResolver(analyticsHubEndpoint(), nil)
	}
	a.native = NewUsageAnalytics(outbox, telemetrycontract.UsageSourceNative)
	a.drainer = NewAnalyticsDrainer(outbox, gcp.SendAnalytics, resolver)
	a.drainer.Start(ctx)
	a.disabledReason = ""
}

// stop ends the drainer after one final drain bounded by its own
// sub-deadline: at most analyticsStopBudget and at most half of ctx's
// remaining time, so the exporter shutdown that follows keeps its share.
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
