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
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// hubRelayedBrokerMessage is the message the Hub sends (as a 502
// runtime_error) when the runtime broker rejects a create or start: the
// broker's raw body, with the trailing newline its JSON encoder writes.
func hubRelayedBrokerMessage(status int, body string) string {
	return fmt.Sprintf("Failed to dispatch to runtime broker: runtime broker returned error %d: %s\n", status, body)
}

const missingFileBrokerBody = `{"error":{"code":"template_error","message":"Failed to create agent: a template or harness-config file is missing on this broker. Re-sync the template or harness-config to the Hub; if the Hub uses local storage, make sure it serves those files over HTTP rather than as local file paths.","details":{"cause":"file_not_found","file":"settings.json"}}}`

// TestWrapHubError_BrokerFailureCategories covers ptone/scion#3323: a broker
// failure relayed by the Hub is shown as a short category, never with the
// broker's raw body or the "scion hub disable" hint.
func TestWrapHubError_BrokerFailureCategories(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		want       []string
		notContain []string
	}{
		{
			name:   "missing template file",
			status: 422,
			body:   missingFileBrokerBody,
			want: []string{
				`a template or harness-config file is missing on the runtime broker (file "settings.json")`,
				"re-sync the template or harness-config to the Hub",
				"broker code: template_error", "broker status: 422", "request: req-123",
			},
			notContain: []string{"local file paths", `"details"`},
		},
		{
			name:       "container create failure",
			status:     500,
			body:       `{"error":{"code":"runtime_error","message":"Failed to create agent: secret internal detail"}}`,
			want:       []string{"could not create or start the agent container", "broker code: runtime_error"},
			notContain: []string{"secret internal detail", "Failed to create agent"},
		},
		{
			name:   "runtime unavailable",
			status: 503,
			body:   `{"error":{"code":"runtime_unavailable","message":"x"}}`,
			want:   []string{"container runtime is not available on the runtime broker"},
		},
		{
			name:   "non-JSON body",
			status: 500,
			body:   `<html>proxy error</html>`,
			want:   []string{"unrecognized error (status 500)"},
		},
		{
			name:   "unknown code",
			status: 500,
			body:   `{"error":{"code":"something_new","message":"x"}}`,
			want:   []string{"the runtime broker reported something_new"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := &apiclient.APIError{
				StatusCode: http.StatusBadGateway,
				Code:       "runtime_error",
				Message:    hubRelayedBrokerMessage(tt.status, tt.body),
				RequestID:  "req-123",
			}
			err := wrapHubError(fmt.Errorf("failed to start agent via Hub: %w", base))
			require.Error(t, err)
			msg := err.Error()
			assert.Contains(t, msg, "failed to start agent via Hub: runtime broker failure: ")
			for _, w := range tt.want {
				assert.Contains(t, msg, w)
			}
			for _, n := range tt.notContain {
				assert.NotContains(t, msg, n)
			}
			assert.NotContains(t, msg, "scion hub disable")
			assert.NotContains(t, msg, "runtime broker returned error")
			assert.NotContains(t, msg, "\n", "summary must be a single line: %q", msg)

			// The structured error stays reachable for callers.
			var got *apiclient.APIError
			require.ErrorAs(t, err, &got)
			assert.Same(t, base, got)
		})
	}
}

// TestWrapHubError_PlainHub5xxKeepsLocalOnlyHint pins that only relayed
// broker failures lose the hint: a Hub 5xx of its own still gets it.
func TestWrapHubError_PlainHub5xxKeepsLocalOnlyHint(t *testing.T) {
	err := wrapHubError(apiErr(http.StatusBadGateway, "runtime_error", "Failed to dispatch to runtime broker: dial tcp: connection refused"))
	assert.Contains(t, err.Error(), "scion hub disable")
}

// TestStartAgentViaHub_BrokerFailureCategory drives scion start's hub path
// against a Hub that relays the broker's 422 missing-file error as a 502.
func TestStartAgentViaHub_BrokerFailureCategory(t *testing.T) {
	hub := &launchMockHub{t: t, createStatus: http.StatusBadGateway, createBody: map[string]interface{}{
		"error": map[string]interface{}{
			"code":    "runtime_error",
			"message": hubRelayedBrokerMessage(422, missingFileBrokerBody),
		},
	}}
	hubCtx := setupLaunchStartTest(t, hub)
	var err error
	_ = captureStderr(t, func() {
		_ = captureStdout(t, func() { err = startAgentViaHub(nil, hubCtx, "a1", "", false, nil) })
	})
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "failed to start agent via Hub: runtime broker failure: a template or harness-config file is missing on the runtime broker")
	assert.Contains(t, msg, "broker code: template_error")
	assert.NotContains(t, msg, "scion hub disable")
	assert.NotContains(t, msg, "runtime broker returned error")
}
