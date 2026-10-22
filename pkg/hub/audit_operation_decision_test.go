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

//go:build !no_sqlite

package hub

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// T2 (remaining-audit P4 slice 1): the ingress operation middleware leaves
// decision dispositions unchanged. The existing P1 decision-log tests run
// unmodified; these add the two pins the design names. Like the P1
// production-constructor tests, they change the process default logger and
// must not run in parallel.

// With the middleware in the production chain, a recorded decision's
// correlation_id is the request's X-Request-ID, and request.id is that same
// ID (the operation context and the request ID are one value).
func TestAuditOperation_T2_DecisionCorrelationEqualsRequestID(t *testing.T) {
	capture := &decisionLogCapture{}
	srv, s := newDecisionLogServer(t, capture)
	admin := seedDecisionLogAdmin(t, s)
	setDecisionLogFlag(t, srv, true)

	rec, _ := userRequest(t, srv, admin, http.MethodPost, stopAllPath, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requestID := rec.Header().Get("X-Request-ID")
	require.NotEmpty(t, requestID)
	require.Equal(t, uint64(1), srv.decisionAuditLogger.counts.get(decisionAuditEnqueued, true), "fail fast: the decision must be enqueued")

	require.NoError(t, srv.CloseAuditWriter(context.Background())) // drains
	lines := capture.recordsFor(requestID)
	require.Len(t, lines, 1, "all: %v", capture.records())
	assert.Equal(t, "authorization", lines[0]["family"])
	assert.Equal(t, requestID, lines[0]["correlation_id"])
	assert.Equal(t, requestID, auditGroup(t, lines[0], "request")["id"])
}

// A construction without RequestLog mints no request ID; the middleware
// stores no operation (no StartOperation fallback), so the decision is
// still excluded_correlation and nothing is written.
func TestAuditOperation_T2_NoRequestLoggerStillExcludedCorrelation(t *testing.T) {
	capture := &decisionLogCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	s := newBareTestStore(t)
	srv := newTestServerFromStore(t, s, nil)
	slog.SetDefault(prev)
	// Deliberately no SetRequestLogger: the fallback loggingMiddleware sets
	// no request ID.
	admin := seedDecisionLogAdmin(t, s)
	setDecisionLogFlag(t, srv, true)
	counts := srv.decisionAuditLogger.counts
	before := counts.get(decisionAuditExcludedCorrelation, true)

	rec, _ := userRequest(t, srv, admin, http.MethodPost, stopAllPath, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Empty(t, rec.Header().Get("X-Request-ID"), "fixture must have no request logger")

	assert.Equal(t, before+1, counts.get(decisionAuditExcludedCorrelation, true))
	assert.Zero(t, counts.get(decisionAuditEnqueued, true))
	require.NoError(t, srv.CloseAuditWriter(context.Background()))
	assert.Empty(t, capture.records())
	assert.Zero(t, srv.auditWriter.Snapshot().Enqueued)
}

// Re-entry characterization (R14 disclosure (b)): conduit_relay_admit.go
// re-runs applyMiddleware on a request derived from the outer one, and the
// inner RequestLog mints a new request ID. The middleware keeps the outer
// operation, so a decision made inside carries the OUTER correlation and
// omits request.id (validate.go:185 at fce92df9 forbids request.id !=
// correlation_id). Its disposition is unchanged: it is still recorded.
func TestAuditOperation_T2_ReentryDecisionKeepsOuterCorrelation(t *testing.T) {
	capture := &decisionLogCapture{}
	srv, s := newDecisionLogServer(t, capture)
	member, _, projectID := seedDecisionLogProject(t, s)
	setDecisionLogFlag(t, srv, true)
	counts := srv.decisionAuditLogger.counts
	before := counts.get(decisionAuditEnqueued, true)
	identity := NewAuthenticatedUser(member.ID, member.Email, member.DisplayName, member.Role, "web")

	var decision Decision
	decided := 0
	inner := auditOperationMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		c := r.Context()
		req := AuthzRequestFromContext(c, Resource{Type: "project", ID: projectID}, ActionRead)
		req.Permission = "project.read"
		decision = srv.authzService.Decide(c, req)
		decided++
	}))
	outer := auditOperationMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fwd := r.Clone(logging.ContextWithRequestMeta(r.Context(), &logging.RequestMeta{RequestID: "t2-reentry-inner"}))
		inner.ServeHTTP(w, fwd)
	}))
	ctx := logging.ContextWithRequestMeta(contextWithIdentity(context.Background(), identity),
		&logging.RequestMeta{RequestID: "t2-reentry-outer"})
	outer.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/conduit", nil).WithContext(ctx))

	require.Equal(t, 1, decided)
	require.True(t, decision.Allowed, "fixture: the member may read the project: %+v", decision)
	require.Equal(t, before+1, counts.get(decisionAuditEnqueued, true), "disposition unchanged: recorded")

	require.NoError(t, srv.CloseAuditWriter(context.Background())) // drains
	assert.Empty(t, capture.recordsFor("t2-reentry-inner"), "the inner request ID is not the correlation")
	lines := capture.recordsFor("t2-reentry-outer")
	require.Len(t, lines, 1, "all: %v", capture.records())
	assert.Equal(t, "authorization", lines[0]["family"])
	if request, ok := lines[0]["request"].(map[string]any); ok {
		assert.NotContains(t, request, "id", "request.id is omitted when it differs from the correlation")
	}
}
