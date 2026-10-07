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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// testServerNoCloudLogs creates a test server and explicitly disables the
// logQueryService so cloud-logs endpoints return 501.
func testServerNoCloudLogs(t *testing.T) (*Server, store.Store) {
	t.Helper()
	srv, s := testServer(t)
	srv.logQueryService = nil
	return srv, s
}

func createTestAgent(t *testing.T, s store.Store) *store.Agent {
	t.Helper()
	ctx := context.Background()
	project := &store.Project{
		ID:   api.NewUUID(),
		Name: "test-project-" + api.NewUUID()[:8],
		Slug: "test-project-" + api.NewUUID()[:8],
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	agent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "test-agent-" + api.NewUUID()[:8],
		Slug:      "test-agent-" + api.NewUUID()[:8],
		ProjectID: project.ID,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	return agent
}

func TestHandleAgentCloudLogs_NotConfigured(t *testing.T) {
	srv, s := testServerNoCloudLogs(t)
	agent := createTestAgent(t, s)

	req := httptest.NewRequest("GET", "/api/v1/agents/"+agent.ID+"/cloud-logs", nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotImplemented)
	}

	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Error.Message != "Cloud Logging is not configured" {
		t.Errorf("message = %q, want %q", resp.Error.Message, "Cloud Logging is not configured")
	}
	if resp.Error.Code != "not_implemented" {
		t.Errorf("code = %q, want %q", resp.Error.Code, "not_implemented")
	}
}

func TestHandleAgentCloudLogsStream_NotConfigured(t *testing.T) {
	srv, s := testServerNoCloudLogs(t)
	agent := createTestAgent(t, s)

	req := httptest.NewRequest("GET", "/api/v1/agents/"+agent.ID+"/cloud-logs/stream", nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotImplemented)
	}
}

func TestHandleAgentCloudLogs_MethodNotAllowed(t *testing.T) {
	srv, s := testServerNoCloudLogs(t)
	agent := createTestAgent(t, s)

	// POST should not be allowed for cloud-logs
	req := httptest.NewRequest("POST", "/api/v1/agents/"+agent.ID+"/cloud-logs", nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleAgentCloudLogs_AgentNotFound(t *testing.T) {
	srv, _ := testServerNoCloudLogs(t)

	req := httptest.NewRequest("GET", "/api/v1/agents/nonexistent/cloud-logs", nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Agent lookup runs before the logQueryService nil check (PR #1393),
	// so a non-existent agent gets 404 without revealing whether Cloud Logging
	// is configured. This is the security improvement from PR #1393.
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleAgentCloudLogs_QueryParameterParsing(t *testing.T) {
	srv, s := testServerNoCloudLogs(t)
	agent := createTestAgent(t, s)

	// When logQueryService is nil, all valid requests return 501 (auth + params parsed first)
	since := time.Now().Add(-1 * time.Hour).Format(time.RFC3339Nano)
	until := time.Now().Format(time.RFC3339Nano)
	req := httptest.NewRequest("GET",
		"/api/v1/agents/"+agent.ID+"/cloud-logs?tail=50&since="+since+"&until="+until+"&severity=ERROR",
		nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Should get 501 since logQueryService is nil
	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotImplemented)
	}
}

// ---------------------------------------------------------------------------
// Message-logs endpoint tests
// ---------------------------------------------------------------------------

func TestHandleAgentMessageLogs_NotConfigured(t *testing.T) {
	srv, s := testServerNoCloudLogs(t)
	agent := createTestAgent(t, s)

	req := httptest.NewRequest("GET", "/api/v1/agents/"+agent.ID+"/message-logs", nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotImplemented)
	}

	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Error.Message != "Cloud Logging is not configured" {
		t.Errorf("message = %q, want %q", resp.Error.Message, "Cloud Logging is not configured")
	}
}

func TestHandleAgentMessageLogsStream_NotConfigured(t *testing.T) {
	srv, s := testServerNoCloudLogs(t)
	agent := createTestAgent(t, s)

	req := httptest.NewRequest("GET", "/api/v1/agents/"+agent.ID+"/message-logs/stream", nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotImplemented)
	}
}

func TestHandleAgentMessageLogs_MethodNotAllowed(t *testing.T) {
	srv, s := testServerNoCloudLogs(t)
	agent := createTestAgent(t, s)

	req := httptest.NewRequest("POST", "/api/v1/agents/"+agent.ID+"/message-logs", nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleAgentCloudLogs_Unauthenticated(t *testing.T) {
	srv, s := testServerNoCloudLogs(t)
	agent := createTestAgent(t, s)

	req := httptest.NewRequest("GET", "/api/v1/agents/"+agent.ID+"/cloud-logs", nil)
	// No auth header
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Should get 404 or 401 - the auth middleware should prevent access
	if w.Code == http.StatusOK || w.Code == http.StatusNotImplemented {
		t.Errorf("status = %d, expected auth error", w.Code)
	}
}

// TestAgentCloudLogOptions_QueryAndStreamAgree checks that the
// non-streaming and streaming agent cloud-logs endpoints build the same
// Cloud Logging filter for the same agent, and that neither narrows on
// labels.project_id (many server entries carry agent_id without it).
func TestAgentCloudLogOptions_QueryAndStreamAgree(t *testing.T) {
	tests := []struct {
		name  string
		agent *store.Agent
		query url.Values
	}{
		{
			name:  "agent with project",
			agent: &store.Agent{ID: "8f0c2a4e-agent", ProjectID: "proj-123"},
			query: url.Values{},
		},
		{
			name:  "with severity and broker filters",
			agent: &store.Agent{ID: "8f0c2a4e-agent", ProjectID: "proj-123"},
			query: url.Values{"severity": {"ERROR"}, "broker_id": {"broker-1"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			list := agentCloudLogListOptions("hub-a", tt.agent, tt.query)
			stream := agentCloudLogQueryOptions("hub-a", tt.agent, tt.query)

			listFilter := BuildLogFilter(list, "gcp-proj")
			streamFilter := BuildLogFilter(stream, "gcp-proj")

			if listFilter != streamFilter {
				t.Errorf("query and stream filters differ:\n query:  %s\n stream: %s", listFilter, streamFilter)
			}
			if strings.Contains(listFilter, "labels.project_id") {
				t.Errorf("query filter must not narrow on project_id: %s", listFilter)
			}
			if !strings.Contains(listFilter, `labels.agent_id = "8f0c2a4e-agent"`) {
				t.Errorf("query filter missing agent_id clause: %s", listFilter)
			}
		})
	}
}

// TestAgentCloudLogListOptions_ParsesRange checks paging and time-range
// parsing for the non-streaming endpoint.
func TestAgentCloudLogListOptions_ParsesRange(t *testing.T) {
	agent := &store.Agent{ID: "a1", ProjectID: "p1"}
	q := url.Values{
		"tail":  {"25"},
		"since": {"2026-01-02T03:04:05Z"},
		"until": {"2026-01-03T03:04:05Z"},
	}
	opts := agentCloudLogListOptions("hub-a", agent, q)
	if opts.Tail != 25 {
		t.Errorf("Tail = %d, want 25", opts.Tail)
	}
	if got := opts.Since.UTC().Format(time.RFC3339); got != "2026-01-02T03:04:05Z" {
		t.Errorf("Since = %s", got)
	}
	if got := opts.Until.UTC().Format(time.RFC3339); got != "2026-01-03T03:04:05Z" {
		t.Errorf("Until = %s", got)
	}
	if opts.HubName != "hub-a" || opts.AgentID != "a1" {
		t.Errorf("unexpected base options: %+v", opts)
	}
}

// fakeLogQuerier records the options the handlers pass to the log
// query service.
type fakeLogQuerier struct {
	queryOpts []LogQueryOptions
	tailOpts  []LogQueryOptions
}

func (f *fakeLogQuerier) Query(_ context.Context, opts LogQueryOptions) (*LogQueryResult, error) {
	f.queryOpts = append(f.queryOpts, opts)
	return &LogQueryResult{}, nil
}

func (f *fakeLogQuerier) Tail(_ context.Context, opts LogQueryOptions) (<-chan CloudLogEntry, func(), error) {
	f.tailOpts = append(f.tailOpts, opts)
	ch := make(chan CloudLogEntry)
	close(ch) // end the stream at once
	return ch, func() {}, nil
}

func (f *fakeLogQuerier) GCPProjectID() string { return "gcp-proj" }

func (f *fakeLogQuerier) Close() error { return nil }

// TestHandleAgentCloudLogs_HandlersBuildSameFilter checks, through the
// HTTP handlers, that the list and stream endpoints pass the same filter
// to the log query service, with no project_id clause, and that the list
// endpoint also keeps its paging and time-range options.
func TestHandleAgentCloudLogs_HandlersBuildSameFilter(t *testing.T) {
	srv, s := testServer(t)
	fake := &fakeLogQuerier{}
	srv.logQueryService = fake
	agent := createTestAgent(t, s)

	params := url.Values{
		"severity":  {"ERROR"},
		"broker_id": {"broker-1"},
		"tail":      {"25"},
		"since":     {"2026-01-02T03:04:05Z"},
		"until":     {"2026-01-03T03:04:05Z"},
	}.Encode()
	for _, path := range []string{
		"/api/v1/agents/" + agent.ID + "/cloud-logs?" + params,
		"/api/v1/agents/" + agent.ID + "/cloud-logs/stream?" + params,
	} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: status = %d, body = %s", path, w.Code, w.Body.String())
		}
	}

	if len(fake.queryOpts) != 1 || len(fake.tailOpts) != 1 {
		t.Fatalf("calls: Query = %d, Tail = %d, want 1 each", len(fake.queryOpts), len(fake.tailOpts))
	}
	list, stream := fake.queryOpts[0], fake.tailOpts[0]
	if list.Tail != 25 || list.Since.IsZero() || list.Until.IsZero() {
		t.Errorf("list options lost paging or time range: %+v", list)
	}

	// The time range adds timestamp clauses that the stream has no use
	// for; compare the remaining filter.
	list.Tail, list.Since, list.Until = 0, time.Time{}, time.Time{}
	listFilter := BuildLogFilter(list, "gcp-proj")
	streamFilter := BuildLogFilter(stream, "gcp-proj")
	if listFilter != streamFilter {
		t.Errorf("list and stream filters differ:\n list:   %s\n stream: %s", listFilter, streamFilter)
	}
	for name, f := range map[string]string{"list": listFilter, "stream": streamFilter} {
		if strings.Contains(f, "labels.project_id") {
			t.Errorf("%s filter must not narrow on project_id: %s", name, f)
		}
		if !strings.Contains(f, `labels.agent_id = "`+agent.ID+`"`) {
			t.Errorf("%s filter missing agent_id clause: %s", name, f)
		}
	}
}
