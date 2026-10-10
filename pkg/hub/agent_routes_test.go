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
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// agentRouteCase is one pinned method+path -> sub-route mapping.
type agentRouteCase struct {
	method string
	path   string
	want   AgentSubRoute
}

func agentRouteCases() []agentRouteCase {
	const a = "agent-1"
	const p = "proj-1"
	byID := func(method, suffix string, id AgentSubRouteID, op authzop.OperationID, sfx AgentSubRouteSuffix) agentRouteCase {
		return agentRouteCase{method, "/api/v1/agents/" + a + suffix, AgentSubRoute{RouteID: id, OperationID: op, Method: method, AgentID: a, Suffix: sfx}}
	}
	proj := func(method, suffix string, id AgentSubRouteID, op authzop.OperationID) agentRouteCase {
		return agentRouteCase{method, "/api/v1/projects/" + p + "/agents/" + a + suffix, AgentSubRoute{RouteID: id, OperationID: op, Method: method, AgentID: a, ProjectID: p}}
	}
	none := AgentSubRouteSuffix{}
	post := http.MethodPost
	get := http.MethodGet
	return []agentRouteCase{
		{post, "/api/v1/agents/stop-all", AgentSubRoute{RouteID: AgentRouteStopAll, OperationID: opAgentStopAll, Method: post}},
		byID(get, "", AgentRouteRoot, opAgentRead, none),
		byID(get, "/", AgentRouteRoot, opAgentRead, none),
		byID(http.MethodPatch, "", AgentRouteRoot, opAgentUpdate, none),
		byID(http.MethodPatch, "/", AgentRouteRoot, opAgentUpdate, none),
		byID(http.MethodDelete, "", AgentRouteRoot, opAgentDelete, none),
		byID(http.MethodPut, "", AgentRouteRoot, "", none),
		byID(get, "/pty", AgentRoutePTY, opAgentAttach, none),
		byID(get, "/workspace", AgentRouteWorkspace, "", none),
		byID(post, "/workspace/sync-from", AgentRouteWorkspace, "", AgentSubRouteSuffix{Opaque: "/sync-from"}),
		byID(post, "/workspace/sync-to/finalize", AgentRouteWorkspace, "", AgentSubRouteSuffix{Opaque: "/sync-to/finalize"}),
		byID(get, "/groups", AgentRouteGroups, "", none),
		byID(get, "/ports", AgentRoutePorts, opAgentPortAccess, none),
		byID(get, "/ports/", AgentRoutePorts, opAgentPortAccess, none),
		byID(post, "/ports", AgentRoutePorts, "", none),
		byID(get, "/ports/tunnel", AgentRoutePortsTunnel, "", none),
		byID(http.MethodDelete, "/ports/3000", AgentRoutePortItem, "", AgentSubRouteSuffix{Param: "3000"}),
		byID(get, "/ports/3000/proxy", AgentRoutePortProxy, opAgentPortAccess, AgentSubRouteSuffix{Param: "3000"}),
		byID(http.MethodPut, "/ports/3000/proxy/", AgentRoutePortProxy, opAgentPortAccess, AgentSubRouteSuffix{Param: "3000", Opaque: "/"}),
		byID(get, "/ports/3000/proxy/a/b%2Fc", AgentRoutePortProxy, opAgentPortAccess, AgentSubRouteSuffix{Param: "3000", Opaque: "/a/b%2Fc"}),
		byID(get, "/logs", AgentRouteLogs, "", none),
		byID(http.MethodPatch, "/logs", AgentRouteLogs, "", none),
		byID(get, "/cloud-logs", AgentRouteCloudLogs, "", none),
		byID(get, "/cloud-logs/stream", AgentRouteCloudLogsStream, "", none),
		byID(get, "/message-logs", AgentRouteMessageLogs, "", none),
		byID(get, "/message-logs/stream", AgentRouteMessageLogsStream, "", none),
		byID(get, "/messages", AgentRouteMessages, "", none),
		byID(get, "/messages/stream", AgentRouteMessagesStream, "", none),
		byID(get, "/secrets", AgentRouteSecrets, "", none),
		byID(http.MethodPut, "/secrets/MY_KEY", AgentRouteSecrets, "", AgentSubRouteSuffix{Opaque: "/MY_KEY"}),
		byID(get, "/metrics/summary", AgentRouteMetricsSummary, "", none),
		byID(post, "/hold/lift", AgentRouteHoldLift, opAgentHoldLift, none),
		byID(post, "/status", AgentRouteActionStatus, "", none),
		byID(post, "/start", AgentRouteActionStart, opAgentLifecycleControl, none),
		byID(get, "/start", AgentRouteActionStart, "", none),
		byID(post, "/stop", AgentRouteActionStop, opAgentLifecycleControl, none),
		byID(post, "/suspend", AgentRouteActionSuspend, opAgentLifecycleControl, none),
		byID(post, "/restart", AgentRouteActionRestart, opAgentLifecycleControl, none),
		byID(post, "/message", AgentRouteActionMessage, "", none),
		byID(post, "/exec", AgentRouteActionExec, opAgentExec, none),
		byID(post, "/restore", AgentRouteActionRestore, opAgentLifecycleRestore, none),
		byID(post, "/env", AgentRouteActionEnv, opAgentEnv, none),
		byID(post, "/token/refresh", AgentRouteActionTokenRefresh, opAgentTokenRefresh, none),
		byID(post, "/refresh-token", AgentRouteActionRefreshToken, opAgentTokenRefresh, none),
		byID(post, "/outbound-message", AgentRouteActionOutbound, opAgentOutboundMessage, none),
		byID(post, "/metrics", AgentRouteActionMetrics, opAgentMetricsReport, none),
		byID(post, "/set_message_mode", AgentRouteActionMessageMode, opAgentSetMessageMode, none),
		byID(get, "/set_message_mode", AgentRouteActionMessageMode, "", none),
		byID(http.MethodPut, "/set_message_mode", AgentRouteActionMessageMode, "", none),
		byID(post, "/reincarnate", AgentRouteActionReincarnate, opAgentReincarnate, none),
		byID(post, "/reset-auth", AgentRouteActionResetAuth, opAgentResetAuth, none),
		byID(post, "/keys", AgentRouteActionKeys, "", none),

		{get, "/api/v1/projects/" + p + "/agents", AgentSubRoute{RouteID: ProjectAgentRouteCollection, Method: get, ProjectID: p}},
		{post, "/api/v1/projects/" + p + "/agents/", AgentSubRoute{RouteID: ProjectAgentRouteCollection, Method: post, ProjectID: p}},
		{post, "/api/v1/projects/" + p + "/agents/stop-all", AgentSubRoute{RouteID: ProjectAgentRouteStopAll, Method: post, ProjectID: p}},
		proj(get, "", ProjectAgentRouteRoot, ""),
		proj(http.MethodDelete, "/", ProjectAgentRouteRoot, ""),
		proj(get, "/logs", ProjectAgentRouteLogs, ""),
		proj(get, "/cloud-logs", ProjectAgentRouteCloudLogs, ""),
		proj(get, "/cloud-logs/stream", ProjectAgentRouteCloudLogsStream, ""),
		proj(get, "/message-logs", ProjectAgentRouteMessageLogs, ""),
		proj(get, "/message-logs/stream", ProjectAgentRouteMessageLogsStream, ""),
		proj(post, "/status", ProjectAgentRouteActionStatus, ""),
		proj(post, "/start", ProjectAgentRouteActionStart, opAgentLifecycleControl),
		proj(post, "/stop", ProjectAgentRouteActionStop, opAgentLifecycleControl),
		proj(post, "/suspend", ProjectAgentRouteActionSuspend, opAgentLifecycleControl),
		proj(post, "/restart", ProjectAgentRouteActionRestart, opAgentLifecycleControl),
		proj(post, "/message", ProjectAgentRouteActionMessage, ""),
		proj(post, "/exec", ProjectAgentRouteActionExec, opAgentExec),
		proj(post, "/restore", ProjectAgentRouteActionRestore, opAgentLifecycleRestore),
		proj(post, "/env", ProjectAgentRouteActionEnv, opAgentEnv),
		proj(post, "/outbound-message", ProjectAgentRouteActionOutbound, opAgentOutboundMessage),
		proj(post, "/set_message_mode", ProjectAgentRouteActionMessageMode, opAgentSetMessageMode),
		proj(get, "/set_message_mode", ProjectAgentRouteActionMessageMode, ""),
		proj(http.MethodPut, "/set_message_mode", ProjectAgentRouteActionMessageMode, ""),
		proj(post, "/reincarnate", ProjectAgentRouteActionReincarnate, opAgentReincarnate),
		proj(post, "/reset-auth", ProjectAgentRouteActionResetAuth, opAgentResetAuth),
		proj(post, "/keys", ProjectAgentRouteActionKeys, ""),
	}
}

// TestResolveAgentSubRoute_Table pins every agent sub-route mapping.
func TestResolveAgentSubRoute_Table(t *testing.T) {
	seen := map[AgentSubRouteID]bool{}
	for _, tc := range agentRouteCases() {
		got, ok := ResolveAgentSubRoute(tc.method, tc.path)
		require.True(t, ok, "%s %s must resolve", tc.method, tc.path)
		assert.Equal(t, tc.want, got, "%s %s", tc.method, tc.path)
		seen[got.RouteID] = true
	}
	for _, row := range agentSubRouteTable {
		assert.True(t, seen[row.id], "route %s has no pinned case", row.id)
	}
}

// TestResolveAgentSubRoute_DecodesGrammarSegments pins that agent and
// project segments are unescaped after validation.
func TestResolveAgentSubRoute_DecodesGrammarSegments(t *testing.T) {
	got, ok := ResolveAgentSubRoute(http.MethodGet, "/api/v1/agents/my%20agent/logs")
	require.True(t, ok)
	assert.Equal(t, "my agent", got.AgentID)

	got, ok = ResolveAgentSubRoute(http.MethodPost, "/api/v1/projects/abc__my-proj/agents/my-agent/start")
	require.True(t, ok)
	assert.Equal(t, "abc__my-proj", got.ProjectID)
	assert.Equal(t, "my-agent", got.AgentID)
}

// TestResolveAgentSubRoute_RejectsUnknownAndMalformed pins the fail-closed
// cases: unknown sub-routes and malformed grammar segments do not resolve.
func TestResolveAgentSubRoute_RejectsUnknownAndMalformed(t *testing.T) {
	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/agents/"},
		{http.MethodGet, "/api/v1/agents//logs"},
		{http.MethodGet, "/api/v1/agents/a/attach"},
		{http.MethodPost, "/api/v1/agents/a/has-prompt"},
		{http.MethodPost, "/api/v1/agents/a/start/"},
		{http.MethodPost, "/api/v1/agents/a/start/x"},
		{http.MethodGet, "/api/v1/agents/a/logs/"},
		{http.MethodGet, "/api/v1/agents/a/workspaceX"},
		{http.MethodGet, "/api/v1/agents/a/portsX"},
		{http.MethodGet, "/api/v1/agents/a/ports/3000/other"},
		{http.MethodGet, "/api/v1/agents/a//"},
		{http.MethodGet, "/api/v1/agents/a%2Fb/logs"},
		{http.MethodGet, "/api/v1/agents/a%2fb/logs"},
		{http.MethodGet, "/api/v1/agents/a%5Cb/logs"},
		{http.MethodGet, "/api/v1/agents/a%5cb/logs"},
		{http.MethodGet, "/api/v1/agents/a%252Fb/logs"},
		{http.MethodGet, "/api/v1/agents/a%00b/logs"},
		{http.MethodGet, "/api/v1/agents/a%00/logs"},
		{http.MethodGet, "/api/v1/agents/a%E2%80%8B%00/logs"},
		{http.MethodGet, "/api/v1/projects/p%00/agents/a"},
		{http.MethodGet, "/api/v1/projects/p/agents/a%00b/logs"},
		{http.MethodGet, `/api/v1/agents/a\b/logs`},
		{http.MethodGet, "/api/v1/agents/./logs"},
		{http.MethodGet, "/api/v1/agents/../logs"},
		{http.MethodGet, "/api/v1/agents/%2E%2E/logs"},
		{http.MethodGet, "/api/v1/agents/a%zz/logs"},
		{http.MethodGet, "/api/v1/agents/a/ports/3%2F0/proxy"},
		{http.MethodGet, "/api/v1/agents/a/ports/3%000/proxy"},
		{http.MethodPost, "/api/v1/agents/stop-all/x"},
		{http.MethodGet, "/api/v1/projects/p/agents/a/pty"},
		{http.MethodGet, "/api/v1/projects/p/agents/a/groups"},
		{http.MethodGet, "/api/v1/projects/p/agents/a/messages/stream"},
		{http.MethodPost, "/api/v1/projects/p/agents/a/unknown"},
		{http.MethodGet, "/api/v1/projects/p%2Fq/agents/a"},
		{http.MethodGet, "/api/v1/projects/../agents/a"},
		{http.MethodGet, "/api/v1/projects/p/agents//"},
		{http.MethodGet, "/api/v1/projects/p/members"},
		{http.MethodGet, "/api/v1/projects/p"},
		{http.MethodGet, "/api/v1/agents"},
		{http.MethodGet, "/api/v1/messages/a"},
	}
	for _, tc := range cases {
		_, ok := ResolveAgentSubRoute(tc.method, tc.path)
		assert.False(t, ok, "%s %s must not resolve", tc.method, tc.path)
	}
}

// TestAgentRouteSegment_NULRejectedByEachCheck pins the escaped-form and
// decoded-form null-byte checks independently, so that removing either one
// on its own fails a test.
func TestAgentRouteSegment_NULRejectedByEachCheck(t *testing.T) {
	assert.False(t, agentRouteGrammarSegmentOK("a%00b"), "escaped-form check")
	_, ok := decodeAgentRouteSegment("a\x00b")
	assert.False(t, ok, "decoded-form check")
}

// TestResolveAgentSubRoute_ClientPaths pins that every agent path the CLI
// (pkg/hubclient) and web client build resolves to the expected route.
func TestResolveAgentSubRoute_ClientPaths(t *testing.T) {
	const id = "0f3c2c55-7c55-4f0b-9f36-b0a4e0c1a001"
	const pid = "4b7e1f0e-5f0a-4d53-9d16-3c3a0f4a0002__my-project"
	byID := "/api/v1/agents/" + id
	inProject := "/api/v1/projects/" + pid + "/agents/" + id
	cases := []struct {
		method string
		path   string
		want   AgentSubRouteID
	}{
		{http.MethodGet, byID, AgentRouteRoot},
		{http.MethodPatch, byID, AgentRouteRoot},
		{http.MethodDelete, byID, AgentRouteRoot},
		{http.MethodPost, byID + "/env", AgentRouteActionEnv},
		{http.MethodPost, byID + "/start", AgentRouteActionStart},
		{http.MethodPost, byID + "/stop", AgentRouteActionStop},
		{http.MethodPost, byID + "/suspend", AgentRouteActionSuspend},
		{http.MethodPost, byID + "/restart", AgentRouteActionRestart},
		{http.MethodPost, byID + "/reset-auth", AgentRouteActionResetAuth},
		{http.MethodPost, byID + "/keys", AgentRouteActionKeys},
		{http.MethodPost, byID + "/restore", AgentRouteActionRestore},
		{http.MethodPost, byID + "/message", AgentRouteActionMessage},
		{http.MethodPost, byID + "/outbound-message", AgentRouteActionOutbound},
		{http.MethodPost, byID + "/exec", AgentRouteActionExec},
		{http.MethodGet, byID + "/logs", AgentRouteLogs},
		{http.MethodGet, byID + "/cloud-logs", AgentRouteCloudLogs},
		{http.MethodGet, byID + "/cloud-logs/stream", AgentRouteCloudLogsStream},
		{http.MethodGet, byID + "/message-logs", AgentRouteMessageLogs},
		{http.MethodGet, byID + "/message-logs/stream", AgentRouteMessageLogsStream},
		{http.MethodGet, byID + "/messages", AgentRouteMessages},
		{http.MethodGet, byID + "/messages/stream", AgentRouteMessagesStream},
		{http.MethodPost, byID + "/set_message_mode", AgentRouteActionMessageMode},
		{http.MethodPost, byID + "/reincarnate", AgentRouteActionReincarnate},
		{http.MethodGet, byID + "/pty", AgentRoutePTY},
		{http.MethodGet, byID + "/workspace", AgentRouteWorkspace},
		{http.MethodPost, byID + "/workspace/sync-from", AgentRouteWorkspace},
		{http.MethodPost, byID + "/workspace/sync-to", AgentRouteWorkspace},
		{http.MethodPost, byID + "/workspace/sync-to/finalize", AgentRouteWorkspace},
		{http.MethodGet, byID + "/ports", AgentRoutePorts},
		{http.MethodPost, byID + "/ports", AgentRoutePorts},
		{http.MethodGet, byID + "/ports/tunnel", AgentRoutePortsTunnel},
		{http.MethodGet, byID + "/ports/3000/proxy/", AgentRoutePortProxy},
		{http.MethodGet, byID + "/ports/3000/proxy/static/app.js", AgentRoutePortProxy},
		{http.MethodGet, byID + "/secrets", AgentRouteSecrets},
		{http.MethodGet, byID + "/secrets/MY_KEY", AgentRouteSecrets},
		{http.MethodPost, byID + "/status", AgentRouteActionStatus},
		{http.MethodPost, byID + "/token/refresh", AgentRouteActionTokenRefresh},
		{http.MethodPost, byID + "/refresh-token", AgentRouteActionRefreshToken},
		{http.MethodPost, byID + "/metrics", AgentRouteActionMetrics},
		{http.MethodGet, byID + "/metrics/summary", AgentRouteMetricsSummary},
		{http.MethodGet, byID + "/groups", AgentRouteGroups},
		{http.MethodPost, "/api/v1/agents/stop-all", AgentRouteStopAll},
		{http.MethodGet, "/api/v1/projects/" + pid + "/agents", ProjectAgentRouteCollection},
		{http.MethodPost, "/api/v1/projects/" + pid + "/agents", ProjectAgentRouteCollection},
		{http.MethodPost, "/api/v1/projects/" + pid + "/agents/stop-all", ProjectAgentRouteStopAll},
		{http.MethodGet, inProject, ProjectAgentRouteRoot},
		{http.MethodPatch, inProject, ProjectAgentRouteRoot},
		{http.MethodDelete, inProject, ProjectAgentRouteRoot},
		{http.MethodPost, inProject + "/env", ProjectAgentRouteActionEnv},
		{http.MethodPost, inProject + "/start", ProjectAgentRouteActionStart},
		{http.MethodPost, inProject + "/stop", ProjectAgentRouteActionStop},
		{http.MethodPost, inProject + "/suspend", ProjectAgentRouteActionSuspend},
		{http.MethodPost, inProject + "/restart", ProjectAgentRouteActionRestart},
		{http.MethodPost, inProject + "/reset-auth", ProjectAgentRouteActionResetAuth},
		{http.MethodPost, inProject + "/keys", ProjectAgentRouteActionKeys},
		{http.MethodPost, inProject + "/restore", ProjectAgentRouteActionRestore},
		{http.MethodPost, inProject + "/message", ProjectAgentRouteActionMessage},
		{http.MethodPost, inProject + "/outbound-message", ProjectAgentRouteActionOutbound},
		{http.MethodPost, inProject + "/exec", ProjectAgentRouteActionExec},
		{http.MethodGet, inProject + "/logs", ProjectAgentRouteLogs},
		{http.MethodGet, inProject + "/cloud-logs", ProjectAgentRouteCloudLogs},
		{http.MethodGet, inProject + "/cloud-logs/stream", ProjectAgentRouteCloudLogsStream},
		{http.MethodPost, inProject + "/set_message_mode", ProjectAgentRouteActionMessageMode},
		{http.MethodPost, inProject + "/reincarnate", ProjectAgentRouteActionReincarnate},
	}
	for _, tc := range cases {
		got, ok := ResolveAgentSubRoute(tc.method, tc.path)
		require.True(t, ok, "%s %s must resolve", tc.method, tc.path)
		assert.Equal(t, tc.want, got.RouteID, "%s %s", tc.method, tc.path)
	}
}

// agentRouteSamplePath renders a concrete request path for row.
func agentRouteSamplePath(row agentSubRouteRow, withOpaque bool) string {
	var b strings.Builder
	if row.form == agentFormProject {
		b.WriteString("/api/v1/projects/proj-1/agents")
	} else {
		b.WriteString("/api/v1/agents")
	}
	if !row.noAgent {
		b.WriteString("/agent-1")
	}
	for _, seg := range row.segs {
		b.WriteByte('/')
		if seg == agentRouteParam {
			b.WriteString("3000")
			continue
		}
		b.WriteString(seg)
	}
	if withOpaque && row.opaque {
		b.WriteString("/sub/path")
	}
	return b.String()
}

func agentRouteEntryKind(row agentSubRouteRow) authzop.EntryPointKind {
	if row.kind == "" {
		return authzop.EntryPointHTTPRoute
	}
	return row.kind
}

// TestAgentSubRoute_CatalogDrift derives the discovered entry points of
// every catalog operation an agent sub-route declares from the resolver
// table itself (each sample is resolved, and the resolved OperationID is
// what files the entry point) and requires CheckDrift to report nothing.
func TestAgentSubRoute_CatalogDrift(t *testing.T) {
	discovered := map[authzop.OperationID][]authzop.DiscoveredEntryPoint{}
	add := func(row agentSubRouteRow, method string, withOpaque bool) {
		route, ok := ResolveAgentSubRoute(method, agentRouteSamplePath(row, withOpaque))
		require.True(t, ok, "sample for %s must resolve", row.id)
		require.Equal(t, row.id, route.RouteID)
		if route.OperationID == "" {
			return
		}
		discovered[route.OperationID] = append(discovered[route.OperationID], authzop.DiscoveredEntryPoint{
			Kind:    agentRouteEntryKind(row),
			Pattern: row.catalogPattern(withOpaque),
			Method:  method,
		})
	}
	for _, row := range agentSubRouteTable {
		methods := make([]string, 0, len(row.ops))
		for m := range row.ops {
			methods = append(methods, m)
		}
		sort.Strings(methods)
		for _, m := range methods {
			add(row, m, false)
		}
		if row.allMethodsOp != "" {
			for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
				if _, listed := row.ops[m]; !listed {
					add(row, m, false)
				}
			}
			if row.opaque {
				add(row, http.MethodGet, true)
			}
		}
	}
	require.NotEmpty(t, discovered)
	for _, op := range []authzop.OperationID{
		opAgentRead, opAgentUpdate, opAgentDelete, opAgentAttach, opAgentPortAccess, opAgentStopAll,
		opAgentLifecycleControl, opAgentLifecycleRestore, opAgentExec, opAgentEnv,
		opAgentResetAuth, opAgentReincarnate, opAgentSetMessageMode,
		opAgentTokenRefresh, opAgentOutboundMessage, opAgentMetricsReport,
	} {
		assert.NotEmpty(t, discovered[op], "operation %s has no resolver entry point", op)
	}
	assert.Empty(t, authzop.CheckDrift(discovered))
}

// TestAgentSubRoute_MuxDispatch sends one request per pinned route through
// the real mux, routeGuard and dispatcher. Every resolved route reaches a
// handler: the response is never the resolver's 404 or the missing-route
// 500. Unknown agent sub-routes get the resolver's 404.
func TestAgentSubRoute_MuxDispatch(t *testing.T) {
	srv, _ := testServer(t)
	for _, tc := range agentRouteCases() {
		rec := doRequest(t, srv, tc.method, tc.path, nil)
		body := rec.Body.String()
		assert.NotContains(t, body, "Agent route not found", "%s %s", tc.method, tc.path)
		assert.NotContains(t, body, "agent route not resolved", "%s %s", tc.method, tc.path)
		assert.NotEqual(t, http.StatusInternalServerError, rec.Code, "%s %s: %s", tc.method, tc.path, body)
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/agents/agent-1/attach"},
		{http.MethodPost, "/api/v1/agents/agent-1/unknown"},
		{http.MethodGet, "/api/v1/agents/a%2Fb/logs"},
		{http.MethodPost, "/api/v1/projects/proj-1/agents/agent-1/unknown"},
	} {
		rec := doRequest(t, srv, tc.method, tc.path, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s %s", tc.method, tc.path)
		assert.Contains(t, rec.Body.String(), "Agent route not found", "%s %s", tc.method, tc.path)
	}
}

// TestAgentSubRoute_HandlerWithoutRouteFailsClosed pins that a dispatcher
// reached without a resolved route answers 500 instead of parsing the path.
func TestAgentSubRoute_HandlerWithoutRouteFailsClosed(t *testing.T) {
	srv, _ := testServer(t)
	rec := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodGet, "/api/v1/agents/agent-1", nil)
	require.NoError(t, err)
	srv.handleAgentByID(rec, req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// TestAgentRouteRoot_MethodNotAllowed_SetsAllowHeader pins that a 405 from
// the agent root and the project-agent root (an unsupported method such as
// PUT on /api/v1/agents/{id} or /api/v1/projects/{p}/agents/{id}) carries
// Allow: GET, PATCH, DELETE.
func TestAgentRouteRoot_MethodNotAllowed_SetsAllowHeader(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("agent_route_root_allow_header_project")
	createTestProject(t, s, projectID)

	for _, tc := range []struct {
		name string
		path string
	}{
		{"agent root", "/api/v1/agents/agent-1"},
		{"project-agent root", "/api/v1/projects/" + projectID + "/agents/agent-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPut, tc.path, nil)
			assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, "body: %s", rec.Body.String())
			assert.Equal(t, "GET, PATCH, DELETE", rec.Header().Get("Allow"))
		})
	}
}
