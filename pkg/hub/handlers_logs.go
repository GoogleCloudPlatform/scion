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
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// handleAgentLogs handles GET /api/v1/agents/{id}/logs
// and GET /api/v1/groves/{groveId}/agents/{agentId}/logs
// It proxies the request to the agent's runtime broker to read agent.log.
func (s *Server) handleAgentLogs(w http.ResponseWriter, r *http.Request, agentID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		decision := s.authzService.CheckAccess(ctx, userIdent, agentResource(agent), ActionRead)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Access denied", nil)
			return
		}
	}
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		if agent.ProjectID != agentIdent.ProjectID() {
			NotFound(w, "Agent")
			return
		}
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
		slog.Error("agent log relay failed", "agent_id", agentID, "grove_id", agent.ProjectID, "error", err)
		writeError(w, http.StatusBadGateway, ErrCodeInternalError,
			"Failed to retrieve logs from broker: "+err.Error(), nil)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"logs": logs})
}

// handleAgentCloudLogs handles GET /api/v1/agents/{id}/cloud-logs
// and GET /api/v1/groves/{groveId}/agents/{agentId}/cloud-logs
func (s *Server) handleAgentCloudLogs(w http.ResponseWriter, r *http.Request, agentID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	if s.logQueryService == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"Cloud Logging is not configured", nil)
		return
	}

	ctx := r.Context()

	// Verify agent exists and caller has read access
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		decision := s.authzService.CheckAccess(ctx, userIdent, agentResource(agent), ActionRead)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Access denied", nil)
			return
		}
	}
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		if agent.ProjectID != agentIdent.ProjectID() {
			NotFound(w, "Agent")
			return
		}
	}

	// Parse query parameters
	query := r.URL.Query()
	opts := LogQueryOptions{
		AgentID: agent.ID,
		ProjectID: agent.ProjectID,
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
	if v := query.Get("severity"); v != "" {
		opts.Severity = v
	}
	if v := query.Get("broker_id"); v != "" {
		opts.BrokerID = v
	}

	result, err := s.logQueryService.Query(ctx, opts)
	if err != nil {
		slog.Error("cloud log query failed", "agent_id", agentID, "grove_id", agent.ProjectID, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to query cloud logs", nil)
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// handleAgentCloudLogsStream handles GET /api/v1/agents/{id}/cloud-logs/stream
// and GET /api/v1/groves/{groveId}/agents/{agentId}/cloud-logs/stream
// It returns an SSE stream of log entries using the Cloud Logging Tail API.
func (s *Server) handleAgentCloudLogsStream(w http.ResponseWriter, r *http.Request, agentID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
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

	ctx := r.Context()

	// Verify agent exists and caller has read access
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		decision := s.authzService.CheckAccess(ctx, userIdent, agentResource(agent), ActionRead)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Access denied", nil)
			return
		}
	}

	// Parse query filters
	query := r.URL.Query()
	opts := LogQueryOptions{
		AgentID: agent.ID,
	}
	if v := query.Get("severity"); v != "" {
		opts.Severity = v
	}
	if v := query.Get("broker_id"); v != "" {
		opts.BrokerID = v
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	// Open a Tail stream via the Cloud Logging Tail API
	tailCh, tailCancel, err := s.logQueryService.Tail(ctx, opts)
	if err != nil {
		slog.Error("failed to open tail stream", "agent_id", agentID, "grove_id", agent.ProjectID, "error", err)
		fmt.Fprintf(w, "event: error\ndata: {\"message\":\"failed to open log stream\"}\n\n")
		flusher.Flush()
		return
	}
	defer tailCancel()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

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
			fmt.Fprintf(w, "event: log\ndata: %s\n\n", data)
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprintf(w, ":heartbeat %d\n\n", time.Now().UnixMilli())
			flusher.Flush()
		case <-timeout.C:
			fmt.Fprintf(w, "event: timeout\ndata: {\"message\":\"stream timeout, please reconnect\"}\n\n")
			flusher.Flush()
			return
		case <-ctx.Done():
			return
		}
	}
}

// handleAgentMessageLogs handles GET /api/v1/agents/{id}/message-logs
// and GET /api/v1/groves/{groveId}/agents/{agentId}/message-logs
// It queries the dedicated "scion-messages" Cloud Logging log for message
// entries associated with the given agent.
func (s *Server) handleAgentMessageLogs(w http.ResponseWriter, r *http.Request, agentID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	if s.logQueryService == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"Cloud Logging is not configured", nil)
		return
	}

	ctx := r.Context()

	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		decision := s.authzService.CheckAccess(ctx, userIdent, agentResource(agent), ActionRead)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Access denied", nil)
			return
		}
	}
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		if agent.ProjectID != agentIdent.ProjectID() {
			NotFound(w, "Agent")
			return
		}
	}

	query := r.URL.Query()
	opts := LogQueryOptions{
		AgentID: agent.ID,
		ProjectID: agent.ProjectID,
		LogID:   logging.MessageLogID,
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
		slog.Error("message log query failed", "agent_id", agentID, "grove_id", agent.ProjectID, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to query message logs", nil)
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// handleAgentMessageLogsStream handles GET /api/v1/agents/{id}/message-logs/stream
// and GET /api/v1/groves/{groveId}/agents/{agentId}/message-logs/stream
// It returns an SSE stream of message log entries from the "scion-messages" log.
func (s *Server) handleAgentMessageLogsStream(w http.ResponseWriter, r *http.Request, agentID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
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

	ctx := r.Context()

	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		decision := s.authzService.CheckAccess(ctx, userIdent, agentResource(agent), ActionRead)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Access denied", nil)
			return
		}
	}

	opts := LogQueryOptions{
		AgentID: agent.ID,
		ProjectID: agent.ProjectID,
		LogID:   logging.MessageLogID,
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	tailCh, tailCancel, err := s.logQueryService.Tail(ctx, opts)
	if err != nil {
		slog.Error("failed to open message log tail stream", "agent_id", agentID, "grove_id", agent.ProjectID, "error", err)
		fmt.Fprintf(w, "event: error\ndata: {\"message\":\"failed to open message log stream\"}\n\n")
		flusher.Flush()
		return
	}
	defer tailCancel()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

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
			fmt.Fprintf(w, "event: log\ndata: %s\n\n", data)
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprintf(w, ":heartbeat %d\n\n", time.Now().UnixMilli())
			flusher.Flush()
		case <-timeout.C:
			fmt.Fprintf(w, "event: timeout\ndata: {\"message\":\"stream timeout, please reconnect\"}\n\n")
			flusher.Flush()
			return
		case <-ctx.Done():
			return
		}
	}
}

// handleGroveMessageLogs handles GET /api/v1/groves/{groveId}/message-logs
// It queries the "scion-messages" Cloud Logging log for all message entries
// within the given grove (across all agents).
func (s *Server) handleGroveMessageLogs(w http.ResponseWriter, r *http.Request, groveID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	if s.logQueryService == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"Cloud Logging is not configured", nil)
		return
	}

	ctx := r.Context()

	grove, err := s.store.GetProject(ctx, groveID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		decision := s.authzService.CheckAccess(ctx, userIdent, groveResource(grove), ActionRead)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Access denied", nil)
			return
		}
	}
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		if grove.ID != agentIdent.ProjectID() {
			NotFound(w, "Grove")
			return
		}
	}

	query := r.URL.Query()
	opts := LogQueryOptions{
		ProjectID: grove.ID,
		LogID:   logging.MessageLogID,
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
		slog.Error("grove message log query failed", "grove_id", groveID, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to query message logs", nil)
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// handleGroveMessageLogsStream handles GET /api/v1/groves/{groveId}/message-logs/stream
// It returns an SSE stream of all message log entries within the grove.
func (s *Server) handleGroveMessageLogsStream(w http.ResponseWriter, r *http.Request, groveID string) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
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

	ctx := r.Context()

	grove, err := s.store.GetProject(ctx, groveID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		decision := s.authzService.CheckAccess(ctx, userIdent, groveResource(grove), ActionRead)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Access denied", nil)
			return
		}
	}

	opts := LogQueryOptions{
		ProjectID: grove.ID,
		LogID:   logging.MessageLogID,
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	tailCh, tailCancel, err := s.logQueryService.Tail(ctx, opts)
	if err != nil {
		slog.Error("failed to open grove message log tail stream", "grove_id", groveID, "error", err)
		fmt.Fprintf(w, "event: error\ndata: {\"message\":\"failed to open message log stream\"}\n\n")
		flusher.Flush()
		return
	}
	defer tailCancel()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

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
			fmt.Fprintf(w, "event: log\ndata: %s\n\n", data)
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprintf(w, ":heartbeat %d\n\n", time.Now().UnixMilli())
			flusher.Flush()
		case <-timeout.C:
			fmt.Fprintf(w, "event: timeout\ndata: {\"message\":\"stream timeout, please reconnect\"}\n\n")
			flusher.Flush()
			return
		case <-ctx.Done():
			return
		}
	}
}

// resolveGroveAgent resolves an agent by slug or ID within a grove, returning
// the agent if found and it belongs to the specified grove.
func (s *Server) resolveGroveAgent(ctx context.Context, groveID, agentID string) (*store.Agent, error) {
	agent, err := s.store.GetAgentBySlug(ctx, groveID, agentID)
	if err != nil {
		if err == store.ErrNotFound {
			agent, err = s.store.GetAgent(ctx, agentID)
			if err != nil {
				return nil, err
			}
			if agent.ProjectID != groveID {
				return nil, store.ErrNotFound
			}
			return agent, nil
		}
		return nil, err
	}
	return agent, nil
}
