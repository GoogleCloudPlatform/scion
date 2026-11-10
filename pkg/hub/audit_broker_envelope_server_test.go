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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
)

// Production-constructor tests for the converted broker family
// (remaining-audit P4 slice 1, T5-T7). They reuse the P1 harness
// (newDecisionLogServer): the capturing handler is the process default at
// New, so the shared async audit writer wraps it; the server has the real
// middleware chain, request logger and writer. Tests synchronize by
// closing the writer (which drains it) or on the capture's entered
// channel; there are no sleeps. They change the process default logger
// and must not run in parallel.

// brokerFamilyRecords keeps the broker family records.
func brokerFamilyRecords(lines []map[string]any) []map[string]any {
	var out []map[string]any
	for _, line := range lines {
		if line["family"] == "broker" {
			out = append(out, line)
		}
	}
	return out
}

// sendMissingTimestampBrokerRequest sends a broker-authenticated request
// that fails HMAC validation before any store access (missing timestamp)
// and returns the response.
func sendMissingTimestampBrokerRequest(srv *Server) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runtime-brokers", nil)
	req.Header.Set(HeaderBrokerID, "t6-claimed-broker")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// T5: an HMAC failure gives exactly one broker/auth_failure record whose
// correlation_id is the X-Request-ID, at warning; a registration gives
// exactly one broker/register; a successful broker request (auth_success,
// legacy DEBUG) gives no envelope. The decision experiment is off (the
// default) throughout.
func TestBrokerEnvelope_T5_ProductionConstructor(t *testing.T) {
	capture := &decisionLogCapture{}
	srv, s := newDecisionLogServer(t, capture)
	require.False(t, srv.experimentEnabled(experiments.AuthorizationDecisionAuditV2), "default must be off")
	brokerID, signReq := setupSignedBrokerRequest(t, srv.brokerAuthService, s)

	forged := signReq(http.MethodGet, "/api/v1/runtime-brokers", nil)
	forged.Header.Set(HeaderSignature, strings.Repeat("0", 44))
	failRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(failRec, forged)
	require.Equal(t, http.StatusUnauthorized, failRec.Code, failRec.Body.String())
	failID := failRec.Header().Get("X-Request-ID")
	require.NotEmpty(t, failID)

	okRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(okRec, signReq(http.MethodGet, "/api/v1/runtime-brokers", nil))
	successID := okRec.Header().Get("X-Request-ID")
	require.NotEmpty(t, successID)

	member := newHubMemberUser(t, s, "t5-register-member")
	regRec := doRequestAsUserLoopback(t, srv, member, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{Name: "t5-register-broker"})
	require.Equal(t, http.StatusCreated, regRec.Code, regRec.Body.String())
	regID := regRec.Header().Get("X-Request-ID")
	require.NotEmpty(t, regID)
	resp := decodeBrokerRegistration(t, regRec.Body)

	require.NoError(t, srv.CloseAuditWriter(context.Background())) // drains

	fail := brokerFamilyRecords(capture.recordsFor(failID))
	require.Len(t, fail, 1, "exactly one broker record for the HMAC failure; all: %v", capture.records())
	assert.Equal(t, "auth_failure", fail[0]["action"])
	assert.Equal(t, "decision", fail[0]["phase"])
	assert.Equal(t, "deny", fail[0]["outcome"])
	assert.Equal(t, "warning", fail[0]["severity"])
	assert.Equal(t, "WARN", fail[0]["level"])
	assert.Equal(t, failID, fail[0]["correlation_id"])
	assert.Equal(t, failID, auditGroup(t, fail[0], "request")["id"])
	assert.Equal(t, map[string]any{"kind": "runtime_broker", "id": brokerID}, auditGroup(t, fail[0], "resource"))
	assert.Equal(t, "invalid_signature", auditGroup(t, fail[0], "payload")["failure_code"])
	assert.NotContains(t, fail[0], "principal")

	assert.Empty(t, brokerFamilyRecords(capture.recordsFor(successID)), "auth_success stays legacy DEBUG: no envelope")

	reg := brokerFamilyRecords(capture.recordsFor(regID))
	require.Len(t, reg, 1, "exactly one broker record for the registration; all: %v", capture.records())
	assert.Equal(t, "register", reg[0]["action"])
	assert.Equal(t, "commit", reg[0]["phase"])
	assert.Equal(t, "succeeded", reg[0]["outcome"])
	assert.Equal(t, "INFO", reg[0]["level"])
	assert.Equal(t, regID, reg[0]["correlation_id"])
	assert.Equal(t, map[string]any{"kind": "user", "id": member.ID}, auditGroup(t, reg[0], "principal"))
	assert.Equal(t, map[string]any{"kind": "runtime_broker", "id": resp.BrokerID}, auditGroup(t, reg[0], "resource"))
	assert.Equal(t, map[string]any{"operation": "register", "source_ip": "127.0.0.1"}, auditGroup(t, reg[0], "payload"))
	assertSessionCredential(t, reg[0])
	require.NotEmpty(t, resp.JoinToken)
	assert.NotContains(t, fmt.Sprint(capture.records()), resp.JoinToken, "the join token never reaches an audit record")

	snap := srv.auditWriter.Snapshot()
	assert.Equal(t, uint64(2), snap.Enqueued, "only the two converted events were enqueued: %+v", snap)
	assert.Equal(t, snap.Enqueued, snap.Written, "%+v", snap)
}

// T6 (P1-6 pattern): with the audit writer blocked inside its handler and
// its queue full, broker request status codes are unchanged and the
// dropped records are counted by the writer. The test finishes while the
// handler is still blocked; it is released only in cleanup.
func TestBrokerEnvelope_T6_StatusUnchangedWithWriterBlockedAndFull(t *testing.T) {
	capture := &decisionLogCapture{block: make(chan struct{}), entered: make(chan struct{}, 1)}
	srv, s := newDecisionLogServer(t, capture)
	// Registered after the server's Shutdown cleanup, so it runs first.
	t.Cleanup(func() {
		close(capture.block)
		if err := srv.CloseAuditWriter(context.Background()); err != nil {
			t.Errorf("CloseAuditWriter after release: %v", err)
		}
	})

	require.Equal(t, http.StatusUnauthorized, sendMissingTimestampBrokerRequest(srv).Code)
	<-capture.entered // the worker is now blocked inside the inner handler
	for i := 1; srv.auditWriter.Snapshot().DroppedFull == 0; i++ {
		require.Less(t, i, 3*2048, "queue never filled")
		require.Equal(t, http.StatusUnauthorized, sendMissingTimestampBrokerRequest(srv).Code, "status while filling")
	}
	full := srv.auditWriter.Snapshot()
	require.Equal(t, 2048, full.Queued, "count bound reached: %+v", full)

	for i := 0; i < 5; i++ {
		rec := sendMissingTimestampBrokerRequest(srv)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Contains(t, rec.Body.String(), "missing X-Scion-Timestamp header", "body unchanged while full")
	}
	member := newHubMemberUser(t, s, "t6-register-member")
	regRec := doRequestAsUserLoopback(t, srv, member, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{Name: "t6-register-broker"})
	require.Equal(t, http.StatusCreated, regRec.Code, "registration is not blocked by a full audit writer: %s", regRec.Body.String())

	after := srv.auditWriter.Snapshot()
	assert.Equal(t, full.DroppedFull+6, after.DroppedFull, "every record offered while full is counted as dropped: %+v", after)
	assert.Equal(t, 2048, after.Queued)
}

// T7: the converted events are not gated on the decision experiment; with
// hub.authorization_decision_audit_v2 explicitly off they are still
// emitted.
func TestBrokerEnvelope_T7_NotGatedOnDecisionExperiment(t *testing.T) {
	capture := &decisionLogCapture{}
	srv, _ := newDecisionLogServer(t, capture)
	setDecisionLogFlag(t, srv, true)
	setDecisionLogFlag(t, srv, false)

	rec := sendMissingTimestampBrokerRequest(srv)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	id := rec.Header().Get("X-Request-ID")
	require.NotEmpty(t, id)

	require.NoError(t, srv.CloseAuditWriter(context.Background()))
	got := brokerFamilyRecords(capture.recordsFor(id))
	require.Len(t, got, 1, "all: %v", capture.records())
	assert.Equal(t, "auth_failure", got[0]["action"])
	assert.Equal(t, "missing_credentials", auditGroup(t, got[0], "payload")["failure_code"])
	assert.Equal(t, map[string]any{"kind": "runtime_broker", "id": "t6-claimed-broker"}, auditGroup(t, got[0], "resource"))
}
