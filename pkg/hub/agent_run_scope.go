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
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// AgentRunIDHeader carries the agent's launch id (SCION_LAUNCH_ID) on
// agent-token requests. It is only ever compared with the token's run; it
// never makes a request acceptable that the token alone would not be.
const AgentRunIDHeader = "X-Scion-Run-Id"

// agentRunScopeMode is how the hub treats the run an agent token was issued
// for.
type agentRunScopeMode int

const (
	// agentRunScopeOff: the run is not looked at.
	agentRunScopeOff agentRunScopeMode = iota
	// agentRunScopeObserve: the run is checked, and the outcome logged and
	// counted; no request is refused.
	agentRunScopeObserve
	// agentRunScopeEnforce: a token whose run check fails is refused. It
	// cannot be selected through configuration (ParseAgentRunScope).
	agentRunScopeEnforce
)

func (m agentRunScopeMode) String() string {
	switch m {
	case agentRunScopeOff:
		return "off"
	case agentRunScopeObserve:
		return "observe"
	case agentRunScopeEnforce:
		return "enforce"
	}
	return fmt.Sprintf("mode(%d)", int(m))
}

// AgentRunScope is the server.auth.agent_run_scope setting. The zero value
// is "off". Only ParseAgentRunScope builds other values.
type AgentRunScope struct {
	mode agentRunScopeMode
	// legacyUntil, if non-zero, is when tokens issued without a run stop
	// being accepted by the check.
	legacyUntil time.Time
}

// ParseAgentRunScope parses server.auth.agent_run_scope ("", "off" or
// "observe") and server.auth.agent_run_scope_legacy_until ("" or an
// RFC 3339 time). Any other mode is an error.
func ParseAgentRunScope(mode, legacyUntil string) (AgentRunScope, error) {
	var s AgentRunScope
	switch mode {
	case "", "off":
		s.mode = agentRunScopeOff
	case "observe":
		s.mode = agentRunScopeObserve
	default:
		return AgentRunScope{}, fmt.Errorf("invalid server.auth.agent_run_scope %q: must be \"off\" or \"observe\"", mode)
	}
	if legacyUntil != "" {
		t, err := time.Parse(time.RFC3339, legacyUntil)
		if err != nil {
			return AgentRunScope{}, fmt.Errorf("invalid server.auth.agent_run_scope_legacy_until %q: must be an RFC 3339 time", legacyUntil)
		}
		s.legacyUntil = t
	}
	return s, nil
}

// String returns the mode name.
func (s AgentRunScope) String() string { return s.mode.String() }

// Agent run-scope check outcomes, recorded in logs and metrics only. None
// of them reaches a client: every refusal is the same response
// (writeAgentTokenRefused).
const (
	runScopeOutcomeBound       = "bound"        // token run is the agent's current run
	runScopeOutcomeUnscoped    = "unscoped"     // token without a run, accepted
	runScopeOutcomeSuperseded  = "superseded"   // token run is not the agent's current run
	runScopeOutcomeAgentGone   = "agent_gone"   // no live agent row
	runScopeOutcomeUnbound     = "unbound"      // token run not backed by its credential
	runScopeOutcomeLegacyEnded = "legacy_ended" // token without a run, after legacy_until
	runScopeOutcomeHeader      = "header"       // AgentRunIDHeader differs from the token run
	runScopeOutcomeHello       = "hello"        // conduit Hello launch id differs from the token run
	runScopeOutcomeUnavailable = "unavailable"  // the agent row could not be read
)

// Sources of a run-scope check.
const (
	runScopeSourceHTTP    = "http"
	runScopeSourceConduit = "conduit"
)

// runScopeVerdict is what a check decided.
type runScopeVerdict int

const (
	runScopeAllow runScopeVerdict = iota
	runScopeDeny
	runScopeUnavailable
)

// agentRunReader reads the agent row a token names.
type agentRunReader interface {
	GetAgent(ctx context.Context, id string) (*store.Agent, error)
}

// agentRunScopeMetrics counts run-scope check outcomes.
type agentRunScopeMetrics interface {
	RecordAgentRunScope(source, outcome, mode, routeClass string)
}

// runScopeRequest is what a check knows about the request, for the
// decision (header) and the log.
type runScopeRequest struct {
	// header is the AgentRunIDHeader value ("" when absent).
	header     string
	method     string
	path       string
	remoteAddr string
	// route is the matched route pattern. When empty it is resolved from
	// raw, only for a request that is logged.
	route string
	raw   *http.Request
	// hello is the endpoint incarnation a conduit Hello presented.
	hello string
}

// runScopeRequestFrom describes r.
func runScopeRequestFrom(r *http.Request) runScopeRequest {
	return runScopeRequest{
		header:     r.Header.Get(AgentRunIDHeader),
		method:     r.Method,
		path:       r.URL.Path,
		remoteAddr: r.RemoteAddr,
		raw:        r,
	}
}

// Route classes, the bounded route label of the run-scope metric.
const (
	runScopeRouteTokenRefresh = "token_refresh"
	runScopeRouteAgentStatus  = "agent_status"
	runScopeRouteAgent        = "agent"
	runScopeRouteProject      = "project"
	runScopeRouteConduit      = "conduit"
	runScopeRouteOther        = "other"
)

// runScopeRouteClass maps a request path to a route class.
func runScopeRouteClass(path string) string {
	switch {
	case strings.HasPrefix(path, "/api/v1/agents/"):
		switch {
		case strings.HasSuffix(path, "/token/refresh"):
			return runScopeRouteTokenRefresh
		case strings.HasSuffix(path, "/status"):
			return runScopeRouteAgentStatus
		}
		return runScopeRouteAgent
	case path == "/api/v1/conduit" || strings.HasPrefix(path, "/api/v1/conduit/"):
		return runScopeRouteConduit
	case strings.HasPrefix(path, "/api/v1/projects/"):
		return runScopeRouteProject
	}
	return runScopeRouteOther
}

// agentRunScopeChecker checks the run an agent token was issued for. It
// exists only when the mode is not off: with the mode off, nothing reads
// the token's run or the agent row.
type agentRunScopeChecker struct {
	mode        agentRunScopeMode
	legacyUntil time.Time
	agents      agentRunReader
	now         func() time.Time
	log         *slog.Logger
	metrics     atomic.Pointer[agentRunScopeMetrics]
	dedup       *runScopeLogDedup
	summary     runScopeLogSummary
	// route resolves a logged request's route pattern; nil leaves it
	// empty.
	route func(*http.Request) string
}

// newAgentRunScopeChecker returns the checker for setting, or nil when
// the mode is off.
func newAgentRunScopeChecker(setting AgentRunScope, agents agentRunReader, log *slog.Logger) *agentRunScopeChecker {
	if setting.mode == agentRunScopeOff || agents == nil {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	return &agentRunScopeChecker{
		mode:        setting.mode,
		legacyUntil: setting.legacyUntil,
		agents:      agents,
		now:         time.Now,
		log:         log,
		dedup:       newRunScopeLogDedup(runScopeLogDedupSize, runScopeLogDedupWindow),
	}
}

// agentTokenCredentialState is what the credential-status evaluation found
// for a token.
type agentTokenCredentialState struct {
	// evaluated is false when no credential status was evaluated.
	evaluated bool
	// cred is the token's credential row; nil for a token without one.
	cred *store.AgentCredential
}

// check decides a request made with claims.
func (c *agentRunScopeChecker) check(ctx context.Context, claims *AgentTokenClaims, cs agentTokenCredentialState, req runScopeRequest, source string) runScopeVerdict {
	outcome, currentRunID := c.outcome(ctx, claims, cs, req.header)
	c.record(ctx, claims, source, outcome, currentRunID, req)
	switch outcome {
	case runScopeOutcomeBound, runScopeOutcomeUnscoped:
		return runScopeAllow
	case runScopeOutcomeUnavailable:
		if c.mode == agentRunScopeEnforce {
			return runScopeUnavailable
		}
		return runScopeAllow
	default:
		if c.mode == agentRunScopeEnforce {
			return runScopeDeny
		}
		return runScopeAllow
	}
}

// outcome classifies a token and returns the agent's current run when it
// was read. The token's run must be backed by its credential row; a token
// with a run must name the agent's current run; a token without one is
// accepted until legacy_until. The header can only cause a refusal: with a
// run it must equal the token's run, without one it must equal the
// agent's current run.
func (c *agentRunScopeChecker) outcome(ctx context.Context, claims *AgentTokenClaims, cs agentTokenCredentialState, header string) (outcome, currentRunID string) {
	runID := claims.RunID
	switch {
	case !cs.evaluated || cs.cred == nil:
		// No credential row backs the token's run.
		if runID != "" {
			return runScopeOutcomeUnbound, ""
		}
	case cs.cred.RunID != runID:
		return runScopeOutcomeUnbound, ""
	}
	if runID == "" {
		if !c.legacyUntil.IsZero() && !c.now().Before(c.legacyUntil) {
			return runScopeOutcomeLegacyEnded, ""
		}
		if header == "" {
			return runScopeOutcomeUnscoped, ""
		}
		agent, err := c.agents.GetAgent(ctx, claims.Subject)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return runScopeOutcomeUnscoped, ""
		case err != nil:
			c.log.ErrorContext(ctx, "agent token run check: agent lookup failed",
				"agent_id", claims.Subject, "error", err)
			return runScopeOutcomeUnavailable, ""
		case !agent.DeletedAt.IsZero():
			return runScopeOutcomeUnscoped, ""
		case agent.RunID != header:
			return runScopeOutcomeHeader, agent.RunID
		}
		return runScopeOutcomeUnscoped, agent.RunID
	}
	if header != "" && header != runID {
		return runScopeOutcomeHeader, ""
	}
	agent, err := c.agents.GetAgent(ctx, claims.Subject)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return runScopeOutcomeAgentGone, ""
	case err != nil:
		c.log.ErrorContext(ctx, "agent token run check: agent lookup failed",
			"agent_id", claims.Subject, "error", err)
		return runScopeOutcomeUnavailable, ""
	case !agent.DeletedAt.IsZero():
		return runScopeOutcomeAgentGone, agent.RunID
	case agent.RunID != runID:
		return runScopeOutcomeSuperseded, agent.RunID
	}
	return runScopeOutcomeBound, agent.RunID
}

// record logs and counts an outcome. A token without a run is logged at
// Info, any other refusable outcome at Warn, each at most once per agent,
// token run and outcome within the dedup window; a summary of the counts
// is logged once a minute. The log carries ids only, never the token or
// its scopes.
func (c *agentRunScopeChecker) record(ctx context.Context, claims *AgentTokenClaims, source, outcome, currentRunID string, req runScopeRequest) {
	routeClass := runScopeRouteConduit
	if source != runScopeSourceConduit {
		routeClass = runScopeRouteClass(req.path)
	}
	if m := c.metrics.Load(); m != nil {
		(*m).RecordAgentRunScope(source, outcome, c.mode.String(), routeClass)
	}
	switch outcome {
	case runScopeOutcomeBound:
		return
	case runScopeOutcomeUnavailable:
		return // logged where it happened
	}
	now := c.now()
	c.summary.add(ctx, c.log, c.mode.String(), outcome, now)
	if !c.dedup.first(claims.Subject+"|"+claims.RunID+"|"+outcome, now) {
		return
	}
	route := req.route
	if route == "" && req.raw != nil && c.route != nil {
		route = c.route(req.raw)
	}
	jtiHash := ""
	if claims.ID != "" {
		jtiHash = hashJTI(claims.ID)[:8]
	}
	attrs := []any{
		"agent_id", claims.Subject,
		"project_id", claims.ProjectID,
		"outcome", outcome,
		"source", source,
		"mode", c.mode.String(),
		"token_run_id", claims.RunID,
		"current_run_id", currentRunID,
		"header_run_id", req.header,
		"route", route,
		"route_class", routeClass,
		"path", req.path,
		"method", req.method,
		"jti_hash", jtiHash,
		"remote_addr", req.remoteAddr,
	}
	if source == runScopeSourceConduit {
		attrs = append(attrs, "hello_run_id", req.hello)
	}
	switch outcome {
	case runScopeOutcomeUnscoped, runScopeOutcomeLegacyEnded:
		c.log.InfoContext(ctx, "agent_token_run_unscoped", attrs...)
	case runScopeOutcomeUnbound:
		c.log.WarnContext(ctx, "agent_token_run_unbound", attrs...)
	default:
		c.log.WarnContext(ctx, "agent_token_run_superseded", attrs...)
	}
}

// conduitBinding returns the token-run binding for a conduit session of
// claims, so admission compares the Hello's launch id with the token's
// run (enforced: 4401). A token without a run has no binding: the
// request that opened the session was already decided under legacy_until.
func (c *agentRunScopeChecker) conduitBinding(ctx context.Context, claims *AgentTokenClaims, req runScopeRequest) *relay.TokenRunBinding {
	if claims.RunID == "" {
		return nil
	}
	return &relay.TokenRunBinding{
		RunID:   claims.RunID,
		Enforce: c.mode == agentRunScopeEnforce,
		OnMismatch: func(presented string) {
			hreq := req
			hreq.hello = presented
			c.record(ctx, claims, runScopeSourceConduit, runScopeOutcomeHello, "", hreq)
		},
	}
}

// agentTokenRefusedBody is the body of every run-scope refusal.
const agentTokenRefusedBody = `{"error":{"code":"invalid_token","message":"invalid token"}}` + "\n"

// writeAgentTokenRefused writes the run-scope refusal. It is the same
// response, byte for byte, whatever the reason.
func writeAgentTokenRefused(w http.ResponseWriter) {
	h := w.Header()
	h.Set("WWW-Authenticate", `Bearer error="invalid_token"`)
	h.Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(agentTokenRefusedBody))
}

// Log dedup bounds.
const (
	runScopeLogDedupSize   = 4096
	runScopeLogDedupWindow = 10 * time.Minute
)

// runScopeLogDedup remembers recently logged keys, bounded in size.
type runScopeLogDedup struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	seen   map[string]time.Time
}

func newRunScopeLogDedup(max int, window time.Duration) *runScopeLogDedup {
	return &runScopeLogDedup{max: max, window: window, seen: make(map[string]time.Time)}
}

// first reports whether key was not seen within the window, and records
// it. When full, expired keys are dropped first, then the map is cleared.
func (d *runScopeLogDedup) first(key string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if at, ok := d.seen[key]; ok && now.Sub(at) < d.window {
		return false
	}
	if len(d.seen) >= d.max {
		for k, at := range d.seen {
			if now.Sub(at) >= d.window {
				delete(d.seen, k)
			}
		}
		if len(d.seen) >= d.max {
			clear(d.seen)
		}
	}
	d.seen[key] = now
	return true
}

// runScopeLogSummaryWindow is how often the outcome counts are logged.
const runScopeLogSummaryWindow = time.Minute

// runScopeLogSummary counts refusable outcomes, including those whose log
// line the dedup suppressed, and logs the counts once per window. The
// summary for a window is logged by the first record after it ends.
type runScopeLogSummary struct {
	mu     sync.Mutex
	start  time.Time
	counts map[string]int
}

// add counts outcome at now, first logging the previous window's counts
// if it has ended.
func (s *runScopeLogSummary) add(ctx context.Context, log *slog.Logger, mode, outcome string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.counts == nil || now.Sub(s.start) >= runScopeLogSummaryWindow {
		if len(s.counts) > 0 {
			total := 0
			for _, n := range s.counts {
				total += n
			}
			attrs := []any{"mode", mode, "window_start", s.start.UTC().Format(time.RFC3339),
				"window_seconds", int(runScopeLogSummaryWindow / time.Second), "count", total}
			for _, o := range runScopeSummaryOutcomes {
				if n := s.counts[o]; n > 0 {
					attrs = append(attrs, o, n)
				}
			}
			log.InfoContext(ctx, "agent_token_run_scope_summary", attrs...)
		}
		s.start = now
		s.counts = make(map[string]int)
	}
	s.counts[outcome]++
}

// runScopeSummaryOutcomes are the outcomes a summary reports, in order.
var runScopeSummaryOutcomes = []string{
	runScopeOutcomeUnscoped, runScopeOutcomeLegacyEnded, runScopeOutcomeUnbound,
	runScopeOutcomeSuperseded, runScopeOutcomeAgentGone, runScopeOutcomeHeader,
	runScopeOutcomeHello,
}

// OTelAgentRunScopeMetrics counts run-scope check outcomes as
// scion.hub.agent_token.run_scope (attributes source, outcome, mode,
// route_class).
type OTelAgentRunScopeMetrics struct {
	total metric.Int64Counter
}

// NewOTelAgentRunScopeMetrics creates the counter.
func NewOTelAgentRunScopeMetrics(mp metric.MeterProvider) (*OTelAgentRunScopeMetrics, error) {
	if mp == nil {
		return nil, fmt.Errorf("otel agent run scope metrics: nil MeterProvider")
	}
	c, err := mp.Meter(instrumentationScope).Int64Counter("scion.hub.agent_token.run_scope",
		metric.WithUnit("{request}"))
	if err != nil {
		return nil, fmt.Errorf("creating agent_token.run_scope counter: %w", err)
	}
	return &OTelAgentRunScopeMetrics{total: c}, nil
}

// RecordAgentRunScope implements agentRunScopeMetrics.
func (m *OTelAgentRunScopeMetrics) RecordAgentRunScope(source, outcome, mode, routeClass string) {
	m.total.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("source", source),
		attribute.String("outcome", outcome),
		attribute.String("mode", mode),
		attribute.String("route_class", routeClass),
	))
}
