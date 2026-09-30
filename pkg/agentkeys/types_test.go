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

package agentkeys

import (
	"net/http"
	"testing"
)

// allOutcomes lists every Outcome constant this package defines. Kept
// separate from a reflection-based enumeration on purpose: if a new
// constant is added to types.go without being added here too, that is
// exactly the kind of silent gap TestHTTPStatus_Completeness exists to
// catch (a real Outcome with no test coverage), so the list must be
// maintained by hand, not derived from the thing under test.
var allOutcomes = []Outcome{
	OutcomeDispatched,
	OutcomeInvalidRequest,
	OutcomePayloadTooLarge,
	OutcomeUnauthorized,
	OutcomeKeysDenied,
	OutcomeNotFound,
	OutcomeAgentNotRunning,
	OutcomeTerminalNotReady,
	OutcomeCrossProjectKeysUnsupported,
	OutcomeKeysUnsupported,
	OutcomeRawInputRemoved,
	OutcomeRawCombinationUnsupported,
	OutcomeKeysRateLimited,
	OutcomeKeysUnavailable,
	OutcomeKeysOutcomeUnknown,
}

func TestHTTPStatus_Table(t *testing.T) {
	cases := []struct {
		outcome    Outcome
		wantStatus int
		wantOK     bool
	}{
		{OutcomeDispatched, 0, false}, // success has its own 200 response, not an error status
		{OutcomeInvalidRequest, http.StatusBadRequest, true},
		{OutcomePayloadTooLarge, http.StatusRequestEntityTooLarge, true},
		{OutcomeUnauthorized, http.StatusUnauthorized, true},
		{OutcomeKeysDenied, http.StatusForbidden, true},
		{OutcomeNotFound, http.StatusNotFound, true},
		{OutcomeAgentNotRunning, http.StatusConflict, true},
		{OutcomeTerminalNotReady, http.StatusConflict, true},
		{OutcomeCrossProjectKeysUnsupported, http.StatusUnprocessableEntity, true},
		{OutcomeKeysUnsupported, http.StatusUnprocessableEntity, true},
		{OutcomeRawInputRemoved, http.StatusUnprocessableEntity, true},
		{OutcomeRawCombinationUnsupported, http.StatusUnprocessableEntity, true},
		{OutcomeKeysRateLimited, http.StatusTooManyRequests, true},
		{OutcomeKeysUnavailable, http.StatusServiceUnavailable, true},
		{OutcomeKeysOutcomeUnknown, http.StatusBadGateway, true},
		{Outcome("something_nobody_defined"), 0, false},
		{Outcome(""), 0, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.outcome), func(t *testing.T) {
			status, ok := HTTPStatus(tc.outcome)
			if status != tc.wantStatus || ok != tc.wantOK {
				t.Fatalf("HTTPStatus(%q) = (%d, %v), want (%d, %v)", tc.outcome, status, ok, tc.wantStatus, tc.wantOK)
			}
		})
	}
}

// TestHTTPStatus_Completeness fails if a new Outcome constant is ever added
// to types.go without an HTTPStatus mapping for it (or without being added
// to allOutcomes above, which review is expected to catch — see that var's
// doc comment). Every Outcome except OutcomeDispatched must map to a 4xx or
// 5xx status.
func TestHTTPStatus_Completeness(t *testing.T) {
	for _, o := range allOutcomes {
		status, ok := HTTPStatus(o)
		if o == OutcomeDispatched {
			if ok {
				t.Errorf("HTTPStatus(OutcomeDispatched) = (%d, true), want ok=false (success is not an error status)", status)
			}
			continue
		}
		if !ok {
			t.Errorf("HTTPStatus(%q) returned ok=false; every non-success Outcome must have a status", o)
			continue
		}
		if status < 400 || status >= 600 {
			t.Errorf("HTTPStatus(%q) = %d, want a 4xx or 5xx status", o, status)
		}
	}
}

// TestMaxHTTPBodyBytes_CoversWorstCaseCompactEncoding pins the arithmetic
// MaxHTTPBodyBytes's doc comment claims: a compactly encoded (no
// insignificant whitespace) request body containing exactly MaxBytes bytes
// of "keys" content, worst-case-escaped (every byte becomes a 6-byte
// \u00XX sequence), plus the minimal envelope, must still fit within
// MaxHTTPBodyBytes. If either constant ever changes without the other, this
// test fails instead of silently letting a legitimate maximal request start
// being rejected by the transport-level body-size read.
func TestMaxHTTPBodyBytes_CoversWorstCaseCompactEncoding(t *testing.T) {
	const envelope = `{"keys":""}` // the minimal compact envelope, empty value
	worstCase := 6*MaxBytes + len(envelope)
	if worstCase > MaxHTTPBodyBytes {
		t.Fatalf("worst-case compactly-encoded body is %d bytes, which exceeds MaxHTTPBodyBytes (%d); "+
			"a legitimate maximal request would be rejected by the transport-level read before "+
			"ValidateBody's own byte-ceiling check ever runs", worstCase, MaxHTTPBodyBytes)
	}
}
