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

// Request performance tracing (server.hub.perf_trace).
//
// This file holds the per-request trace that the agent-list handlers, the
// authorization store decorator (perftrace_store.go), the decision-audit
// emitter decorator (perftrace_audit.go), the HTTP middleware
// (perftrace_middleware.go) and the SSE endpoint record into. It exists to
// measure where an agent-list request spends its time and how many
// authorization store reads and decision-audit records one request causes.
//
// Observe only. Nothing here reads a decision, changes an argument or a
// return value, filters, sorts, redacts or delays anything. Recording is
// write-only into the trace; the trace is never read by request logic.
//
// Default off. The setting is a startup operational setting
// (server.hub.perf_trace, env SCION_SERVER_HUB_PERFTRACE). With it off:
//   - no middleware, store decorator or audit-emitter decorator is installed,
//     so the store and the emitter the authorization service holds are the
//     unwrapped originals;
//   - no trace is ever put into a request context, so every recording call
//     in a handler is one context lookup that finds nothing, followed by a
//     return. perfPhaseStart returns a shared no-op function and allocates
//     nothing;
//   - no response header is added and no log line is written.
//
// Overhead with it on: two time.Now calls and one uncontended mutex
// acquisition per recorded phase or store call, a few hundred bytes per
// request for the trace, one sql.DB.Stats call at request start and end, and
// one log line per request.
//
// Counted scope: the store decorator counts the reads listed in perfStoreOp
// that reach it with the request context. Reads the authorization service
// makes through other paths (relationship progeny lookups such as
// ListProgeny*, anything inside a store transaction) are not counted, and
// work moved onto a detached background context after the request's log
// line is written records into a trace nobody reads. A CI budget on these
// counts covers the request-path reads only.
//
// Cardinality is bounded by construction. Phases, authorization store
// operations and endpoint classes are small enums declared below; their
// names are fixed strings. Decision-audit records are counted only by
// outcome (allow, deny, other). No request-derived value (path, ID, name,
// email, token, config, query) is ever a key or a label. The request ID is
// logged for correlation only.

import (
	"context"
	"database/sql"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// perfPhase is one timed region of a request. The set is closed: every
// recorded phase is one of the constants below.
type perfPhase uint8

const (
	// perfPhaseListScopeAuthz: the list-level authorization before any
	// agent row is read (ResolveListScopes and the project classification
	// on the global list; the project agent.list gate on the project list).
	perfPhaseListScopeAuthz perfPhase = iota
	// perfPhaseListDBRead: store reads of agent rows and members for the
	// page (each ListAgents / ListAgentMembers / full-row and recheck read).
	// DB time only; no authorization runs inside it.
	perfPhaseListDBRead
	// perfPhaseListReadAuthz: the per-row read decisions that decide which
	// rows a caller may see (AuthorizeListReadBatch).
	perfPhaseListReadAuthz
	// perfPhaseEnrich: enrichAgents (project and broker names).
	perfPhaseEnrich
	// perfPhaseCapabilities: per-item capability evaluation, including the
	// env-view decision used for appliedConfig redaction. On the sorted
	// project list it also includes the re-list read decision for a row
	// whose authorization inputs changed between the member and full-row
	// reads (rare).
	perfPhaseCapabilities
	// perfPhaseMessageability: ComputeMessageability, summed over items.
	perfPhaseMessageability
	// perfPhaseScopeCapabilities: the list-level capabilities block.
	perfPhaseScopeCapabilities
	// perfPhaseSerialize: from the first response write to handler return
	// (JSON encoding and writing the body). Recorded by the middleware.
	perfPhaseSerialize
	// perfPhaseSSEExpand / perfPhaseSSEAuthorize: SSE connect-time wildcard
	// expansion and subject authorization.
	perfPhaseSSEExpand
	perfPhaseSSEAuthorize
	// perfPhaseSSEWrite: writing and flushing delivered SSE events.
	perfPhaseSSEWrite

	perfPhaseCount
)

var perfPhaseNames = [perfPhaseCount]string{
	perfPhaseListScopeAuthz:    "list_scope_authz",
	perfPhaseListDBRead:        "list_db_read",
	perfPhaseListReadAuthz:     "list_read_authz",
	perfPhaseEnrich:            "enrich",
	perfPhaseCapabilities:      "capabilities",
	perfPhaseMessageability:    "messageability",
	perfPhaseScopeCapabilities: "scope_capabilities",
	perfPhaseSerialize:         "serialize",
	perfPhaseSSEExpand:         "sse_expand",
	perfPhaseSSEAuthorize:      "sse_authorize",
	perfPhaseSSEWrite:          "sse_write",
}

func (p perfPhase) String() string {
	if p < perfPhaseCount {
		return perfPhaseNames[p]
	}
	return "unknown"
}

// perfStoreOp is one store method counted by the authorization store
// decorator (perftrace_store.go). These are the reads the authorization
// service makes to prepare its inputs (principal closure, bindings, role
// definitions, constraints, delegation edges, and the user, agent, project
// and membership rows some rules consult).
type perfStoreOp uint8

const (
	perfStoreGetEffectiveGroups perfStoreOp = iota
	perfStoreGetEffectiveGroupsForAgent
	perfStoreGetParentGroups
	perfStoreGetUserGroups
	perfStoreGetGroupMembership
	perfStoreGetGroupBySlug
	perfStoreListRoleBindingsForPrincipals
	perfStoreListRoleBindingsForPrincipal
	perfStoreGetRoleDefinition
	perfStoreGetRoleDefinitionsByIDs
	perfStoreGetRoleDefinitionByName
	perfStoreListAccessConstraints
	perfStoreGetDelegationEdgesForDelegate
	perfStoreGetUser
	perfStoreGetUserAccessToken
	perfStoreGetAgent
	perfStoreGetProject
	perfStoreGetProjectMembership
	perfStoreGetHubSetting

	perfStoreOpCount
)

var perfStoreOpNames = [perfStoreOpCount]string{
	perfStoreGetEffectiveGroups:            "GetEffectiveGroups",
	perfStoreGetEffectiveGroupsForAgent:    "GetEffectiveGroupsForAgent",
	perfStoreGetParentGroups:               "GetParentGroups",
	perfStoreGetUserGroups:                 "GetUserGroups",
	perfStoreGetGroupMembership:            "GetGroupMembership",
	perfStoreGetGroupBySlug:                "GetGroupBySlug",
	perfStoreListRoleBindingsForPrincipals: "ListRoleBindingsForPrincipals",
	perfStoreListRoleBindingsForPrincipal:  "ListRoleBindingsForPrincipal",
	perfStoreGetRoleDefinition:             "GetRoleDefinition",
	perfStoreGetRoleDefinitionsByIDs:       "GetRoleDefinitionsByIDs",
	perfStoreGetRoleDefinitionByName:       "GetRoleDefinitionByName",
	perfStoreListAccessConstraints:         "ListAccessConstraints",
	perfStoreGetDelegationEdgesForDelegate: "GetDelegationEdgesForDelegate",
	perfStoreGetUser:                       "GetUser",
	perfStoreGetUserAccessToken:            "GetUserAccessToken",
	perfStoreGetAgent:                      "GetAgent",
	perfStoreGetProject:                    "GetProject",
	perfStoreGetProjectMembership:          "GetProjectMembership",
	perfStoreGetHubSetting:                 "GetHubSetting",
}

func (o perfStoreOp) String() string {
	if o < perfStoreOpCount {
		return perfStoreOpNames[o]
	}
	return "unknown"
}

// perfEndpoint is the bounded endpoint class a trace is reported under.
type perfEndpoint string

const (
	perfEndpointOther                 perfEndpoint = "other"
	perfEndpointAgentsGlobalLegacy    perfEndpoint = "agents.global.legacy"
	perfEndpointAgentsGlobalSorted    perfEndpoint = "agents.global.sorted"
	perfEndpointAgentsProjectLegacy   perfEndpoint = "agents.project.legacy"
	perfEndpointAgentsProjectSorted   perfEndpoint = "agents.project.sorted"
	perfEndpointAgentsProjectSortedAg perfEndpoint = "agents.project.sorted_agent"
	perfEndpointSSE                   perfEndpoint = "sse.events"
)

// perfAuditOutcome is the bounded label a decision-audit record is counted
// under.
type perfAuditOutcome uint8

const (
	perfAuditAllow perfAuditOutcome = iota
	perfAuditDeny
	perfAuditOther
	perfAuditOutcomeCount
)

// PerfTrace accumulates timing and counts for one request. All methods are
// safe for concurrent use and treat a nil receiver as a no-op, so callers
// never need a nil check.
type PerfTrace struct {
	mu sync.Mutex

	start    time.Time
	endpoint perfEndpoint

	phaseDur [perfPhaseCount]time.Duration
	phaseN   [perfPhaseCount]int64

	storeDur [perfStoreOpCount]time.Duration
	storeN   [perfStoreOpCount]int64

	auditN   [perfAuditOutcomeCount]int64
	auditDur time.Duration

	sseEvents int64

	// db is the hub's connection pool, when the store exposes one; dbStart
	// is its stats at request start. The reported values are deltas over
	// the request. The pool is shared, so a delta includes waits by
	// concurrent requests and the audit writer.
	db      *sql.DB
	dbStart sql.DBStats
}

func newPerfTrace(db *sql.DB) *PerfTrace {
	t := &PerfTrace{start: time.Now(), endpoint: perfEndpointOther, db: db}
	if db != nil {
		t.dbStart = db.Stats()
	}
	return t
}

func (t *PerfTrace) addPhase(p perfPhase, d time.Duration) {
	if t == nil || p >= perfPhaseCount {
		return
	}
	t.mu.Lock()
	t.phaseDur[p] += d
	t.phaseN[p]++
	t.mu.Unlock()
}

func (t *PerfTrace) addStoreCall(op perfStoreOp, d time.Duration) {
	if t == nil || op >= perfStoreOpCount {
		return
	}
	t.mu.Lock()
	t.storeDur[op] += d
	t.storeN[op]++
	t.mu.Unlock()
}

func (t *PerfTrace) addAudit(o perfAuditOutcome, d time.Duration) {
	if t == nil || o >= perfAuditOutcomeCount {
		return
	}
	t.mu.Lock()
	t.auditN[o]++
	t.auditDur += d
	t.mu.Unlock()
}

func (t *PerfTrace) addSSEEvent(d time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.sseEvents++
	t.phaseDur[perfPhaseSSEWrite] += d
	t.phaseN[perfPhaseSSEWrite]++
	t.mu.Unlock()
}

func (t *PerfTrace) setEndpoint(e perfEndpoint) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.endpoint = e
	t.mu.Unlock()
}

// PerfCount is a count and the summed duration of the counted operations.
type PerfCount struct {
	Count    int64
	Duration time.Duration
}

// PerfTraceSnapshot is a point-in-time copy of a PerfTrace. Its counts are
// host-independent (they do not depend on machine speed), so tests and a CI
// gate can hold budgets on them: StoreCalls, AuthzStoreCalls,
// AuditRecords, and the Count of each phase. Durations are host-dependent
// and are for reporting only.
type PerfTraceSnapshot struct {
	Endpoint string
	Elapsed  time.Duration
	// Phases holds only the phases that were entered at least once.
	Phases map[string]PerfCount
	// StoreCalls holds only the authorization store operations that were
	// called at least once.
	StoreCalls map[string]PerfCount
	// AuthzStoreCalls and AuthzStoreTime sum StoreCalls: the store reads the
	// authorization service made to prepare its inputs.
	AuthzStoreCalls int64
	AuthzStoreTime  time.Duration
	// AuditAllow, AuditDeny, AuditOther count decision-audit records handed
	// to the emitter, by outcome. Every authorization decision emits one
	// record at the default sampling rate (1.0), so AuditRecords is also the
	// number of decisions the request made.
	AuditAllow, AuditDeny, AuditOther int64
	AuditRecords                      int64
	// AuditEmitTime is the time spent handing records to the emitter. The
	// emitter queues them; the database write happens off the request path.
	AuditEmitTime time.Duration
	SSEEvents     int64
	// DB pool: wait count and wait time accumulated during the request
	// (pool-wide delta), and the pool's in-use and open connections at
	// snapshot time. DBAvailable is false when the store exposes no pool.
	DBAvailable    bool
	DBWaitCount    int64
	DBWaitDuration time.Duration
	DBInUse        int
	DBOpen         int
}

// Snapshot returns a copy of the trace's current data. A nil receiver
// returns the zero snapshot.
func (t *PerfTrace) Snapshot() PerfTraceSnapshot {
	if t == nil {
		return PerfTraceSnapshot{}
	}
	t.mu.Lock()
	s := PerfTraceSnapshot{
		Endpoint:      string(t.endpoint),
		Elapsed:       time.Since(t.start),
		Phases:        map[string]PerfCount{},
		StoreCalls:    map[string]PerfCount{},
		AuditAllow:    t.auditN[perfAuditAllow],
		AuditDeny:     t.auditN[perfAuditDeny],
		AuditOther:    t.auditN[perfAuditOther],
		AuditEmitTime: t.auditDur,
		SSEEvents:     t.sseEvents,
	}
	for p := perfPhase(0); p < perfPhaseCount; p++ {
		if t.phaseN[p] > 0 {
			s.Phases[p.String()] = PerfCount{Count: t.phaseN[p], Duration: t.phaseDur[p]}
		}
	}
	for op := perfStoreOp(0); op < perfStoreOpCount; op++ {
		if t.storeN[op] > 0 {
			s.StoreCalls[op.String()] = PerfCount{Count: t.storeN[op], Duration: t.storeDur[op]}
			s.AuthzStoreCalls += t.storeN[op]
			s.AuthzStoreTime += t.storeDur[op]
		}
	}
	db, dbStart := t.db, t.dbStart
	t.mu.Unlock()

	s.AuditRecords = s.AuditAllow + s.AuditDeny + s.AuditOther
	if db != nil {
		now := db.Stats()
		s.DBAvailable = true
		s.DBWaitCount = now.WaitCount - dbStart.WaitCount
		s.DBWaitDuration = now.WaitDuration - dbStart.WaitDuration
		s.DBInUse = now.InUse
		s.DBOpen = now.OpenConnections
	}
	return s
}

// Response header names, defined only here. The request opts in with
// headerPerfTraceRequest: 1; the server must have server.hub.perf_trace on,
// and the caller must be an unscoped local platform admin
// (perfHeadersAllowed). perfResponseWriter is the only writer of these
// headers. Values are
// comma-separated name=integer pairs, sorted by name; durations are in
// microseconds. The headers are taken when the response status is written,
// so they never include the serialize phase (the log line does).
const (
	headerPerfTraceRequest     = "X-Scion-Perf-Trace"
	headerPerfTraceEndpoint    = "X-Scion-Perf-Endpoint"
	headerPerfTracePhases      = "X-Scion-Perf-Phases"
	headerPerfTracePhaseCounts = "X-Scion-Perf-Phase-Counts"
	headerPerfTraceStoreCalls  = "X-Scion-Perf-Store-Calls"
	headerPerfTraceStoreTime   = "X-Scion-Perf-Store-Us"
	headerPerfTraceDecisions   = "X-Scion-Perf-Decisions"
	headerPerfTraceDB          = "X-Scion-Perf-DB"
)

func microseconds(d time.Duration) int64 { return d.Microseconds() }

func joinPerfCounts(m map[string]PerfCount, value func(PerfCount) int64) string {
	if len(m) == 0 {
		return ""
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for i, k := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(strconv.FormatInt(value(m[k]), 10))
	}
	return b.String()
}

func kv(parts ...any) string {
	var b strings.Builder
	for i := 0; i+1 < len(parts); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(parts[i].(string))
		b.WriteByte('=')
		switch v := parts[i+1].(type) {
		case int64:
			b.WriteString(strconv.FormatInt(v, 10))
		case int:
			b.WriteString(strconv.Itoa(v))
		}
	}
	return b.String()
}

// HeaderValues renders the snapshot as response header values, keyed by
// header name. Empty values are omitted.
func (s PerfTraceSnapshot) HeaderValues() map[string]string {
	out := map[string]string{
		headerPerfTraceEndpoint: s.Endpoint,
		headerPerfTraceDecisions: kv(
			"count", s.AuditRecords, "allow", s.AuditAllow, "deny", s.AuditDeny,
			"other", s.AuditOther, "audit_us", microseconds(s.AuditEmitTime)),
	}
	if v := joinPerfCounts(s.Phases, func(c PerfCount) int64 { return microseconds(c.Duration) }); v != "" {
		out[headerPerfTracePhases] = v
	}
	if v := joinPerfCounts(s.Phases, func(c PerfCount) int64 { return c.Count }); v != "" {
		out[headerPerfTracePhaseCounts] = v
	}
	if v := joinPerfCounts(s.StoreCalls, func(c PerfCount) int64 { return c.Count }); v != "" {
		out[headerPerfTraceStoreCalls] = v
	}
	if v := joinPerfCounts(s.StoreCalls, func(c PerfCount) int64 { return microseconds(c.Duration) }); v != "" {
		out[headerPerfTraceStoreTime] = v
	}
	if s.DBAvailable {
		out[headerPerfTraceDB] = kv("wait_count", s.DBWaitCount, "wait_us", microseconds(s.DBWaitDuration),
			"in_use", s.DBInUse, "open", s.DBOpen)
	}
	return out
}

// LogAttrs renders the snapshot as structured log attributes. Keys are
// built only from the fixed phase and operation names.
func (s PerfTraceSnapshot) LogAttrs() []slog.Attr {
	attrs := make([]slog.Attr, 0, 2*len(s.Phases)+2*len(s.StoreCalls)+12)
	attrs = append(attrs,
		slog.String("endpoint", s.Endpoint),
		slog.Int64("elapsed_us", microseconds(s.Elapsed)),
	)
	for _, name := range perfSortedNames(s.Phases) {
		c := s.Phases[name]
		attrs = append(attrs,
			slog.Int64("phase_"+name+"_us", microseconds(c.Duration)),
			slog.Int64("phase_"+name+"_n", c.Count))
	}
	for _, name := range perfSortedNames(s.StoreCalls) {
		c := s.StoreCalls[name]
		attrs = append(attrs,
			slog.Int64("store_"+name+"_n", c.Count),
			slog.Int64("store_"+name+"_us", microseconds(c.Duration)))
	}
	attrs = append(attrs,
		slog.Int64("authz_store_calls", s.AuthzStoreCalls),
		slog.Int64("authz_store_us", microseconds(s.AuthzStoreTime)),
		slog.Int64("audit_records", s.AuditRecords),
		slog.Int64("audit_allow", s.AuditAllow),
		slog.Int64("audit_deny", s.AuditDeny),
		slog.Int64("audit_other", s.AuditOther),
		slog.Int64("audit_emit_us", microseconds(s.AuditEmitTime)),
	)
	if s.SSEEvents > 0 {
		attrs = append(attrs, slog.Int64("sse_events", s.SSEEvents))
	}
	if s.DBAvailable {
		attrs = append(attrs,
			slog.Int64("db_wait_count", s.DBWaitCount),
			slog.Int64("db_wait_us", microseconds(s.DBWaitDuration)),
			slog.Int("db_in_use", s.DBInUse),
			slog.Int("db_open", s.DBOpen))
	}
	return attrs
}

func perfSortedNames(m map[string]PerfCount) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

type perfTraceKey struct{}

// contextWithPerfTrace installs t into ctx. Only the perf-trace middleware,
// the SSE handler and tests call it, and only when the setting is on.
func contextWithPerfTrace(ctx context.Context, t *PerfTrace) context.Context {
	return context.WithValue(ctx, perfTraceKey{}, t)
}

// perfTraceFrom returns the request's trace, or nil when tracing is off or
// ctx did not come from a traced request.
func perfTraceFrom(ctx context.Context) *PerfTrace {
	if ctx == nil {
		return nil
	}
	t, _ := ctx.Value(perfTraceKey{}).(*PerfTrace)
	return t
}

func perfNoop() {}

// perfPhaseStart starts timing phase p and returns the function that ends
// it. With no trace in ctx it returns a shared no-op and allocates nothing.
//
//	done := perfPhaseStart(ctx, perfPhaseEnrich)
//	s.enrichAgents(ctx, items)
//	done()
func perfPhaseStart(ctx context.Context, p perfPhase) func() {
	t := perfTraceFrom(ctx)
	if t == nil {
		return perfNoop
	}
	start := time.Now()
	return func() { t.addPhase(p, time.Since(start)) }
}

// perfSetEndpoint labels the request's trace with endpoint class e.
func perfSetEndpoint(ctx context.Context, e perfEndpoint) {
	perfTraceFrom(ctx).setEndpoint(e)
}
