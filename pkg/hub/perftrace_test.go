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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
	assert.Equal(t, "agents.project.legacy", h[HeaderPerfTraceEndpoint])
	assert.Equal(t, "enrich=1500,messageability=30", h[HeaderPerfTracePhases])
	assert.Equal(t, "enrich=1,messageability=2", h[HeaderPerfTracePhaseCounts])
	assert.Equal(t, "GetEffectiveGroups=2,ListRoleBindingsForPrincipals=1", h[HeaderPerfTraceStoreCalls])
	assert.Equal(t, "GetEffectiveGroups=10,ListRoleBindingsForPrincipals=5", h[HeaderPerfTraceStoreTime])
	assert.Equal(t, "count=3,allow=2,deny=1,other=0,audit_us=3", h[HeaderPerfTraceDecisions])
	_, hasDB := h[HeaderPerfTraceDB]
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

func TestPerfTraceMiddleware_HeadersOnlyOnOptInAndNoRequestDataLogged(t *testing.T) {
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
	for _, optIn := range []bool{false, true} {
		logBuf.Reset()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents?token="+secretish, nil)
		req.Header.Set("Authorization", "Bearer "+secretish)
		if optIn {
			req.Header.Set(HeaderPerfTraceRequest, "1")
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusTeapot, rec.Code)
		assert.Equal(t, `{"ok":true}`, rec.Body.String())
		if optIn {
			assert.Equal(t, "agents.global.legacy", rec.Header().Get(HeaderPerfTraceEndpoint))
			assert.Equal(t, "enrich=1", rec.Header().Get(HeaderPerfTracePhaseCounts))
			assert.NotContains(t, rec.Header().Get(HeaderPerfTracePhases), "serialize",
				"headers are taken before the body is written")
		} else {
			for k := range rec.Header() {
				assert.False(t, strings.HasPrefix(k, "X-Scion-Perf"), "unexpected header %s without opt-in", k)
			}
		}
		line := logBuf.String()
		assert.Contains(t, line, `"msg":"perf_trace"`)
		assert.Contains(t, line, `"endpoint":"agents.global.legacy"`)
		assert.Contains(t, line, `"phase_serialize_n":1`)
		assert.NotContains(t, line, secretish)
		assert.NotContains(t, line, "/api/v1/agents")
	}
}

func TestPerfResponseWriter_ForwardsFlushAndUnwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	pw := &perfResponseWriter{ResponseWriter: rec, trace: newPerfTrace(nil)}
	pw.Flush()
	assert.True(t, rec.Flushed)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Same(t, rec, pw.Unwrap())
	_, _, err := pw.Hijack()
	assert.Error(t, err, "recorder cannot hijack; the error is forwarded")
}
