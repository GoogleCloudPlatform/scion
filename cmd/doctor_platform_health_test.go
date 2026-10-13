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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDoctorHub is an in-process Hub serving the routes doctor reads.
type fakeDoctorHub struct {
	healthzBody   string // empty means a healthy /healthz
	summaryStatus int    // 0 means 200
	summaryBody   string // response body for the health summary
	brokersBody   string
	agentsBody    string
	listCalls     atomic.Int32 // broker and agent list calls (the D3/D4 fallback)
}

func (f *fakeDoctorHub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/healthz":
			body := f.healthzBody
			if body == "" {
				body = `{"status":"healthy"}`
			}
			_, _ = io.WriteString(w, body)
		case "/api/v1/auth/me":
			_, _ = io.WriteString(w, `{"id":"u1","email":"admin@example.com"}`)
		case "/api/v1/admin/health/summary":
			if f.summaryStatus != 0 {
				w.WriteHeader(f.summaryStatus)
			}
			_, _ = io.WriteString(w, f.summaryBody)
		case "/api/v1/runtime-brokers":
			f.listCalls.Add(1)
			_, _ = io.WriteString(w, f.brokersBody)
		case "/api/v1/agents":
			f.listCalls.Add(1)
			_, _ = io.WriteString(w, f.agentsBody)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newDoctorTestClient(t *testing.T, url string) hubclient.Client {
	t.Helper()
	c, err := hubclient.New(url, hubclient.WithBearerToken("test-user-token"))
	require.NoError(t, err)
	return c
}

func summaryJSON(t *testing.T, s hubclient.HealthSummary) string {
	t.Helper()
	data, err := json.Marshal(s)
	require.NoError(t, err)
	return string(data)
}

func degradedSummary() hubclient.HealthSummary {
	hb := time.Date(2026, 10, 12, 9, 58, 0, 0, time.UTC)
	nfsBad := false
	return hubclient.HealthSummary{
		Status:      healthSummaryDegraded,
		GeneratedAt: time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC),
		Attention: []hubclient.HealthAttentionItem{
			{Severity: "warning", Kind: "broker_offline", Subject: hubclient.HealthAttentionSubject{Type: "runtime_broker", ID: "b1", Name: "gpu-1"}, Message: `Runtime broker "gpu-1" is offline`},
			{Severity: "warning", Kind: "broker_nfs", Subject: hubclient.HealthAttentionSubject{Type: "runtime_broker", ID: "b2", Name: "east"}, Message: `Runtime broker "east" reports its NFS workspace storage unhealthy`},
			{Severity: "warning", Kind: "agents", Subject: hubclient.HealthAttentionSubject{Type: "agent", ID: "a1", Name: "fixer"}, Message: "Agent web / fixer has crashed"},
		},
		Hub: hubclient.HealthSummaryHub{
			Status: healthSummaryHealthy, InstanceID: "i1", Version: "1.4.0",
			Instances:       &hubclient.HealthSummaryHubFleet{Live: 2, Healthy: 2},
			UnhealthyChecks: []hubclient.HealthSummaryHubCheck{},
		},
		Brokers: hubclient.HealthSummaryBrokers{
			Items: []hubclient.HealthSummaryBroker{
				{ID: "b1", Name: "gpu-1", Version: "1.4.0", Status: "offline", LastHeartbeat: &hb},
				{
					ID: "b2", Name: "east", Version: "1.4.0", Status: "online", LastHeartbeat: &hb,
					Runtime:          &hubclient.HealthBrokerRuntime{Type: "docker", Profile: "default"},
					WorkspaceStorage: &hubclient.HealthBrokerStorage{Backend: "nfs", NFSHealthy: &nfsBad},
					Health:           &hubclient.HealthBrokerSelf{Status: "degraded", Checks: map[string]string{"runtime": "unavailable", "disk": "healthy"}},
					Agents:           hubclient.HealthBrokerAgents{Running: 3, Attention: 1},
				},
			},
			Total: 2,
		},
		Agents: &hubclient.HealthSummaryAgents{
			Total: 20, Active: 14, Errored: 1, Considered: 16,
			ByPhase:  []hubclient.HealthPhaseCount{{Phase: "running", Count: 14}, {Phase: "stopped", Count: 4}},
			Problems: []hubclient.HealthAgentGroup{{Kind: "crashed", Count: 1, Items: []hubclient.HealthAgentRef{{ID: "a1", Name: "fixer", ProjectSlug: "web"}}}},
		},
		Dispatch:          &hubclient.HealthSummaryDispatch{FailedBrokerDispatch1h: 2},
		Integrations:      []hubclient.HealthSummaryIntegration{},
		IntegrationCounts: hubclient.HealthSummaryIntegrationCounts{Total: 1, Healthy: 1},
		HubInstances: &hubclient.HealthSummaryHubInstances{
			AsOf:  time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC),
			Items: []hubclient.HealthHubInstance{{ID: "i1", Label: "hub-a", State: "live", Serving: true, Status: "healthy", StartedAt: time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)}},
			Live:  1, Total: 1,
		},
	}
}

func checkNames(checks []scionruntime.CheckResult) []string {
	out := make([]string, 0, len(checks))
	for _, c := range checks {
		out = append(out, c.Name)
	}
	return out
}

func findCheck(t *testing.T, checks []scionruntime.CheckResult, name string) scionruntime.CheckResult {
	t.Helper()
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %q check in %v", name, checkNames(checks))
	return scionruntime.CheckResult{}
}

func TestRenderDoctorHealthSummary_HeadlineFollowsSummaryStatus(t *testing.T) {
	tests := []struct {
		status     string
		wantStatus string
		wantMsg    string
	}{
		{status: "healthy", wantStatus: "pass", wantMsg: "Platform is healthy"},
		{status: "degraded", wantStatus: "warn", wantMsg: "Platform is degraded (3 item(s) need attention)"},
		{status: "unhealthy", wantStatus: "fail", wantMsg: "Platform is unhealthy (3 item(s) need attention)"},
		{status: "something-new", wantStatus: "warn", wantMsg: `Platform reported status "something-new"`},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			s := degradedSummary()
			s.Status = tt.status
			if tt.status == "healthy" {
				s.Attention = nil
			}
			got := renderDoctorHealthSummary(&s, false)
			require.NotEmpty(t, got)
			assert.Equal(t, doctorHealthSummaryCheck, got[0].Name)
			assert.Equal(t, tt.wantStatus, got[0].Status)
			assert.Contains(t, got[0].Message, tt.wantMsg)
		})
	}
}

func TestRenderDoctorHealthSummary_AttentionInOrderWithSeverity(t *testing.T) {
	s := degradedSummary()
	s.Attention = append([]hubclient.HealthAttentionItem{
		{Severity: "critical", Kind: "hub_instance", Subject: hubclient.HealthAttentionSubject{Type: "hub"}, Message: "No hub instance is reporting"},
	}, s.Attention...)

	got := renderDoctorHealthSummary(&s, false)

	var attention []scionruntime.CheckResult
	for _, c := range got {
		if c.Name == "attention" {
			attention = append(attention, c)
		}
	}
	require.Len(t, attention, 4)
	// The hub's order is kept and its severities map to fail and warn.
	assert.Equal(t, scionruntime.CheckResult{Name: "attention", Status: "fail", Message: "No hub instance is reporting"}, attention[0])
	assert.Equal(t, `Runtime broker "gpu-1" is offline`, attention[1].Message)
	assert.Equal(t, "Agent web / fixer has crashed", attention[3].Message)
	for _, c := range attention[1:] {
		assert.Equal(t, "warn", c.Status)
	}
	// Attention comes straight after the headline, before the data lines.
	assert.Equal(t, []string{doctorHealthSummaryCheck, "attention", "attention", "attention", "attention", "hub", "runtime-brokers", "agents", "dispatch", "integrations"}, checkNames(got))
	// Data lines are information only; doctor does not grade them.
	for _, c := range got[5:] {
		assert.Equal(t, "info", c.Status, c.Name)
	}
}

func TestRenderDoctorHealthSummary_Verbose(t *testing.T) {
	s := degradedSummary()

	brief := renderDoctorHealthSummary(&s, false)
	assert.NotContains(t, findCheck(t, brief, "runtime-brokers").Message, "\n")
	assert.NotContains(t, checkNames(brief), "hub-instances")

	verbose := renderDoctorHealthSummary(&s, true)
	brokers := findCheck(t, verbose, "runtime-brokers").Message
	assert.Contains(t, brokers, "1 of 2 listed online, 2 registered")
	assert.Contains(t, brokers, "east; online; version 1.4.0; runtime docker/default; storage nfs (unhealthy); self-health degraded (runtime: unavailable); agents running 3, attention 1")
	assert.NotContains(t, brokers, "disk: healthy")
	assert.Contains(t, findCheck(t, verbose, "agents").Message, "by phase: running 14, stopped 4")
	instances := findCheck(t, verbose, "hub-instances").Message
	assert.Contains(t, instances, "hub-a (serving); live; healthy")
	assert.Contains(t, instances, "up 2h0m0s")
	assert.Contains(t, findCheck(t, verbose, "generated").Message, "served by hub-a")
}

func TestRenderDoctorHealthSummary_TestIdentityLineOnlyWhenEnabled(t *testing.T) {
	s := degradedSummary()
	live := 2
	s.Hub.LiveTestAdminIdentities = &live
	assert.NotContains(t, checkNames(renderDoctorHealthSummary(&s, true)), "test-identities",
		"no line while every test identity setting is off")

	s.Hub.TestIdentitiesEnabled = true
	s.Hub.TestAdminIdentitiesEnabled = true
	line := findCheck(t, renderDoctorHealthSummary(&s, false), "test-identities")
	assert.Equal(t, "info", line.Status)
	assert.Equal(t, "Enabled on this Hub: test identities, test admin identities (2 live admin test identities)", line.Message)
}

func TestRenderDoctorHealthSummary_ServiceAccountCheck(t *testing.T) {
	s := degradedSummary()
	s.ServiceAccountCheck = &hubclient.HealthSummarySACheck{
		Status: "degraded", Cause: "the hub identity lacks access", Remedy: "Grant the role.",
		DocsURL: "https://example.com/docs", Instances: []string{"hub-a"},
	}
	line := findCheck(t, renderDoctorHealthSummary(&s, false), "service-account-check")
	assert.Equal(t, "The service account assignment check cannot run: the hub identity lacks access (instances: hub-a)", line.Message)
	assert.Equal(t, "Grant the role. See https://example.com/docs", line.Remediation)
}

func TestDoctorHubChecks_SummaryReplacesListChecks(t *testing.T) {
	f := &fakeDoctorHub{summaryBody: summaryJSON(t, degradedSummary())}
	srv := f.serve(t)

	var buf bytes.Buffer
	r := &doctorReport{stdout: &buf}
	ph := r.hubChecks(srv.URL, newDoctorTestClient(t, srv.URL), "")
	r.platformHealthSection(ph)

	assert.Zero(t, f.listCalls.Load(), "the broker and agent lists are not read when the summary is readable")
	assert.Equal(t, "skip", findCheck(t, r.HubChecks, "broker-connectivity").Status)
	assert.Equal(t, "skip", findCheck(t, r.HubChecks, "agent-health").Status)
	assert.Equal(t, "warn", r.PlatformHealth[0].Status)
	assert.JSONEq(t, f.summaryBody, string(r.HealthSummary))

	// Degraded is a warning: exit zero.
	assert.NoError(t, r.finish())

	out := buf.String()
	assert.Contains(t, out, "! health-summary: Platform is degraded (3 item(s) need attention)")
	assert.Less(t, strings.Index(out, `gpu-1" is offline`), strings.Index(out, "fixer has crashed"))
}

func TestDoctorExit_UnhealthySummaryExitsZero(t *testing.T) {
	s := degradedSummary()
	s.Status = healthSummaryUnhealthy
	f := &fakeDoctorHub{summaryBody: summaryJSON(t, s)}
	srv := f.serve(t)

	var buf bytes.Buffer
	r := &doctorReport{stdout: &buf}
	r.platformHealthSection(r.hubChecks(srv.URL, newDoctorTestClient(t, srv.URL), ""))
	assert.NoError(t, r.finish(), "check results do not set the exit status")
	assert.Equal(t, "fail", r.overallStatus())
	assert.Contains(t, buf.String(), "✗ health-summary: Platform is unhealthy")
}

func TestDoctorExit_HubConnectivityFailureExitsZero(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens: D1 fails

	r := &doctorReport{stdout: io.Discard}
	r.platformHealthSection(r.hubChecks(url, newDoctorTestClient(t, url), ""))
	assert.Equal(t, "fail", r.HubChecks[0].Status)
	assert.Equal(t, "skip", r.PlatformHealth[0].Status)
	assert.Equal(t, "Skipped (Hub connectivity check failed)", r.PlatformHealth[0].Message)
	assert.NoError(t, r.finish(), "check results do not set the exit status")
	assert.Equal(t, "fail", r.overallStatus())
}

func TestDoctorJSON_HubConnectivityFailureExitsZero(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens: D1 fails

	var buf bytes.Buffer
	r := &doctorReport{stdout: &buf, json: true}
	r.platformHealthSection(r.hubChecks(url, newDoctorTestClient(t, url), ""))
	require.NoError(t, r.finish(), "check results do not set the exit status")

	var doc struct {
		Status    string                     `json:"status"`
		HubChecks []scionruntime.CheckResult `json:"hubChecks"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &doc), "stdout: %s", buf.String())
	assert.Equal(t, "fail", doc.Status)
	assert.Equal(t, "fail", findCheck(t, doc.HubChecks, "hub-connectivity").Status)
}

// failingWriter fails every write.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestDoctorJSON_OutputWriteFailureExitsNonZero(t *testing.T) {
	r := &doctorReport{stdout: failingWriter{}, json: true}
	r.add(&r.General, scionruntime.CheckResult{Name: "git", Status: "pass", Message: "ok"})
	err := r.finish()
	require.Error(t, err, "failing to write the output is a failure of the command itself")
	assert.Contains(t, err.Error(), "write failed")
}

func TestDoctorJSON_StatusFollowsChecks(t *testing.T) {
	tests := []struct {
		name     string
		statuses []string
		want     string
	}{
		{name: "all pass", statuses: []string{"pass", "info", "skip"}, want: "pass"},
		{name: "a warning", statuses: []string{"pass", "warn", "skip"}, want: "warn"},
		{name: "a failure", statuses: []string{"warn", "fail", "pass"}, want: "fail"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			r := &doctorReport{stdout: &buf, json: true}
			for i, st := range tt.statuses {
				r.add(&r.General, scionruntime.CheckResult{Name: fmt.Sprintf("c%d", i), Status: st, Message: "m"})
			}
			require.NoError(t, r.finish())
			var doc struct {
				Status string `json:"status"`
			}
			require.NoError(t, json.Unmarshal(buf.Bytes(), &doc))
			assert.Equal(t, tt.want, doc.Status)
		})
	}
}

// fallbackAgentsBody lists one agent per case the D4 fallback must handle.
const fallbackAgentsBody = `{"agents":[
	{"id":"1","name":"ok","phase":"running","activity":"working"},
	{"id":"2","name":"stalled","phase":"running","activity":"stalled"},
	{"id":"3","name":"crashed","phase":"running","activity":"crashed"},
	{"id":"4","name":"offline","phase":"running","activity":"offline"},
	{"id":"5","name":"errored","phase":"error","activity":"crashed"},
	{"id":"6","name":"stopped-crashed","phase":"stopped","activity":"crashed"}
]}`

func TestDoctorHubChecks_ForbiddenFallsBackToListChecks(t *testing.T) {
	f := &fakeDoctorHub{
		summaryStatus: http.StatusForbidden,
		summaryBody:   `{"error":{"code":"forbidden","message":"non-user identity"}}`,
		brokersBody:   `{"brokers":[{"id":"b1","name":"east","status":"online"},{"id":"b2","name":"west","status":"offline"}]}`,
		agentsBody:    fallbackAgentsBody,
	}
	srv := f.serve(t)

	r := &doctorReport{stdout: io.Discard}
	r.platformHealthSection(r.hubChecks(srv.URL, newDoctorTestClient(t, srv.URL), ""))

	require.Len(t, r.PlatformHealth, 1)
	assert.Equal(t, "skip", r.PlatformHealth[0].Status)
	assert.Equal(t, doctorHealthSummaryForbidden, r.PlatformHealth[0].Message)
	assert.Contains(t, r.PlatformHealth[0].Remediation, "hub_health:read")
	assert.Nil(t, r.HealthSummary)

	assert.Equal(t, int32(2), f.listCalls.Load())
	d3 := findCheck(t, r.HubChecks, "broker-connectivity")
	assert.Equal(t, "1/2 broker(s) online", d3.Message)

	// Same policy as the summary: stalled is not counted, offline is,
	// stopped agents are not considered, each agent counts once.
	d4 := findCheck(t, r.HubChecks, "agent-health")
	assert.Equal(t, "warn", d4.Status)
	assert.Equal(t, "3 of 5 agent(s) not stopped need attention: 1 in error, 1 crashed, 1 offline", d4.Message)
	assert.NotContains(t, d4.Message, "stalled")

	assert.NoError(t, r.finish(), "a summary the caller may not read does not fail the command")
}

func TestCheckDoctorAgentHealth_StalledOnlyIsHealthy(t *testing.T) {
	f := &fakeDoctorHub{agentsBody: `{"agents":[{"id":"1","name":"a","phase":"running","activity":"stalled"},{"id":"2","name":"b","phase":"stopped","activity":"offline"}]}`}
	srv := f.serve(t)

	got := checkDoctorAgentHealth(srv.URL, true, newDoctorTestClient(t, srv.URL))
	assert.Equal(t, "pass", got.Status)
	assert.Equal(t, "No agent in error, crashed or offline (2 agent(s), 1 not stopped)", got.Message)
}

func TestDoctorHubChecks_UnavailableShowsFixedMessage(t *testing.T) {
	f := &fakeDoctorHub{
		summaryStatus: http.StatusServiceUnavailable,
		summaryBody:   `<html>proxy says something else</html>`,
		brokersBody:   `{"brokers":[{"id":"b1","name":"east","status":"online"}]}`,
		agentsBody:    `{"agents":[]}`,
	}
	srv := f.serve(t)

	r := &doctorReport{stdout: io.Discard}
	r.platformHealthSection(r.hubChecks(srv.URL, newDoctorTestClient(t, srv.URL), ""))

	require.Len(t, r.PlatformHealth, 1)
	assert.Equal(t, "warn", r.PlatformHealth[0].Status)
	assert.Equal(t, doctorHealthSummaryUnavailable, r.PlatformHealth[0].Message)
	assert.Equal(t, int32(2), f.listCalls.Load(), "the list checks run as a fallback")
	assert.NoError(t, r.finish())
}

func TestDoctorJSON_CleanForNonDiagnosableRuntime(t *testing.T) {
	// No docker binary: the docker check fails deterministically.
	t.Setenv("PATH", t.TempDir())

	f := &fakeDoctorHub{summaryBody: summaryJSON(t, degradedSummary())}
	srv := f.serve(t)

	var buf bytes.Buffer
	r := &doctorReport{stdout: &buf, json: true}
	r.text("Scion Doctor\n") // text lines must never reach stdout in JSON mode
	r.heading("General")
	r.add(&r.General, checkGit())
	r.platformHealthSection(r.hubChecks(srv.URL, newDoctorTestClient(t, srv.URL), ""))
	r.runtimeSection("docker", nil, scionruntime.DiagnosticOpts{})
	require.NoError(t, r.finish())

	// stdout is exactly one JSON object.
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	var doc map[string]json.RawMessage
	require.NoError(t, dec.Decode(&doc), "stdout: %s", buf.String())
	var extra json.RawMessage
	assert.ErrorIs(t, dec.Decode(&extra), io.EOF, "nothing may follow the JSON object")
	assert.True(t, strings.HasPrefix(buf.String(), "{"), "stdout must start with the JSON object")

	// The failed docker check makes the status fail, with exit zero.
	assert.JSONEq(t, `"fail"`, string(doc["status"]))
	assert.JSONEq(t, `"docker"`, string(doc["runtime"]))
	assert.JSONEq(t, f.summaryBody, string(doc["healthSummary"]))
	for _, key := range []string{"general", "hubChecks", "platformHealth", "checks"} {
		assert.Contains(t, doc, key)
	}
	var checks []scionruntime.CheckResult
	require.NoError(t, json.Unmarshal(doc["checks"], &checks))
	assert.Equal(t, []string{"runtime", "docker"}, checkNames(checks))
	assert.Equal(t, "fail", checks[1].Status)
}

func TestDoctorJSON_UnhealthyIsReportedInJSONAndExitsZero(t *testing.T) {
	s := degradedSummary()
	s.Status = healthSummaryUnhealthy
	f := &fakeDoctorHub{summaryBody: summaryJSON(t, s)}
	srv := f.serve(t)

	var buf bytes.Buffer
	r := &doctorReport{stdout: &buf, json: true}
	r.platformHealthSection(r.hubChecks(srv.URL, newDoctorTestClient(t, srv.URL), ""))
	require.NoError(t, r.finish(), "the result is carried by the JSON, not the exit status")

	var doc struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &doc))
	assert.Equal(t, "fail", doc.Status)
}

func TestDoctorPlatformHealth_NoHubConfigured(t *testing.T) {
	r := &doctorReport{stdout: io.Discard}
	r.platformHealthSection(r.hubChecks("", nil, ""))
	assert.Equal(t, scionruntime.CheckResult{Name: doctorHealthSummaryCheck, Status: "skip", Message: "No Hub endpoint configured"}, r.PlatformHealth[0])
	assert.NoError(t, r.finish())
}

// TestHealthSummaryClientTypeMatchesHub keeps hubclient.HealthSummary in
// step with the hub's wire type: every JSON field path must exist in both.
// JSON output passes the raw body through, so a lagging client type loses
// nothing there, but the text renderer would not see a new field.
func TestHealthSummaryClientTypeMatchesHub(t *testing.T) {
	hubPaths := map[string]bool{}
	jsonFieldPaths(reflect.TypeOf(hub.HealthSummaryResponse{}), "", hubPaths)
	clientPaths := map[string]bool{}
	jsonFieldPaths(reflect.TypeOf(hubclient.HealthSummary{}), "", clientPaths)

	var missing, extra []string
	for p := range hubPaths {
		if !clientPaths[p] {
			missing = append(missing, p)
		}
	}
	for p := range clientPaths {
		if !hubPaths[p] {
			extra = append(extra, p)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	assert.Empty(t, missing, "hub fields not mirrored in hubclient.HealthSummary")
	assert.Empty(t, extra, "hubclient.HealthSummary fields the hub does not send")
	assert.NotEmpty(t, hubPaths)
}

// jsonFieldPaths records the JSON path of every field reachable from t.
func jsonFieldPaths(t reflect.Type, prefix string, out map[string]bool) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || t == reflect.TypeOf(time.Time{}) {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		path := prefix + name
		out[path] = true
		jsonFieldPaths(f.Type, path+".", out)
	}
}

func TestDoctorHubChecks_UnhealthyHealthzStillReadsSummary(t *testing.T) {
	s := degradedSummary()
	s.Status = healthSummaryUnhealthy
	f := &fakeDoctorHub{
		healthzBody: `{"status":"unhealthy","checks":{"database":"unhealthy"}}`,
		summaryBody: summaryJSON(t, s),
	}
	srv := f.serve(t)

	r := &doctorReport{stdout: io.Discard}
	r.platformHealthSection(r.hubChecks(srv.URL, newDoctorTestClient(t, srv.URL), ""))

	assert.Equal(t, "fail", r.HubChecks[0].Status, "D1 fails on an unhealthy /healthz")
	// The Hub answered, so the summary is still read and explains why.
	assert.Equal(t, "fail", r.PlatformHealth[0].Status)
	assert.Contains(t, r.PlatformHealth[0].Message, "Platform is unhealthy")
	assert.NotNil(t, r.HealthSummary)
	assert.NoError(t, r.finish(), "check results do not set the exit status")
	assert.Equal(t, "fail", r.overallStatus())
}

func TestDoctorHubChecks_UnhealthyHealthzSummaryUnavailable(t *testing.T) {
	f := &fakeDoctorHub{
		healthzBody:   `{"status":"unhealthy","checks":{"database":"unhealthy"}}`,
		summaryStatus: http.StatusServiceUnavailable,
		summaryBody:   `{"error":{"code":"unavailable","message":"x"}}`,
	}
	srv := f.serve(t)

	r := &doctorReport{stdout: io.Discard}
	r.platformHealthSection(r.hubChecks(srv.URL, newDoctorTestClient(t, srv.URL), ""))

	require.Len(t, r.PlatformHealth, 1)
	assert.Equal(t, "warn", r.PlatformHealth[0].Status)
	assert.Equal(t, doctorHealthSummaryUnavailable, r.PlatformHealth[0].Message)
}

func TestStripControlChars(t *testing.T) {
	tests := map[string]string{
		"plain text":                      "plain text",
		"line one\nline two":              "line one\nline two",
		"a\x1b[31mred\x1b[0m":             "a[31mred[0m",
		"bell\x07 tab\t cr\r del\x7f":     "bell tab cr del",
		"c1 \u009b2J and \u0085next line": "c1 2J and next line",
		"unicode stays: \u00e9\u2713":     "unicode stays: \u00e9\u2713",
	}
	for in, want := range tests {
		assert.Equal(t, want, stripControlChars(in), "%q", in)
	}
}

func TestDoctorReport_HubTextCannotDriveTheTerminal(t *testing.T) {
	s := degradedSummary()
	s.Attention[0].Message = "Runtime broker \"gpu\x1b]0;x\x07-1\" is offline"
	s.Brokers.Items[0].Name = "gpu\x1b[2J-1"
	s.Integrations = []hubclient.HealthSummaryIntegration{{Name: "chat", Platform: "slack", Health: "degraded", Reason: "reason\u009b31m here"}}
	s.IntegrationsDetail = true

	var buf bytes.Buffer
	r := &doctorReport{stdout: &buf, color: true, verbose: true}
	r.platformHealthSection(doctorPlatformHealth{summary: &s})

	out := buf.String()
	assert.Contains(t, out, `Runtime broker "gpu]0;x-1" is offline`)
	assert.Contains(t, out, "gpu[2J-1; offline")
	assert.Contains(t, out, "reason31m here")
	assert.NotContains(t, out, "\x07")
	assert.NotContains(t, out, "\u009b")
	assert.NotContains(t, out, "gpu\x1b")
	// Verbose rows keep their line breaks.
	assert.Contains(t, findCheck(t, r.PlatformHealth, "runtime-brokers").Message, "\n    gpu[2J-1")
}

func TestDoctorJSON_VerboseAddsNoTables(t *testing.T) {
	f := &fakeDoctorHub{summaryBody: summaryJSON(t, degradedSummary())}
	srv := f.serve(t)

	var buf bytes.Buffer
	r := &doctorReport{stdout: &buf, json: true, verbose: true}
	r.platformHealthSection(r.hubChecks(srv.URL, newDoctorTestClient(t, srv.URL), ""))
	require.NoError(t, r.finish())

	var doc struct {
		PlatformHealth []scionruntime.CheckResult `json:"platformHealth"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &doc))
	require.NotEmpty(t, doc.PlatformHealth)
	for _, c := range doc.PlatformHealth {
		assert.NotContains(t, c.Message, "\n", c.Name)
	}
	assert.NotContains(t, checkNames(doc.PlatformHealth), "hub-instances")
}

// fakeDiagnosable is a runtime with its own diagnostics.
type fakeDiagnosable struct{ checks []scionruntime.CheckResult }

func (f fakeDiagnosable) RunDiagnostics(scionruntime.DiagnosticOpts) scionruntime.DiagnosticReport {
	return scionruntime.DiagnosticReport{Runtime: "kubernetes", Checks: f.checks}
}

func failingDiagnosable() fakeDiagnosable {
	return fakeDiagnosable{checks: []scionruntime.CheckResult{
		{Name: "cluster", Status: "pass", Message: "cluster reachable"},
		{Name: "rbac", Status: "fail", Message: "cannot create pods", Remediation: "Grant the role"},
	}}
}

func TestDoctorRuntime_DiagnosableFailureTextExitsZero(t *testing.T) {
	var buf bytes.Buffer
	r := &doctorReport{stdout: &buf}
	r.runtimeSection("kubernetes", failingDiagnosable(), scionruntime.DiagnosticOpts{})

	assert.NoError(t, r.finish(), "check results do not set the exit status")
	assert.Equal(t, "fail", r.overallStatus())
	out := buf.String()
	assert.Contains(t, out, "Runtime Diagnostics (kubernetes)")
	assert.Contains(t, out, "✗ rbac: cannot create pods")
	assert.Contains(t, out, "1 checks passed, 0 warnings, 1 failures")
}

func TestDoctorRuntime_DiagnosableFailureJSONExitsZero(t *testing.T) {
	var buf bytes.Buffer
	r := &doctorReport{stdout: &buf, json: true}
	r.runtimeSection("kubernetes", failingDiagnosable(), scionruntime.DiagnosticOpts{})

	require.NoError(t, r.finish(), "check results do not set the exit status")

	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	var doc struct {
		Status  string                     `json:"status"`
		Runtime string                     `json:"runtime"`
		Checks  []scionruntime.CheckResult `json:"checks"`
	}
	require.NoError(t, dec.Decode(&doc), "stdout: %s", buf.String())
	var extra json.RawMessage
	assert.ErrorIs(t, dec.Decode(&extra), io.EOF, "nothing may follow the JSON object")
	assert.Equal(t, "fail", doc.Status)
	assert.Equal(t, "kubernetes", doc.Runtime)
	assert.Equal(t, []string{"runtime", "cluster", "rbac"}, checkNames(doc.Checks))
}

func TestDoctorJSON_NoProject(t *testing.T) {
	var buf bytes.Buffer
	r := &doctorReport{stdout: &buf, json: true}
	r.heading("Runtime")
	require.NoError(t, r.noProject())

	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	var doc map[string]json.RawMessage
	require.NoError(t, dec.Decode(&doc), "stdout: %s", buf.String())
	var extra json.RawMessage
	assert.ErrorIs(t, dec.Decode(&extra), io.EOF)
	assert.NotContains(t, doc, "runtime", "no runtime without a project")
	assert.JSONEq(t, `"warn"`, string(doc["status"]))
	var checks []scionruntime.CheckResult
	require.NoError(t, json.Unmarshal(doc["checks"], &checks))
	require.Len(t, checks, 1)
	assert.Equal(t, "project", checks[0].Name)
	assert.Equal(t, "warn", checks[0].Status)
}

func TestDoctorReport_HubTextCannotForgeLines(t *testing.T) {
	s := degradedSummary()
	forged := "  ✓ health-summary: Platform is healthy"
	s.Brokers.Items[0].Name = "x\n" + forged
	s.Attention[0].Message = "Runtime broker is offline\r\n" + forged
	s.Brokers.Items[1].Health.Checks = map[string]string{"run\ntime": "un\navailable"}
	raw := summaryJSON(t, s)
	s.Raw = json.RawMessage(raw)

	var buf bytes.Buffer
	r := &doctorReport{stdout: &buf, verbose: true}
	r.platformHealthSection(doctorPlatformHealth{summary: &s})

	for _, line := range strings.Split(buf.String(), "\n") {
		assert.NotEqual(t, forged, line, "a hub value started its own line")
		assert.False(t, strings.HasPrefix(line, forged), "line %q", line)
	}
	out := buf.String()
	assert.Contains(t, out, "x   ✓ health-summary: Platform is healthy; offline")
	assert.Contains(t, out, "Runtime broker is offline    ✓ health-summary")
	assert.Contains(t, out, "self-health degraded (run time: un available)")
	// The renderer's own table rows still break lines.
	assert.Contains(t, findCheck(t, r.PlatformHealth, "runtime-brokers").Message, "\n    x ")
	// The summary and its raw body are left as the hub sent them.
	assert.Equal(t, "x\n"+forged, s.Brokers.Items[0].Name)
	assert.JSONEq(t, raw, string(r.HealthSummary))
}

func TestSingleLine(t *testing.T) {
	assert.Equal(t, "a b  c d", singleLine("a\nb\r\nc\x1b\x07 d"))
	assert.Equal(t, "plain", singleLine("plain"))
}

func TestDoctorPlatformHealth_ErrorBodyIsOneLine(t *testing.T) {
	f := &fakeDoctorHub{
		summaryStatus: http.StatusBadGateway,
		summaryBody:   "upstream failed\n  ✓ health-summary: Platform is healthy",
		brokersBody:   `{"brokers":[]}`,
		agentsBody:    `{"agents":[]}`,
	}
	srv := f.serve(t)

	var buf bytes.Buffer
	r := &doctorReport{stdout: &buf}
	r.platformHealthSection(r.hubChecks(srv.URL, newDoctorTestClient(t, srv.URL), ""))

	require.Len(t, r.PlatformHealth, 1)
	got := r.PlatformHealth[0]
	assert.Equal(t, "warn", got.Status)
	assert.NotContains(t, got.Message, "\n")
	assert.Contains(t, got.Message, "Could not read the health summary")
	assert.Contains(t, got.Message, "upstream failed   ✓ health-summary")
	for _, line := range strings.Split(buf.String(), "\n") {
		assert.False(t, strings.HasPrefix(line, "  ✓ health-summary"), "line %q", line)
	}
}

func TestRenderDoctorHealthSummary_NoCopyLeavesSummaryUntouched(t *testing.T) {
	orig := cloneHealthSummary
	t.Cleanup(func() { cloneHealthSummary = orig })
	cloneHealthSummary = func(*hubclient.HealthSummary) (*hubclient.HealthSummary, error) {
		return nil, errors.New("no copy")
	}

	s := degradedSummary()
	s.Status = healthSummaryUnhealthy
	s.Brokers.Items[0].Name = "x\ny"
	got := renderDoctorHealthSummary(&s, true)

	assert.Equal(t, "x\ny", s.Brokers.Items[0].Name, "the summary is not modified")
	require.Len(t, got, 2)
	assert.Equal(t, "fail", got[0].Status)
	assert.Contains(t, got[0].Message, "Platform is unhealthy (3 item(s) need attention)")
	assert.Equal(t, "warn", got[1].Status)
	assert.Contains(t, got[1].Message, "--format json")
}

func TestRenderDoctorHealthSummary_BrokerRuntimeMissingParts(t *testing.T) {
	s := degradedSummary()
	s.Brokers.Items[1].Runtime = &hubclient.HealthBrokerRuntime{Profile: "default"}
	brokers := findCheck(t, renderDoctorHealthSummary(&s, true), "runtime-brokers").Message
	assert.Contains(t, brokers, "; runtime -/default;")
	assert.NotContains(t, brokers, "runtime /default")

	s.Brokers.Items[1].Runtime = &hubclient.HealthBrokerRuntime{Type: "docker"}
	brokers = findCheck(t, renderDoctorHealthSummary(&s, true), "runtime-brokers").Message
	assert.Contains(t, brokers, "; runtime docker/-;")
}

func TestRenderDoctorHealthSummary_PassingCheckValues(t *testing.T) {
	s := degradedSummary()
	// A healthy broker reports its runtime as available.
	s.Brokers.Items[1].Health = &hubclient.HealthBrokerSelf{Status: "healthy", Checks: map[string]string{"docker": "available", "disk": "healthy"}}
	s.HubInstances.Items[0].Checks = map[string]string{"database": "healthy", "plugins": "available"}

	got := renderDoctorHealthSummary(&s, true)
	brokers := findCheck(t, got, "runtime-brokers").Message
	assert.Contains(t, brokers, "; self-health healthy; agents running 3")
	assert.NotContains(t, brokers, "docker:")
	assert.NotContains(t, brokers, "available")
	instances := findCheck(t, got, "hub-instances").Message
	assert.NotContains(t, instances, "checks ")

	// A failing value is still listed, at both call sites.
	s.Brokers.Items[1].Health.Checks["docker"] = "unavailable"
	s.HubInstances.Items[0].Checks["plugins"] = "unavailable"
	got = renderDoctorHealthSummary(&s, true)
	brokers = findCheck(t, got, "runtime-brokers").Message
	assert.Contains(t, brokers, "self-health healthy (docker: unavailable)")
	assert.Contains(t, findCheck(t, got, "hub-instances").Message, "checks plugins: unavailable")

	// A broker that is not online shows its last report as such.
	s.Brokers.Items[0].Health = &hubclient.HealthBrokerSelf{Status: "healthy", Checks: map[string]string{"docker": "available"}}
	brokers = findCheck(t, renderDoctorHealthSummary(&s, true), "runtime-brokers").Message
	assert.Contains(t, brokers, "gpu-1; offline; version 1.4.0; last reported self-health healthy;")
	assert.Contains(t, brokers, "east; online;")
	assert.NotContains(t, brokers, "east; online; version 1.4.0; runtime docker/default; storage nfs (unhealthy); last reported")
}

func TestSingleLineHealthSummary_KeepsRawBodyUnchanged(t *testing.T) {
	s := degradedSummary()
	s.Brokers.Items[0].Name = "x\ny"
	// Raw bytes are attached after the string walk and kept verbatim.
	s.Raw = json.RawMessage("{\"status\":\"degraded\",\n\"x\":\"\\u0007\"}")
	want := append(json.RawMessage(nil), s.Raw...)

	got, err := singleLineHealthSummary(&s)
	require.NoError(t, err)
	assert.Equal(t, "x y", got.Brokers.Items[0].Name)
	assert.Equal(t, "x\ny", s.Brokers.Items[0].Name, "the input is not modified")
	assert.Equal(t, want, got.Raw)
	assert.Equal(t, want, s.Raw)
}
