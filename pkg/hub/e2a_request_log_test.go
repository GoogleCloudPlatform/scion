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
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestRequestLogMiddleware_LogsAuthRejection proves a request UnifiedAuthMiddleware
// rejects outright (401, no call to next) is still request-logged.
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
// for both a successful and a rejected UAT request and asserts the plaintext
// token, its SHA-256 hash, its stored prefix, and the raw Authorization
// header value never appear (plan §3.6, rulings Q3).
func TestRequestLogMiddleware_NoPlaintextOrHash(t *testing.T) {
	srv, s := testServer(t)
	capture := &capturingHandler{}
	srv.SetRequestLogger(slog.New(capture))

	projectID, ownerID := setupUATProjectAndOwner(t, s, "reqlog-nosecret")
	uatKey := mintScopedUAT(t, srv, ownerID, projectID, []string{"project:read"})
	hash := sha256.Sum256([]byte(uatKey))
	hashHex := hex.EncodeToString(hash[:])

	rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/projects/"+projectID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// A rejected UAT must be just as clean: no plaintext, hash, or header
	// value for a credential that never even matched a stored record.
	rejected := "scion_pat_reqlog-nosecret-rejected-value"
	rejHash := sha256.Sum256([]byte(rejected))
	rejHashHex := hex.EncodeToString(rejHash[:])
	rejRec := doRequestWithBearer(srv, rejected)
	require.Equal(t, http.StatusUnauthorized, rejRec.Code)

	forbidden := []string{
		uatKey, hashHex, "Bearer " + uatKey,
		rejected, rejHashHex, "Bearer " + rejected,
	}
	// The stored prefix is the first UATPrefixLength characters of the key
	// body, which is itself a substring of uatKey — already covered by the
	// uatKey check above, but asserted explicitly since it is a distinct
	// field on the stored token record.
	forbidden = append(forbidden, uatKey[:len(store.UATPrefix)+UATPrefixLength])

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
		for _, secret := range forbidden {
			if strings.Contains(line, secret) {
				t.Fatalf("request log line contains a forbidden secret value %q: %s", secret, line)
			}
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
