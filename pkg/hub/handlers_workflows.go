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
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/gorilla/websocket"
)

// maxWorkflowSourceSize caps the inline sourceYaml at 256 KB.
const maxWorkflowSourceSize = 256 * 1024

// workflowLogsUpgrader configures the WebSocket upgrader for the logs endpoint.
var workflowLogsUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		// Auth is checked before the upgrade.
		return true
	},
}

// handleGroveWorkflowRuns routes requests under
// /api/v1/groves/{groveID}/workflows/runs[/{runSubPath}]
func (s *Server) handleGroveWorkflowRuns(w http.ResponseWriter, r *http.Request, groveID, runSubPath string) {
	// Require authenticated identity.
	identity := GetIdentityFromContext(r.Context())
	if identity == nil {
		Unauthorized(w)
		return
	}

	// Agent-identity grove isolation (Phase 4: agent-created runs).
	if agentIdent := GetAgentIdentityFromContext(r.Context()); agentIdent != nil {
		if agentIdent.GroveID() != groveID {
			Forbidden(w)
			return
		}
	}

	if runSubPath == "" {
		// Collection endpoint: POST (create) or GET (list).
		switch r.Method {
		case http.MethodPost:
			s.createWorkflowRun(w, r, groveID)
		case http.MethodGet:
			s.listWorkflowRuns(w, r, groveID)
		default:
			MethodNotAllowed(w)
		}
		return
	}

	// Individual run: runSubPath may be "{runID}" or "{runID}/logs".
	// We don't use the groveID-scoped path for individual run operations
	// (those live under /api/v1/workflows/runs/{runID}), so 404 here.
	NotFound(w, "workflow run resource")
}

// handleWorkflowRunRoutes routes requests under
// /api/v1/workflows/runs[/{runID}[/{action}]]
func (s *Server) handleWorkflowRunRoutes(w http.ResponseWriter, r *http.Request) {
	// Require authenticated identity.
	identity := GetIdentityFromContext(r.Context())
	if identity == nil {
		Unauthorized(w)
		return
	}

	// Parse path: /api/v1/workflows/runs/{runID}[/{action}]
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/workflows/runs")
	path = strings.TrimPrefix(path, "/")

	if path == "" {
		NotFound(w, "workflow run")
		return
	}

	parts := strings.SplitN(path, "/", 2)
	runID := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch action {
	case "":
		if r.Method != http.MethodGet {
			MethodNotAllowed(w)
			return
		}
		s.getWorkflowRun(w, r, runID)
	case "cancel":
		if r.Method != http.MethodPost {
			MethodNotAllowed(w)
			return
		}
		s.cancelWorkflowRun(w, r, runID)
	case "logs":
		s.streamWorkflowRunLogs(w, r, runID)
	default:
		NotFound(w, "workflow run action")
	}
}

// createWorkflowRun handles POST /api/v1/groves/{groveID}/workflows/runs
func (s *Server) createWorkflowRun(w http.ResponseWriter, r *http.Request, groveID string) {
	ctx := r.Context()

	// Enforce request body size limit.
	r.Body = http.MaxBytesReader(w, r.Body, maxWorkflowSourceSize+4096)

	var req api.WorkflowRunCreateRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	// Validate required fields.
	if strings.TrimSpace(req.SourceYAML) == "" {
		ValidationError(w, "sourceYaml is required and must not be empty", nil)
		return
	}
	if len(req.SourceYAML) > maxWorkflowSourceSize {
		writeError(w, http.StatusRequestEntityTooLarge, "workflow_source_too_large",
			"sourceYaml exceeds the maximum allowed size (256 KB)", nil)
		return
	}

	// Verify the grove exists.
	grove, err := s.store.GetGrove(ctx, groveID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Grove")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	// Build inputs JSON early so we can use it for both auth paths.
	inputsJSON := req.Inputs
	if inputsJSON == "" {
		inputsJSON = "{}"
	} else if !json.Valid([]byte(inputsJSON)) {
		ValidationError(w, "inputs must be valid JSON", nil)
		return
	}

	// Authorization: either a user with write access, or an agent with the
	// grove:workflow:run scope whose grove ID matches the request grove.
	var createdByUserID *string
	var createdByAgentID *string

	agentIdent := GetAgentIdentityFromContext(ctx)
	userIdent := GetUserIdentityFromContext(ctx)

	switch {
	case agentIdent != nil:
		// Agent path: scope and grove isolation checks.
		if !agentIdent.HasScope(ScopeWorkflowRun) {
			Forbidden(w)
			return
		}
		if agentIdent.GroveID() != groveID {
			Forbidden(w)
			return
		}
		// Verify the grove opts in to agent-initiated workflow runs.
		if !grove.AllowsWorkflowInvocation() {
			Forbidden(w)
			return
		}
		agentID := agentIdent.ID()
		createdByAgentID = &agentID

	case userIdent != nil:
		// User path: standard access-control check.
		decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
			Type:       "workflow_run",
			ParentType: "grove",
			ParentID:   groveID,
			OwnerID:    grove.CreatedBy,
		}, ActionCreate)
		if !decision.Allowed {
			Forbidden(w)
			return
		}
		userID := userIdent.ID()
		createdByUserID = &userID

	default:
		Forbidden(w)
		return
	}

	run := &store.WorkflowRun{
		ID:               api.NewUUID(),
		GroveID:          groveID,
		SourceYaml:       req.SourceYAML,
		InputsJSON:       inputsJSON,
		Status:           store.WorkflowRunStatusQueued,
		CreatedByUserID:  createdByUserID,
		CreatedByAgentID: createdByAgentID,
	}

	if err := s.store.CreateWorkflowRun(ctx, run); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Re-fetch to populate timestamps.
	created, err := s.store.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		// Created OK but can't re-fetch — return what we have.
		writeJSON(w, http.StatusCreated, api.WorkflowRunResponse{Run: toWorkflowRunSummary(run)})
		// Still attempt dispatch even if re-fetch failed.
		if s.workflowDispatcher != nil {
			s.workflowDispatcher.DispatchAsync(run.ID)
		}
		return
	}

	// Fire-and-forget dispatch: picks a broker and sends the run_workflow command.
	if s.workflowDispatcher != nil {
		s.workflowDispatcher.DispatchAsync(created.ID)
	}

	writeJSON(w, http.StatusCreated, api.WorkflowRunResponse{Run: toWorkflowRunSummary(created)})
}

// listWorkflowRuns handles GET /api/v1/groves/{groveID}/workflows/runs
func (s *Server) listWorkflowRuns(w http.ResponseWriter, r *http.Request, groveID string) {
	ctx := r.Context()
	query := r.URL.Query()

	limit := 50
	if l := query.Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			if n > 200 {
				n = 200
			}
			limit = n
		}
	}

	filter := store.WorkflowRunFilter{
		GroveID: groveID,
		Status:  query.Get("status"),
	}

	result, err := s.store.ListWorkflowRuns(ctx, store.WorkflowRunListOptions{
		Filter: filter,
		Limit:  limit,
		Cursor: query.Get("cursor"),
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	summaries := make([]api.WorkflowRunSummary, len(result.Items))
	for i := range result.Items {
		summaries[i] = toWorkflowRunSummary(&result.Items[i])
	}

	writeJSON(w, http.StatusOK, api.WorkflowRunListResponse{
		Runs:       summaries,
		NextCursor: result.NextCursor,
	})
}

// getWorkflowRun handles GET /api/v1/workflows/runs/{runID}
func (s *Server) getWorkflowRun(w http.ResponseWriter, r *http.Request, runID string) {
	ctx := r.Context()

	run, err := s.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "workflow_run_not_found", "Workflow run not found", nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Parse include= query parameter for heavy fields.
	includeStr := r.URL.Query().Get("include")
	includes := map[string]bool{}
	for _, field := range strings.Split(includeStr, ",") {
		includes[strings.TrimSpace(field)] = true
	}

	detail := api.WorkflowRunDetail{
		WorkflowRunSummary: toWorkflowRunSummary(run),
	}
	if includes["source"] {
		detail.Source = &run.SourceYaml
	}
	if includes["inputs"] {
		detail.Inputs = &run.InputsJSON
	}
	if includes["result"] && run.ResultJSON != nil {
		detail.Result = run.ResultJSON
	}
	if run.ErrorMessage != nil {
		detail.Error = run.ErrorMessage
	}

	writeJSON(w, http.StatusOK, api.WorkflowRunDetailResponse{Run: detail})
}

// cancelWorkflowRun handles POST /api/v1/workflows/runs/{runID}/cancel.
//
// Per design doc Section 3.5, cancellation is idempotent:
//   - Fresh cancellation of a non-terminal run → 202 Accepted with the updated run.
//   - Cancel of an already-terminal run (including already-canceled) → 200 OK
//     with the current run state (no error).
//
// The underlying store returns ErrVersionConflict along with the current run
// when the run is already terminal; we surface that as a 200 OK with body.
func (s *Server) cancelWorkflowRun(w http.ResponseWriter, r *http.Request, runID string) {
	ctx := r.Context()

	// Fetch run before cancel so we know the broker ID and prior status.
	runBefore, fetchErr := s.store.GetWorkflowRun(ctx, runID)
	if fetchErr != nil {
		if fetchErr == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "workflow_run_not_found", "Workflow run not found", nil)
			return
		}
		writeErrorFromErr(w, fetchErr, "")
		return
	}

	updated, err := s.store.CancelWorkflowRun(ctx, runID)
	if err != nil {
		switch err {
		case store.ErrNotFound:
			writeError(w, http.StatusNotFound, "workflow_run_not_found", "Workflow run not found", nil)
			return
		case store.ErrVersionConflict:
			// Already terminal: idempotent no-op, return current state with 200.
			if updated != nil {
				writeJSON(w, http.StatusOK, api.WorkflowRunResponse{Run: toWorkflowRunSummary(updated)})
				return
			}
			// Defensive fallback: no run returned along with the conflict.
			writeError(w, http.StatusConflict, "workflow_run_terminal",
				"Workflow run is already in a terminal state", nil)
			return
		default:
			writeErrorFromErr(w, err, "")
			return
		}
	}

	// If the run was provisioning or running, send cancel command to broker.
	if s.workflowDispatcher != nil && runBefore.BrokerID != nil &&
		(runBefore.Status == store.WorkflowRunStatusProvisioning ||
			runBefore.Status == store.WorkflowRunStatusRunning) {
		brokerID := *runBefore.BrokerID
		go func() {
			if err := s.workflowDispatcher.SendCancel(context.Background(), runID, brokerID); err != nil {
				slog.Warn("Failed to send cancel command to broker", "runID", runID, "brokerID", brokerID, "error", err)
			}
		}()
	}

	writeJSON(w, http.StatusAccepted, api.WorkflowRunResponse{Run: toWorkflowRunSummary(updated)})
}

// streamWorkflowRunLogs handles GET (WSS upgrade) /api/v1/workflows/runs/{runID}/logs.
//
// The endpoint upgrades the connection to WebSocket, replays any buffered log
// chunks from the dispatcher, then streams live events until the run reaches a
// terminal state or the client disconnects.
//
// Each WebSocket text message is a JSON object with an "event" field:
//
//	{"event":"log","stream":"stdout","line":"<plain UTF-8 text>","ts":"<RFC3339Nano>"}
//	{"event":"status","status":"running","ts":"<RFC3339Nano>"}
//	{"event":"terminal","status":"succeeded","exitCode":0,"ts":"<RFC3339Nano>"}
//	{"event":"error","message":"..."}
func (s *Server) streamWorkflowRunLogs(w http.ResponseWriter, r *http.Request, runID string) {
	ctx := r.Context()

	// Verify the run exists before upgrading.
	run, err := s.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "workflow_run_not_found", "Workflow run not found", nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Upgrade to WebSocket.
	conn, err := workflowLogsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// Helper to send a JSON message, ignoring write errors (connection may be gone).
	sendMsg := func(v interface{}) {
		data, err := json.Marshal(v)
		if err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, data)
	}

	// If the run is already in a terminal state and the dispatcher is gone,
	// emit a terminal event immediately and close.
	isTerminal := func(status string) bool {
		switch status {
		case store.WorkflowRunStatusSucceeded,
			store.WorkflowRunStatusFailed,
			store.WorkflowRunStatusCanceled,
			store.WorkflowRunStatusTimedOut:
			return true
		}
		return false
	}

	if isTerminal(run.Status) && s.workflowDispatcher == nil {
		sendMsg(map[string]interface{}{
			"event":  "terminal",
			"status": run.Status,
			"ts":     time.Now().UTC().Format(time.RFC3339Nano),
		})
		_ = conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "run already terminal"))
		return
	}

	// Subscribe to dispatcher events (also grabs buffered logs for replay).
	var buffered []WorkflowLogEntry
	var sub WorkflowRunSubscriber
	var unsub func()
	if s.workflowDispatcher != nil {
		buffered, sub, unsub = s.workflowDispatcher.Subscribe(runID)
		defer unsub()
	}

	// Replay buffered logs.
	for i := range buffered {
		entry := &buffered[i]
		sendMsg(map[string]interface{}{
			"event":  "log",
			"stream": entry.Stream,
			"line":   entry.Line,
			"ts":     entry.Timestamp.UTC().Format(time.RFC3339Nano),
		})
	}

	// If there is no dispatcher (shouldn't happen normally), close now.
	if sub == nil {
		if isTerminal(run.Status) {
			sendMsg(map[string]interface{}{
				"event":  "terminal",
				"status": run.Status,
				"ts":     time.Now().UTC().Format(time.RFC3339Nano),
			})
		}
		_ = conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "no dispatcher"))
		return
	}

	// Forward live events until terminal or client disconnects.
	// We use a goroutine to read (and discard) client frames so we can detect disconnection.
	clientGone := make(chan struct{})
	go func() {
		defer close(clientGone)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-clientGone:
			return
		case evt, ok := <-sub:
			if !ok {
				// Subscriber channel was closed by cleanupRun.
				_ = conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, "run cleanup"))
				return
			}
			switch evt.Kind {
			case WorkflowRunEventLog:
				if evt.Log != nil {
					sendMsg(map[string]interface{}{
						"event":  "log",
						"stream": evt.Log.Stream,
						"line":   evt.Log.Line,
						"ts":     evt.Log.Timestamp.UTC().Format(time.RFC3339Nano),
					})
				}
			case WorkflowRunEventStatus:
				sendMsg(map[string]interface{}{
					"event":  "status",
					"status": evt.Status,
					"ts":     evt.Timestamp.UTC().Format(time.RFC3339Nano),
				})
			case WorkflowRunEventTerminal:
				sendMsg(map[string]interface{}{
					"event":    "terminal",
					"status":   evt.Status,
					"exitCode": evt.ExitCode,
					"error":    evt.Error,
					"ts":       evt.Timestamp.UTC().Format(time.RFC3339Nano),
				})
				_ = conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, "run completed"))
				return
			}
		}
	}
}

// toWorkflowRunSummary converts a store.WorkflowRun to the wire summary type.
func toWorkflowRunSummary(r *store.WorkflowRun) api.WorkflowRunSummary {
	s := api.WorkflowRunSummary{
		ID:         r.ID,
		GroveID:    r.GroveID,
		BrokerID:   r.BrokerID,
		Status:     r.Status,
		TraceURL:   r.TraceURL,
		StartedAt:  r.StartedAt,
		FinishedAt: r.FinishedAt,
		CreatedAt:  r.Created,
		CreatedBy: api.WorkflowRunCreatedBy{
			UserID:  r.CreatedByUserID,
			AgentID: r.CreatedByAgentID,
		},
	}
	return s
}
