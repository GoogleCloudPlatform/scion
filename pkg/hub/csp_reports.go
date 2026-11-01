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
	"net"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/GoogleCloudPlatform/scion/pkg/cspreport"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// ---------------------------------------------------------------------------
// Browser policy reports: POST /api/v1/csp-reports.
//
// The route needs no sign-in: browsers send reports on their own. The
// handler (pkg/cspreport) only logs: each report becomes one csp_violation
// record on the hub.csp-reports logger, and nothing is persisted in
// application storage. The report's sample is logged only with
// server.hub.csp_report_raw_samples (off by default). Repeats of one
// violation are collapsed into one record per cspReportDedupeInterval and
// answered with 204; a csp_violation_repeats record then counts them and
// shows the latest (cspreport.Dedupe).
//
// A per-client read ceiling, checked before a request is read, bounds the
// work one client can cause; a request past it is answered 204 unread.
// Two token buckets bound the log volume, reusing geExchangeRateLimiter and
// geExchangeClientIP (ge_exchange_ratelimit.go) as externalBearerRateLimiter
// does: one per client, charged once per request that holds a report to
// log (a request of repeats only is not charged) or that cannot be read,
// and one for the whole hub, charged per record written. A refused report
// is answered 204 like any other, since a browser cannot act on a refusal;
// refusals are counted (cspreport.Handler.Counts) and summarized in at most
// one csp_violation_refused record a minute.
// ---------------------------------------------------------------------------

const (
	// cspReportRatePerSecond and cspReportBurst are the per-client-IP
	// budget, in requests that hold a report to log. Some browsers send
	// each report as its own request, so one page load can send dozens at
	// once; the burst lets one page load's reports fit.
	cspReportRatePerSecond = 0.5
	cspReportBurst         = 60

	// cspReportReadRatePerSecond and cspReportReadBurst are the per-client
	// read ceiling, in requests: a request past it is answered 204 without
	// being read. It is far above what a page sends (the burst covers
	// several page loads at once, and the rate covers one page that sends
	// a report on every render), so it bounds the work one client can
	// cause without losing the repeat counts of a busy page.
	cspReportReadRatePerSecond = 20.0
	cspReportReadBurst         = 200

	// cspReportGlobalRatePerSecond and cspReportGlobalBurst cap the
	// records the hub as a whole writes (every record, the dropped-count
	// record included, takes one token).
	cspReportGlobalRatePerSecond = 10.0
	cspReportGlobalBurst         = 100

	// cspReportGlobalKey is the single bucket key of the hub-wide limiter.
	cspReportGlobalKey = "*"

	// cspReportDedupeInterval and cspReportDedupeMaxKeys are the
	// cspreport.Dedupe settings: each distinct violation is logged at most
	// once per interval, and at most this many are tracked.
	cspReportDedupeInterval = 10 * time.Minute
	cspReportDedupeMaxKeys  = 512
)

// cspReportLimiter is the per-client-IP and hub-wide limit of the report
// endpoint.
type cspReportLimiter struct {
	read        *geExchangeRateLimiter
	perIP       *geExchangeRateLimiter
	global      *geExchangeRateLimiter
	trustedNets []*net.IPNet
}

// newCSPReportLimiter builds the limiter. trustedProxies is parsed once,
// as newExternalBearerRateLimiter does.
func newCSPReportLimiter(trustedProxies []string) *cspReportLimiter {
	read := newGEExchangeRateLimiter()
	read.rate = cspReportReadRatePerSecond
	read.burst = cspReportReadBurst
	perIP := newGEExchangeRateLimiter()
	perIP.rate = cspReportRatePerSecond
	perIP.burst = cspReportBurst
	global := newGEExchangeRateLimiter()
	global.rate = cspReportGlobalRatePerSecond
	global.burst = cspReportGlobalBurst
	return &cspReportLimiter{
		read:        read,
		perIP:       perIP,
		global:      global,
		trustedNets: parseTrustedProxies(trustedProxies),
	}
}

// AllowRead reports whether r's client is within its read ceiling.
func (l *cspReportLimiter) AllowRead(r *http.Request) bool {
	ok, _ := l.read.Allow(cspReportClientKey(geExchangeClientIP(r, l.trustedNets)))
	return ok
}

// Allow reports whether r's client is within its per-request budget.
func (l *cspReportLimiter) Allow(r *http.Request) bool {
	ok, _ := l.perIP.Allow(cspReportClientKey(geExchangeClientIP(r, l.trustedNets)))
	return ok
}

// AllowRecord takes one token of the hub-wide record budget.
func (l *cspReportLimiter) AllowRecord() bool {
	ok, _ := l.global.Allow(cspReportGlobalKey)
	return ok
}

// cspReportClientKey is the per-client bucket key: the IPv4 address, or
// the /64 of an IPv6 address, since one IPv6 host usually holds a whole
// /64. Anything else is used as is.
func cspReportClientKey(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() != nil {
		return ip
	}
	return parsed.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// StartCleanup evicts stale per-IP buckets until ctx is cancelled. Without
// it the bounded bucket map fills after maxEntries distinct client IPs and
// refuses every new one.
func (l *cspReportLimiter) StartCleanup(ctx context.Context) {
	l.read.StartCleanup(ctx)
	l.perIP.StartCleanup(ctx)
}

// newCSPReportHandler returns the report endpoint with its limiter.
// rawSamples (server.hub.csp_report_raw_samples, off by default) adds each
// report's sample to its record.
func newCSPReportHandler(limiter *cspReportLimiter, rawSamples bool) *cspreport.Handler {
	return &cspreport.Handler{
		AllowRead:   limiter.AllowRead,
		Allow:       limiter.Allow,
		AllowRecord: limiter.AllowRecord,
		Dedupe:      cspreport.NewDedupe(cspReportDedupeInterval, cspReportDedupeMaxKeys),
		Log:         logging.Subsystem("hub.csp-reports"),
		RawSamples:  rawSamples,
	}
}

// RegisterCSPReportMetrics exports the report endpoint's running totals
// (cspreport.Handler.Counts) as three OTel counters, read at each
// collection: scion.hub.csp_reports.refused_requests,
// scion.hub.csp_reports.refused_reports and scion.hub.csp_reports.collapsed.
// They carry no attributes. A Server built without New has no report
// endpoint and registers nothing.
func (s *Server) RegisterCSPReportMetrics(mp metric.MeterProvider) error {
	if mp == nil {
		return errors.New("otel csp report metrics: nil MeterProvider")
	}
	h := s.cspReports
	if h == nil {
		return nil
	}
	meter := mp.Meter(instrumentationScope)
	refusedRequests, err := meter.Int64ObservableCounter("scion.hub.csp_reports.refused_requests",
		metric.WithUnit("{request}"),
		metric.WithDescription("Browser policy report requests refused before their reports were read"))
	if err != nil {
		return err
	}
	refusedReports, err := meter.Int64ObservableCounter("scion.hub.csp_reports.refused_reports",
		metric.WithUnit("{report}"),
		metric.WithDescription("Browser policy reports refused by the per-client or the hub-wide budget"))
	if err != nil {
		return err
	}
	collapsed, err := meter.Int64ObservableCounter("scion.hub.csp_reports.collapsed",
		metric.WithUnit("{report}"),
		metric.WithDescription("Browser policy reports collapsed as repeats"))
	if err != nil {
		return err
	}
	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		c := h.Counts()
		o.ObserveInt64(refusedRequests, c.RefusedRequests)
		o.ObserveInt64(refusedReports, c.RefusedReports)
		o.ObserveInt64(collapsed, c.Collapsed)
		return nil
	}, refusedRequests, refusedReports, collapsed)
	return err
}

// handleCSPReport handles POST /api/v1/csp-reports. A Server built without
// New has no handler and answers 404.
func (s *Server) handleCSPReport(w http.ResponseWriter, r *http.Request) {
	if s.cspReports == nil {
		http.NotFound(w, r)
		return
	}
	s.cspReports.ServeHTTP(w, r)
}
