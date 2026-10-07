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
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// perfPair is two servers over one store: off has server.hub.perf_trace
// off, on has it on. Same data and IDs, so their responses and audit
// records can be compared directly.
type perfPair struct {
	off, on       *Server
	store         store.Store
	alice, bob    *store.User
	project       *store.Project
	otherProject  *store.Project
	offAudit      *perfRecordingEmitter
	onAudit       *perfRecordingEmitter
	agentsInAlice int
	// counter, when set, is an independent counting store under both
	// servers (see perfIndependentCounter).
	counter *perfIndependentCounter
	// agentToken is an agent JWT for an agent in alice's project;
	// uatKey is a project-scoped user access token of alice's.
	agentToken string
	uatKey     string
}

func newPerfServer(t *testing.T, s store.Store, on bool) *Server {
	t.Helper()
	cfg := testServerConfig()
	cfg.PerfTrace = on
	srv, err := New(cfg, s)
	require.NoError(t, err)
	srv.SetHubID("test-hub-id")
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	waitUserScopedDataSweep(t, srv)
	return srv
}

// newPerfPair builds the pair with agentCount agents in alice's project and
// two in a project bob owns.
func newPerfPair(t *testing.T, agentCount int) *perfPair {
	t.Helper()
	return newPerfPairWith(t, agentCount, false)
}

// newPerfPairWith is newPerfPair; with independent set, both servers run on
// an independent counting store wrapped around the real one.
func newPerfPairWith(t *testing.T, agentCount int, independent bool) *perfPair {
	t.Helper()
	s, err := newTestStore(":memory:")
	if err != nil {
		if strings.Contains(err.Error(), "sqlite driver not registered") {
			t.Skip("sqlite driver not registered")
		}
		t.Fatalf("test store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	require.NoError(t, s.Migrate(ctx))
	_ = s.DeleteHubSetting(ctx, "migration_delegation_edge_backfill_v1")

	p := &perfPair{store: s, agentsInAlice: agentCount}
	serverStore := s
	if independent {
		p.counter = newPerfIndependentCounter(s)
		serverStore = p.counter
	}
	p.off = newPerfServer(t, serverStore, false)
	p.alice, p.bob, p.project = setupDemoPolicyOn(t, p.off, s)

	p.otherProject = &store.Project{
		ID: tid("project-perf-other"), Name: "Other", Slug: "perf-other",
		OwnerID: p.bob.ID, CreatedBy: p.bob.ID,
	}
	require.NoError(t, s.CreateProject(ctx, p.otherProject))
	p.off.seedProjectCreatorMembership(ctx, p.otherProject)

	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	mk := func(i int, project *store.Project, owner *store.User, phase string) {
		name := fmt.Sprintf("perf-%s-%d", project.Slug, i)
		require.NoError(t, s.CreateAgent(ctx, &store.Agent{
			ID: tid(name), Slug: name, Name: name, ProjectID: project.ID,
			Phase: phase, CreatedBy: owner.ID, OwnerID: owner.ID,
			Created: base.Add(time.Duration(i) * time.Minute), Updated: base,
		}))
	}
	phases := []string{"running", "stopped", "error"}
	for i := 0; i < agentCount; i++ {
		mk(i, p.project, p.alice, phases[i%len(phases)])
	}
	for i := 0; i < 2; i++ {
		mk(i, p.otherProject, p.bob, "running")
	}

	p.on = newPerfServer(t, serverStore, true)

	// Both servers share the store and its signing keys, so one agent JWT
	// and one user access token work on both.
	if agentCount > 0 {
		var err error
		p.agentToken, err = p.off.GetAgentTokenService().GenerateAgentToken(
			tid(fmt.Sprintf("perf-%s-0", p.project.Slug)), p.project.ID, []AgentTokenScope{ScopeProjectRead}, nil)
		require.NoError(t, err)
	}
	p.uatKey = mintScopedUAT(t, p.off, p.alice.ID, p.project.ID, []string{"agent:manage"})
	// Replace each server's audit emitter with a recorder, wired exactly as
	// New() wires the real one, so records can be compared across the pair.
	p.offAudit, p.onAudit = &perfRecordingEmitter{}, &perfRecordingEmitter{}
	p.off.authzService.SetDecisionAuditEmitter(wrapAuditEmitterForPerfTrace(p.offAudit, p.off.config.PerfTrace))
	p.on.authzService.SetDecisionAuditEmitter(wrapAuditEmitterForPerfTrace(p.onAudit, p.on.config.PerfTrace))
	return p
}

type perfCaller int

const (
	perfAsAlice perfCaller = iota
	perfAsBob
	perfAsDev
	perfAsAgentJWT
	perfAsScopedUAT
)

var perfCallerNames = map[perfCaller]string{
	perfAsAlice: "member-owner", perfAsBob: "non-member", perfAsDev: "dev-admin",
	perfAsAgentJWT: "agent-jwt", perfAsScopedUAT: "scoped-uat",
}

func (p *perfPair) request(t *testing.T, srv *Server, who perfCaller, path string, optIn bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	switch who {
	case perfAsDev:
		req.Header.Set("Authorization", "Bearer "+testDevToken)
	case perfAsAgentJWT:
		req.Header.Set("X-Scion-Agent-Token", p.agentToken)
	case perfAsScopedUAT:
		req.Header.Set("Authorization", "Bearer "+p.uatKey)
	default:
		u := p.alice
		if who == perfAsBob {
			u = p.bob
		}
		token, _, _, err := srv.userTokenService.GenerateTokenPair(u.ID, u.Email, u.DisplayName, u.Role, ClientTypeWeb)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if optIn {
		req.Header.Set(HeaderPerfTraceRequest, "1")
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// perfTracedRequest serves req on srv with a trace installed by the caller,
// and returns the response and the trace's final snapshot. It is the test
// helper a CI budget reads counts from: StoreCalls, AuthzStoreCalls,
// AuditRecords and phase Counts are host-independent. srv must have
// server.hub.perf_trace on (the store and audit decorators are installed
// only then); the middleware keeps a trace it finds in the context. The
// trace reads the server's DB pool, as the middleware's own would.
func perfTracedRequest(t *testing.T, srv *Server, req *http.Request) (*httptest.ResponseRecorder, PerfTraceSnapshot) {
	t.Helper()
	require.True(t, srv.config.PerfTrace, "perfTracedRequest needs a server with PerfTrace on")
	tr := newPerfTrace(srv.perfTraceDB())
	req = req.WithContext(contextWithPerfTrace(req.Context(), tr))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec, tr.Snapshot()
}

// perfComparableBody drops serverTime, which differs between any two
// requests, and re-encodes the rest canonically.
func perfComparableBody(t *testing.T, body []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return string(body) // non-JSON bodies compare verbatim
	}
	if m, ok := v.(map[string]any); ok {
		delete(m, "serverTime")
	}
	out, err := json.Marshal(v)
	require.NoError(t, err)
	return string(out)
}

// perfComparableAudits renders records without their per-request fields
// (ID, Timestamp, CorrelationID) and sorts them, so two runs of the same
// request compare as multisets.
func perfComparableAudits(records []*store.DecisionAuditRecord) []string {
	out := make([]string, 0, len(records))
	for _, r := range records {
		c := *r
		c.ID, c.Timestamp, c.CorrelationID = "", time.Time{}, ""
		b, _ := json.Marshal(c)
		out = append(out, string(b))
	}
	sort.Strings(out)
	return out
}

var perfListPaths = []struct {
	name string
	path func(p *perfPair) string
}{
	{"global legacy", func(p *perfPair) string { return "/api/v1/agents" }},
	{"global legacy compact", func(p *perfPair) string { return "/api/v1/agents?view=compact" }},
	{"global legacy scoped", func(p *perfPair) string { return "/api/v1/agents?projectId=" + p.project.ID }},
	{"global sorted fit stats", func(p *perfPair) string { return "/api/v1/agents?sort=updated&fit=500&stats=1" }},
	{"global sorted paged", func(p *perfPair) string { return "/api/v1/agents?sort=created&dir=asc&limit=2" }},
	{"project legacy", func(p *perfPair) string { return "/api/v1/projects/" + p.project.ID + "/agents" }},
	{"project legacy compact", func(p *perfPair) string { return "/api/v1/projects/" + p.project.ID + "/agents?view=compact" }},
	{"project sorted fit stats", func(p *perfPair) string {
		return "/api/v1/projects/" + p.project.ID + "/agents?sort=updated&fit=500&stats=1"
	}},
	{"project sorted paged compact", func(p *perfPair) string {
		return "/api/v1/projects/" + p.project.ID + "/agents?sort=created&dir=desc&limit=2&view=compact"
	}},
	{"other project as non-member", func(p *perfPair) string { return "/api/v1/projects/" + p.otherProject.ID + "/agents" }},
}

// TestPerfTrace_OffInstallsNothing pins the off path: the authorization
// service holds the original store and emitter, no middleware runs, no
// header is added even on an opt-in request, and no logger is set.
func TestPerfTrace_OffInstallsNothing(t *testing.T) {
	p := newPerfPair(t, 3)

	srv := newPerfServer(t, p.store, false)
	assert.Same(t, p.store, srv.authzService.store, "off: authorization store must be the original")
	_, isStoreEmitter := srv.authzService.decisionAuditEmitter.(*StoreDecisionAuditEmitter)
	assert.True(t, isStoreEmitter, "off: audit emitter must be the original")
	assert.Nil(t, srv.perfTraceLog)
	assert.False(t, DefaultServerConfig().PerfTrace, "default must be off")

	srvOn := newPerfServer(t, p.store, true)
	assert.IsType(t, perfAuthzStore{}, srvOn.authzService.store)
	assert.IsType(t, perfAuditEmitter{}, srvOn.authzService.decisionAuditEmitter)

	for _, tc := range perfListPaths {
		rec := p.request(t, p.off, perfAsAlice, tc.path(p), true)
		for k := range rec.Header() {
			assert.False(t, strings.HasPrefix(k, "X-Scion-Perf"), "%s: header %s with tracing off", tc.name, k)
		}
	}
}

// TestPerfTrace_OnMatchesOff: with tracing on (with and without the opt-in
// header), every list variant returns the same status and body and emits
// the same decision-audit records as with tracing off.
func TestPerfTrace_OnMatchesOff(t *testing.T) {
	p := newPerfPair(t, 5)
	for _, who := range []perfCaller{perfAsAlice, perfAsBob, perfAsDev, perfAsAgentJWT, perfAsScopedUAT} {
		for _, tc := range perfListPaths {
			for _, optIn := range []bool{false, true} {
				name := fmt.Sprintf("%s/%s/optin=%v", perfCallerNames[who], tc.name, optIn)
				path := tc.path(p)

				p.offAudit.take()
				p.onAudit.take()
				offRec := p.request(t, p.off, who, path, optIn)
				offAudits := perfComparableAudits(p.offAudit.take())
				onRec := p.request(t, p.on, who, path, optIn)
				onAudits := perfComparableAudits(p.onAudit.take())

				require.Equal(t, offRec.Code, onRec.Code, name)
				require.Equal(t, perfComparableBody(t, offRec.Body.Bytes()), perfComparableBody(t, onRec.Body.Bytes()), name)
				require.Equal(t, offAudits, onAudits, name)
				require.Equal(t, offRec.Header().Get("Content-Type"), onRec.Header().Get("Content-Type"), name)
				// Headers only for the unscoped local admin (the dev user) who
				// opted in; never for any other caller.
				if optIn && who == perfAsDev {
					assert.NotEmpty(t, onRec.Header().Get(HeaderPerfTraceEndpoint), name)
				} else {
					assertNoPerfHeaders(t, onRec.Header())
				}
			}
		}
	}
}

func parsePerfHeader(t *testing.T, v string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	if v == "" {
		return out
	}
	for _, part := range strings.Split(v, ",") {
		k, val, ok := strings.Cut(part, "=")
		require.True(t, ok, "malformed perf header part %q", part)
		n, err := strconv.ParseInt(val, 10, 64)
		require.NoError(t, err)
		out[k] = n
	}
	return out
}

// TestPerfTrace_CountersOnSmallFixture checks the counters against
// independent counts on a small fixture.
func TestPerfTrace_CountersOnSmallFixture(t *testing.T) {
	const n = 3
	p := newPerfPairWith(t, n, true)

	token, _, _, err := p.on.userTokenService.GenerateTokenPair(p.alice.ID, p.alice.Email, p.alice.DisplayName, p.alice.Role, ClientTypeWeb)
	require.NoError(t, err)
	get := func(path, bearer string) (*httptest.ResponseRecorder, PerfTraceSnapshot, []*store.DecisionAuditRecord, map[string]int64) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set(HeaderPerfTraceRequest, "1")
		p.onAudit.take()
		p.counter.reset()
		rec, snap := perfTracedRequest(t, p.on, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		return rec, snap, p.onAudit.take(), p.counter.reset()
	}
	// assertIndependent: the decorator's per-op counts equal the counts
	// taken by method name under it, with no forwarding mismatch.
	assertIndependent := func(t *testing.T, snap PerfTraceSnapshot, independent map[string]int64) {
		t.Helper()
		got := map[string]int64{}
		for name, c := range snap.StoreCalls {
			got[name] = c.Count
		}
		assert.Equal(t, independent, got, "decorator counts must equal independent counts per op")
		assert.Positive(t, snap.AuthzStoreCalls)
	}

	t.Run("global legacy, member", func(t *testing.T) {
		rec, snap, audits, independent := get("/api/v1/agents", token)
		var body ListAgentsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.Len(t, body.Agents, n, "alice sees exactly her project's agents")
		assertNoPerfHeaders(t, rec.Header())
		assertIndependent(t, snap, independent)

		assert.Equal(t, "agents.global.legacy", snap.Endpoint)
		assert.Equal(t, int64(len(audits)), snap.AuditRecords, "one counted record per emitted record")
		assert.Positive(t, snap.AuditRecords)
		var allow, deny int64
		for _, r := range audits {
			switch r.Result {
			case "allow":
				allow++
			case "deny":
				deny++
			}
		}
		assert.Equal(t, allow, snap.AuditAllow)
		assert.Equal(t, deny, snap.AuditDeny)

		assert.Equal(t, int64(n), snap.Phases["messageability"].Count, "one messageability call per returned agent")
		assert.Equal(t, int64(1), snap.Phases["enrich"].Count)
		assert.Equal(t, int64(1), snap.Phases["capabilities"].Count)
		assert.Equal(t, int64(1), snap.Phases["scope_capabilities"].Count)
		assert.Equal(t, int64(2), snap.Phases["list_scope_authz"].Count, "scope resolution and classification")
		assert.GreaterOrEqual(t, snap.Phases["list_db_read"].Count, int64(1))
		assert.GreaterOrEqual(t, snap.Phases["list_read_authz"].Count, int64(1))
		assert.Equal(t, int64(1), snap.Phases["serialize"].Count)
		assert.True(t, snap.DBAvailable, "the trace finds the sqlite pool through the store")
		assert.Positive(t, snap.DBOpen)
	})

	t.Run("project sorted, member", func(t *testing.T) {
		_, snap, audits, independent := get("/api/v1/projects/"+p.project.ID+"/agents?sort=updated&fit=500", token)
		assertIndependent(t, snap, independent)
		assert.Equal(t, "agents.project.sorted", snap.Endpoint)
		assert.Equal(t, int64(len(audits)), snap.AuditRecords)
		assert.Equal(t, int64(1), snap.Phases["list_scope_authz"].Count, "project agent.list gate")
		assert.Equal(t, int64(1), snap.Phases["list_read_authz"].Count)
		// count pre-check, member read, full-row read, recheck read
		assert.Equal(t, int64(4), snap.Phases["list_db_read"].Count)
		assert.Equal(t, int64(1), snap.Phases["enrich"].Count)
		assert.Equal(t, int64(1), snap.Phases["capabilities"].Count)
		assert.Equal(t, int64(1), snap.Phases["scope_capabilities"].Count)
	})

	t.Run("admin opt-in headers match the trace", func(t *testing.T) {
		rec, snap, _, independent := get("/api/v1/projects/"+p.project.ID+"/agents", testDevToken)
		assertIndependent(t, snap, independent)
		// Headers are taken when the status is written: every count but
		// serialize is final by then.
		assert.Equal(t, "agents.project.legacy", rec.Header().Get(HeaderPerfTraceEndpoint))
		counts := parsePerfHeader(t, rec.Header().Get(HeaderPerfTracePhaseCounts))
		for name, c := range snap.Phases {
			if name == "serialize" {
				assert.NotContains(t, counts, name)
				continue
			}
			assert.Equal(t, c.Count, counts[name], "phase %s", name)
		}
		storeCalls := parsePerfHeader(t, rec.Header().Get(HeaderPerfTraceStoreCalls))
		var total int64
		for name, c := range snap.StoreCalls {
			assert.Equal(t, c.Count, storeCalls[name], "store op %s", name)
			total += storeCalls[name]
		}
		assert.Equal(t, snap.AuthzStoreCalls, total)
		decisions := parsePerfHeader(t, rec.Header().Get(HeaderPerfTraceDecisions))
		assert.Equal(t, snap.AuditRecords, decisions["count"])
		db := parsePerfHeader(t, rec.Header().Get(HeaderPerfTraceDB))
		for _, k := range []string{"wait_count", "wait_us", "in_use", "open"} {
			assert.Contains(t, db, k)
		}
		assert.Positive(t, db["open"])
	})

	t.Run("counts are stable across identical requests", func(t *testing.T) {
		_, a, _, _ := get("/api/v1/projects/"+p.project.ID+"/agents", token)
		_, b, _, _ := get("/api/v1/projects/"+p.project.ID+"/agents", token)
		assert.Equal(t, a.AuditRecords, b.AuditRecords)
		assert.Equal(t, a.AuthzStoreCalls, b.AuthzStoreCalls)
		for name, c := range a.StoreCalls {
			assert.Equal(t, c.Count, b.StoreCalls[name].Count, name)
		}
	})
}

// TestPerfTrace_RejectedAndUnauthenticatedRequests pins the middleware
// placement: a request auth rejects (401) never reaches the trace, so it
// gets no perf headers and writes no perf_trace line; an unauthenticated
// endpoint is traced and logged but never gets perf headers, even when the
// caller is the admin, and its body equals tracing off.
func TestPerfTrace_RejectedAndUnauthenticatedRequests(t *testing.T) {
	p := newPerfPair(t, 1)
	logs := &perfLockedBuffer{}
	p.on.perfTraceLog = slog.New(slog.NewJSONHandler(logs, nil))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer not-a-valid-token")
	req.Header.Set(HeaderPerfTraceRequest, "1")
	rec := httptest.NewRecorder()
	p.on.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assertNoPerfHeaders(t, rec.Header())
	assert.Empty(t, logs.lines(), "a rejected request writes no perf_trace line")

	for _, path := range []string{"/healthz", "/api/v1/settings/public"} {
		offRec := p.request(t, p.off, perfAsDev, path, true)
		onRec := p.request(t, p.on, perfAsDev, path, true)
		assert.Equal(t, offRec.Code, onRec.Code, path)
		assert.Equal(t, perfComparableBody(t, offRec.Body.Bytes()), perfComparableBody(t, onRec.Body.Bytes()), path)
		assertNoPerfHeaders(t, onRec.Header())
	}
	lines := logs.lines()
	require.Len(t, lines, 2, "unauthenticated endpoints are still traced")
	for _, l := range lines {
		assert.Contains(t, l, `"endpoint":"other"`)
	}
}

// TestPerfTrace_AdminSettingsRejectsPerfTrace: server.hub.perf_trace is
// Layer-0, so an admin settings write carrying it gets 422 with the key.
func TestPerfTrace_AdminSettingsRejectsPerfTrace(t *testing.T) {
	srv, _, ops := newTestDBServer(t)
	req := adminRequest(http.MethodPut, "/api/v1/admin/server-config", `{"server": {"hub": {"perf_trace": true}}}`)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, req, ops)
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "layer0_rejected", resp["error"])
	assert.Contains(t, resp["keys"], "server.hub.perf_trace")
}

// perfLockedBuffer collects log output from concurrent writers.
type perfLockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *perfLockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *perfLockedBuffer) lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := strings.TrimSpace(b.buf.String())
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// TestPerfTrace_SSE: with tracing on, the SSE stream is byte-identical to
// tracing off, and the trace counts the delivered event.
func TestPerfTrace_SSE(t *testing.T) {
	run := func(on bool) (body string, lines []string) {
		out := &perfLockedBuffer{}
		logger := slog.New(slog.NewJSONHandler(out, nil))
		restore := perfTraceLogger
		perfTraceLogger = func() *slog.Logger { return logger }
		defer func() { perfTraceLogger = restore }()

		synctest.Test(t, func(t *testing.T) {
			ws, pub, req := newSSEOrderingRequest(t)
			ws.config.PerfTrace = on
			ctx, cancel := context.WithCancel(req.Context())
			defer cancel()
			w := &sseFlushWriter{
				ResponseRecorder: httptest.NewRecorder(),
				onFirstFlush: func() {
					pub.publish("user.user-1.message", map[string]string{"text": "hello"})
				},
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				ws.handleSSE(w, req.WithContext(ctx))
			}()
			synctest.Wait()
			cancel()
			<-done
			body = w.Body.String()
		})
		return body, out.lines()
	}

	offBody, offLines := run(false)
	onBody, onLines := run(true)
	assert.Contains(t, offBody, "hello")
	assert.Equal(t, offBody, onBody)
	assert.Empty(t, offLines)
	require.Len(t, onLines, 2, "one line at connect, one at close")
	assert.Contains(t, onLines[0], `"sse_stage":"connect"`)
	assert.Contains(t, onLines[0], `"endpoint":"sse.events"`)
	assert.Contains(t, onLines[0], `"phase_sse_authorize_n":1`)
	assert.Contains(t, onLines[1], `"sse_stage":"close"`)
	assert.Contains(t, onLines[1], `"sse_events":1`)
	assert.Contains(t, onLines[1], `"phase_sse_write_n":1`)
	for _, l := range onLines {
		assert.NotContains(t, l, "user-1", "no subject or user ID in perf lines")
	}
}
