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

	"github.com/GoogleCloudPlatform/scion/pkg/util/asyncwrite"
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
// legacy DEBUG) gives no envelope.
func TestBrokerEnvelope_T5_ProductionConstructor(t *testing.T) {
	capture := &decisionLogCapture{}
	srv, s := newDecisionLogServer(t, capture)
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
// unauthenticated auth_failure traffic hitting its reserved-headroom cap,
// broker request status codes are unchanged, excess auth_failure records
// are counted as dropped by the writer, and broker lifecycle records
// (register) still enqueue into the reserved headroom. The test finishes
// while the handler is still blocked; it is released only in cleanup.
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

	authFailureCap := asyncwrite.DefaultCapacity - brokerUnauthenticatedReserveSlots
	require.Equal(t, http.StatusUnauthorized, sendMissingTimestampBrokerRequest(srv).Code)
	<-capture.entered // the worker is now blocked inside the inner handler
	for i := 1; srv.auditWriter.Snapshot().DroppedFull == 0; i++ {
		require.Less(t, i, 3*asyncwrite.DefaultCapacity, "auth_failure cap never reached")
		require.Equal(t, http.StatusUnauthorized, sendMissingTimestampBrokerRequest(srv).Code, "status while filling")
	}
	capped := srv.auditWriter.Snapshot()
	require.Equal(t, authFailureCap, capped.Queued, "auth_failure count cap reached: %+v", capped)

	for i := 0; i < 5; i++ {
		rec := sendMissingTimestampBrokerRequest(srv)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Contains(t, rec.Body.String(), "missing X-Scion-Timestamp header", "body unchanged while capped")
	}
	member := newHubMemberUser(t, s, "t6-register-member")
	regRec := doRequestAsUserLoopback(t, srv, member, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{Name: "t6-register-broker"})
	require.Equal(t, http.StatusCreated, regRec.Code, "registration is not blocked by an auth_failure-capped audit writer: %s", regRec.Body.String())

	after := srv.auditWriter.Snapshot()
	assert.Equal(t, capped.DroppedFull+5, after.DroppedFull, "excess auth_failure records are counted as dropped, while register uses reserved headroom: %+v", after)
	assert.Equal(t, authFailureCap+1, after.Queued, "register was admitted into reserved headroom: %+v", after)
}

// An unauthenticated broker/auth_failure flood while the audit writer is
// stalled cannot starve authorization/decide or commit-phase broker
// lifecycle audit records: auth_failure sheds once the reserved headroom
// boundary is reached, and both decisions and lifecycle records are
// admitted and written once the writer drains.
func TestBrokerEnvelope_AuthFailureFloodDoesNotStarveDecisionsOrLifecycle(t *testing.T) {
	capture := &decisionLogCapture{block: make(chan struct{}), entered: make(chan struct{}, 1)}
	srv, s := newDecisionLogServer(t, capture)
	admin := seedDecisionLogAdmin(t, s)
	member := newHubMemberUser(t, s, "flood-register-member")

	released := false
	t.Cleanup(func() {
		if !released {
			close(capture.block)
		}
	})

	authFailureCap := asyncwrite.DefaultCapacity - brokerUnauthenticatedReserveSlots
	require.Equal(t, http.StatusUnauthorized, sendMissingTimestampBrokerRequest(srv).Code)
	<-capture.entered // worker blocked inside the inner handler

	for i := 1; srv.auditWriter.Snapshot().DroppedFull == 0; i++ {
		require.Less(t, i, 3*asyncwrite.DefaultCapacity, "auth_failure cap never reached")
		require.Equal(t, http.StatusUnauthorized, sendMissingTimestampBrokerRequest(srv).Code)
	}
	// Send additional flood requests past the cap.
	for i := 0; i < 20; i++ {
		require.Equal(t, http.StatusUnauthorized, sendMissingTimestampBrokerRequest(srv).Code)
	}
	capped := srv.auditWriter.Snapshot()
	require.Equal(t, authFailureCap, capped.Queued, "auth_failure must stay within its share: %+v", capped)
	require.GreaterOrEqual(t, capped.DroppedFull, uint64(21))

	// Emit an in-domain authorization/decide event and a broker/register
	// lifecycle event while auth_failure is capped.
	decideRec, _ := userRequest(t, srv, admin, http.MethodPost, stopAllPath, nil, nil)
	require.Equal(t, http.StatusOK, decideRec.Code, decideRec.Body.String())
	decideID := decideRec.Header().Get("X-Request-ID")
	require.NotEmpty(t, decideID)

	regRec := doRequestAsUserLoopback(t, srv, member, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{Name: "flood-register-broker"})
	require.Equal(t, http.StatusCreated, regRec.Code, regRec.Body.String())
	regID := regRec.Header().Get("X-Request-ID")
	require.NotEmpty(t, regID)

	counts := srv.decisionAuditLogger.counts
	assert.Equal(t, uint64(1), counts.get(decisionAuditEnqueued, true), "decision must be enqueued into reserved headroom")
	assert.Equal(t, uint64(0), counts.get(decisionAuditNotEnqueued, true), "decision must not be dropped")

	after := srv.auditWriter.Snapshot()
	assert.Equal(t, capped.DroppedFull, after.DroppedFull, "neither decision nor register should be dropped")
	assert.Equal(t, authFailureCap+2, after.Queued, "both decision and register should be queued")

	// Release and drain; verify both records were actually written.
	released = true
	close(capture.block)
	require.NoError(t, srv.CloseAuditWriter(context.Background()))

	decideLines := capture.recordsFor(decideID)
	require.Len(t, decideLines, 1, "authorization/decide record must be written")
	assert.Equal(t, "authorization", decideLines[0]["family"])
	assert.Equal(t, "decide", decideLines[0]["action"])

	regLines := brokerFamilyRecords(capture.recordsFor(regID))
	require.Len(t, regLines, 1, "broker/register record must be written")
	assert.Equal(t, "register", regLines[0]["action"])
}

// An unauthenticated failed broker/join flood while the audit writer is
// stalled is isolated to the same reserved headroom boundary as
// broker/auth_failure: failed joins shed once the unauthenticated share is
// full, while authorization/decide, broker/register, and successful
// commit-phase broker/join records are still admitted and written.
func TestBrokerEnvelope_FailedJoinFloodDoesNotStarveDecisionsOrLifecycle(t *testing.T) {
	capture := &decisionLogCapture{block: make(chan struct{}), entered: make(chan struct{}, 1)}
	srv, s := newDecisionLogServer(t, capture)
	admin := seedDecisionLogAdmin(t, s)
	member := newHubMemberUser(t, s, "join-flood-register-member")

	released := false
	t.Cleanup(func() {
		if !released {
			close(capture.block)
		}
	})

	bogusJoin := BrokerJoinRequest{BrokerID: "flood-bogus-broker", JoinToken: "bogus-join-token"}
	unauthCap := asyncwrite.DefaultCapacity - brokerUnauthenticatedReserveSlots

	first := doRequestNoAuthLoopback(t, srv, http.MethodPost, "/api/v1/brokers/join", bogusJoin)
	require.Equal(t, http.StatusUnauthorized, first.Code, first.Body.String())
	<-capture.entered // worker blocked inside the inner handler

	for i := 1; srv.auditWriter.Snapshot().DroppedFull == 0; i++ {
		require.Less(t, i, 3*asyncwrite.DefaultCapacity, "failed-join cap never reached")
		rec := doRequestNoAuthLoopback(t, srv, http.MethodPost, "/api/v1/brokers/join", bogusJoin)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	}
	for i := 0; i < 20; i++ {
		rec := doRequestNoAuthLoopback(t, srv, http.MethodPost, "/api/v1/brokers/join", bogusJoin)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	}
	capped := srv.auditWriter.Snapshot()
	require.Equal(t, unauthCap, capped.Queued, "failed broker/join must stay within its unauthenticated share: %+v", capped)
	require.GreaterOrEqual(t, capped.DroppedFull, uint64(21))

	// Emit an authorization/decide record, a broker/register record, and a
	// successful broker/join commit record while failed joins are capped.
	decideRec, _ := userRequest(t, srv, admin, http.MethodPost, stopAllPath, nil, nil)
	require.Equal(t, http.StatusOK, decideRec.Code, decideRec.Body.String())
	decideID := decideRec.Header().Get("X-Request-ID")
	require.NotEmpty(t, decideID)

	regRec := doRequestAsUserLoopback(t, srv, member, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{Name: "join-flood-broker"})
	require.Equal(t, http.StatusCreated, regRec.Code, regRec.Body.String())
	regID := regRec.Header().Get("X-Request-ID")
	require.NotEmpty(t, regID)
	regResp := decodeBrokerRegistration(t, regRec.Body)

	joinRec := doRequestNoAuthLoopback(t, srv, http.MethodPost, "/api/v1/brokers/join", BrokerJoinRequest{
		BrokerID:  regResp.BrokerID,
		JoinToken: regResp.JoinToken,
	})
	require.Equal(t, http.StatusOK, joinRec.Code, joinRec.Body.String())
	joinID := joinRec.Header().Get("X-Request-ID")
	require.NotEmpty(t, joinID)

	counts := srv.decisionAuditLogger.counts
	assert.Equal(t, uint64(1), counts.get(decisionAuditEnqueued, true), "decision must be enqueued into reserved headroom")
	assert.Equal(t, uint64(0), counts.get(decisionAuditNotEnqueued, true), "decision must not be dropped")

	after := srv.auditWriter.Snapshot()
	assert.Equal(t, capped.DroppedFull, after.DroppedFull, "decide, register, and successful join must not be dropped")
	assert.Equal(t, unauthCap+3, after.Queued, "decide, register, and successful join must be queued")

	// Release and drain; verify all three records were written.
	released = true
	close(capture.block)
	require.NoError(t, srv.CloseAuditWriter(context.Background()))

	decideLines := capture.recordsFor(decideID)
	require.Len(t, decideLines, 1, "authorization/decide record must be written")
	assert.Equal(t, "authorization", decideLines[0]["family"])
	assert.Equal(t, "decide", decideLines[0]["action"])

	regLines := brokerFamilyRecords(capture.recordsFor(regID))
	require.Len(t, regLines, 1, "broker/register record must be written")
	assert.Equal(t, "register", regLines[0]["action"])

	joinLines := brokerFamilyRecords(capture.recordsFor(joinID))
	require.Len(t, joinLines, 1, "successful broker/join record must be written")
	assert.Equal(t, "join", joinLines[0]["action"])
	assert.Equal(t, "commit", joinLines[0]["phase"])
	assert.Equal(t, "succeeded", joinLines[0]["outcome"])
}
