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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// ---------------------------------------------------------------------------
// Unit tests: trace, decorators, middleware.
// ---------------------------------------------------------------------------

func TestPerfPhaseStart_NoTraceIsNoopWithoutAllocation(t *testing.T) {
	ctx := context.Background()
	allocs := testing.AllocsPerRun(100, func() {
		done := perfPhaseStart(ctx, perfPhaseEnrich)
		done()
		perfSetEndpoint(ctx, perfEndpointAgentsGlobalLegacy)
		perfStoreCallStart(ctx, perfStoreGetUser)()
	})
	assert.Zero(t, allocs, "recording calls must not allocate when no trace is installed")
	assert.Nil(t, perfTraceFrom(ctx))
	assert.Equal(t, PerfTraceSnapshot{}, (*PerfTrace)(nil).Snapshot())
}

func TestPerfTrace_NamesAreBoundedAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for p := perfPhase(0); p < perfPhaseCount; p++ {
		name := p.String()
		require.NotEmpty(t, name, "phase %d has no name", p)
		require.False(t, seen[name], "duplicate phase name %q", name)
		seen[name] = true
	}
	seen = map[string]bool{}
	for op := perfStoreOp(0); op < perfStoreOpCount; op++ {
		name := op.String()
		require.NotEmpty(t, name, "store op %d has no name", op)
		require.False(t, seen[name], "duplicate store op name %q", name)
		seen[name] = true
	}
	assert.Equal(t, "unknown", perfPhaseCount.String())
	assert.Equal(t, "unknown", perfStoreOpCount.String())
}

func TestPerfTrace_SnapshotHeadersAndLogAttrs(t *testing.T) {
	tr := newPerfTrace(nil)
	ctx := contextWithPerfTrace(context.Background(), tr)
	perfSetEndpoint(ctx, perfEndpointAgentsProjectLegacy)
	tr.addPhase(perfPhaseEnrich, 1500*time.Microsecond)
	tr.addPhase(perfPhaseMessageability, 10*time.Microsecond)
	tr.addPhase(perfPhaseMessageability, 20*time.Microsecond)
	tr.addStoreCall(perfStoreGetEffectiveGroups, 7*time.Microsecond)
	tr.addStoreCall(perfStoreGetEffectiveGroups, 3*time.Microsecond)
	tr.addStoreCall(perfStoreListRoleBindingsForPrincipals, 5*time.Microsecond)
	tr.addAudit(perfAuditAllow, time.Microsecond)
	tr.addAudit(perfAuditAllow, time.Microsecond)
	tr.addAudit(perfAuditDeny, time.Microsecond)

	snap := tr.Snapshot()
	assert.Equal(t, "agents.project.legacy", snap.Endpoint)
	assert.Equal(t, PerfCount{Count: 1, Duration: 1500 * time.Microsecond}, snap.Phases["enrich"])
	assert.Equal(t, PerfCount{Count: 2, Duration: 30 * time.Microsecond}, snap.Phases["messageability"])
	assert.Len(t, snap.Phases, 2, "phases never entered are omitted")
	assert.Equal(t, int64(2), snap.StoreCalls["GetEffectiveGroups"].Count)
	assert.Equal(t, int64(3), snap.AuthzStoreCalls)
	assert.Equal(t, 15*time.Microsecond, snap.AuthzStoreTime)
	assert.Equal(t, int64(3), snap.AuditRecords)
	assert.Equal(t, int64(2), snap.AuditAllow)
	assert.Equal(t, int64(1), snap.AuditDeny)
	assert.False(t, snap.DBAvailable)

	h := snap.HeaderValues()
	assert.Equal(t, "agents.project.legacy", h[headerPerfTraceEndpoint])
	assert.Equal(t, "enrich=1500,messageability=30", h[headerPerfTracePhases])
	assert.Equal(t, "enrich=1,messageability=2", h[headerPerfTracePhaseCounts])
	assert.Equal(t, "GetEffectiveGroups=2,ListRoleBindingsForPrincipals=1", h[headerPerfTraceStoreCalls])
	assert.Equal(t, "GetEffectiveGroups=10,ListRoleBindingsForPrincipals=5", h[headerPerfTraceStoreTime])
	assert.Equal(t, "count=3,allow=2,deny=1,other=0,audit_us=3", h[headerPerfTraceDecisions])
	_, hasDB := h[headerPerfTraceDB]
	assert.False(t, hasDB)

	keys := map[string]bool{}
	for _, a := range snap.LogAttrs() {
		keys[a.Key] = true
	}
	for _, k := range []string{"endpoint", "phase_enrich_us", "phase_messageability_n",
		"store_GetEffectiveGroups_n", "authz_store_calls", "audit_records", "audit_deny"} {
		assert.True(t, keys[k], "missing log attr %q", k)
	}
}

// perfFakeStore records the arguments each counted method received.
type perfFakeStore struct {
	store.Store
	mu    sync.Mutex
	calls []string
}

func (f *perfFakeStore) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

var errPerfFake = errors.New("fake store error")

func (f *perfFakeStore) GetEffectiveGroups(_ context.Context, userID string) ([]string, error) {
	f.record("GetEffectiveGroups:" + userID)
	return []string{"g1", "g2"}, nil
}

func (f *perfFakeStore) GetUser(_ context.Context, id string) (*store.User, error) {
	f.record("GetUser:" + id)
	return nil, errPerfFake
}

func (f *perfFakeStore) ListRoleBindingsForPrincipals(_ context.Context, principals []store.PrincipalRef, scopeTypes, scopeIDs []string) ([]*store.RoleBinding, error) {
	f.record(fmt.Sprintf("ListRoleBindingsForPrincipals:%d:%v:%v", len(principals), scopeTypes, scopeIDs))
	return []*store.RoleBinding{{ID: "rb-1"}}, nil
}

func (f *perfFakeStore) ListAccessConstraints(_ context.Context, limit, offset int) ([]*store.AccessConstraint, error) {
	f.record(fmt.Sprintf("ListAccessConstraints:%d:%d", limit, offset))
	return nil, nil
}

func TestPerfAuthzStore_ForwardsUnchangedAndCounts(t *testing.T) {
	inner := &perfFakeStore{}
	assert.Same(t, inner, wrapAuthzStoreForPerfTrace(inner, false), "off must return the original store")
	wrapped := wrapAuthzStoreForPerfTrace(inner, true)
	require.IsType(t, perfAuthzStore{}, wrapped)

	tr := newPerfTrace(nil)
	ctx := contextWithPerfTrace(context.Background(), tr)

	groups, err := wrapped.GetEffectiveGroups(ctx, "u-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"g1", "g2"}, groups)
	_, err = wrapped.GetUser(ctx, "u-2")
	assert.Same(t, errPerfFake, err, "errors pass through unchanged")
	rbs, err := wrapped.ListRoleBindingsForPrincipals(ctx, []store.PrincipalRef{{Type: "user", ID: "u-1"}}, []string{"project"}, []string{"p-1"})
	require.NoError(t, err)
	require.Len(t, rbs, 1)
	assert.Equal(t, "rb-1", rbs[0].ID)
	_, _ = wrapped.ListAccessConstraints(ctx, 50, 100)
	_, _ = wrapped.GetEffectiveGroups(ctx, "u-3")

	// No trace in context: still forwarded, nothing counted anywhere.
	_, _ = wrapped.GetEffectiveGroups(context.Background(), "u-4")

	assert.Equal(t, []string{
		"GetEffectiveGroups:u-1", "GetUser:u-2", "ListRoleBindingsForPrincipals:1:[project]:[p-1]",
		"ListAccessConstraints:50:100", "GetEffectiveGroups:u-3", "GetEffectiveGroups:u-4",
	}, inner.calls)

	snap := tr.Snapshot()
	assert.Equal(t, int64(2), snap.StoreCalls["GetEffectiveGroups"].Count)
	assert.Equal(t, int64(1), snap.StoreCalls["GetUser"].Count)
	assert.Equal(t, int64(1), snap.StoreCalls["ListRoleBindingsForPrincipals"].Count)
	assert.Equal(t, int64(1), snap.StoreCalls["ListAccessConstraints"].Count)
	assert.Equal(t, int64(5), snap.AuthzStoreCalls)
}

type perfRecordingEmitter struct {
	mu      sync.Mutex
	records []*store.DecisionAuditRecord
}

func (e *perfRecordingEmitter) EmitDecisionAudit(_ context.Context, r *store.DecisionAuditRecord) {
	e.mu.Lock()
	e.records = append(e.records, r)
	e.mu.Unlock()
}

func (e *perfRecordingEmitter) take() []*store.DecisionAuditRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.records
	e.records = nil
	return out
}

func TestPerfAuditEmitter_ForwardsEachRecordOnceAndCountsByOutcome(t *testing.T) {
	inner := &perfRecordingEmitter{}
	assert.Same(t, inner, wrapAuditEmitterForPerfTrace(inner, false), "off must return the original emitter")
	assert.Nil(t, wrapAuditEmitterForPerfTrace(nil, true))
	wrapped := wrapAuditEmitterForPerfTrace(inner, true)

	tr := newPerfTrace(nil)
	ctx := contextWithPerfTrace(context.Background(), tr)
	allow := &store.DecisionAuditRecord{ID: "a", Result: "allow"}
	deny := &store.DecisionAuditRecord{ID: "d", Result: "deny", Reason: "no grant"}
	odd := &store.DecisionAuditRecord{ID: "o", Result: "weird"}
	untraced := &store.DecisionAuditRecord{ID: "u", Result: "allow"}
	wrapped.EmitDecisionAudit(ctx, allow)
	wrapped.EmitDecisionAudit(ctx, deny)
	wrapped.EmitDecisionAudit(ctx, odd)
	wrapped.EmitDecisionAudit(context.Background(), untraced)

	got := inner.take()
	require.Len(t, got, 4)
	assert.Same(t, allow, got[0])
	assert.Same(t, deny, got[1])
	assert.Same(t, odd, got[2])
	assert.Same(t, untraced, got[3])
	assert.Equal(t, store.DecisionAuditRecord{ID: "d", Result: "deny", Reason: "no grant"}, *deny, "record unchanged")

	snap := tr.Snapshot()
	assert.Equal(t, int64(1), snap.AuditAllow)
	assert.Equal(t, int64(1), snap.AuditDeny)
	assert.Equal(t, int64(1), snap.AuditOther)
	assert.Equal(t, int64(3), snap.AuditRecords)
}

func TestPerfTraceMiddleware_HeadersOnlyForAdminOptInAndNoRequestDataLogged(t *testing.T) {
	var logBuf bytes.Buffer
	srv := &Server{perfTraceLog: slog.New(slog.NewJSONHandler(&logBuf, nil))}
	handler := srv.perfTraceMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		perfSetEndpoint(r.Context(), perfEndpointAgentsGlobalLegacy)
		perfPhaseStart(r.Context(), perfPhaseEnrich)()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	const secretish = "sk-test-do-not-log-0123456789"
	admin := NewAuthenticatedUser("u-admin", "admin@example.com", "Admin", "admin", "web")
	member := NewAuthenticatedUser("u-member", "member@example.com", "Member", "member", "web")
	for _, tc := range []struct {
		name      string
		identity  Identity
		optIn     bool
		wantPerfH bool
	}{
		{"admin opt-in", admin, true, true},
		{"admin no opt-in", admin, false, false},
		{"member opt-in", member, true, false},
		{"no identity opt-in", nil, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logBuf.Reset()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/agents?token="+secretish, nil)
			req.Header.Set("Authorization", "Bearer "+secretish)
			if tc.identity != nil {
				req = req.WithContext(contextWithIdentity(req.Context(), tc.identity))
			}
			if tc.optIn {
				req.Header.Set(headerPerfTraceRequest, "1")
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusTeapot, rec.Code)
			assert.Equal(t, `{"ok":true}`, rec.Body.String())
			if tc.wantPerfH {
				assert.Equal(t, "agents.global.legacy", rec.Header().Get(headerPerfTraceEndpoint))
				assert.Equal(t, "enrich=1", rec.Header().Get(headerPerfTracePhaseCounts))
				assert.NotContains(t, rec.Header().Get(headerPerfTracePhases), "serialize",
					"headers are taken before the body is written")
			} else {
				assertNoPerfHeaders(t, rec.Header())
			}
			line := logBuf.String()
			assert.Contains(t, line, `"msg":"perf_trace"`)
			assert.Contains(t, line, `"endpoint":"agents.global.legacy"`)
			assert.Contains(t, line, `"phase_serialize_n":1`)
			assert.NotContains(t, line, secretish)
			assert.NotContains(t, line, "/api/v1/agents")
			assert.NotContains(t, line, "u-admin")
			assert.NotContains(t, line, "admin@example.com")
		})
	}
}

func assertNoPerfHeaders(t *testing.T, h http.Header) {
	t.Helper()
	for k := range h {
		assert.False(t, strings.HasPrefix(strings.ToLower(k), "x-scion-perf"), "unexpected perf header %s", k)
	}
}

// TestPerfHeadersAllowed_IdentityKinds: only an unscoped local platform
// admin on an authenticated endpoint gets the perf headers. Every other
// identity kind is tested explicitly.
func TestPerfHeadersAllowed_IdentityKinds(t *testing.T) {
	admin := NewAuthenticatedUser("u-admin", "admin@example.com", "Admin", "admin", "web")
	member := NewAuthenticatedUser("u-member", "member@example.com", "Member", "member", "web")
	dev := NewDevUser(DevUserConfig{Username: "dev", DisplayName: "Dev", Email: "dev@localhost"})
	agent := &agentIdentityWrapper{&AgentTokenClaims{ProjectID: "p-1", Scopes: []AgentTokenScope{ScopeProjectRead}}}
	agent.Subject = "agent-1"
	for _, tc := range []struct {
		name     string
		path     string
		identity Identity
		set      bool
		want     bool
	}{
		{"unscoped local admin", "/api/v1/agents", admin, true, true},
		{"dev user (always role admin, local, unscoped)", "/api/v1/agents", dev, true, true},
		{"admin on unauthenticated endpoint /healthz", "/healthz", admin, true, false},
		{"admin on unauthenticated endpoint /api/v1/settings/public", "/api/v1/settings/public", admin, true, false},
		{"no identity", "/api/v1/agents", nil, false, false},
		{"nil identity value", "/api/v1/agents", nil, true, false},
		{"typed-nil user", "/api/v1/agents", (*AuthenticatedUser)(nil), true, false},
		{"scoped token of an admin", "/api/v1/agents", NewScopedUserIdentity(admin, "p-1", []string{"agent:read"}), true, false},
		{"agent JWT", "/api/v1/agents", agent, true, false},
		{"broker HMAC", "/api/v1/agents", NewBrokerIdentity("broker-1"), true, false},
		{"federated user with admin role", "/api/v1/agents", NewFederatedUserIdentity("https://other.example", "sub", "fed@example.com", "Fed", "admin", nil), true, false},
		{"federated agent", "/api/v1/agents", NewFederatedAgentIdentity("https://other.example", "a-1", "p-1", "a", "u", nil, nil), true, false},
		{"non-admin user", "/api/v1/agents", member, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.set {
				ctx := contextWithIdentity(req.Context(), tc.identity)
				if b, ok := tc.identity.(BrokerIdentity); ok {
					ctx = contextWithBrokerIdentity(ctx, b) // as the broker auth middleware does
				}
				req = req.WithContext(ctx)
			}
			assert.Equal(t, tc.want, perfHeadersAllowed(req))
		})
	}
}

// TestPerfHeaders_SingleChokepoint: the X-Scion-Perf-* response header
// names are written as literals only in perftrace.go, and only
// perftrace_middleware.go sets response headers from them (through
// HeaderValues, behind perfHeadersAllowed). apibench reads them separately.
func TestPerfHeaders_SingleChokepoint(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		require.NoError(t, err)
		src := string(data)
		if f != "perftrace.go" {
			// A quoted literal is a header name in code; comments may mention them.
			assert.NotContains(t, strings.ToLower(src), `"x-scion-perf`, "%s must not spell a perf header name", f)
			if f != "perftrace_middleware.go" {
				assert.NotContains(t, src, "HeaderValues()", "%s must not render perf headers", f)
			}
			for _, name := range []string{"headerPerfTraceEndpoint", "headerPerfTracePhases", "headerPerfTracePhaseCounts",
				"headerPerfTraceStoreCalls", "headerPerfTraceStoreTime", "headerPerfTraceDecisions", "headerPerfTraceDB"} {
				assert.NotContains(t, src, name, "%s must not reference %s", f, name)
			}
		}
	}
	defs, err := os.ReadFile("perftrace.go")
	require.NoError(t, err)
	assert.Equal(t, 8, strings.Count(string(defs), `= "X-Scion-Perf-`), "each header name is defined once, as a constant")
	mw, err := os.ReadFile("perftrace_middleware.go")
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(mw), "HeaderValues()"), "headers are rendered at one call site")

	// perfResponseWriter is constructed in exactly one place, the
	// middleware, where emitHeaders is set behind perfHeadersAllowed.
	constructions := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		require.NoError(t, err)
		n := strings.Count(string(data), "perfResponseWriter{")
		if n > 0 {
			assert.Equal(t, "perftrace_middleware.go", f, "perfResponseWriter constructed outside the middleware")
		}
		constructions += n
	}
	assert.Equal(t, 1, constructions)
	assert.Equal(t, 1, strings.Count(string(mw), "emitHeaders:"), "emitHeaders is set at one place")
	assert.Contains(t, string(mw), "emitHeaders:    r.Header.Get(headerPerfTraceRequest) == \"1\" && perfHeadersAllowed(r),")
}

// TestPerfTraceLogLine_AttributeKeySet pins the exact set of log keys.
func TestPerfTraceLogLine_AttributeKeySet(t *testing.T) {
	tr := newPerfTrace(nil)
	for p := perfPhase(0); p < perfPhaseCount; p++ {
		tr.addPhase(p, time.Microsecond)
	}
	for op := perfStoreOp(0); op < perfStoreOpCount; op++ {
		tr.addStoreCall(op, time.Microsecond)
	}
	tr.addAudit(perfAuditAllow, time.Microsecond)
	tr.addSSEEvent(time.Microsecond)
	snap := tr.Snapshot()
	snap.DBAvailable = true

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/secret-project/agents?q=x", nil)
	req = req.WithContext(logging.ContextWithRequestMeta(req.Context(), &logging.RequestMeta{RequestID: "req-1", ProjectID: "secret-project"}))
	logPerfTraceLine(logger, req, func() PerfTraceSnapshot { return snap }, slog.String("sse_stage", "close"))

	var got map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	want := map[string]bool{
		"time": true, "level": true, "msg": true,
		"endpoint": true, "elapsed_us": true, "method": true, "request_id": true, "sse_stage": true,
		"authz_store_calls": true, "authz_store_us": true,
		"audit_records": true, "audit_allow": true, "audit_deny": true, "audit_other": true, "audit_emit_us": true,
		"sse_events": true, "db_wait_count": true, "db_wait_us": true, "db_in_use": true, "db_open": true,
	}
	for p := perfPhase(0); p < perfPhaseCount; p++ {
		want["phase_"+p.String()+"_us"] = true
		want["phase_"+p.String()+"_n"] = true
	}
	for op := perfStoreOp(0); op < perfStoreOpCount; op++ {
		want["store_"+op.String()+"_n"] = true
		want["store_"+op.String()+"_us"] = true
	}
	gotKeys := map[string]bool{}
	for k := range got {
		gotKeys[k] = true
	}
	assert.Equal(t, want, gotKeys)
	assert.NotContains(t, buf.String(), "secret-project")
}

func TestPerfResponseWriter_ForwardsFlushAndUnwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	pw := &perfResponseWriter{ResponseWriter: rec, trace: newPerfTrace(nil)}
	pw.Flush()
	assert.True(t, rec.Flushed)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Same(t, rec, pw.Unwrap())
	_, _, err := pw.Hijack()
	assert.ErrorIs(t, err, http.ErrNotSupported, "recorder cannot hijack; the error wraps http.ErrNotSupported")
}

func TestPerfTraceMiddleware_PanicStillLogs(t *testing.T) {
	var logBuf bytes.Buffer
	srv := &Server{perfTraceLog: slog.New(slog.NewJSONHandler(&logBuf, nil))}
	handler := srv.perfTraceMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		perfPhaseStart(r.Context(), perfPhaseEnrich)()
		panic("boom")
	}))
	assert.PanicsWithValue(t, "boom", func() {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil))
	})
	assert.Contains(t, logBuf.String(), `"phase_enrich_n":1`)
}

// perfSentinelStore implements the 19 counted methods. Each records its
// name and arguments and returns a per-method sentinel value and error.
type perfSentinelStore struct {
	store.Store
	got []string
}

var perfSentinelErr = map[string]error{}

func perfSentinel(name string) error {
	if _, ok := perfSentinelErr[name]; !ok {
		perfSentinelErr[name] = errors.New("sentinel error " + name)
	}
	return perfSentinelErr[name]
}

func (f *perfSentinelStore) rec(name string, args ...any) error {
	f.got = append(f.got, fmt.Sprintf("%s%v", name, args))
	return perfSentinel(name)
}

func (f *perfSentinelStore) GetEffectiveGroups(_ context.Context, a string) ([]string, error) {
	return []string{"v-GetEffectiveGroups"}, f.rec("GetEffectiveGroups", a)
}
func (f *perfSentinelStore) GetEffectiveGroupsForAgent(_ context.Context, a string) ([]string, error) {
	return []string{"v-GetEffectiveGroupsForAgent"}, f.rec("GetEffectiveGroupsForAgent", a)
}
func (f *perfSentinelStore) GetParentGroups(_ context.Context, a string) ([]string, error) {
	return []string{"v-GetParentGroups"}, f.rec("GetParentGroups", a)
}
func (f *perfSentinelStore) GetUserGroups(_ context.Context, a string) ([]store.GroupMember, error) {
	return []store.GroupMember{{GroupID: "v-GetUserGroups"}}, f.rec("GetUserGroups", a)
}
func (f *perfSentinelStore) GetGroupMembership(_ context.Context, a, b, c string) (*store.GroupMember, error) {
	return &store.GroupMember{GroupID: "v-GetGroupMembership"}, f.rec("GetGroupMembership", a, b, c)
}
func (f *perfSentinelStore) GetGroupBySlug(_ context.Context, a string) (*store.Group, error) {
	return &store.Group{ID: "v-GetGroupBySlug"}, f.rec("GetGroupBySlug", a)
}
func (f *perfSentinelStore) ListRoleBindingsForPrincipals(_ context.Context, a []store.PrincipalRef, b, c []string) ([]*store.RoleBinding, error) {
	return []*store.RoleBinding{{ID: "v-ListRoleBindingsForPrincipals"}}, f.rec("ListRoleBindingsForPrincipals", a, b, c)
}
func (f *perfSentinelStore) ListRoleBindingsForPrincipal(_ context.Context, a, b string) ([]*store.RoleBinding, error) {
	return []*store.RoleBinding{{ID: "v-ListRoleBindingsForPrincipal"}}, f.rec("ListRoleBindingsForPrincipal", a, b)
}
func (f *perfSentinelStore) GetRoleDefinition(_ context.Context, a string) (*store.RoleDefinition, error) {
	return &store.RoleDefinition{ID: "v-GetRoleDefinition"}, f.rec("GetRoleDefinition", a)
}
func (f *perfSentinelStore) GetRoleDefinitionsByIDs(_ context.Context, a []string) (map[string]*store.RoleDefinition, error) {
	return map[string]*store.RoleDefinition{"k": {ID: "v-GetRoleDefinitionsByIDs"}}, f.rec("GetRoleDefinitionsByIDs", a)
}
func (f *perfSentinelStore) GetRoleDefinitionByName(_ context.Context, a, b string) (*store.RoleDefinition, error) {
	return &store.RoleDefinition{ID: "v-GetRoleDefinitionByName"}, f.rec("GetRoleDefinitionByName", a, b)
}
func (f *perfSentinelStore) ListAccessConstraints(_ context.Context, a, b int) ([]*store.AccessConstraint, error) {
	return []*store.AccessConstraint{{ID: "v-ListAccessConstraints"}}, f.rec("ListAccessConstraints", a, b)
}
func (f *perfSentinelStore) GetDelegationEdgesForDelegate(_ context.Context, a, b string) ([]*store.DelegationEdge, error) {
	return []*store.DelegationEdge{{ID: "v-GetDelegationEdgesForDelegate"}}, f.rec("GetDelegationEdgesForDelegate", a, b)
}
func (f *perfSentinelStore) GetUser(_ context.Context, a string) (*store.User, error) {
	return &store.User{ID: "v-GetUser"}, f.rec("GetUser", a)
}
func (f *perfSentinelStore) GetUserAccessToken(_ context.Context, a string) (*store.UserAccessToken, error) {
	return &store.UserAccessToken{ID: "v-GetUserAccessToken"}, f.rec("GetUserAccessToken", a)
}
func (f *perfSentinelStore) GetAgent(_ context.Context, a string) (*store.Agent, error) {
	return &store.Agent{ID: "v-GetAgent"}, f.rec("GetAgent", a)
}
func (f *perfSentinelStore) GetProject(_ context.Context, a string) (*store.Project, error) {
	return &store.Project{ID: "v-GetProject"}, f.rec("GetProject", a)
}
func (f *perfSentinelStore) GetProjectMembership(_ context.Context, a, b string) (*store.ProjectMembership, error) {
	return &store.ProjectMembership{ProjectID: "v-GetProjectMembership"}, f.rec("GetProjectMembership", a, b)
}
func (f *perfSentinelStore) GetHubSetting(_ context.Context, a string) (*store.HubSetting, error) {
	return &store.HubSetting{Section: "v-GetHubSetting"}, f.rec("GetHubSetting", a)
}

// TestPerfAuthzStore_AllMethodsForwardAndCountExactlyTheirOp covers all 19
// wrapped methods: distinct arguments, results and errors pass through
// unchanged, and exactly that op's counter goes up by one.
func TestPerfAuthzStore_AllMethodsForwardAndCountExactlyTheirOp(t *testing.T) {
	type call struct {
		op       perfStoreOp
		wantArgs string
		do       func(ctx context.Context, s store.Store) (any, error)
		wantVal  func(v any) string
	}
	pr := []store.PrincipalRef{{Type: "user", ID: "pr-1"}, {Type: "group", ID: "pr-2"}}
	calls := []call{
		{perfStoreGetEffectiveGroups, "GetEffectiveGroups[a1]", func(c context.Context, s store.Store) (any, error) { return s.GetEffectiveGroups(c, "a1") }, func(v any) string { return v.([]string)[0] }},
		{perfStoreGetEffectiveGroupsForAgent, "GetEffectiveGroupsForAgent[a2]", func(c context.Context, s store.Store) (any, error) { return s.GetEffectiveGroupsForAgent(c, "a2") }, func(v any) string { return v.([]string)[0] }},
		{perfStoreGetParentGroups, "GetParentGroups[a3]", func(c context.Context, s store.Store) (any, error) { return s.GetParentGroups(c, "a3") }, func(v any) string { return v.([]string)[0] }},
		{perfStoreGetUserGroups, "GetUserGroups[a4]", func(c context.Context, s store.Store) (any, error) { return s.GetUserGroups(c, "a4") }, func(v any) string { return v.([]store.GroupMember)[0].GroupID }},
		{perfStoreGetGroupMembership, "GetGroupMembership[g5 mt5 mid5]", func(c context.Context, s store.Store) (any, error) {
			return s.GetGroupMembership(c, "g5", "mt5", "mid5")
		}, func(v any) string { return v.(*store.GroupMember).GroupID }},
		{perfStoreGetGroupBySlug, "GetGroupBySlug[a6]", func(c context.Context, s store.Store) (any, error) { return s.GetGroupBySlug(c, "a6") }, func(v any) string { return v.(*store.Group).ID }},
		{perfStoreListRoleBindingsForPrincipals, "ListRoleBindingsForPrincipals[[{user pr-1} {group pr-2}] [st7] [si7a si7b]]", func(c context.Context, s store.Store) (any, error) {
			return s.ListRoleBindingsForPrincipals(c, pr, []string{"st7"}, []string{"si7a", "si7b"})
		}, func(v any) string { return v.([]*store.RoleBinding)[0].ID }},
		{perfStoreListRoleBindingsForPrincipal, "ListRoleBindingsForPrincipal[pt8 pid8]", func(c context.Context, s store.Store) (any, error) {
			return s.ListRoleBindingsForPrincipal(c, "pt8", "pid8")
		}, func(v any) string { return v.([]*store.RoleBinding)[0].ID }},
		{perfStoreGetRoleDefinition, "GetRoleDefinition[a9]", func(c context.Context, s store.Store) (any, error) { return s.GetRoleDefinition(c, "a9") }, func(v any) string { return v.(*store.RoleDefinition).ID }},
		{perfStoreGetRoleDefinitionsByIDs, "GetRoleDefinitionsByIDs[[i10a i10b]]", func(c context.Context, s store.Store) (any, error) {
			return s.GetRoleDefinitionsByIDs(c, []string{"i10a", "i10b"})
		}, func(v any) string { return v.(map[string]*store.RoleDefinition)["k"].ID }},
		{perfStoreGetRoleDefinitionByName, "GetRoleDefinitionByName[n11 sc11]", func(c context.Context, s store.Store) (any, error) {
			return s.GetRoleDefinitionByName(c, "n11", "sc11")
		}, func(v any) string { return v.(*store.RoleDefinition).ID }},
		{perfStoreListAccessConstraints, "ListAccessConstraints[12 120]", func(c context.Context, s store.Store) (any, error) { return s.ListAccessConstraints(c, 12, 120) }, func(v any) string { return v.([]*store.AccessConstraint)[0].ID }},
		{perfStoreGetDelegationEdgesForDelegate, "GetDelegationEdgesForDelegate[dt13 did13]", func(c context.Context, s store.Store) (any, error) {
			return s.GetDelegationEdgesForDelegate(c, "dt13", "did13")
		}, func(v any) string { return v.([]*store.DelegationEdge)[0].ID }},
		{perfStoreGetUser, "GetUser[a14]", func(c context.Context, s store.Store) (any, error) { return s.GetUser(c, "a14") }, func(v any) string { return v.(*store.User).ID }},
		{perfStoreGetUserAccessToken, "GetUserAccessToken[a15]", func(c context.Context, s store.Store) (any, error) { return s.GetUserAccessToken(c, "a15") }, func(v any) string { return v.(*store.UserAccessToken).ID }},
		{perfStoreGetAgent, "GetAgent[a16]", func(c context.Context, s store.Store) (any, error) { return s.GetAgent(c, "a16") }, func(v any) string { return v.(*store.Agent).ID }},
		{perfStoreGetProject, "GetProject[a17]", func(c context.Context, s store.Store) (any, error) { return s.GetProject(c, "a17") }, func(v any) string { return v.(*store.Project).ID }},
		{perfStoreGetProjectMembership, "GetProjectMembership[p18 u18]", func(c context.Context, s store.Store) (any, error) { return s.GetProjectMembership(c, "p18", "u18") }, func(v any) string { return v.(*store.ProjectMembership).ProjectID }},
		{perfStoreGetHubSetting, "GetHubSetting[a19]", func(c context.Context, s store.Store) (any, error) { return s.GetHubSetting(c, "a19") }, func(v any) string { return v.(*store.HubSetting).Section }},
	}
	require.Len(t, calls, int(perfStoreOpCount), "every counted op has a case")
	seen := map[perfStoreOp]bool{}
	for _, c := range calls {
		seen[c.op] = true
	}
	require.Len(t, seen, int(perfStoreOpCount), "no op is covered twice")

	for _, c := range calls {
		name := c.op.String()
		t.Run(name, func(t *testing.T) {
			inner := &perfSentinelStore{}
			wrapped := wrapAuthzStoreForPerfTrace(inner, true)
			tr := newPerfTrace(nil)
			ctx := contextWithPerfTrace(context.Background(), tr)

			v, err := c.do(ctx, wrapped)
			assert.Same(t, perfSentinel(name), err, "error passes through unchanged")
			assert.Equal(t, "v-"+name, c.wantVal(v), "result passes through unchanged")
			assert.Equal(t, []string{c.wantArgs}, inner.got, "the same method gets the same arguments")

			snap := tr.Snapshot()
			assert.Equal(t, int64(1), snap.AuthzStoreCalls, "exactly one counted call")
			assert.Equal(t, map[string]int64{name: 1}, func() map[string]int64 {
				out := map[string]int64{}
				for k, v := range snap.StoreCalls {
					out[k] = v.Count
				}
				return out
			}(), "only this op's counter moved")
		})
	}
}
