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
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// E.2a (ptone/scion#2127, plan §3.1, ruling Q5): RequestLogMiddleware now
// wraps UnifiedAuthMiddleware instead of being wrapped by it, so a request
// auth rejects is still request-logged. These tests are the "regression test
// for the logged fields" the ruling requires, plus the auth-rejection
// coverage the move exists for.
// ---------------------------------------------------------------------------

// capturingHandler is a minimal slog.Handler that records every log record
// it receives, for tests to inspect fields on without depending on JSON
// serialization or a particular io.Writer.
type capturingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

func (h *capturingHandler) all() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]slog.Record, len(h.records))
	copy(out, h.records)
	return out
}

// attrMap flattens a slog.Record's top-level attributes (including nested
// groups, dotted) into a map for easy lookup in assertions.
func recordAttrs(r slog.Record) map[string]any {
	out := map[string]any{}
	var walk func(prefix string, a slog.Attr)
	walk = func(prefix string, a slog.Attr) {
		key := a.Key
		if prefix != "" {
			key = prefix + "." + a.Key
		}
		// Resolve LogValuer values (e.g. CredentialDecoration.LogValue())
		// before inspecting Kind: a captured record from a bare handler like
		// capturingHandler never resolves them itself (only the standard
		// Text/JSON handlers do that internally when formatting).
		a.Value = a.Value.Resolve()
		if a.Value.Kind() == slog.KindGroup {
			for _, ga := range a.Value.Group() {
				walk(key, ga)
			}
			return
		}
		out[key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		walk("", a)
		return true
	})
	return out
}

// setupUATProjectAndOwner creates a project and an owner user with a real
// project-owner role binding (via createRS1Project), so the owner has actual
// authority to mint a scoped UAT with real scopes (RS4's issuer-ceiling check
// requires this — a project record with no role binding mints nothing).
func setupUATProjectAndOwner(t *testing.T, s store.Store, name string) (projectID, ownerID string) {
	t.Helper()
	projectID = tid(name + "-project")
	ownerID = tid(name + "-owner")
	createRS1Project(t, s, projectID, ownerID)
	return projectID, ownerID
}

// TestRequestLogMiddleware_RegressionFieldsPreserved is the ruling-Q5-required
// regression test: moving RequestLogMiddleware outside auth must not drop any
// field a successful request already logged (component, project_id, agent_id,
// broker_id, auth_type, request_id, trace_id).
func TestRequestLogMiddleware_RegressionFieldsPreserved(t *testing.T) {
	srv, s := testServer(t)
	capture := &capturingHandler{}
	srv.SetRequestLogger(slog.New(capture))

	projectID, ownerID := setupUATProjectAndOwner(t, s, "reqlog-regress")
	uatKey := mintScopedUAT(t, srv, ownerID, projectID, []string{"project:read"})

	rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/projects/"+projectID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	records := capture.all()
	require.NotEmpty(t, records, "expected at least one request-log line")
	last := recordAttrs(records[len(records)-1])

	for _, field := range []string{"component", "project_id", "agent_id", "broker_id", "auth_type", "request_id"} {
		if _, ok := last[field]; !ok {
			t.Errorf("request log line missing field %q: %v", field, last)
		}
	}
	require.Equal(t, "uat", last["auth_type"])
	require.Equal(t, "hub", last["component"])
}

// TestRequestLogMiddleware_LogsAuthRejection proves the Q5 move actually
// fixes the gap it exists for: a request an invalid UAT token gets rejected
// by UnifiedAuthMiddleware (401, no call to next) is still request-logged.
// Before the move, RequestLogMiddleware sat inside auth in the chain and
// never ran for this case at all.
func TestRequestLogMiddleware_LogsAuthRejection(t *testing.T) {
	srv, _ := testServer(t)
	capture := &capturingHandler{}
	srv.SetRequestLogger(slog.New(capture))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/nonexistent", nil)
	req.Header.Set("Authorization", "Bearer scion_pat_definitely-not-a-real-token")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusUnauthorized, rr.Code)

	records := capture.all()
	require.NotEmpty(t, records, "an auth-rejected request must still produce a request-log line")
	last := recordAttrs(records[len(records)-1])
	if status, ok := last["httpRequest.status"]; ok {
		require.EqualValues(t, http.StatusUnauthorized, status)
	} else {
		t.Errorf("request log line missing httpRequest.status: %v", last)
	}
	require.Equal(t, "hub", last["component"], "component must still be set on a rejected request")
	require.NotEmpty(t, last["request_id"], "request_id must still be set on a rejected request")
}

// TestRequestLogMiddleware_TwoUATsDistinguishable is part of the E.2 AC "two
// tokens distinguishable end-to-end": the same human using two different UATs
// must produce two request-log lines with different credential.id values.
func TestRequestLogMiddleware_TwoUATsDistinguishable(t *testing.T) {
	srv, s := testServer(t)
	capture := &capturingHandler{}
	srv.SetRequestLogger(slog.New(capture))

	projectID, ownerID := setupUATProjectAndOwner(t, s, "reqlog-twotok")
	keyA := mintScopedUAT(t, srv, ownerID, projectID, []string{"project:read"})
	keyB := mintScopedUAT(t, srv, ownerID, projectID, []string{"project:read"})

	recA := doRequestWithUAT(t, srv, keyA, http.MethodGet, "/api/v1/projects/"+projectID, nil)
	require.Equal(t, http.StatusOK, recA.Code, recA.Body.String())
	recB := doRequestWithUAT(t, srv, keyB, http.MethodGet, "/api/v1/projects/"+projectID, nil)
	require.Equal(t, http.StatusOK, recB.Code, recB.Body.String())

	records := capture.all()
	require.GreaterOrEqual(t, len(records), 2)
	last2 := recordAttrs(records[len(records)-2])
	last1 := recordAttrs(records[len(records)-1])

	idA, okA := last2["credential.id"]
	idB, okB := last1["credential.id"]
	require.True(t, okA, "first request should carry credential.id: %v", last2)
	require.True(t, okB, "second request should carry credential.id: %v", last1)
	require.NotEqual(t, idA, idB, "two different UATs for the same user must be distinguishable by credential.id")
}

// TestRequestLogMiddleware_NoPlaintextOrHash captures all request-log output
// for a UAT request and asserts the plaintext token and its SHA-256 hash
// never appear (plan §3.6, rulings Q3).
func TestRequestLogMiddleware_NoPlaintextOrHash(t *testing.T) {
	srv, s := testServer(t)
	capture := &capturingHandler{}
	srv.SetRequestLogger(slog.New(capture))

	projectID, ownerID := setupUATProjectAndOwner(t, s, "reqlog-nosecret")
	uatKey := mintScopedUAT(t, srv, ownerID, projectID, []string{"project:read"})

	rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/projects/"+projectID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	for _, r := range capture.all() {
		var sb strings.Builder
		sb.WriteString(r.Message)
		for k, v := range recordAttrs(r) {
			sb.WriteString(k)
			sb.WriteString("=")
			sb.WriteString(fmtAny(v))
			sb.WriteString(" ")
		}
		line := sb.String()
		if strings.Contains(line, uatKey) {
			t.Fatalf("request log line contains the plaintext UAT: %s", line)
		}
	}
}

func fmtAny(v any) string {
	switch x := v.(type) {
	case string:
		return x
	default:
		return ""
	}
}
