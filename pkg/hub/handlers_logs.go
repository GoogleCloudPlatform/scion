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
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// brokerCodeRuntimeLogsUnsupported mirrors the wire value of
// pkg/runtimebroker.ErrCodeRuntimeLogsUnsupported, the code a runtime broker
// sends when its runtime declines a logs request outright (e.g. the
// substrate runtime's ErrLogsNotSupported). Kept as a literal rather than an
// import: pkg/hub only ever talks to the broker over HTTP.
const brokerCodeRuntimeLogsUnsupported = "runtime_logs_unsupported"

// runtimeLogsUnsupportedMessage is the hub's own fixed text for a
// runtime_logs_unsupported response — never the broker-supplied message.
// Any broker (including one this hub does not otherwise trust — a
// user-registered or misconfigured one) can put arbitrary text in its own
// response body; matching the code is not a reason to repeat that text
// verbatim under the hub's response.
const runtimeLogsUnsupportedMessage = "agent logs are not available on this agent's runtime"

// handleAgentLogs handles GET /api/v1/agents/{id}/logs
// and GET /api/v1/projects/{projectId}/agents/{agentId}/logs
// It proxies the request to the agent's runtime broker to read agent.log.
func (s *Server) handleAgentLogs(w http.ResponseWriter, r *http.Request, agentID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	if !checkAgentReadScope(w, r) {
		return
	}

	ctx := r.Context()

	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Project isolation runs before the authorization check so a cross-project
	// agent caller keeps its 404 and is not told the agent exists.
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		if agent.ProjectID != agentIdent.ProjectID() {
			NotFound(w, "Agent")
			return
		}
	}
	if !s.authorize(w, r, agentResource(agent), ActionRead) {
		return
	}

	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"No agent dispatcher configured", nil)
		return
	}

	tail := 0
	if v := r.URL.Query().Get("tail"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			tail = n
		}
	}

	logs, err := dispatcher.DispatchAgentLogs(ctx, agent, tail)
	if err != nil {
		slog.Error("agent log relay failed", "agent_id", agentID, "project_id", agent.ProjectID, "error", err)
		// The dispatcher's placement backstop refused before anything was
		// sent: its own 409 and code.
		if writeRuntimeTargetRefusal(w, err) {
			return
		}
		// The broker declined outright (e.g. the substrate runtime's
		// ErrLogsNotSupported) rather than failing to reach the runtime.
		// Pass its status and code straight through instead of re-wrapping
		// them in a generic gateway error — matching on both the status and
		// the code keeps every other broker error, including any other 501,
		// on the unchanged path below. The message is the hub's own fixed
		// text, not the broker's: any broker can put arbitrary text in its
		// response body, and this response must stay clean regardless.
		var se *brokerStatusError
		if errors.As(err, &se) && se.StatusCode == http.StatusNotImplemented && se.brokerErrorCode() == brokerCodeRuntimeLogsUnsupported {
			writeError(w, http.StatusNotImplemented, brokerCodeRuntimeLogsUnsupported, runtimeLogsUnsupportedMessage, nil)
			return
		}
		if writeBrokerRuntimeUnavailable(w, err, agent.Runtime) {
			return
		}
		if writeBrokerRoutedUnavailable(w, err) {
			return
		}
		writeError(w, http.StatusBadGateway, ErrCodeInternalError,
			"Failed to retrieve logs from broker: "+err.Error(), nil)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"logs": logs})
}

// handleAgentCloudLogs handles GET /api/v1/agents/{id}/cloud-logs
// and GET /api/v1/projects/{projectId}/agents/{agentId}/cloud-logs
func (s *Server) handleAgentCloudLogs(w http.ResponseWriter, r *http.Request, agentID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	if !checkAgentReadScope(w, r) {
		return
	}

	ctx := r.Context()

	// Verify agent exists and caller has read access
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Project isolation runs before the authorization check so a cross-project
	// agent caller keeps its 404 and is not told the agent exists.
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		if agent.ProjectID != agentIdent.ProjectID() {
			NotFound(w, "Agent")
			return
		}
	}
	if !s.authorize(w, r, agentResource(agent), ActionRead) {
		return
	}

	if s.logQueryService == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"Cloud Logging is not configured", nil)
		return
	}

	// Parse query parameters
	opts := agentCloudLogListOptions(s.config.HubName, agent, r.URL.Query())

	result, err := s.logQueryService.Query(ctx, opts)
	if err != nil {
		slog.Error("cloud log query failed", "agent_id", agentID, "project_id", agent.ProjectID, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to query cloud logs", nil)
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// agentCloudLogQueryOptions builds the filter options shared by the
// agent cloud-logs query and stream endpoints, so both select the same
// set of entries for an agent.
//
// It deliberately does not set ProjectID: many server log entries carry
// agent_id without a project_id label, so a project_id clause would drop
// them. Agent IDs are UUIDs, so the agent_id clause alone is selective.
func agentCloudLogQueryOptions(hubName string, agent *store.Agent, query url.Values) LogQueryOptions {
	opts := LogQueryOptions{
		HubName: hubName,
		AgentID: agent.ID,
	}
	if v := query.Get("severity"); v != "" {
		opts.Severity = v
	}
	if v := query.Get("broker_id"); v != "" {
		opts.BrokerID = v
	}
	return opts
}

// agentCloudLogListOptions builds the options for the non-streaming
// agent cloud-logs endpoint: the shared filter plus paging and time range.
func agentCloudLogListOptions(hubName string, agent *store.Agent, query url.Values) LogQueryOptions {
	opts := agentCloudLogQueryOptions(hubName, agent, query)

	if v := query.Get("tail"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			opts.Tail = n
		}
	}
	if v := query.Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			opts.Since = t
		}
	}
	if v := query.Get("until"); v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			opts.Until = t
		}
	}
	return opts
}

// handleAgentCloudLogsStream handles GET /api/v1/agents/{id}/cloud-logs/stream
// and GET /api/v1/projects/{projectId}/agents/{agentId}/cloud-logs/stream
// It returns an SSE stream of log entries using the Cloud Logging Tail API.
// A stream opened with a user access token ends once the token stops
// validating or loses agent.read (streamTokenStillAuthorized).
func (s *Server) handleAgentCloudLogsStream(w http.ResponseWriter, r *http.Request, agentID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	if !checkAgentReadScope(w, r) {
		return
	}

	// The stream also ends when the server begins shutting down, so it
	// does not hold the HTTP drain (ptone/scion#5054).
	ctx, endStream := s.shutdownMark.streamContext(r)
	defer endStream()

	// Verify agent exists and caller has read access
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Project isolation runs before the authorization check so a cross-project
	// agent caller keeps its 404 and is not told the agent exists.
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		if agent.ProjectID != agentIdent.ProjectID() {
			NotFound(w, "Agent")
			return
		}
	}
	if !s.authorize(w, r, agentResource(agent), ActionRead) {
		return
	}

	if s.logQueryService == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"Cloud Logging is not configured", nil)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Parse query filters
	opts := agentCloudLogQueryOptions(s.config.HubName, agent, r.URL.Query())

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	// Open a Tail stream via the Cloud Logging Tail API
	tailCh, tailCancel, err := s.logQueryService.Tail(ctx, opts)
	if err != nil {
		slog.Error("failed to open tail stream", "agent_id", agentID, "project_id", agent.ProjectID, "error", err)
		_, _ = fmt.Fprintf(w, "event: error\ndata: {\"message\":\"failed to open log stream\"}\n\n")
		flusher.Flush()
		return
	}
	defer tailCancel()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	recheck, stopRecheck := streamTokenRecheck(ctx)
	defer stopRecheck()

	// Server-side timeout: 10 minutes
	timeout := time.NewTimer(10 * time.Minute)
	defer timeout.Stop()

	for {
		select {
		case entry, ok := <-tailCh:
			if !ok {
				// Tail stream closed
				return
			}
			data, err := json.Marshal(entry)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "event: log\ndata: %s\n\n", data)
			flusher.Flush()
		case <-heartbeat.C:
			_, _ = fmt.Fprintf(w, ":heartbeat %d\n\n", time.Now().UnixMilli())
			flusher.Flush()
		case <-recheck:
			if !s.streamTokenStillAuthorized(ctx, agentResource(agent), ActionRead, "agent.read") {
				endStreamForCredential(ctx, w, flusher)
				return
			}
		case <-timeout.C:
			_, _ = fmt.Fprintf(w, "event: timeout\ndata: {\"message\":\"stream timeout, please reconnect\"}\n\n")
			flusher.Flush()
			return
		case <-ctx.Done():
			return
		}
	}
}

// handleAgentMessageLogs handles GET /api/v1/agents/{id}/message-logs
// and GET /api/v1/projects/{projectId}/agents/{agentId}/message-logs
// It queries the dedicated "scion-messages" Cloud Logging log for message
// entries associated with the given agent.
func (s *Server) handleAgentMessageLogs(w http.ResponseWriter, r *http.Request, agentID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	if !checkAgentReadScope(w, r) {
		return
	}

	ctx := r.Context()

	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Project isolation runs before the authorization check so a cross-project
	// agent caller keeps its 404 and is not told the agent exists.
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		if agent.ProjectID != agentIdent.ProjectID() {
			NotFound(w, "Agent")
			return
		}
	}
	fullHistory, ok := s.authorizeAgentMessageLogs(w, r, agent)
	if !ok {
		return
	}

	if s.logQueryService == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"Cloud Logging is not configured", nil)
		return
	}

	query := r.URL.Query()
	opts := LogQueryOptions{
		HubName:   s.config.HubName,
		AgentID:   agent.ID,
		ProjectID: agent.ProjectID,
		LogID:     logging.MessageLogID,
	}

	if opts.ParticipantID, ok = agentMessageLogParticipant(ctx, w, fullHistory); !ok {
		return
	}

	if v := query.Get("tail"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			opts.Tail = n
		}
	}
	if v := query.Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			opts.Since = t
		}
	}
	if v := query.Get("until"); v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			opts.Until = t
		}
	}

	result, err := s.logQueryService.Query(ctx, opts)
	if err != nil {
		slog.Error("message log query failed", "agent_id", agentID, "project_id", agent.ProjectID, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to query message logs", nil)
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// handleAgentMessageLogsStream handles GET /api/v1/agents/{id}/message-logs/stream
// and GET /api/v1/projects/{projectId}/agents/{agentId}/message-logs/stream
// It returns an SSE stream of message log entries from the "scion-messages" log.
// It applies the message-log list's rule: agent.attach gives the full
// history, agent.read alone only the messages the caller took part in. A
// stream opened with a user access token ends once the token stops
// validating or loses the permission that admitted it.
func (s *Server) handleAgentMessageLogsStream(w http.ResponseWriter, r *http.Request, agentID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	if !checkAgentReadScope(w, r) {
		return
	}

	// The stream also ends when the server begins shutting down, so it
	// does not hold the HTTP drain (ptone/scion#5054).
	ctx, endStream := s.shutdownMark.streamContext(r)
	defer endStream()

	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Project isolation runs before the authorization check so a cross-project
	// agent caller keeps its 404 and is not told the agent exists.
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		if agent.ProjectID != agentIdent.ProjectID() {
			NotFound(w, "Agent")
			return
		}
	}
	// The stream applies the message-log list's rule
	// (authorizeAgentMessageLogs) for every caller.
	fullHistory, ok := s.authorizeAgentMessageLogs(w, r, agent)
	if !ok {
		return
	}

	if s.logQueryService == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"Cloud Logging is not configured", nil)
		return
	}

	opts := LogQueryOptions{
		HubName:   s.config.HubName,
		AgentID:   agent.ID,
		ProjectID: agent.ProjectID,
		LogID:     logging.MessageLogID,
	}
	if opts.ParticipantID, ok = agentMessageLogParticipant(ctx, w, fullHistory); !ok {
		return
	}

	// A token stream is re-checked on the permission that admitted it.
	res := agentResource(agent)
	recheckAction, recheckPermission := ActionRead, "agent.read"
	if fullHistory {
		recheckAction, recheckPermission = ActionAttach, "agent.attach"
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	tailCh, tailCancel, err := s.logQueryService.Tail(ctx, opts)
	if err != nil {
		slog.Error("failed to open message log tail stream", "agent_id", agentID, "project_id", agent.ProjectID, "error", err)
		_, _ = fmt.Fprintf(w, "event: error\ndata: {\"message\":\"failed to open message log stream\"}\n\n")
		flusher.Flush()
		return
	}
	defer tailCancel()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	recheck, stopRecheck := streamTokenRecheck(ctx)
	defer stopRecheck()

	timeout := time.NewTimer(10 * time.Minute)
	defer timeout.Stop()

	for {
		select {
		case entry, ok := <-tailCh:
			if !ok {
				return
			}
			data, err := json.Marshal(entry)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "event: log\ndata: %s\n\n", data)
			flusher.Flush()
		case <-heartbeat.C:
			_, _ = fmt.Fprintf(w, ":heartbeat %d\n\n", time.Now().UnixMilli())
			flusher.Flush()
		case <-recheck:
			if !s.streamTokenStillAuthorized(ctx, res, recheckAction, recheckPermission) {
				endStreamForCredential(ctx, w, flusher)
				return
			}
		case <-timeout.C:
			_, _ = fmt.Fprintf(w, "event: timeout\ndata: {\"message\":\"stream timeout, please reconnect\"}\n\n")
			flusher.Flush()
			return
		case <-ctx.Done():
			return
		}
	}
}

// handleProjectMessageLogs handles GET /api/v1/projects/{projectId}/message-logs
// It queries the "scion-messages" Cloud Logging log for all message entries
// within the given project (across all agents).
func (s *Server) handleProjectMessageLogs(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	if !checkAgentReadScope(w, r) {
		return
	}

	ctx := r.Context()

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Project isolation runs before the authorization check so a cross-project
	// agent caller keeps its 404 and is not told the project exists.
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		if project.ID != agentIdent.ProjectID() {
			NotFound(w, "Project")
			return
		}
	}
	if !s.authorize(w, r, projectResource(project), ActionRead) {
		return
	}

	if s.logQueryService == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"Cloud Logging is not configured", nil)
		return
	}

	query := r.URL.Query()
	opts := LogQueryOptions{
		HubName:   s.config.HubName,
		ProjectID: project.ID,
		LogID:     logging.MessageLogID,
	}

	if v := query.Get("tail"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			opts.Tail = n
		}
	}
	if v := query.Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			opts.Since = t
		}
	}
	if v := query.Get("until"); v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			opts.Until = t
		}
	}

	result, err := s.logQueryService.Query(ctx, opts)
	if err != nil {
		slog.Error("project message log query failed", "project_id", projectID, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to query message logs", nil)
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// handleProjectMessageLogsStream handles GET /api/v1/projects/{projectId}/message-logs/stream
// It returns an SSE stream of all message log entries within the project.
// A stream opened with a user access token ends once the token stops
// validating or loses project.read.
func (s *Server) handleProjectMessageLogsStream(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	if !checkAgentReadScope(w, r) {
		return
	}

	// The stream also ends when the server begins shutting down, so it
	// does not hold the HTTP drain (ptone/scion#5054).
	ctx, endStream := s.shutdownMark.streamContext(r)
	defer endStream()

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Project isolation runs before the authorization check so a cross-project
	// agent caller keeps its 404 and is not told the project exists.
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		if project.ID != agentIdent.ProjectID() {
			NotFound(w, "Project")
			return
		}
	}
	if !s.authorize(w, r, projectResource(project), ActionRead) {
		return
	}

	if s.logQueryService == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"Cloud Logging is not configured", nil)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	opts := LogQueryOptions{
		HubName:   s.config.HubName,
		ProjectID: project.ID,
		LogID:     logging.MessageLogID,
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	tailCh, tailCancel, err := s.logQueryService.Tail(ctx, opts)
	if err != nil {
		slog.Error("failed to open project message log tail stream", "project_id", projectID, "error", err)
		_, _ = fmt.Fprintf(w, "event: error\ndata: {\"message\":\"failed to open message log stream\"}\n\n")
		flusher.Flush()
		return
	}
	defer tailCancel()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	recheck, stopRecheck := streamTokenRecheck(ctx)
	defer stopRecheck()

	timeout := time.NewTimer(10 * time.Minute)
	defer timeout.Stop()

	for {
		select {
		case entry, ok := <-tailCh:
			if !ok {
				return
			}
			data, err := json.Marshal(entry)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "event: log\ndata: %s\n\n", data)
			flusher.Flush()
		case <-heartbeat.C:
			_, _ = fmt.Fprintf(w, ":heartbeat %d\n\n", time.Now().UnixMilli())
			flusher.Flush()
		case <-recheck:
			if !s.streamTokenStillAuthorized(ctx, projectResource(project), ActionRead, "project.read") {
				endStreamForCredential(ctx, w, flusher)
				return
			}
		case <-timeout.C:
			_, _ = fmt.Fprintf(w, "event: timeout\ndata: {\"message\":\"stream timeout, please reconnect\"}\n\n")
			flusher.Flush()
			return
		case <-ctx.Done():
			return
		}
	}
}

// authorizeAgentMessageLogs decides a read of an agent's message logs, for
// the message-log list and its stream alike. agent.attach on the agent
// gives the full history; otherwise the caller needs agent.read and sees
// only the messages it took part in (agentMessageLogParticipant). It
// writes 401 or 403 and returns ok false when the caller is refused.
func (s *Server) authorizeAgentMessageLogs(w http.ResponseWriter, r *http.Request, agent *store.Agent) (fullHistory bool, ok bool) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return false, false
	}
	if s.agentFullHistoryDecision(ctx, identity, agent).Allowed {
		return true, true
	}
	res := agentResource(agent)
	decision := s.authzService.CheckAccess(ctx, identity, res, ActionRead)
	if !decision.Allowed {
		logAuthzDenial(r, identity, res, ActionRead, decision.Reason)
		Forbidden(w)
		return false, false
	}
	return false, true
}

// agentMessageLogParticipant returns the participant filter of a message
// log read: none with the full history; otherwise the caller's user ID, so
// the caller sees only the messages it sent or received, matching the
// hub-store message path. A caller without the full history that is not a
// user is refused with 403 (ok false): it gets no unscoped query.
func agentMessageLogParticipant(ctx context.Context, w http.ResponseWriter, fullHistory bool) (participantID string, ok bool) {
	if fullHistory {
		return "", true
	}
	user := GetUserIdentityFromContext(ctx)
	if user == nil {
		Forbidden(w)
		return "", false
	}
	return user.ID(), true
}

// resolveProjectAgent resolves an agent by slug or ID within a project, returning
// the agent if found and it belongs to the specified project.
func (s *Server) resolveProjectAgent(ctx context.Context, projectID, agentID string) (*store.Agent, error) {
	agent, err := s.store.GetAgentBySlug(ctx, projectID, agentID)
	if err != nil {
		if err == store.ErrNotFound {
			agent, err = s.store.GetAgent(ctx, agentID)
			if err != nil {
				return nil, err
			}
			if agent.ProjectID != projectID {
				return nil, store.ErrNotFound
			}
			return agent, nil
		}
		return nil, err
	}
	return agent, nil
}
