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

package hubclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestReportAgentLaunch_200 covers the three 200 results the Hub can answer
// (design §3.2): applied, duplicate, completed.
func TestReportAgentLaunch_200(t *testing.T) {
	for _, result := range []string{AgentLaunchReportResultApplied, AgentLaunchReportResultDuplicate, AgentLaunchReportResultCompleted} {
		t.Run(result, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"result":"` + result + `"}`))
			}))
			defer server.Close()

			client, err := New(server.URL)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			got, err := client.RuntimeBrokers().ReportAgentLaunch(context.Background(), "broker-1", "agent-1", &AgentLaunchReport{LaunchID: "L1", State: AgentLaunchReportStateClaim})
			if err != nil {
				t.Fatalf("ReportAgentLaunch: %v", err)
			}
			if got.HTTPStatus != 0 || got.Result != result {
				t.Fatalf("got %+v, want Result=%s HTTPStatus=0", got, result)
			}
		})
	}
}

// TestReportAgentLaunch_403 covers "broker is not agent.RuntimeBrokerID".
func TestReportAgentLaunch_403(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	client, err := New(server.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := client.RuntimeBrokers().ReportAgentLaunch(context.Background(), "broker-1", "agent-1", &AgentLaunchReport{LaunchID: "L1"})
	if err != nil {
		t.Fatalf("ReportAgentLaunch: %v", err)
	}
	if got.HTTPStatus != http.StatusForbidden {
		t.Fatalf("got %+v, want HTTPStatus=403", got)
	}
}

// TestReportAgentLaunch_404AgentLaunchUnknown covers the structured 404 the
// Hub sends when the agent row does not exist: a definitive result, not an
// error (unlike a plain 404).
func TestReportAgentLaunch_404AgentLaunchUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"agent_launch_unknown"}`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := client.RuntimeBrokers().ReportAgentLaunch(context.Background(), "broker-1", "agent-1", &AgentLaunchReport{LaunchID: "L1"})
	if err != nil {
		t.Fatalf("ReportAgentLaunch: %v (want a result, not an error)", err)
	}
	if got.HTTPStatus != http.StatusNotFound || got.Code != AgentLaunchReportCodeUnknownLaunch {
		t.Fatalf("got %+v, want HTTPStatus=404 Code=agent_launch_unknown", got)
	}
}

// TestReportAgentLaunch_PlainNotFoundIsRetryable covers design §5 N-9: a
// plain 404 (no structured body, e.g. a Hub node without this route in a
// mixed-version HA cluster) must be an error the sender retries, never the
// definitive agent_launch_unknown result.
func TestReportAgentLaunch_PlainNotFoundIsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()

	client, err := New(server.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := client.RuntimeBrokers().ReportAgentLaunch(context.Background(), "broker-1", "agent-1", &AgentLaunchReport{LaunchID: "L1"})
	if err == nil {
		t.Fatalf("got a result %+v, want an error (plain 404 means retry)", got)
	}
}

// TestReportAgentLaunch_409StaleLaunch covers the structured 409 with its
// reason.
func TestReportAgentLaunch_409StaleLaunch(t *testing.T) {
	for _, reason := range []string{
		AgentLaunchReportReasonSuperseded, AgentLaunchReportReasonDeleted, AgentLaunchReportReasonStopped,
		AgentLaunchReportReasonTimedOut, AgentLaunchReportReasonLost, AgentLaunchReportReasonFailed,
		AgentLaunchReportReasonNotLaunched, AgentLaunchReportReasonOtherOwner,
	} {
		t.Run(reason, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"code":"stale_launch","reason":"` + reason + `"}`))
			}))
			defer server.Close()

			client, err := New(server.URL)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			got, err := client.RuntimeBrokers().ReportAgentLaunch(context.Background(), "broker-1", "agent-1", &AgentLaunchReport{LaunchID: "L1"})
			if err != nil {
				t.Fatalf("ReportAgentLaunch: %v", err)
			}
			if got.HTTPStatus != http.StatusConflict || got.Code != AgentLaunchReportCodeStaleLaunch || got.Reason != reason {
				t.Fatalf("got %+v, want HTTPStatus=409 Code=stale_launch Reason=%s", got, reason)
			}
		})
	}
}

// TestReportAgentLaunch_409UnrecognizedCodeIsRetryable covers a 409 whose
// code is not stale_launch: that is not something the sender can classify by
// Reason, so it must be treated as retryable, not guessed at.
func TestReportAgentLaunch_409UnrecognizedCodeIsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"something_else"}`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := client.RuntimeBrokers().ReportAgentLaunch(context.Background(), "broker-1", "agent-1", &AgentLaunchReport{LaunchID: "L1"})
	if err == nil {
		t.Fatalf("got a result %+v, want an error (unrecognized 409 code)", got)
	}
}

// TestReportAgentLaunch_400And401AreDefinitive covers these being protocol/
// auth failures, never transient like an unreachable Hub or a 5xx, so the
// sender must not retry them forever.
func TestReportAgentLaunch_400And401AreDefinitive(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer server.Close()

			client, err := New(server.URL)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			got, err := client.RuntimeBrokers().ReportAgentLaunch(context.Background(), "broker-1", "agent-1", &AgentLaunchReport{LaunchID: "L1"})
			if err != nil {
				t.Fatalf("ReportAgentLaunch: %v (want a definitive result, not an error)", err)
			}
			if got.HTTPStatus != status {
				t.Fatalf("got %+v, want HTTPStatus=%d", got, status)
			}
		})
	}
}

// TestReportAgentLaunch_5xxIsRetryable covers the ordinary "unreachable"
// bucket for a Hub-side failure.
func TestReportAgentLaunch_5xxIsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client, err := New(server.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := client.RuntimeBrokers().ReportAgentLaunch(context.Background(), "broker-1", "agent-1", &AgentLaunchReport{LaunchID: "L1"})
	if err == nil {
		t.Fatalf("got a result %+v, want an error (5xx means retry)", got)
	}
}
