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
	"net/http"
	"net/url"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
)

// AgentSubRouteID identifies one agent sub-route shape. Each ID belongs to
// exactly one path form (the by-id form under /api/v1/agents/ or the project
// alias form under /api/v1/projects/{projectId}/agents).
type AgentSubRouteID string

// Agent sub-routes under /api/v1/agents/.
const (
	AgentRouteStopAll            AgentSubRouteID = "agents.stopAll"
	AgentRouteRoot               AgentSubRouteID = "agents.byId"
	AgentRoutePTY                AgentSubRouteID = "agents.pty"
	AgentRouteWorkspace          AgentSubRouteID = "agents.workspace"
	AgentRouteGroups             AgentSubRouteID = "agents.groups"
	AgentRoutePorts              AgentSubRouteID = "agents.ports"
	AgentRoutePortsTunnel        AgentSubRouteID = "agents.ports.tunnel"
	AgentRoutePortItem           AgentSubRouteID = "agents.ports.item"
	AgentRoutePortProxy          AgentSubRouteID = "agents.ports.proxy"
	AgentRouteLogs               AgentSubRouteID = "agents.logs"
	AgentRouteCloudLogs          AgentSubRouteID = "agents.cloudLogs"
	AgentRouteCloudLogsStream    AgentSubRouteID = "agents.cloudLogs.stream"
	AgentRouteMessageLogs        AgentSubRouteID = "agents.messageLogs"
	AgentRouteMessageLogsStream  AgentSubRouteID = "agents.messageLogs.stream"
	AgentRouteMessages           AgentSubRouteID = "agents.messages"
	AgentRouteMessagesStream     AgentSubRouteID = "agents.messages.stream"
	AgentRouteSecrets            AgentSubRouteID = "agents.secrets"
	AgentRouteMetricsSummary     AgentSubRouteID = "agents.metrics.summary"
	AgentRouteActionStatus       AgentSubRouteID = "agents.action.status"
	AgentRouteActionStart        AgentSubRouteID = "agents.action.start"
	AgentRouteActionStop         AgentSubRouteID = "agents.action.stop"
	AgentRouteActionSuspend      AgentSubRouteID = "agents.action.suspend"
	AgentRouteActionRestart      AgentSubRouteID = "agents.action.restart"
	AgentRouteActionMessage      AgentSubRouteID = "agents.action.message"
	AgentRouteActionExec         AgentSubRouteID = "agents.action.exec"
	AgentRouteActionRestore      AgentSubRouteID = "agents.action.restore"
	AgentRouteActionEnv          AgentSubRouteID = "agents.action.env"
	AgentRouteActionTokenRefresh AgentSubRouteID = "agents.action.tokenRefresh"
	AgentRouteActionRefreshToken AgentSubRouteID = "agents.action.refreshToken"
	AgentRouteActionOutbound     AgentSubRouteID = "agents.action.outboundMessage"
	AgentRouteActionMetrics      AgentSubRouteID = "agents.action.metrics"
	AgentRouteActionMessageMode  AgentSubRouteID = "agents.action.setMessageMode"
	AgentRouteActionReincarnate  AgentSubRouteID = "agents.action.reincarnate"
	AgentRouteActionResetAuth    AgentSubRouteID = "agents.action.resetAuth"
	AgentRouteActionKeys         AgentSubRouteID = "agents.action.keys"
)

// Agent sub-routes under /api/v1/projects/{projectId}/agents.
const (
	ProjectAgentRouteCollection        AgentSubRouteID = "projects.agents"
	ProjectAgentRouteStopAll           AgentSubRouteID = "projects.agents.stopAll"
	ProjectAgentRouteRoot              AgentSubRouteID = "projects.agents.byId"
	ProjectAgentRouteLogs              AgentSubRouteID = "projects.agents.logs"
	ProjectAgentRouteCloudLogs         AgentSubRouteID = "projects.agents.cloudLogs"
	ProjectAgentRouteCloudLogsStream   AgentSubRouteID = "projects.agents.cloudLogs.stream"
	ProjectAgentRouteMessageLogs       AgentSubRouteID = "projects.agents.messageLogs"
	ProjectAgentRouteMessageLogsStream AgentSubRouteID = "projects.agents.messageLogs.stream"
	ProjectAgentRouteActionStatus      AgentSubRouteID = "projects.agents.action.status"
	ProjectAgentRouteActionStart       AgentSubRouteID = "projects.agents.action.start"
	ProjectAgentRouteActionStop        AgentSubRouteID = "projects.agents.action.stop"
	ProjectAgentRouteActionSuspend     AgentSubRouteID = "projects.agents.action.suspend"
	ProjectAgentRouteActionRestart     AgentSubRouteID = "projects.agents.action.restart"
	ProjectAgentRouteActionMessage     AgentSubRouteID = "projects.agents.action.message"
	ProjectAgentRouteActionExec        AgentSubRouteID = "projects.agents.action.exec"
	ProjectAgentRouteActionRestore     AgentSubRouteID = "projects.agents.action.restore"
	ProjectAgentRouteActionEnv         AgentSubRouteID = "projects.agents.action.env"
	ProjectAgentRouteActionOutbound    AgentSubRouteID = "projects.agents.action.outboundMessage"
	ProjectAgentRouteActionMessageMode AgentSubRouteID = "projects.agents.action.setMessageMode"
	ProjectAgentRouteActionReincarnate AgentSubRouteID = "projects.agents.action.reincarnate"
	ProjectAgentRouteActionResetAuth   AgentSubRouteID = "projects.agents.action.resetAuth"
	ProjectAgentRouteActionKeys        AgentSubRouteID = "projects.agents.action.keys"
)

// AgentSubRouteSuffix is the typed tail of an agent sub-route.
type AgentSubRouteSuffix struct {
	// Param is the decoded value of the route's single parameter segment
	// (the port number on the port item and proxy routes), or "".
	Param string
	// Opaque is the raw (still escaped) payload after the route's literal
	// segments on routes that carry one (port proxy path, secret key,
	// workspace action). It keeps its leading "/" when present, so "" and
	// "/" are distinct.
	Opaque string
}

// AgentSubRoute is the immutable classification of one request against the
// agent sub-route table. It carries no authorization result.
type AgentSubRoute struct {
	RouteID AgentSubRouteID
	// OperationID is the authzop catalog operation for this RouteID and
	// Method, or "" when the catalog declares none.
	OperationID authzop.OperationID
	Method      string
	// AgentID is the decoded agent path segment: an agent ID on the by-id
	// form, an agent ID or slug on the project form, "" on collection rows.
	AgentID string
	// ProjectID is the decoded project path segment on the project form
	// (possibly in {uuid}__{slug} form), "" on the by-id form.
	ProjectID string
	Suffix    AgentSubRouteSuffix
}

type agentRouteForm int

const (
	agentFormByID agentRouteForm = iota
	agentFormProject
)

// agentRouteParam marks the single parameter segment of a row's literal path.
const agentRouteParam = "{param}"

type agentSubRouteRow struct {
	id   AgentSubRouteID
	form agentRouteForm
	// noAgent rows have no agent segment (collection and stop-all rows).
	noAgent bool
	// segs are the literal segments after the agent segment (or after the
	// collection prefix on noAgent rows). agentRouteParam marks a parameter.
	segs []string
	// opaque rows accept any remaining raw payload after segs.
	opaque bool
	// trailingSlash rows accept one empty trailing segment.
	trailingSlash bool
	// ops maps HTTP method to catalog operation; allMethodsOp applies to
	// every method not listed in ops. Every method resolves; handlers keep
	// their own method checks and 405 responses.
	ops          map[string]authzop.OperationID
	allMethodsOp authzop.OperationID
	// kind is the catalog entry point kind for this row.
	kind authzop.EntryPointKind
}

func (row agentSubRouteRow) operation(method string) authzop.OperationID {
	if op, ok := row.ops[method]; ok {
		return op
	}
	return row.allMethodsOp
}

// Catalog operation IDs used by agent sub-routes.
const (
	opAgentRead             authzop.OperationID = "agent.read"
	opAgentUpdate           authzop.OperationID = "agent.update"
	opAgentDelete           authzop.OperationID = "agent.lifecycle.delete"
	opAgentAttach           authzop.OperationID = "agent.attach"
	opAgentPortAccess       authzop.OperationID = "agent.portaccess"
	opAgentStopAll          authzop.OperationID = "agent.stopall"
	opAgentLifecycleControl authzop.OperationID = "agent.lifecycle.control"
	opAgentLifecycleRestore authzop.OperationID = "agent.lifecycle.restore"
	opAgentExec             authzop.OperationID = "agent.lifecycle.exec"
	opAgentEnv              authzop.OperationID = "agent.lifecycle.env"
	opAgentResetAuth        authzop.OperationID = "agent.lifecycle.resetauth"
	opAgentReincarnate      authzop.OperationID = "agent.lifecycle.reincarnate"
	opAgentSetMessageMode   authzop.OperationID = "agent.setmessagemode"
	opAgentTokenRefresh     authzop.OperationID = "agent.token.refresh"
	opAgentOutboundMessage  authzop.OperationID = "agent.outbound.message"
	opAgentMetricsReport    authzop.OperationID = "agent.metrics.report"
)

func postOp(op authzop.OperationID) map[string]authzop.OperationID {
	return map[string]authzop.OperationID{http.MethodPost: op}
}

// agentSubRouteTable lists every agent sub-route. Rows are matched in order;
// the first match wins. G and D append rows here (see B/plan/g-interfaces.md).
// Every row names a catalog operation through ops or allMethodsOp, or its
// pattern is a catalog entry point, or its pattern is listed in
// authzop.PendingBearerDispositions
// (TestBearerDisposition_AgentSubRoutesNameAnOperation).
var agentSubRouteTable = []agentSubRouteRow{
	// --- /api/v1/agents/ ---
	{id: AgentRouteStopAll, form: agentFormByID, noAgent: true, segs: []string{"stop-all"}, ops: postOp(opAgentStopAll)},
	{id: AgentRouteRoot, form: agentFormByID, trailingSlash: true, ops: map[string]authzop.OperationID{
		http.MethodGet:    opAgentRead,
		http.MethodDelete: opAgentDelete,
		http.MethodPatch:  opAgentUpdate,
	}},
	{id: AgentRoutePTY, form: agentFormByID, segs: []string{"pty"}, ops: map[string]authzop.OperationID{http.MethodGet: opAgentAttach}, kind: authzop.EntryPointWebSocket},
	{id: AgentRouteWorkspace, form: agentFormByID, segs: []string{"workspace"}, opaque: true},
	{id: AgentRouteGroups, form: agentFormByID, segs: []string{"groups"}},
	{id: AgentRoutePorts, form: agentFormByID, segs: []string{"ports"}, trailingSlash: true, ops: map[string]authzop.OperationID{http.MethodGet: opAgentPortAccess}},
	{id: AgentRoutePortsTunnel, form: agentFormByID, segs: []string{"ports", "tunnel"}},
	{id: AgentRoutePortProxy, form: agentFormByID, segs: []string{"ports", agentRouteParam, "proxy"}, opaque: true, allMethodsOp: opAgentPortAccess},
	{id: AgentRoutePortItem, form: agentFormByID, segs: []string{"ports", agentRouteParam}},
	{id: AgentRouteLogs, form: agentFormByID, segs: []string{"logs"}},
	{id: AgentRouteCloudLogs, form: agentFormByID, segs: []string{"cloud-logs"}},
	{id: AgentRouteCloudLogsStream, form: agentFormByID, segs: []string{"cloud-logs", "stream"}},
	{id: AgentRouteMessageLogs, form: agentFormByID, segs: []string{"message-logs"}},
	{id: AgentRouteMessageLogsStream, form: agentFormByID, segs: []string{"message-logs", "stream"}},
	{id: AgentRouteMessages, form: agentFormByID, segs: []string{"messages"}},
	{id: AgentRouteMessagesStream, form: agentFormByID, segs: []string{"messages", "stream"}},
	{id: AgentRouteSecrets, form: agentFormByID, segs: []string{"secrets"}, opaque: true},
	{id: AgentRouteMetricsSummary, form: agentFormByID, segs: []string{"metrics", "summary"}},
	{id: AgentRouteActionStatus, form: agentFormByID, segs: []string{"status"}},
	{id: AgentRouteActionStart, form: agentFormByID, segs: []string{"start"}, ops: postOp(opAgentLifecycleControl)},
	{id: AgentRouteActionStop, form: agentFormByID, segs: []string{"stop"}, ops: postOp(opAgentLifecycleControl)},
	{id: AgentRouteActionSuspend, form: agentFormByID, segs: []string{"suspend"}, ops: postOp(opAgentLifecycleControl)},
	{id: AgentRouteActionRestart, form: agentFormByID, segs: []string{"restart"}, ops: postOp(opAgentLifecycleControl)},
	{id: AgentRouteActionMessage, form: agentFormByID, segs: []string{"message"}},
	{id: AgentRouteActionExec, form: agentFormByID, segs: []string{"exec"}, ops: postOp(opAgentExec)},
	{id: AgentRouteActionRestore, form: agentFormByID, segs: []string{"restore"}, ops: postOp(opAgentLifecycleRestore)},
	{id: AgentRouteActionEnv, form: agentFormByID, segs: []string{"env"}, ops: postOp(opAgentEnv)},
	{id: AgentRouteActionTokenRefresh, form: agentFormByID, segs: []string{"token", "refresh"}, ops: postOp(opAgentTokenRefresh)},
	{id: AgentRouteActionRefreshToken, form: agentFormByID, segs: []string{"refresh-token"}, ops: postOp(opAgentTokenRefresh)},
	{id: AgentRouteActionOutbound, form: agentFormByID, segs: []string{"outbound-message"}, ops: postOp(opAgentOutboundMessage)},
	{id: AgentRouteActionMetrics, form: agentFormByID, segs: []string{"metrics"}, ops: postOp(opAgentMetricsReport)},
	{id: AgentRouteActionMessageMode, form: agentFormByID, segs: []string{"set_message_mode"}, ops: postOp(opAgentSetMessageMode)},
	{id: AgentRouteActionReincarnate, form: agentFormByID, segs: []string{"reincarnate"}, ops: postOp(opAgentReincarnate)},
	{id: AgentRouteActionResetAuth, form: agentFormByID, segs: []string{"reset-auth"}, ops: postOp(opAgentResetAuth)},
	// AgentRouteActionKeys has no ops entry, like message above: the keys
	// action is routed and authorized by its own dedicated branch
	// (authorizeAgentKeys in handleAgentAction). The keys action has no
	// catalog operation.
	{id: AgentRouteActionKeys, form: agentFormByID, segs: []string{"keys"}},

	// --- /api/v1/projects/{projectId}/agents ---
	{id: ProjectAgentRouteCollection, form: agentFormProject, noAgent: true, trailingSlash: true},
	{id: ProjectAgentRouteStopAll, form: agentFormProject, noAgent: true, segs: []string{"stop-all"}},
	{id: ProjectAgentRouteRoot, form: agentFormProject, trailingSlash: true},
	{id: ProjectAgentRouteLogs, form: agentFormProject, segs: []string{"logs"}},
	{id: ProjectAgentRouteCloudLogs, form: agentFormProject, segs: []string{"cloud-logs"}},
	{id: ProjectAgentRouteCloudLogsStream, form: agentFormProject, segs: []string{"cloud-logs", "stream"}},
	{id: ProjectAgentRouteMessageLogs, form: agentFormProject, segs: []string{"message-logs"}},
	{id: ProjectAgentRouteMessageLogsStream, form: agentFormProject, segs: []string{"message-logs", "stream"}},
	{id: ProjectAgentRouteActionStatus, form: agentFormProject, segs: []string{"status"}},
	{id: ProjectAgentRouteActionStart, form: agentFormProject, segs: []string{"start"}, ops: postOp(opAgentLifecycleControl)},
	{id: ProjectAgentRouteActionStop, form: agentFormProject, segs: []string{"stop"}, ops: postOp(opAgentLifecycleControl)},
	{id: ProjectAgentRouteActionSuspend, form: agentFormProject, segs: []string{"suspend"}, ops: postOp(opAgentLifecycleControl)},
	{id: ProjectAgentRouteActionRestart, form: agentFormProject, segs: []string{"restart"}, ops: postOp(opAgentLifecycleControl)},
	{id: ProjectAgentRouteActionMessage, form: agentFormProject, segs: []string{"message"}},
	{id: ProjectAgentRouteActionExec, form: agentFormProject, segs: []string{"exec"}, ops: postOp(opAgentExec)},
	{id: ProjectAgentRouteActionRestore, form: agentFormProject, segs: []string{"restore"}, ops: postOp(opAgentLifecycleRestore)},
	{id: ProjectAgentRouteActionEnv, form: agentFormProject, segs: []string{"env"}, ops: postOp(opAgentEnv)},
	{id: ProjectAgentRouteActionOutbound, form: agentFormProject, segs: []string{"outbound-message"}, ops: postOp(opAgentOutboundMessage)},
	{id: ProjectAgentRouteActionMessageMode, form: agentFormProject, segs: []string{"set_message_mode"}, ops: postOp(opAgentSetMessageMode)},
	{id: ProjectAgentRouteActionReincarnate, form: agentFormProject, segs: []string{"reincarnate"}, ops: postOp(opAgentReincarnate)},
	{id: ProjectAgentRouteActionResetAuth, form: agentFormProject, segs: []string{"reset-auth"}, ops: postOp(opAgentResetAuth)},
	// See AgentRouteActionKeys above: same early-branch choke point
	// (handleProjectAgentAction's authorizeAgentKeys call), no ops entry.
	{id: ProjectAgentRouteActionKeys, form: agentFormProject, segs: []string{"keys"}},
}

// agentRoutePatternPrefix returns the catalog-style pattern prefix of a form.
func agentRoutePatternPrefix(form agentRouteForm) string {
	if form == agentFormProject {
		return "/api/v1/projects/{projectId}/agents"
	}
	return "/api/v1/agents"
}

// catalogPattern renders a row as an authzop catalog pattern. withOpaque adds
// the "/{subpath}" (proxy) or "/{key}" style placeholder for opaque rows.
func (row agentSubRouteRow) catalogPattern(withOpaque bool) string {
	var b strings.Builder
	b.WriteString(agentRoutePatternPrefix(row.form))
	if !row.noAgent {
		b.WriteString("/{id}")
	}
	for _, seg := range row.segs {
		b.WriteByte('/')
		if seg == agentRouteParam {
			b.WriteString("{port}")
			continue
		}
		b.WriteString(seg)
	}
	if withOpaque && row.opaque {
		b.WriteString("/{subpath}")
	}
	return b.String()
}

// isAgentSubRoutePath reports whether escapedPath lies in the agent
// sub-route namespace: /api/v1/agents/... or
// /api/v1/projects/{projectId}/agents[/...]. Paths in that namespace must
// resolve through ResolveAgentSubRoute; paths outside it are not agent
// sub-routes.
func isAgentSubRoutePath(escapedPath string) bool {
	if strings.HasPrefix(escapedPath, "/api/v1/agents/") {
		return true
	}
	rest, ok := strings.CutPrefix(escapedPath, "/api/v1/projects/")
	if !ok {
		return false
	}
	_, after, found := strings.Cut(rest, "/")
	if !found {
		return false
	}
	return after == "agents" || strings.HasPrefix(after, "agents/")
}

// agentRouteGrammarSegmentOK reports whether an escaped grammar segment is
// acceptable: non-empty, not a dot segment, and free of encoded separators,
// encoded backslashes and double encoding.
func agentRouteGrammarSegmentOK(seg string) bool {
	if seg == "" || seg == "." || seg == ".." {
		return false
	}
	lower := strings.ToLower(seg)
	for _, bad := range []string{"%2f", "%5c", "%25", "%00"} {
		if strings.Contains(lower, bad) {
			return false
		}
	}
	return !strings.Contains(seg, `\`)
}

// decodeAgentRouteSegment validates and unescapes one grammar segment.
func decodeAgentRouteSegment(seg string) (string, bool) {
	if !agentRouteGrammarSegmentOK(seg) {
		return "", false
	}
	decoded, err := url.PathUnescape(seg)
	if err != nil || decoded == "" || decoded == "." || decoded == ".." || strings.Contains(decoded, "\x00") {
		return "", false
	}
	return decoded, true
}

// ResolveAgentSubRoute classifies method and escapedPath (the request's
// URL.EscapedPath()) against agentSubRouteTable. It performs no
// authorization and no store reads. ok is false for any path in the agent
// namespace that no row accepts, and for any path outside that namespace.
func ResolveAgentSubRoute(method, escapedPath string) (AgentSubRoute, bool) {
	var (
		form      agentRouteForm
		projectID string
		rest      []string
	)
	switch {
	case strings.HasPrefix(escapedPath, "/api/v1/agents/"):
		form = agentFormByID
		rest = strings.Split(strings.TrimPrefix(escapedPath, "/api/v1/agents/"), "/")
	case strings.HasPrefix(escapedPath, "/api/v1/projects/"):
		parts := strings.Split(strings.TrimPrefix(escapedPath, "/api/v1/projects/"), "/")
		if len(parts) < 2 || parts[1] != "agents" {
			return AgentSubRoute{}, false
		}
		pid, ok := decodeAgentRouteSegment(parts[0])
		if !ok {
			return AgentSubRoute{}, false
		}
		form = agentFormProject
		projectID = pid
		rest = parts[2:]
	default:
		return AgentSubRoute{}, false
	}

	for _, row := range agentSubRouteTable {
		if row.form != form {
			continue
		}
		route, ok := row.match(rest)
		if !ok {
			continue
		}
		route.Method = method
		route.ProjectID = projectID
		route.OperationID = row.operation(method)
		return route, true
	}
	return AgentSubRoute{}, false
}

// match applies one row to the segments after the form prefix.
func (row agentSubRouteRow) match(rest []string) (AgentSubRoute, bool) {
	route := AgentSubRoute{RouteID: row.id}
	segs := rest
	if !row.noAgent {
		if len(segs) == 0 {
			return AgentSubRoute{}, false
		}
		agentID, ok := decodeAgentRouteSegment(segs[0])
		if !ok {
			return AgentSubRoute{}, false
		}
		route.AgentID = agentID
		segs = segs[1:]
	}
	for _, lit := range row.segs {
		if len(segs) == 0 {
			return AgentSubRoute{}, false
		}
		if lit == agentRouteParam {
			param, ok := decodeAgentRouteSegment(segs[0])
			if !ok {
				return AgentSubRoute{}, false
			}
			route.Suffix.Param = param
		} else if segs[0] != lit {
			return AgentSubRoute{}, false
		}
		segs = segs[1:]
	}
	switch {
	case row.opaque:
		if len(segs) > 0 {
			route.Suffix.Opaque = "/" + strings.Join(segs, "/")
		}
	case len(segs) == 0:
	case len(segs) == 1 && segs[0] == "" && row.trailingSlash:
	default:
		return AgentSubRoute{}, false
	}
	// On the by-id form the empty path "/api/v1/agents/" splits to [""]
	// and never reaches here: the agent segment rejects "".
	return route, true
}

// DecodedOpaque returns the unescaped opaque payload. ok is false when the
// payload is not a valid escape sequence.
func (s AgentSubRouteSuffix) DecodedOpaque() (string, bool) {
	decoded, err := url.PathUnescape(s.Opaque)
	if err != nil {
		return "", false
	}
	return decoded, true
}

type agentSubRouteContextKey struct{}

// withAgentSubRoute stores route in ctx. The value is stored by value, so
// handlers cannot change it.
func withAgentSubRoute(ctx context.Context, route AgentSubRoute) context.Context {
	return context.WithValue(ctx, agentSubRouteContextKey{}, route)
}

// agentSubRouteFromContext returns the route stored by routeGuard.
func agentSubRouteFromContext(ctx context.Context) (AgentSubRoute, bool) {
	route, ok := ctx.Value(agentSubRouteContextKey{}).(AgentSubRoute)
	return route, ok
}

// agentSubRouteGuardedRoutes are the mux routes whose agent sub-routes
// routeGuard resolves.
var agentSubRouteGuardedRoutes = map[string]bool{
	"agents.byId":   true,
	"projects.byId": true,
}

// resolveAgentSubRouteForRequest resolves r once for routeGuard. For a path
// in the agent namespace it returns the request carrying the route, or
// writes 404 and returns ok=false when no row accepts it. Paths outside the
// namespace pass through unchanged.
func resolveAgentSubRouteForRequest(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	escaped := r.URL.EscapedPath()
	if !isAgentSubRoutePath(escaped) {
		return r, true
	}
	route, ok := ResolveAgentSubRoute(r.Method, escaped)
	if !ok {
		NotFound(w, "Agent route")
		return nil, false
	}
	return r.WithContext(withAgentSubRoute(r.Context(), route)), true
}

// requireAgentSubRoute returns the route routeGuard stored for r. A missing
// route is a wiring error and fails closed with 500.
func requireAgentSubRoute(w http.ResponseWriter, r *http.Request) (AgentSubRoute, bool) {
	route, ok := agentSubRouteFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusInternalServerError, ErrCodeRuntimeError, "agent route not resolved", nil)
		return AgentSubRoute{}, false
	}
	return route, true
}
