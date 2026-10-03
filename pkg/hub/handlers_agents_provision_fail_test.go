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
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// skillFailDispatcher is a createAgentDispatcher whose provision and start
// dispatches can be made to fail with a chosen error.
type skillFailDispatcher struct {
	createAgentDispatcher
	provisionErr     error
	startErr         error
	provisionedAgent *store.Agent
}

func (d *skillFailDispatcher) DispatchAgentProvision(ctx context.Context, agent *store.Agent) error {
	d.provisionedAgent = agent
	if d.provisionErr != nil {
		return d.provisionErr
	}
	return d.createAgentDispatcher.DispatchAgentProvision(ctx, agent)
}

func (d *skillFailDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, task string, resume bool) error {
	d.startCalled = true
	return d.startErr
}

const testSkillRef = "gh://owner/repo/my-skill@main"

// brokerSkillError builds the error the broker transport returns for a
// required skill the broker could not resolve.
func brokerSkillError(status int, cause, retryAfter string) error {
	body := `{"error":{"code":"skill_resolution_failed","message":"Failed to provision agent: required skill \"` +
		testSkillRef + `\" could not be resolved: ` + cause + `","details":{"skill":"` + testSkillRef + `","cause":"` + cause + `"}}}`
	return &brokerStatusError{StatusCode: status, Body: body, RetryAfter: retryAfter}
}

// assertSkillErrorRelayed checks that rec carries the broker's skill
// resolution failure unchanged: status, code, the ref in the message and the
// {skill, cause} details.
func assertSkillErrorRelayed(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCause string) {
	t.Helper()
	require.Equal(t, wantStatus, rec.Code, rec.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, skillResolutionErrorCode, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, testSkillRef)
	assert.NotContains(t, resp.Error.Message, "Failed to dispatch to runtime broker",
		"the broker message is relayed without a hub prefix")
	assert.Equal(t, testSkillRef, resp.Error.Details["skill"])
	assert.Equal(t, wantCause, resp.Error.Details["cause"])
}

func TestCreateAgent_ProvisionOnlySkillResolutionFailure(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		cause      string
		retryAfter string
	}{
		// A skill the caller cannot read is reported as not_found by the
		// broker; it must stay a 404.
		{name: "not found", status: http.StatusNotFound, cause: "not_found"},
		{name: "forbidden", status: http.StatusForbidden, cause: "forbidden"},
		{name: "rate limited", status: http.StatusTooManyRequests, cause: "rate_limited", retryAfter: "60"},
		{name: "timeout", status: http.StatusGatewayTimeout, cause: "timeout"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			disp := &skillFailDispatcher{provisionErr: brokerSkillError(tc.status, tc.cause, tc.retryAfter)}
			srv, s, project := setupCreateAgentServer(t, disp)
			logs := &levelCapturingHandler{}
			srv.agentLifecycleLog = slog.New(logs)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name:          "skill-fail-agent",
				ProjectID:     project.ID,
				Task:          "some task",
				ProvisionOnly: true,
			})

			assertSkillErrorRelayed(t, rec, tc.status, tc.cause)
			assert.Equal(t, tc.retryAfter, rec.Header().Get("Retry-After"))

			require.NotNil(t, disp.provisionedAgent, "provision must have been dispatched")
			_, err := s.GetAgent(context.Background(), disp.provisionedAgent.ID)
			assert.True(t, errors.Is(err, store.ErrNotFound), "the failed agent record must be deleted, got err=%v", err)
			assert.True(t, disp.deleteCalled, "the broker-side files must be cleaned up")

			found := false
			logs.mu.Lock()
			for _, r := range logs.records {
				if r.Level == slog.LevelWarn && strings.Contains(r.Message, "required skill could not be resolved") {
					found = true
				}
			}
			logs.mu.Unlock()
			assert.True(t, found, "the failure must be logged at WARN")
		})
	}
}

// TestCreateAgent_ProvisionOnlyOtherFailureStaysWarning pins that only the
// skill resolution failure changed: any other provision error still yields
// 201 with a warning and keeps the record.
func TestCreateAgent_ProvisionOnlyOtherFailureStaysWarning(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "plain error", err: errors.New("connection refused")},
		{name: "broker 404 not_found", err: &brokerStatusError{
			StatusCode: http.StatusNotFound,
			Body:       `{"error":{"code":"not_found","message":"Failed to provision agent: template not found"}}`,
		}},
		{name: "broker 500", err: &brokerStatusError{StatusCode: http.StatusInternalServerError, Body: "boom"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			disp := &skillFailDispatcher{provisionErr: tc.err}
			srv, s, project := setupCreateAgentServer(t, disp)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name:          "other-fail-agent",
				ProjectID:     project.ID,
				Task:          "some task",
				ProvisionOnly: true,
			})

			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			var resp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			require.NotNil(t, resp.Agent)
			require.NotEmpty(t, resp.Warnings)
			assert.Contains(t, resp.Warnings[0], "Failed to provision on runtime broker")
			_, err := s.GetAgent(context.Background(), resp.Agent.ID)
			assert.NoError(t, err, "the record is kept")
			assert.False(t, disp.deleteCalled)
		})
	}
}
