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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/auditevent"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// serveAuditOperation runs auditOperationMiddleware once on a request whose
// context is ctx and returns the operation the downstream handler saw.
func serveAuditOperation(t *testing.T, ctx context.Context) (auditevent.AuditOperationContext, bool) {
	t.Helper()
	var (
		got    auditevent.AuditOperationContext
		gotOK  bool
		called int
	)
	h := auditOperationMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called++
		got, gotOK = auditevent.OperationFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health-probe", nil).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if called != 1 {
		t.Fatalf("downstream handler called %d times, want 1", called)
	}
	return got, gotOK
}

func withTestRequestID(ctx context.Context, id string) context.Context {
	return logging.ContextWithRequestMeta(ctx, &logging.RequestMeta{RequestID: id})
}

// T1: a trusted request ID becomes the operation's correlation ID.
func TestAuditOperationMiddleware_T1_TrustedRequestIDSetsOperation(t *testing.T) {
	op, ok := serveAuditOperation(t, withTestRequestID(context.Background(), "req-trusted-1"))
	if !ok {
		t.Fatal("no operation context for a request with a trusted request ID")
	}
	if op.CorrelationID != "req-trusted-1" {
		t.Errorf("correlation ID = %q, want the request ID", op.CorrelationID)
	}
}

// T1: an operation already on the context is kept on re-entry, as when
// conduit_relay_admit.go re-runs applyMiddleware and the inner RequestLog
// mints a new request ID. Q10 characterization: correlation stays the outer
// operation while the current request ID differs.
func TestAuditOperationMiddleware_T1_ReentryKeepsOuterOperation(t *testing.T) {
	outer, err := auditevent.NewOperationContext("req-outer")
	if err != nil {
		t.Fatalf("NewOperationContext: %v", err)
	}
	ctx := auditevent.ContextWithOperation(context.Background(), outer)
	ctx = withTestRequestID(ctx, "req-inner")

	op, ok := serveAuditOperation(t, ctx)
	if !ok {
		t.Fatal("re-entry lost the outer operation context")
	}
	if op.CorrelationID != "req-outer" {
		t.Errorf("correlation ID = %q, want the outer operation %q", op.CorrelationID, "req-outer")
	}
}

// T1: the full re-entry shape: the middleware runs twice, the second time
// under a new request ID on a context derived from the first request.
func TestAuditOperationMiddleware_T1_NestedChainsKeepFirstOperation(t *testing.T) {
	var inner auditevent.AuditOperationContext
	var innerOK bool
	innerChain := auditOperationMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		inner, innerOK = auditevent.OperationFromContext(r.Context())
	}))
	outerChain := auditOperationMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fwd := httptest.NewRequest(http.MethodGet, "/api/v1/conduit", nil).
			WithContext(withTestRequestID(r.Context(), "req-second"))
		innerChain.ServeHTTP(w, fwd)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conduit/relay-admit", nil).
		WithContext(withTestRequestID(context.Background(), "req-first"))
	outerChain.ServeHTTP(httptest.NewRecorder(), req)

	if !innerOK {
		t.Fatal("inner chain saw no operation context")
	}
	if inner.CorrelationID != "req-first" {
		t.Errorf("inner correlation ID = %q, want %q", inner.CorrelationID, "req-first")
	}
}

// T1: no request ID means no operation context (no StartOperation fallback).
func TestAuditOperationMiddleware_T1_NoRequestIDStoresNothing(t *testing.T) {
	if op, ok := serveAuditOperation(t, context.Background()); ok {
		t.Errorf("operation context %+v stored without a request ID", op)
	}
	if op, ok := serveAuditOperation(t, withTestRequestID(context.Background(), "")); ok {
		t.Errorf("operation context %+v stored for an empty request ID", op)
	}
}

// T1: a request ID longer than 128 bytes fails NewOperationContext and
// stores nothing; exactly 128 bytes is accepted.
func TestAuditOperationMiddleware_T1_OversizeRequestIDStoresNothing(t *testing.T) {
	if op, ok := serveAuditOperation(t, withTestRequestID(context.Background(), strings.Repeat("a", 129))); ok {
		t.Errorf("operation context %+v stored for a 129-byte request ID", op)
	}
	op, ok := serveAuditOperation(t, withTestRequestID(context.Background(), strings.Repeat("b", 128)))
	if !ok || op.CorrelationID != strings.Repeat("b", 128) {
		t.Errorf("128-byte request ID: got %+v ok=%v, want it stored", op, ok)
	}
}
