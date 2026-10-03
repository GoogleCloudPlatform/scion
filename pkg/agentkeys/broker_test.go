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
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestCapExecuteBefore_MissingDeadlineFailsClosed(t *testing.T) {
	now := time.Now()
	_, err := CapExecuteBefore(now, time.Time{}, DefaultAdmissionWindow)
	if !errors.Is(err, ErrMissingDeadline) {
		t.Fatalf("err = %v, want ErrMissingDeadline", err)
	}
}

func TestCapExecuteBefore_RequestDeadlineWithinWindow(t *testing.T) {
	now := time.Now()
	requestDeadline := now.Add(5 * time.Second)
	got, err := CapExecuteBefore(now, requestDeadline, DefaultAdmissionWindow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Equal(requestDeadline) {
		t.Fatalf("got %v, want the request deadline %v (tighter than the window)", got, requestDeadline)
	}
}

func TestCapExecuteBefore_WindowCapsALongRequestDeadline(t *testing.T) {
	now := time.Now()
	requestDeadline := now.Add(10 * time.Minute)
	got, err := CapExecuteBefore(now, requestDeadline, DefaultAdmissionWindow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := now.Add(DefaultAdmissionWindow)
	if !got.Equal(want) {
		t.Fatalf("got %v, want the window cap %v", got, want)
	}
}

func TestCapExecuteBefore_AlreadyExpiredRequestDeadline(t *testing.T) {
	now := time.Now()
	requestDeadline := now.Add(-1 * time.Second)
	got, err := CapExecuteBefore(now, requestDeadline, DefaultAdmissionWindow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Equal(requestDeadline) {
		t.Fatalf("got %v, want the (already past) request deadline %v", got, requestDeadline)
	}
	if !got.Before(now) {
		t.Fatalf("expected the capped deadline to be in the past so callers reject before dispatch")
	}
}

func TestCapExecuteBefore_InvalidWindowFailsLoudly(t *testing.T) {
	now := time.Now()
	requestDeadline := now.Add(5 * time.Second)

	for _, window := range []time.Duration{0, -1 * time.Second} {
		_, err := CapExecuteBefore(now, requestDeadline, window)
		if !errors.Is(err, ErrInvalidWindow) {
			t.Fatalf("window=%v: err = %v, want ErrInvalidWindow", window, err)
		}
	}
}

func TestClassifyDispatchError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want Outcome
	}{
		{"nil is treated as unknown, not a panic invitation", nil, OutcomeKeysOutcomeUnknown},
		{"ErrNotDispatched", ErrNotDispatched, OutcomeKeysUnavailable},
		{"wrapped ErrNotDispatched", fmt.Errorf("broker offline: %w", ErrNotDispatched), OutcomeKeysUnavailable},
		{"an ordinary, unclassified error", errors.New("some transport hiccup"), OutcomeKeysOutcomeUnknown},
		// The three manager-level sentinels (ErrTargetNotFound, ErrAgentNotRunning,
		// ErrTerminalNotReady) are broker-process-internal: a correct task 1.1/1.2
		// implementation translates each into a *BrokerOutcomeError before any HTTP
		// response leaves the broker process, so none of them should ever reach
		// ClassifyDispatchError directly. These three cases pin the fail-safe for
		// when one does anyway (a bug forwarding a manager error verbatim): all three
		// fall through to OutcomeKeysOutcomeUnknown, identically to any other
		// unrecognized error — none is silently treated as if it proved a
		// Hub-decided outcome.
		{"ErrTargetNotFound is not classified directly", ErrTargetNotFound, OutcomeKeysOutcomeUnknown},
		{"ErrAgentNotRunning is not classified directly", ErrAgentNotRunning, OutcomeKeysOutcomeUnknown},
		{"ErrTerminalNotReady is not classified directly", ErrTerminalNotReady, OutcomeKeysOutcomeUnknown},
		{"BrokerOutcomeError: not_found (broker-decided AgentID mismatch or missing target)", &BrokerOutcomeError{Outcome: OutcomeNotFound}, OutcomeNotFound},
		{"BrokerOutcomeError: agent_not_running", &BrokerOutcomeError{Outcome: OutcomeAgentNotRunning}, OutcomeAgentNotRunning},
		{"BrokerOutcomeError: terminal_not_ready", &BrokerOutcomeError{Outcome: OutcomeTerminalNotReady}, OutcomeTerminalNotReady},
		{"BrokerOutcomeError: keys_unsupported (old broker / managed backend)", &BrokerOutcomeError{Outcome: OutcomeKeysUnsupported}, OutcomeKeysUnsupported},
		{"BrokerOutcomeError: keys_unavailable (broker's own admission-deadline expiry)", &BrokerOutcomeError{Outcome: OutcomeKeysUnavailable}, OutcomeKeysUnavailable},
		{"wrapped BrokerOutcomeError", fmt.Errorf("execute keys: %w", &BrokerOutcomeError{Outcome: OutcomeAgentNotRunning}), OutcomeAgentNotRunning},
		{
			"BrokerOutcomeError asserting a non-allow-listed outcome is not trusted",
			&BrokerOutcomeError{Outcome: OutcomeKeysDenied},
			OutcomeKeysOutcomeUnknown,
		},
		{
			"BrokerOutcomeError asserting success is not trusted (success is never an error)",
			&BrokerOutcomeError{Outcome: OutcomeDispatched},
			OutcomeKeysOutcomeUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyDispatchError(tc.err); got != tc.want {
				t.Fatalf("ClassifyDispatchError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestValidBrokerOutcome(t *testing.T) {
	allowed := []Outcome{
		OutcomeNotFound,
		OutcomeAgentNotRunning,
		OutcomeTerminalNotReady,
		OutcomeKeysUnsupported,
		OutcomeKeysUnavailable,
	}
	for _, o := range allowed {
		if !ValidBrokerOutcome(o) {
			t.Errorf("ValidBrokerOutcome(%q) = false, want true (broker-assertable)", o)
		}
	}

	disallowed := []Outcome{
		OutcomeDispatched,
		OutcomeInvalidRequest,
		OutcomePayloadTooLarge,
		OutcomeUnauthorized,
		OutcomeKeysDenied,
		OutcomeCrossProjectKeysUnsupported,
		OutcomeRawInputRemoved,
		OutcomeRawCombinationUnsupported,
		OutcomeKeysRateLimited,
		OutcomeKeysOutcomeUnknown,
		Outcome("something_made_up"),
	}
	for _, o := range disallowed {
		if ValidBrokerOutcome(o) {
			t.Errorf("ValidBrokerOutcome(%q) = true, want false (Hub-decided-only or not a real outcome)", o)
		}
	}
}

func TestBrokerOutcomeError_Error(t *testing.T) {
	withMsg := &BrokerOutcomeError{Outcome: OutcomeNotFound, Message: "agent_id mismatch"}
	if got := withMsg.Error(); got != "not_found: agent_id mismatch" {
		t.Fatalf("Error() = %q, want %q", got, "not_found: agent_id mismatch")
	}

	withoutMsg := &BrokerOutcomeError{Outcome: OutcomeKeysUnsupported}
	if got := withoutMsg.Error(); got != "keys_unsupported" {
		t.Fatalf("Error() = %q, want %q", got, "keys_unsupported")
	}
}
