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

// TestAgentKeysMessageBridge_NoLeak_413_BrokenRead_ValidateError covers plan
// row AK-32(B): the bridge's own pre-authorization rejection paths --
// oversize body (413), a body read that errors mid-flight, and a
// ValidateKeysJSON content rejection (steps 2-4) -- must never leak the
// keys content into the HTTP response or captured logs, using the same
// distinctive-secret/log-capture convention as keys_no_content_leak_test.go
// (keysContentSentinel, installSentinelLogCapture).

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentKeysMessageBridge_NoLeak_413_BrokenRead_ValidateError(t *testing.T) {
	t.Run("413_oversize_body", func(t *testing.T) {
		f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
		log := installSentinelLogCapture(t)
		token := f.agentToken(t, tid("bridge-noleak-413"), f.projectA.ID, ScopeAgentLifecycle)

		pad := strings.Repeat("a", agentKeysBridgePreAuthMaxBodyBytes+1024)
		body := []byte(`{"raw":true,"message":"` + keysContentSentinel + `","padding":"` + pad + `"}`)

		rec := doRawBridgeRequest(t, f.srv, "/api/v1/agents/"+f.agentInA.ID+"/message", token, body)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413; body: %s", rec.Code, rec.Body.String())
		}
		// Positive control. This path
		// (http.MaxBytesReader's error) writes a generic writeError with no
		// agent-keys audit call at all -- there is no route=message_raw_bridge
		// line to look for here, so the request-specific signal is the
		// generic API-error log line writeError itself emits, carrying this
		// exact status and code.
		if !strings.Contains(log.String(), "status=413") || !strings.Contains(log.String(), "payload_too_large") {
			t.Fatalf("positive control failed: expected the capture to show a 413 payload_too_large API error line, got:\n%s", log.String())
		}
		assertNoLeakResponseAndLog(t, rec, log)
		if d.callCount() != 0 {
			t.Errorf("dispatcher called %d times, want 0", d.callCount())
		}
		assertNoKeysSideEffects(t, storeSpy, events)
	})

	t.Run("broken_read_mid_flight", func(t *testing.T) {
		f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
		log := installSentinelLogCapture(t)
		token := f.agentToken(t, tid("bridge-noleak-brokenread"), f.projectA.ID, ScopeAgentLifecycle)

		fullBody := []byte(`{"raw":true,"message":"` + keysContentSentinel + `"}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/message",
			&brokenChunkReadCloser{data: fullBody, err: errors.New("simulated broken chunk")})
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)

		// This subtest asserts the status explicitly: a complete raw-shaped document surviving a broken
		// read is classified via writeAgentKeysBridgeFixedRejection, which
		// maps agentkeys.OutcomeInvalidRequest to 400.
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
		}
		// Positive control: this path goes through logAgentKeysValidationAudit
		// (via writeAgentKeysBridgeFixedRejection), so the bridge's own
		// route tag must appear.
		if !strings.Contains(log.String(), "route=message_raw_bridge") {
			t.Fatalf("positive control failed: expected a message_raw_bridge audit line in the capture, got:\n%s", log.String())
		}
		assertNoLeakResponseAndLog(t, rec, log)
		if d.callCount() != 0 {
			t.Errorf("dispatcher called %d times, want 0", d.callCount())
		}
		assertNoKeysSideEffects(t, storeSpy, events)
	})

	t.Run("validate_keys_json_error_nul_byte", func(t *testing.T) {
		f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
		log := installSentinelLogCapture(t)
		token := f.agentToken(t, tid("bridge-noleak-nul"), f.projectA.ID, ScopeAgentLifecycle)

		body := []byte(`{"raw":true,"message":"` + keysContentSentinel + `\u0000"}`)
		rec := doRawBridgeRequest(t, f.srv, "/api/v1/agents/"+f.agentInA.ID+"/message", token, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
		}
		// Positive control: this path goes through logAgentKeysValidationAudit
		// (via writeAgentKeysBridgeMalformed), so the bridge's own route tag
		// must appear.
		if !strings.Contains(log.String(), "route=message_raw_bridge") {
			t.Fatalf("positive control failed: expected a message_raw_bridge audit line in the capture, got:\n%s", log.String())
		}
		assertNoLeakResponseAndLog(t, rec, log)
		if d.callCount() != 0 {
			t.Errorf("dispatcher called %d times, want 0", d.callCount())
		}
		assertNoKeysSideEffects(t, storeSpy, events)
	})
}

// assertNoLeakResponseAndLog fails the test if the response body or the
// captured log buffer contains keysContentSentinel.
func assertNoLeakResponseAndLog(t *testing.T, rec *httptest.ResponseRecorder, log interface{ String() string }) {
	t.Helper()
	if strings.Contains(rec.Body.String(), keysContentSentinel) {
		t.Errorf("response body leaked key content: %s", rec.Body.String())
	}
	if strings.Contains(log.String(), keysContentSentinel) {
		t.Errorf("captured log leaked key content:\n%s", log.String())
	}
}
