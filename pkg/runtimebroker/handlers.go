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

package runtimebroker

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/templatecache"
)

// ============================================================================
// Health Endpoints
// ============================================================================

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	checks := make(map[string]string)

	// Check runtime availability
	if s.runtime != nil {
		checks[s.runtime.Name()] = "available"
	} else {
		checks["runtime"] = "unavailable"
	}

	status := "healthy"
	for _, v := range checks {
		if v != "available" && v != "healthy" {
			status = "degraded"
			break
		}
	}

	resp := HealthResponse{
		Status:  status,
		Version: s.version,
		Uptime:  time.Since(s.startTime).Round(time.Second).String(),
		Checks:  checks,
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	// Check if we have a functional runtime
	if s.runtime == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "not_ready",
			"reason": "no runtime available",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ready",
	})
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	runtimeType := "unknown"
	if s.runtime != nil {
		runtimeType = s.runtime.Name()
	}

	resp := BrokerInfoResponse{
		BrokerID: s.config.BrokerID,
		Name:     s.config.BrokerName,
		Version:  s.version,
		Capabilities: &BrokerCapabilities{
			WebPTY: false, // TODO: Implement WebSocket PTY
			Sync:   true,
			Attach: true,
			Exec:   true,
		},
		Profiles: []BrokerProfile{
			{Name: "default", Type: runtimeType, Available: true},
		},
	}

	writeJSON(w, http.StatusOK, resp)
}

// ============================================================================
// Agent Endpoints
// ============================================================================

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listAgents(w, r)
	case http.MethodPost:
		s.createAgent(w, r)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()

	filter := map[string]string{
		"scion.agent": "true",
	}

	// Add optional filters
	if groveID := query.Get("groveId"); groveID != "" {
		filter["scion.grove"] = groveID
	}
	if status := query.Get("status"); status != "" {
		filter["status"] = status
	}

	agents, err := s.manager.List(ctx, filter)
	if err != nil {
		RuntimeError(w, "Failed to list agents: "+err.Error())
		return
	}

	// Convert to API response format
	responses := make([]AgentResponse, 0, len(agents))
	for _, agent := range agents {
		responses = append(responses, AgentInfoToResponse(agent))
	}

	// Apply pagination
	limit := 50
	if l := query.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	totalCount := len(responses)
	if len(responses) > limit {
		responses = responses[:limit]
	}

	writeJSON(w, http.StatusOK, ListAgentsResponse{
		Agents:     responses,
		TotalCount: totalCount,
	})
}

func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req CreateAgentRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Validate required fields
	if req.Name == "" {
		ValidationError(w, "name is required", nil)
		return
	}

	// Debug log incoming request
	if s.config.Debug {
		slog.Debug("Creating agent", "name", req.Name, "slug", req.Slug, "groveID", req.GroveID)
		slog.Debug("Hub credentials",
			"hubEndpoint", req.HubEndpoint,
			"hasToken", req.AgentToken != "",
			"slug", req.Slug,
		)
		if req.Config != nil {
			slog.Debug("Agent configuration",
				"template", req.Config.Template,
				"image", req.Config.Image,
				"templateID", req.Config.TemplateID,
			)
		}
	}

	// Build merged environment:
	// 1. Start with resolvedEnv (from Hub, contains user/grove/broker vars and secrets)
	// 2. Override with config.Env (explicitly set in request)
	// 3. Add Hub authentication credentials if provided
	env := make(map[string]string)

	// First, apply resolved env from Hub (if present)
	if len(req.ResolvedEnv) > 0 {
		for k, v := range req.ResolvedEnv {
			env[k] = v
		}
	}

	// Then, apply config.Env (takes precedence over resolvedEnv)
	if req.Config != nil && len(req.Config.Env) > 0 {
		for _, e := range req.Config.Env {
			parts := strings.SplitN(e, "=", 2)
			if len(parts) == 2 {
				env[parts[0]] = parts[1]
			}
		}
	}

	// Add Hub authentication credentials if provided
	// These enable the agent (via sciontool) to authenticate with the Hub
	if req.AgentToken != "" {
		env["SCION_HUB_TOKEN"] = req.AgentToken
		if s.config.Debug {
			slog.Debug("SCION_HUB_TOKEN set", "length", len(req.AgentToken))
		}
	}
	// Set Hub URL: prefer request's HubEndpoint, fall back to server's configured HubEndpoint
	hubEndpoint := req.HubEndpoint
	if hubEndpoint == "" && s.config.HubEndpoint != "" {
		hubEndpoint = s.config.HubEndpoint
		if s.config.Debug {
			slog.Debug("Using server Hub endpoint as fallback", "endpoint", hubEndpoint)
		}
	}
	if hubEndpoint != "" {
		env["SCION_HUB_URL"] = hubEndpoint
		if s.config.Debug {
			slog.Debug("SCION_HUB_URL set", "url", hubEndpoint)
		}
	}
	if req.Slug != "" {
		env["SCION_AGENT_SLUG"] = req.Slug
		if s.config.Debug {
			slog.Debug("SCION_AGENT_SLUG set", "slug", req.Slug)
		}
	}
	if req.ID != "" {
		env["SCION_AGENT_ID"] = req.ID
		if s.config.Debug {
			slog.Debug("SCION_AGENT_ID set", "id", req.ID)
		}
	}

	if s.config.BrokerName != "" {
		env["SCION_BROKER_NAME"] = s.config.BrokerName
	}

	// Pass debug mode to the container so sciontool logs debug info
	if s.config.Debug {
		env["SCION_DEBUG"] = "1"
	}

	// Debug log final env count
	if s.config.Debug {
		slog.Debug("Final environment count", "count", len(env))
		for k, v := range env {
			if k == "SCION_HUB_TOKEN" {
				slog.Debug("  ENV", "key", k, "value", "<redacted>")
			} else {
				slog.Debug("  ENV", "key", k, "value", v)
			}
		}
	}

	opts := api.StartOptions{
		Name:      req.Name,
		Detached:  boolPtr(true),
		GrovePath: req.GrovePath,
	}

	if req.Config != nil {
		opts.Template = req.Config.Template
		opts.Image = req.Config.Image
		opts.Task = req.Config.Task
	}

	// Debug log grove path
	if s.config.Debug && req.GrovePath != "" {
		slog.Debug("Using grove path from Hub", "path", req.GrovePath)
	}

	// Hydrate template if Hub mode is enabled and template info is provided
	if s.hydrator != nil && req.Config != nil {
		templatePath, err := s.hydrateTemplate(ctx, req.Config)
		if err != nil {
			// Check if it's a Hub connectivity error
			if templatecache.IsHubConnectivityError(err) {
				HubUnreachableError(w, err.Error())
				return
			}
			TemplateError(w, "Failed to hydrate template: "+err.Error())
			return
		}
		if templatePath != "" {
			opts.Template = templatePath
			if s.config.Debug {
				slog.Debug("Using hydrated template", "path", templatePath)
			}
		}
	}

	// Always set env (may be empty, which is fine)
	opts.Env = env

	// Start the agent
	agentInfo, err := s.manager.Start(ctx, opts)
	if err != nil {
		RuntimeError(w, "Failed to create agent: "+err.Error())
		return
	}

	resp := CreateAgentResponse{
		Agent:   agentInfoPtr(AgentInfoToResponse(*agentInfo)),
		Created: true,
	}

	writeJSON(w, http.StatusCreated, resp)
}

// hydrateTemplate fetches and caches a template from the Hub if template info is provided.
// Returns the local template path, or empty string if no Hub template was specified.
func (s *Server) hydrateTemplate(ctx context.Context, cfg *CreateAgentConfig) (string, error) {
	// Check if we have template info from Hub
	if cfg.TemplateID == "" && cfg.TemplateHash == "" {
		// No Hub template info provided, use local template handling
		return "", nil
	}

	// If we have a template hash, try to use it for cache lookup
	if cfg.TemplateHash != "" && cfg.TemplateID != "" {
		return s.hydrator.HydrateWithHash(ctx, cfg.TemplateID, cfg.TemplateHash)
	}

	// Just have template ID, do full hydration
	if cfg.TemplateID != "" {
		return s.hydrator.Hydrate(ctx, cfg.TemplateID)
	}

	return "", nil
}

func (s *Server) handleAgentByID(w http.ResponseWriter, r *http.Request) {
	id, action := extractAction(r, "/api/v1/agents")

	if id == "" {
		NotFound(w, "Agent")
		return
	}

	// Handle WebSocket attach for PTY
	if action == "attach" && isPTYWebSocketUpgrade(r) {
		s.handleAgentAttach(w, r)
		return
	}

	// Handle actions
	if action != "" {
		s.handleAgentAction(w, r, id, action)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getAgent(w, r, id)
	case http.MethodDelete:
		s.deleteAgent(w, r, id)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	// List agents and find the matching one
	agents, err := s.manager.List(ctx, map[string]string{"scion.agent": "true"})
	if err != nil {
		RuntimeError(w, "Failed to list agents: "+err.Error())
		return
	}

	for _, agent := range agents {
		if agent.Name == id || agent.ContainerID == id || agent.Slug == id {
			writeJSON(w, http.StatusOK, AgentInfoToResponse(agent))
			return
		}
	}

	NotFound(w, "Agent")
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	query := r.URL.Query()

	deleteFiles := query.Get("deleteFiles") == "true"
	removeBranch := query.Get("removeBranch") == "true"

	// Get the agent's grove path before stopping (needed for file deletion)
	var grovePath string
	agents, err := s.manager.List(ctx, map[string]string{"scion.agent": "true"})
	if err == nil {
		for _, agent := range agents {
			if agent.Name == id || agent.ContainerID == id || agent.Slug == id {
				grovePath = agent.GrovePath
				break
			}
		}
	}

	_, err = s.manager.Delete(ctx, id, deleteFiles, grovePath, removeBranch)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			NotFound(w, "Agent")
			return
		}
		RuntimeError(w, "Failed to delete agent: "+err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAgentAction(w http.ResponseWriter, r *http.Request, id, action string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	switch action {
	case "start":
		s.startAgent(w, r, id)
	case "stop":
		s.stopAgent(w, r, id)
	case "restart":
		s.restartAgent(w, r, id)
	case "message":
		s.sendMessage(w, r, id)
	case "exec":
		s.execCommand(w, r, id)
	case "logs":
		s.getLogs(w, r, id)
	case "stats":
		s.getStats(w, r, id)
	case "has-prompt":
		s.checkAgentPrompt(w, r, id)
	default:
		NotFound(w, "Action")
	}
}

func (s *Server) startAgent(w http.ResponseWriter, r *http.Request, id string) {
	// In the current architecture, "start" means resuming a stopped agent.
	// For now, we return a simple acknowledgment since the manager doesn't
	// have a separate Start method for existing agents.
	// TODO: Implement proper agent resume functionality

	writeJSON(w, http.StatusAccepted, map[string]string{
		"status": "accepted",
		"message": "Start operation accepted",
	})
}

func (s *Server) stopAgent(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	if err := s.manager.Stop(ctx, id); err != nil {
		if strings.Contains(err.Error(), "not found") {
			NotFound(w, "Agent")
			return
		}
		RuntimeError(w, "Failed to stop agent: "+err.Error())
		return
	}

	// Send an immediate heartbeat so the hub gets the updated container status
	// without waiting for the next periodic heartbeat interval.
	if s.heartbeat != nil {
		go func() {
			if err := s.heartbeat.ForceHeartbeat(context.Background()); err != nil {
				slog.Error("Failed to send forced heartbeat after stop", "agent", id, "error", err)
			}
		}()
	}

	writeJSON(w, http.StatusAccepted, map[string]string{
		"status": "accepted",
		"message": "Stop operation accepted",
	})
}

func (s *Server) restartAgent(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	// Stop then start
	if err := s.manager.Stop(ctx, id); err != nil {
		if strings.Contains(err.Error(), "not found") {
			NotFound(w, "Agent")
			return
		}
		RuntimeError(w, "Failed to restart agent: "+err.Error())
		return
	}

	// TODO: Implement proper restart with start after stop

	writeJSON(w, http.StatusAccepted, map[string]string{
		"status": "accepted",
		"message": "Restart operation accepted",
	})
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	var req MessageRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Message == "" {
		ValidationError(w, "message is required", nil)
		return
	}

	if err := s.manager.Message(ctx, id, req.Message, req.Interrupt); err != nil {
		if strings.Contains(err.Error(), "not found") {
			NotFound(w, "Agent")
			return
		}
		RuntimeError(w, "Failed to send message: "+err.Error())
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (s *Server) execCommand(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	var req ExecRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if len(req.Command) == 0 {
		ValidationError(w, "command is required", nil)
		return
	}

	output, err := s.runtime.Exec(ctx, id, req.Command)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			NotFound(w, "Agent")
			return
		}
		RuntimeError(w, "Failed to execute command: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, ExecResponse{
		Output:   output,
		ExitCode: 0, // TODO: Get actual exit code from runtime
	})
}

func (s *Server) getLogs(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	logs, err := s.runtime.GetLogs(ctx, id)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			NotFound(w, "Agent")
			return
		}
		RuntimeError(w, "Failed to get logs: "+err.Error())
		return
	}

	// Return logs as plain text
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(logs))
}

func (s *Server) getStats(w http.ResponseWriter, r *http.Request, id string) {
	// TODO: Implement real stats from runtime
	// For now, return placeholder data
	writeJSON(w, http.StatusOK, StatsResponse{
		CPUUsagePercent:  0.0,
		MemoryUsageBytes: 0,
	})
}

// HasPromptResponse is the response for the has-prompt action.
type HasPromptResponse struct {
	HasPrompt bool `json:"hasPrompt"`
}

func (s *Server) checkAgentPrompt(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	// Find the agent to get its grove path
	agents, err := s.manager.List(ctx, map[string]string{"scion.agent": "true"})
	if err != nil {
		RuntimeError(w, "Failed to list agents: "+err.Error())
		return
	}

	var agent *api.AgentInfo
	for i := range agents {
		if agents[i].Name == id || agents[i].ContainerID == id || agents[i].Slug == id {
			agent = &agents[i]
			break
		}
	}

	if agent == nil {
		NotFound(w, "Agent")
		return
	}

	if agent.GrovePath == "" {
		// No grove path means we can't check prompt.md
		writeJSON(w, http.StatusOK, HasPromptResponse{HasPrompt: false})
		return
	}

	// Check if prompt.md exists and has content
	// Path: <grovePath>/agents/<agentName>/prompt.md
	promptPath := filepath.Join(agent.GrovePath, "agents", agent.Name, "prompt.md")
	content, err := os.ReadFile(promptPath)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, HasPromptResponse{HasPrompt: false})
			return
		}
		// Log the error but return false
		slog.Warn("Failed to read prompt.md", "path", promptPath, "error", err)
		writeJSON(w, http.StatusOK, HasPromptResponse{HasPrompt: false})
		return
	}

	hasPrompt := len(strings.TrimSpace(string(content))) > 0
	writeJSON(w, http.StatusOK, HasPromptResponse{HasPrompt: hasPrompt})
}

// Helper functions

func boolPtr(b bool) *bool {
	return &b
}

func agentInfoPtr(a AgentResponse) *AgentResponse {
	return &a
}
