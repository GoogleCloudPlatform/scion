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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHubHealthCmdRegistration(t *testing.T) {
	found := false
	for _, c := range hubCmd.Commands() {
		if c.Name() == "health" {
			found = true
			break
		}
	}
	assert.True(t, found, "expected 'health' subcommand to be registered under hubCmd")
}

func baseSummary() *hubclient.HealthSummaryResponse {
	return &hubclient.HealthSummaryResponse{
		Status: "healthy",
		Hub: hubclient.HealthSummaryHub{
			Status:           "healthy",
			Version:          "0c07fdee",
			Uptime:           "25h10m",
			ConnectedBrokers: 1,
			ActiveAgents:     12,
			Projects:         3,
		},
		Database: hubclient.HealthSummaryDB{
			Status:             "healthy",
			PoolActive:         1,
			PoolMax:            5,
			PoolIdle:           4,
			PoolWaitCountTotal: 42,
		},
		Brokers: []hubclient.HealthSummaryBrkr{
			{
				ID:               "broker-1",
				Name:             "Hosted Broker",
				Status:           "online",
				Runtime:          "docker",
				RuntimeAvailable: true,
				AgentCount:       12,
				AgentHealthy:     12,
				LastHeartbeat:    time.Now().Add(-5 * time.Minute),
			},
		},
		Agents: hubclient.HealthSummaryAgents{
			Total: 40,
			ByPhase: map[string]int{
				"running":   6,
				"stopped":   30,
				"suspended": 4,
			},
		},
	}
}

func renderSummary(s *hubclient.HealthSummaryResponse) string {
	var buf bytes.Buffer
	printHubHealthSummary(&buf, s, "https://hub.example.com/")
	return buf.String()
}

func TestPrintHubHealthSummary(t *testing.T) {
	s := baseSummary()
	s.Agents.Stalled = []string{"stalled-agent-1"}
	s.Agents.Errored = []string{"errored-agent-1"}
	output := renderSummary(s)

	assert.Contains(t, output, "SCION HUB HEALTH & METRICS")
	assert.Contains(t, output, "https://hub.example.com/")
	assert.Contains(t, output, "Overall Status:      healthy")
	assert.Contains(t, output, "Active=1 / Max=5")
	assert.Contains(t, output, "Hosted Broker")
	assert.Contains(t, output, "5m ago", "heartbeat uses relative time like 'hub brokers'")
	assert.Contains(t, output, "stalled-agent-1")
	assert.Contains(t, output, "errored-agent-1")
}

func TestPrintHubHealthSummary_PhaseTallyAccountsForEveryAgent(t *testing.T) {
	s := baseSummary()
	s.Agents.ByPhase["hibernating"] = 2 // unknown phase
	s.Agents.Total = 42
	output := renderSummary(s)

	assert.Contains(t, output,
		"Phases: Total=42 | Running=6 | Error=0 | Stopped=30 | Suspended=4 | Other=2")
	assert.NotContains(t, output, "Errors:", "old two-phase line must be gone")
}

func TestPrintHubHealthSummary_Hints(t *testing.T) {
	s := baseSummary()
	s.Agents.Stalled = []string{"s1"}
	s.Agents.Crashed = []string{"c1"}
	s.Agents.Errored = []string{"e1"}
	output := renderSummary(s)

	assert.Contains(t, output, "x Crashed Agents (1): c1")
	assert.Contains(t, output, "'scion logs <agent>' to view termination logs")
	assert.Contains(t, output, "x Errored Agents (1): e1")
	assert.NotContains(t, output, "reset-auth", "errored agents are not necessarily auth failures")
	assert.Contains(t, output, "--project <project>", "fleet-wide names need a project hint")
}

func TestPrintHubHealthSummary_NeverHeartbeatAndNoBrokers(t *testing.T) {
	s := baseSummary()
	s.Brokers[0].LastHeartbeat = time.Time{}
	assert.Contains(t, renderSummary(s), "never")

	s.Brokers = nil
	assert.Contains(t, renderSummary(s), "No runtime brokers registered.")
}

func TestPrintHubHealthSummary_Dispatch(t *testing.T) {
	s := baseSummary()
	assert.NotContains(t, renderSummary(s), "Dispatch Queue Issues", "nil dispatch shows nothing")

	s.Dispatch = &hubclient.HealthSummaryDispatch{}
	assert.NotContains(t, renderSummary(s), "Dispatch Queue Issues", "zero dispatch shows nothing")

	s.Dispatch = &hubclient.HealthSummaryDispatch{StuckMessages: 3, Failed1h: 1}
	assert.Contains(t, renderSummary(s), "Dispatch Queue Issues: Stuck Messages=3, Failed (1h)=1")
}

func names(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s-%d", prefix, i)
	}
	return out
}

func TestFormatAgentNameList(t *testing.T) {
	assert.Equal(t, "(2): a, b", formatAgentNameList([]string{"a", "b"}))

	got := formatAgentNameList(names("x", 12))
	assert.Equal(t, "(12): x-0, x-1, x-2, x-3, x-4, x-5, x-6, x-7, x-8, x-9, … and 2 more", got)

	got = formatAgentNameList(names("x", hubHealthUnhealthyListCap))
	assert.Contains(t, got, "(100+): ")
	assert.Contains(t, got, "… and 90+ more")
	assert.NotContains(t, got, "x-10")
}

// newHealthTestServer serves the admin summary and /healthz with the given
// status codes and bodies.
func newHealthTestServer(t *testing.T, summaryStatus int, summaryBody string, healthzStatus int) (*httptest.Server, *int) {
	t.Helper()
	healthzCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/admin/health/summary":
			w.WriteHeader(summaryStatus)
			_, _ = w.Write([]byte(summaryBody))
		case "/healthz":
			healthzCalls++
			w.WriteHeader(healthzStatus)
			_, _ = w.Write([]byte(`{"status":"ok","version":"v1.2.3","scionVersion":"0.3.0","uptime":"1h"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &healthzCalls
}

func newTestHubClient(t *testing.T, url string) hubclient.Client {
	t.Helper()
	c, err := hubclient.New(url)
	require.NoError(t, err)
	return c
}

func TestReportHubHealth_ForbiddenFallsBackToBasic(t *testing.T) {
	srv, healthzCalls := newHealthTestServer(t, http.StatusForbidden,
		`{"error":{"code":"forbidden","message":"requires hub.health.read"}}`, http.StatusOK)
	client := newTestHubClient(t, srv.URL)

	var buf bytes.Buffer
	err := reportHubHealth(context.Background(), client, &buf, srv.URL, false)
	require.NoError(t, err)
	assert.Equal(t, 1, *healthzCalls)
	assert.Contains(t, buf.String(), "Hub Status: ok")
	assert.Contains(t, buf.String(), "requires the Hub admin role")
}

func TestReportHubHealth_ForbiddenJSONIsTypedAndDiscriminated(t *testing.T) {
	srv, _ := newHealthTestServer(t, http.StatusForbidden, `{}`, http.StatusOK)
	client := newTestHubClient(t, srv.URL)

	var buf bytes.Buffer
	require.NoError(t, reportHubHealth(context.Background(), client, &buf, srv.URL, true))

	var out HubHealthOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &out))
	assert.False(t, out.Detailed)
	assert.Nil(t, out.Summary)
	require.NotNil(t, out.Basic)
	assert.Equal(t, "ok", out.Basic.Status)
	assert.Equal(t, "0.3.0", out.Basic.ScionVersion)
	assert.NotEmpty(t, out.Note)
}

func TestReportHubHealth_ServerErrorMentioning403IsNotTreatedAsForbidden(t *testing.T) {
	// A 500 whose body (and request ID) contains "403" must surface as an
	// error, not be masked as "requires admin role".
	srv, healthzCalls := newHealthTestServer(t, http.StatusInternalServerError,
		`{"error":{"code":"internal","message":"upstream returned 403 for req a403f"}}`, http.StatusOK)
	client := newTestHubClient(t, srv.URL)

	var buf bytes.Buffer
	err := reportHubHealth(context.Background(), client, &buf, srv.URL, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to fetch Hub health summary")
	assert.Equal(t, 0, *healthzCalls, "must not fall back on non-403 errors")
	assert.Empty(t, buf.String())
}

func TestReportHubHealth_ForbiddenAndBasicFails(t *testing.T) {
	srv, _ := newHealthTestServer(t, http.StatusForbidden, `{}`, http.StatusInternalServerError)
	client := newTestHubClient(t, srv.URL)

	err := reportHubHealth(context.Background(), client, &bytes.Buffer{}, srv.URL, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "basic health check failed")
}

func TestReportHubHealth_DetailedJSON(t *testing.T) {
	body, err := json.Marshal(baseSummary())
	require.NoError(t, err)
	srv, healthzCalls := newHealthTestServer(t, http.StatusOK, string(body), http.StatusOK)
	client := newTestHubClient(t, srv.URL)

	var buf bytes.Buffer
	require.NoError(t, reportHubHealth(context.Background(), client, &buf, srv.URL, true))
	assert.Equal(t, 0, *healthzCalls)

	var out HubHealthOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &out))
	assert.True(t, out.Detailed)
	assert.Nil(t, out.Basic)
	require.NotNil(t, out.Summary)
	assert.Equal(t, 40, out.Summary.Agents.Total)
}
