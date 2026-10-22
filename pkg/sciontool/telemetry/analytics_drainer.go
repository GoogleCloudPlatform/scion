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
		defer resp.Body.Close()
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

	cancel context.CancelFunc
	done   chan struct{}
}

// NewAnalyticsDrainer builds a drainer for outbox. resolve may be nil when
// the hub ID is already pinned.
func NewAnalyticsDrainer(outbox *AnalyticsOutbox, send AnalyticsSendFunc, resolve HubIDResolver) *AnalyticsDrainer {
	return &AnalyticsDrainer{
		outbox:   outbox,
		send:     send,
		resolve:  resolve,
		interval: analyticsDrainInterval,
		now:      time.Now,
	}
}

// Start runs the drain loop in a goroutine until Stop.
func (d *AnalyticsDrainer) Start(ctx context.Context) {
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
	if d.cancel != nil {
		d.cancel()
		<-d.done
	}
	d.mu.Lock()
	d.retryAfter = time.Time{} // the final attempt ignores backoff
	d.mu.Unlock()
	d.RunOnce(ctx)
}

// RunOnce performs one drain step: pin the hub ID if needed, finalize,
// send (unless backing off), and occasionally expire the index. It returns
// the number of lines sent.
func (d *AnalyticsDrainer) RunOnce(ctx context.Context) int {
	if ctx.Err() != nil {
		return 0
	}
	if d.outbox.PinnedHubID() == "" && d.resolve != nil {
		rctx, cancel := context.WithTimeout(ctx, analyticsHubIDTimeout)
		id, err := d.resolve(rctx)
		cancel()
		if err != nil {
			log.Debug("Analytics drainer: hub ID not resolved yet: %v", err)
		} else if _, err := d.outbox.PinHubID(id); err != nil {
			log.Debug("Analytics drainer: pinning hub ID: %v", err)
		}
	}
	if d.outbox.PinnedHubID() != "" {
		if _, err := d.outbox.Finalize(); err != nil {
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

	if d.now().Sub(d.lastExpiry) >= analyticsIndexExpiryEvery {
		if _, err := d.outbox.ExpireIndex(); err == nil {
			d.lastExpiry = d.now()
		}
	}
	return sent
}

// Backoff reports the current retry backoff (zero when healthy).
func (d *AnalyticsDrainer) Backoff() time.Duration {
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
	// send counts drainer send retries for WP3 diagnostics (health.go).
	send *countingSend

	// outboxDir and resolver override the defaults (tests).
	outboxDir string
	resolver  HubIDResolver
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
		dir = DefaultAnalyticsOutboxDir()
	}
	outbox, err := OpenAnalyticsOutbox(dir)
	if err != nil {
		a.disabledReason = "outbox_unavailable"
		log.Error("Usage analytics disabled: %v", err)
		return
	}
	resolver := a.resolver
	if resolver == nil {
		resolver = NewHealthzHubIDResolver(analyticsHubEndpoint(), nil)
	}
	a.native = NewUsageAnalytics(outbox, telemetrycontract.UsageSourceNative)
	a.send = newCountingSend(gcp.SendAnalytics)
	a.drainer = NewAnalyticsDrainer(outbox, a.send.Send, resolver)
	a.drainer.Start(ctx)
	a.disabledReason = ""
}

// stop ends the drainer after one final bounded drain within ctx.
func (a *analyticsState) stop(ctx context.Context) {
	if a.drainer == nil {
		return
	}
	a.drainer.Stop(ctx)
	a.drainer = nil
}
