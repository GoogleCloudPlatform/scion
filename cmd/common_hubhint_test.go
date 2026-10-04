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

package cmd

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"syscall"
	"testing"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// transportErr builds an error shaped like the one apiclient.Transport
// returns when the hub cannot be reached at all.
func transportErr() error {
	return fmt.Errorf("request failed: %w", &url.Error{
		Op:  "Get",
		URL: "https://hub.invalid.example/api/v1/agents",
		Err: syscall.ECONNREFUSED,
	})
}

func apiErr(status int, code, msg string) error {
	return &apiclient.APIError{StatusCode: status, Code: code, Message: msg}
}

func TestWrapHubError_StatusTable(t *testing.T) {
	const hint = "scion hub disable"
	tests := []struct {
		name        string
		err         error
		agent       bool
		wantHint    bool
		wantContain string
		wantLogin   bool
	}{
		{name: "400 bad request", err: apiErr(400, "invalid_request", "bad field"), wantContain: "bad field"},
		{name: "403 forbidden", err: apiErr(403, "forbidden", "not allowed"), wantContain: "not allowed"},
		{name: "404 not found", err: apiErr(404, "agent_not_found", `Agent "x" not found in project`), wantContain: `Agent "x" not found`},
		{name: "409 conflict", err: apiErr(409, "conflict", "already exists"), wantContain: "already exists"},
		{name: "422 unprocessable", err: apiErr(422, "no_runtime_broker", "no broker"), wantContain: "no broker"},
		{name: "500 internal", err: apiErr(500, "internal_error", "boom"), wantHint: true, wantContain: "boom"},
		{name: "503 unavailable", err: apiErr(503, "unavailable", "down"), wantHint: true, wantContain: "down"},
		{name: "transport failure", err: transportErr(), wantHint: true, wantContain: "connection refused"},
		{name: "wrapped 404 keeps no hint", err: fmt.Errorf("failed to send message to agent 'x' via Hub: %w", apiErr(404, "agent_not_found", "gone")), wantContain: "failed to send message to agent 'x'"},
		{name: "401 standalone gets login hint", err: apiErr(401, "unauthorized", "token expired"), wantLogin: true},
		{name: "401 agent keeps cause", err: apiErr(401, "unauthorized", "token expired"), agent: true, wantContain: "hub rejected this agent's credentials"},
		{name: "500 agent omits hint", err: apiErr(500, "internal_error", "boom"), agent: true, wantContain: "boom"},
		{name: "transport agent omits hint", err: transportErr(), agent: true, wantContain: "connection refused"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.agent {
				t.Setenv("SCION_AGENT_ID", "agent-uuid-123")
			} else {
				t.Setenv("SCION_AGENT_ID", "")
			}
			got := wrapHubError(tt.err)
			if got == nil {
				t.Fatal("expected non-nil error")
			}
			msg := got.Error()
			if hasHint := strings.Contains(msg, hint); hasHint != tt.wantHint {
				t.Errorf("hint present = %v, want %v; msg: %q", hasHint, tt.wantHint, msg)
			}
			if tt.wantContain != "" && !strings.Contains(msg, tt.wantContain) {
				t.Errorf("expected %q in %q", tt.wantContain, msg)
			}
			if hasLogin := strings.Contains(msg, "scion hub auth login"); hasLogin != tt.wantLogin {
				t.Errorf("login hint present = %v, want %v; msg: %q", hasLogin, tt.wantLogin, msg)
			}
			if !errors.Is(got, tt.err) {
				t.Errorf("wrapped error should unwrap to the original cause")
			}
			if !isHubFailure(got) {
				t.Errorf("wrapped error should be recognised as a hub failure")
			}
		})
	}
}

func TestWrapHubError_NilAndIdempotent(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "")
	if wrapHubError(nil) != nil {
		t.Fatal("wrapHubError(nil) should be nil")
	}
	once := wrapHubError(transportErr())
	twice := wrapHubError(fmt.Errorf("outer: %w", once))
	if n := strings.Count(twice.Error(), "scion hub disable"); n != 1 {
		t.Errorf("expected exactly one hint after double wrap, got %d: %q", n, twice)
	}
}

func TestWrapHubError_SuppressesLocalHintForAgents(t *testing.T) {
	// A connectivity failure is the case where the local-only hint is shown
	// to standalone users, so it is the meaningful case for agent suppression.
	baseErr := transportErr()

	t.Run("standalone CLI includes local-only hint", func(t *testing.T) {
		t.Setenv("SCION_AGENT_ID", "")
		got := wrapHubError(baseErr)
		if !strings.Contains(got.Error(), "local-only mode") {
			t.Errorf("expected local-only mode hint for standalone CLI, got: %s", got)
		}
	})

	t.Run("hub-managed agent omits local-only hint", func(t *testing.T) {
		t.Setenv("SCION_AGENT_ID", "agent-uuid-123")
		got := wrapHubError(baseErr)
		if strings.Contains(got.Error(), "local-only mode") {
			t.Errorf("expected no local-only mode hint for hub-managed agent, got: %s", got)
		}
		if !errors.Is(got, baseErr) {
			t.Errorf("expected wrapped error to contain base error")
		}
	})
}

func TestShowUsageForError_HubFailuresSuppressUsage(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "")
	parent := &cobra.Command{Use: "scion"}
	sub := &cobra.Command{Use: "message"}
	parent.AddCommand(sub)

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"raw hub 404 APIError", apiErr(404, "agent_not_found", "gone"), false},
		{"fmt-wrapped hub 400 APIError", fmt.Errorf("failed: %w", apiErr(400, "invalid_request", "bad")), false},
		{"wrapHubError 404", wrapHubError(apiErr(404, "agent_not_found", "gone")), false},
		{"wrapHubError connectivity", wrapHubError(transportErr()), false},
		// Hub launch outcomes (#2360) are runtime results, not usage errors.
		{"incomplete create (hub 409)", incompleteCreateError("a1", &apiclient.APIError{StatusCode: 409, Code: errCodeAgentCreateIncomplete, Message: "create did not complete"}), false},
		{"launch failed", &launchFailedError{Agent: "a1", Phase: "error", Reason: "agent 'a1' did not start"}, false},
		{"launch wait timeout", &launchWaitTimeoutError{Agent: "a1"}, false},
		{"launch wait interrupted", &launchWaitInterruptedError{Agent: "a1"}, false},
		{"fmt-wrapped launch wait timeout", fmt.Errorf("start: %w", &launchWaitTimeoutError{Agent: "a1"}), false},
		{"non-hub runtime error keeps usage", errors.New("some local failure"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := showUsageForError(sub, tt.err, true); got != tt.want {
				t.Errorf("showUsageForError = %v, want %v", got, tt.want)
			}
		})
	}
}
