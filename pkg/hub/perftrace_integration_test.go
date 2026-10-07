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
	p.off = newPerfServer(t, s, false)
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

	p.on = newPerfServer(t, s, true)
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
)

func (p *perfPair) request(t *testing.T, srv *Server, who perfCaller, path string, optIn bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	switch who {
	case perfAsDev:
		req.Header.Set("Authorization", "Bearer "+testDevToken)
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
// only then); the middleware keeps a trace it finds in the context.
func perfTracedRequest(t *testing.T, srv *Server, req *http.Request) (*httptest.ResponseRecorder, PerfTraceSnapshot) {
	t.Helper()
	require.True(t, srv.config.PerfTrace, "perfTracedRequest needs a server with PerfTrace on")
	tr := newPerfTrace(nil)
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
	for _, who := range []perfCaller{perfAsAlice, perfAsBob, perfAsDev} {
		for _, tc := range perfListPaths {
			for _, optIn := range []bool{false, true} {
				name := fmt.Sprintf("caller%d/%s/optin=%v", who, tc.name, optIn)
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
				if optIn && onRec.Code == http.StatusOK {
					assert.NotEmpty(t, onRec.Header().Get(HeaderPerfTraceEndpoint), name)
				} else if !optIn {
					assert.Empty(t, onRec.Header().Get(HeaderPerfTraceEndpoint), name)
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
	p := newPerfPair(t, n)

	token, _, _, err := p.on.userTokenService.GenerateTokenPair(p.alice.ID, p.alice.Email, p.alice.DisplayName, p.alice.Role, ClientTypeWeb)
	require.NoError(t, err)
	get := func(path string) (*httptest.ResponseRecorder, PerfTraceSnapshot, []*store.DecisionAuditRecord) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(HeaderPerfTraceRequest, "1")
		p.onAudit.take()
		rec, snap := perfTracedRequest(t, p.on, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		return rec, snap, p.onAudit.take()
	}

	t.Run("global legacy", func(t *testing.T) {
		rec, snap, audits := get("/api/v1/agents")
		var body ListAgentsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.Len(t, body.Agents, n, "alice sees exactly her project's agents")

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
		assert.Positive(t, snap.AuthzStoreCalls)

		// Headers are taken when the status is written: every count but
		// serialize is final by then.
		assert.Equal(t, "agents.global.legacy", rec.Header().Get(HeaderPerfTraceEndpoint))
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
	})

	t.Run("project sorted", func(t *testing.T) {
		_, snap, audits := get("/api/v1/projects/" + p.project.ID + "/agents?sort=updated&fit=500")
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

	t.Run("counts are stable across identical requests", func(t *testing.T) {
		_, a, _ := get("/api/v1/projects/" + p.project.ID + "/agents")
		_, b, _ := get("/api/v1/projects/" + p.project.ID + "/agents")
		assert.Equal(t, a.AuditRecords, b.AuditRecords)
		assert.Equal(t, a.AuthzStoreCalls, b.AuthzStoreCalls)
		for name, c := range a.StoreCalls {
			assert.Equal(t, c.Count, b.StoreCalls[name].Count, name)
		}
	})
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
