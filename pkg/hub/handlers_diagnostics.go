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
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// handleDiagnosticsLogs handles GET /api/v1/admin/diagnostics/logs
// It returns recent log entries from all system log IDs with source classification.
// Authorization: enforced by routeGuard via hub.diagnostics.read permission.
func (s *Server) handleDiagnosticsLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	if s.logQueryService == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"Cloud Logging is not configured. Set SCION_CLOUD_LOGGING=true and configure a GCP project ID.", nil)
		return
	}

	ctx := r.Context()
	query := r.URL.Query()

	// Parse query parameters
	opts := LogQueryOptions{
		HubName: s.config.HubName,
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
	if v := query.Get("agent_id"); v != "" {
		opts.AgentID = v
	}
	if v := query.Get("project_id"); v != "" {
		opts.ProjectID = v
	}
	if v := query.Get("source"); v != "" {
		opts.Sources = strings.Split(v, ",")
	}
	if v := query.Get("search"); v != "" {
		opts.Search = v
	}

	result, err := s.logQueryService.Query(ctx, opts)
	if err != nil {
		slog.Error("diagnostics log query failed", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to query diagnostics logs", nil)
		return
	}

	// Convert entries to DiagnosticLogEntry with source classification
	diagEntries := make([]DiagnosticLogEntry, 0, len(result.Entries))
	for _, entry := range result.Entries {
		diagEntries = append(diagEntries, toDiagnosticEntry(entry))
	}

	resp := DiagnosticsLogResponse{
		Entries:      diagEntries,
		HasMore:      result.HasMore,
		GCPProjectID: s.logQueryService.projectID,
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleDiagnosticsLogsStream handles GET /api/v1/admin/diagnostics/logs/stream
// It streams log entries from all system log IDs via SSE with source classification.
// Authorization: enforced by routeGuard via hub.diagnostics.read permission.
func (s *Server) handleDiagnosticsLogsStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	if s.logQueryService == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"Cloud Logging is not configured. Set SCION_CLOUD_LOGGING=true and configure a GCP project ID.", nil)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	ctx := r.Context()
	query := r.URL.Query()

	// Parse query filters — no LogID or AgentID to stream all system logs
	opts := LogQueryOptions{
		HubName: s.config.HubName,
	}
	if v := query.Get("severity"); v != "" {
		opts.Severity = v
	}
	if v := query.Get("broker_id"); v != "" {
		opts.BrokerID = v
	}
	if v := query.Get("agent_id"); v != "" {
		opts.AgentID = v
	}
	if v := query.Get("project_id"); v != "" {
		opts.ProjectID = v
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
		slog.Error("failed to open diagnostics tail stream", "error", err)
		_, _ = fmt.Fprintf(w, "event: error\ndata: {\"message\":\"failed to open diagnostics log stream\"}\n\n")
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
			diagEntry := toDiagnosticEntry(entry)
			data, err := json.Marshal(diagEntry)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "event: log\ndata: %s\n\n", data)
			flusher.Flush()
		case <-heartbeat.C:
			_, _ = fmt.Fprintf(w, ":heartbeat %d\n\n", time.Now().UnixMilli())
			flusher.Flush()
		case <-timeout.C:
			_, _ = fmt.Fprintf(w, "event: timeout\ndata: {\"message\":\"Stream timeout\",\"reconnect\":true}\n\n")
			flusher.Flush()
			return
		case <-ctx.Done():
			return
		}
	}
}
