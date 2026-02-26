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
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ptone/scion-agent/pkg/agent"
	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/config"
	"github.com/ptone/scion-agent/pkg/gcp"
	"github.com/ptone/scion-agent/pkg/harness"
	"github.com/ptone/scion-agent/pkg/templatecache"
)

// ============================================================================
// Health Endpoints
// ============================================================================

// GetHealthInfo returns the current health status of the Runtime Broker server.
// This can be called directly by co-located components (e.g., the WebServer)
// to build composite health responses without making an HTTP round-trip.
func (s *Server) GetHealthInfo(ctx context.Context) *HealthResponse {
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

	return &HealthResponse{
		Status:  status,
		Version: s.version,
		Uptime:  time.Since(s.startTime).Round(time.Second).String(),
		Checks:  checks,
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	resp := s.GetHealthInfo(r.Context())
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

// handleHubConnections returns live status of all hub connections.
func (s *Server) handleHubConnections(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	s.hubMu.RLock()
	defer s.hubMu.RUnlock()

	mode := "single-hub"
	if len(s.hubConnections) > 1 {
		mode = "multi-hub"
	}

	connections := make([]HubConnectionInfo, 0, len(s.hubConnections))
	for _, conn := range s.hubConnections {
		info := HubConnectionInfo{
			Name:              conn.Name,
			HubEndpoint:       conn.HubEndpoint,
			BrokerID:          conn.BrokerID,
			AuthMode:          string(conn.AuthMode),
			Status:            string(conn.GetStatus()),
			IsColocated:       conn.IsColocated,
			HasHeartbeat:      conn.Heartbeat != nil,
			HasControlChannel: conn.ControlChannel != nil,
		}
		connections = append(connections, info)
	}

	resp := HubConnectionStatusResponse{
		Connections: connections,
		Mode:        mode,
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

	// Add Hub authentication credentials for the agent.
	// Uses SCION_SERVER_AUTH_DEV_TOKEN which maps to the non-deprecated
	// server.auth.dev_token setting.
	// Priority: explicit agent token from dispatcher > broker's own dev token.
	if agentToken := req.AgentToken; agentToken != "" {
		env["SCION_SERVER_AUTH_DEV_TOKEN"] = agentToken
		if s.config.Debug {
			slog.Debug("SCION_SERVER_AUTH_DEV_TOKEN set from agent token", "length", len(agentToken))
		}
	} else if devToken := os.Getenv("SCION_SERVER_AUTH_DEV_TOKEN"); devToken != "" {
		env["SCION_SERVER_AUTH_DEV_TOKEN"] = devToken
		if s.config.Debug {
			slog.Debug("SCION_SERVER_AUTH_DEV_TOKEN set from broker env", "length", len(devToken))
		}
	}
	// Set Hub URL with priority:
	// 1. Grove settings hub.endpoint (most specific: user's project-level config)
	// 2. Request's HubEndpoint (from Hub dispatcher's server config)
	// 3. Broker's configured HubEndpoint (server-level fallback)
	hubEndpoint := req.HubEndpoint
	if hubEndpoint == "" && s.config.HubEndpoint != "" {
		hubEndpoint = s.config.HubEndpoint
		if s.config.Debug {
			slog.Debug("Using server Hub endpoint as fallback", "endpoint", hubEndpoint)
		}
	}
	// Override with grove settings if available. The grove's hub.endpoint reflects
	// the externally-accessible Hub URL (e.g. a tunnel/DNS endpoint) that agents
	// inside containers need to reach the Hub. This takes precedence because the
	// Hub's own server config may only know its localhost address.
	// Use LoadSettingsFromDir to read only the grove's settings file without
	// picking up the broker's environment variables or global settings.
	if req.GrovePath != "" {
		if groveSettings, err := config.LoadSettingsFromDir(req.GrovePath); err == nil {
			if !groveSettings.IsHubExplicitlyDisabled() {
				if ep := groveSettings.GetHubEndpoint(); ep != "" {
					hubEndpoint = ep
					if s.config.Debug {
						slog.Debug("Hub endpoint resolved from grove settings", "endpoint", ep, "grovePath", req.GrovePath)
					}
				}
			}
		}
	}
	if hubEndpoint != "" {
		env["SCION_HUB_ENDPOINT"] = hubEndpoint
		env["SCION_HUB_URL"] = hubEndpoint // legacy compat
		if s.config.Debug {
			slog.Debug("SCION_HUB_ENDPOINT set", "endpoint", hubEndpoint)
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
	if req.CreatorName != "" {
		env["SCION_CREATOR"] = req.CreatorName
	}

	// Pass debug mode to the container so sciontool logs debug info
	if s.config.Debug {
		env["SCION_DEBUG"] = "1"
	}

	// Debug log final env count
	if s.config.Debug {
		slog.Debug("Final environment count", "count", len(env))
		for k, v := range env {
			if k == "SCION_SERVER_AUTH_DEV_TOKEN" {
				slog.Debug("  ENV", "key", k, "value", "<redacted>")
			} else {
				slog.Debug("  ENV", "key", k, "value", v)
			}
		}
	}

	// Env-gather: if GatherEnv is true, evaluate env completeness
	if req.GatherEnv {
		required, secretInfo := s.extractRequiredEnvKeys(req)
		if s.config.Debug {
			slog.Debug("Env-gather: evaluating env completeness",
				"gatherEnv", req.GatherEnv,
				"grovePath", req.GrovePath,
				"requiredKeys", len(required),
				"required", required,
			)
		}
		if len(required) > 0 {
			// Build lookup set of keys satisfied by resolved secrets
			secretTargets := make(map[string]struct{})
			for _, s := range req.ResolvedSecrets {
				if s.Type == "environment" || s.Type == "" {
					secretTargets[s.Target] = struct{}{}
				}
			}

			var hubHas, brokerHas, needs []string
			for _, key := range required {
				val, hasVal := env[key]
				if hasVal && val != "" {
					// Determine source
					if _, fromHub := req.ResolvedEnv[key]; fromHub {
						hubHas = append(hubHas, key)
					} else {
						brokerHas = append(brokerHas, key)
					}
				} else if _, fromSecret := secretTargets[key]; fromSecret {
					// Key will be projected from a resolved secret at container start
					hubHas = append(hubHas, key)
				} else {
					// Check if broker can supply from its own env
					if brokerVal := os.Getenv(key); brokerVal != "" {
						env[key] = brokerVal
						brokerHas = append(brokerHas, key)
					} else {
						needs = append(needs, key)
					}
				}
			}

			if len(needs) > 0 {
				// Store pending state for finalize-env
				s.pendingEnvGatherMu.Lock()
				s.pendingEnvGather[req.Name] = &pendingAgentState{
					Request:   &req,
					MergedEnv: env,
					CreatedAt: time.Now(),
				}
				s.pendingEnvGatherMu.Unlock()

				if s.config.Debug {
					slog.Debug("Env-gather: returning 202 with requirements",
						"required", required,
						"hubHas", hubHas,
						"brokerHas", brokerHas,
						"needs", needs,
					)
				}

					// Build SecretInfo for needed keys only
				var respSecretInfo map[string]api.SecretKeyInfo
				for _, key := range needs {
					if info, ok := secretInfo[key]; ok {
						if respSecretInfo == nil {
							respSecretInfo = make(map[string]api.SecretKeyInfo)
						}
						respSecretInfo[key] = info
					}
				}

				writeJSON(w, http.StatusAccepted, EnvRequirementsResponse{
					AgentID:    req.ID,
					Required:   required,
					HubHas:     hubHas,
					BrokerHas:  brokerHas,
					Needs:      needs,
					SecretInfo: respSecretInfo,
				})
				return
			}

			if s.config.Debug {
				slog.Debug("Env-gather: all required keys satisfied, proceeding with start",
					"required", required,
					"hubHas", hubHas,
					"brokerHas", brokerHas,
				)
			}
		}
	}

	// For hub-native groves (GroveSlug set, no local provider path), resolve
	// the conventional grove path (~/.scion/groves/<slug>/) so the agent is
	// created in the correct location instead of the broker's local grove.
	if req.GroveSlug != "" && req.GrovePath == "" {
		globalDir, err := config.GetGlobalDir()
		if err != nil {
			RuntimeError(w, "Failed to get global dir: "+err.Error())
			return
		}
		req.GrovePath = filepath.Join(globalDir, "groves", req.GroveSlug)
		if s.config.Debug {
			slog.Debug("Resolved hub-native grove path from slug",
				"slug", req.GroveSlug,
				"path", req.GrovePath,
			)
		}
	}

	opts := api.StartOptions{
		Name:      req.Name,
		Detached:  boolPtr(!req.Attach),
		GrovePath: req.GrovePath,
	}

	if req.Config != nil {
		opts.Template = req.Config.Template
		opts.Image = req.Config.Image
		opts.Task = req.Config.Task
		opts.Workspace = req.Config.Workspace
		opts.Profile = req.Config.Profile
	}

	// Save template slug before hydration may replace opts.Template with a cache path
	templateSlug := ""
	if req.Config != nil {
		templateSlug = req.Config.Template
	}

	// Debug log grove path
	if s.config.Debug && req.GrovePath != "" {
		slog.Debug("Using grove path from Hub", "path", req.GrovePath)
	}

	// Reject global groves in multi-hub mode
	if s.isMultiHubMode() && s.isGlobalGrove(req.GroveID, req.GrovePath) {
		writeJSON(w, http.StatusConflict, map[string]interface{}{
			"error": map[string]string{
				"code":    "global_grove_disabled",
				"message": "Global grove is disabled when broker is connected to multiple hubs",
			},
		})
		return
	}

	// Hydrate template if Hub mode is enabled and template info is provided
	hydrator := s.resolveHydrator(r)
	if hydrator != nil && req.Config != nil {
		templatePath, err := s.hydrateTemplate(ctx, req.Config, hydrator)
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

	// Preserve human-friendly template slug for container labels
	if templateSlug != "" {
		opts.TemplateName = templateSlug
	}

	// Git clone mode: inject env vars and skip workspace mounting.
	if req.Config != nil && req.Config.GitClone != nil {
		gc := req.Config.GitClone
		env["SCION_GIT_CLONE_URL"] = gc.URL
		if gc.Branch != "" {
			env["SCION_GIT_BRANCH"] = gc.Branch
		}
		if gc.Depth > 0 {
			env["SCION_GIT_DEPTH"] = strconv.Itoa(gc.Depth)
		}
		opts.Workspace = ""
		opts.GrovePath = ""
		opts.GitClone = gc
		if s.config.Debug {
			slog.Debug("Git clone mode enabled",
				"cloneURL", gc.URL, "branch", gc.Branch, "depth", gc.Depth)
		}
	}

	// Always set env (may be empty, which is fine)
	opts.Env = env

	// Pass through resolved secrets from the Hub
	if len(req.ResolvedSecrets) > 0 {
		opts.ResolvedSecrets = req.ResolvedSecrets
		if s.config.Debug {
			slog.Debug("Received resolved secrets from Hub", "count", len(req.ResolvedSecrets))
		}
	}

	// If WorkspaceStoragePath is set, download workspace from GCS (non-git bootstrap)
	if req.WorkspaceStoragePath != "" {
		// For hub-native groves (GroveSlug set), use the conventional path
		// ~/.scion/groves/<slug>/ instead of the worktree-based path.
		var workspaceDir string
		if req.GroveSlug != "" {
			globalDir, err := config.GetGlobalDir()
			if err != nil {
				RuntimeError(w, "Failed to get global dir: "+err.Error())
				return
			}
			workspaceDir = filepath.Join(globalDir, "groves", req.GroveSlug)
		} else {
			workspaceDir = filepath.Join(s.config.WorktreeBase, req.Name, "workspace")
		}
		if err := os.MkdirAll(workspaceDir, 0755); err != nil {
			RuntimeError(w, "Failed to create workspace directory: "+err.Error())
			return
		}

		bucket := s.config.StorageBucket
		if bucket == "" {
			RuntimeError(w, "Storage bucket not configured for workspace bootstrap")
			return
		}

		if s.config.Debug {
			slog.Debug("Downloading workspace from GCS",
				"bucket", bucket,
				"storagePath", req.WorkspaceStoragePath+"/files",
				"workspaceDir", workspaceDir,
				"groveSlug", req.GroveSlug,
			)
		}

		if err := gcp.SyncFromGCS(ctx, bucket, req.WorkspaceStoragePath+"/files", workspaceDir); err != nil {
			RuntimeError(w, "Failed to download workspace from GCS: "+err.Error())
			return
		}

		opts.Workspace = workspaceDir
		opts.GrovePath = "" // Prevent git worktree logic in ProvisionAgent
	}

	mgr := s.resolveManagerForOpts(opts)

	// Branch based on provision-only flag
	if req.ProvisionOnly {
		// Provision only: set up dirs, worktree, templates without starting the container
		cfg, err := mgr.Provision(ctx, opts)
		if err != nil {
			RuntimeError(w, "Failed to provision agent: "+err.Error())
			return
		}

		// Build a response with "created" status (no container launched)
		agentResp := &AgentResponse{
			ID:     req.ID,
			Slug:   req.Slug,
			Name:   req.Name,
			Status: AgentStatusCreated,
		}
		if cfg != nil {
			agentResp.HarnessConfig = cfg.HarnessConfig
		}
		if s.runtime != nil {
			agentResp.RuntimeType = s.runtime.Name()
		}

		writeJSON(w, http.StatusCreated, CreateAgentResponse{
			Agent:   agentResp,
			Created: true,
		})
		return
	}

	// Full start: provision and launch the container
	agentInfo, err := mgr.Start(ctx, opts)
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
func (s *Server) hydrateTemplate(ctx context.Context, cfg *CreateAgentConfig, hydrator *templatecache.Hydrator) (string, error) {
	// Check if we have template info from Hub
	if cfg.TemplateID == "" && cfg.TemplateHash == "" {
		// No Hub template info provided, use local template handling
		return "", nil
	}

	// If we have a template hash, try to use it for cache lookup
	if cfg.TemplateHash != "" && cfg.TemplateID != "" {
		return hydrator.HydrateWithHash(ctx, cfg.TemplateID, cfg.TemplateHash)
	}

	// Just have template ID, do full hydration
	if cfg.TemplateID != "" {
		return hydrator.Hydrate(ctx, cfg.TemplateID)
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
	softDelete := query.Get("softDelete") == "true"

	// Get the agent's grove path before stopping (needed for file deletion)
	var grovePath string
	agents, err := s.manager.List(ctx, map[string]string{"scion.agent": "true"})
	if err == nil {
		for _, a := range agents {
			if a.Name == id || a.ContainerID == id || a.Slug == id {
				grovePath = a.GrovePath
				break
			}
		}
	}

	// If this is a soft-delete, mark agent-info.json with deleted status before cleanup
	if softDelete && grovePath != "" {
		deletedAtStr := query.Get("deletedAt")
		if err := agent.UpdateAgentConfig(id, grovePath, "deleted", "", ""); err != nil {
			slog.Warn("Failed to mark agent as deleted in agent-info.json", "agent", id, "error", err)
		}
		if deletedAtStr != "" {
			if deletedAt, err := time.Parse(time.RFC3339, deletedAtStr); err == nil {
				if err := agent.UpdateAgentDeletedAt(id, grovePath, deletedAt); err != nil {
					slog.Warn("Failed to write deletedAt to agent-info.json", "agent", id, "error", err)
				}
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
	case "finalize-env":
		s.finalizeEnv(w, r, id)
	default:
		NotFound(w, "Action")
	}
}

func (s *Server) startAgent(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	// Read optional task, grovePath, and groveSlug from request body
	var startReq struct {
		Task      string `json:"task"`
		GrovePath string `json:"grovePath"`
		GroveSlug string `json:"groveSlug"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&startReq); err != nil {
			slog.Debug("No task in start request body (ignoring decode error)", "error", err)
		}
	}

	slog.Debug("startAgent called", "id", id, "task", startReq.Task, "grovePath", startReq.GrovePath, "groveSlug", startReq.GroveSlug)

	// For hub-native groves (GroveSlug set, no local provider path), resolve
	// the conventional grove path (~/.scion/groves/<slug>/) so the agent is
	// started in the correct location instead of the broker's local grove.
	if startReq.GroveSlug != "" && startReq.GrovePath == "" {
		globalDir, err := config.GetGlobalDir()
		if err != nil {
			RuntimeError(w, "Failed to get global dir: "+err.Error())
			return
		}
		startReq.GrovePath = filepath.Join(globalDir, "groves", startReq.GroveSlug)
		if s.config.Debug {
			slog.Debug("Resolved hub-native grove path from slug in startAgent",
				"slug", startReq.GroveSlug,
				"path", startReq.GrovePath,
			)
		}
	}

	// Build start options
	opts := api.StartOptions{
		Name: id,
		Task: startReq.Task,
	}

	// Use grove path from request if provided
	if startReq.GrovePath != "" {
		opts.GrovePath = startReq.GrovePath
	} else {
		// Fall back to looking up grove path from an existing container
		agents, err := s.manager.List(ctx, map[string]string{"scion.agent": "true"})
		if err != nil {
			RuntimeError(w, "Failed to list agents: "+err.Error())
			return
		}

		for i := range agents {
			if agents[i].Name == id || agents[i].ContainerID == id || agents[i].Slug == id {
				if agents[i].GrovePath != "" {
					opts.GrovePath = agents[i].GrovePath
				}
				break
			}
		}
	}

	// Resolve saved profile for runtime selection
	if opts.GrovePath != "" {
		opts.Profile = agent.GetSavedProfile(id, opts.GrovePath)
	}

	mgr := s.resolveManagerForOpts(opts)
	agentInfo, err := mgr.Start(ctx, opts)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			NotFound(w, "Agent")
			return
		}
		RuntimeError(w, "Failed to start agent: "+err.Error())
		return
	}

	agentResp := AgentInfoToResponse(*agentInfo)
	writeJSON(w, http.StatusAccepted, CreateAgentResponse{
		Agent:   &agentResp,
		Created: false,
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
	// In multi-hub mode, force heartbeat on all connections.
	s.hubMu.RLock()
	for _, conn := range s.hubConnections {
		if conn.Heartbeat != nil {
			hb := conn.Heartbeat
			go func() {
				if err := hb.ForceHeartbeat(context.Background()); err != nil {
					slog.Error("Failed to send forced heartbeat after stop", "agent", id, "error", err)
				}
			}()
		}
	}
	s.hubMu.RUnlock()

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

// extractRequiredEnvKeys determines the set of env keys required by the agent's
// harness and settings profile. It uses a two-phase approach:
//
// Phase 1 (harness-aware): Resolves the active harness-config name, loads the
// on-disk harness-config directory to determine the harness type and auth method,
// then calls the harness's RequiredEnvKeys() method to get intrinsic requirements.
//
// Phase 2 (settings-based): Extracts keys with empty values from settings
// harness_configs[*].env and profiles[*].env, allowing users to declare custom
// env requirements.
func (s *Server) extractRequiredEnvKeys(req CreateAgentRequest) ([]string, map[string]api.SecretKeyInfo) {
	required := make(map[string]struct{})

	var settings *config.VersionedSettings
	if req.GrovePath != "" {
		vs, _, err := config.LoadEffectiveSettings(req.GrovePath)
		if err == nil {
			settings = vs
		}
	}

	profileName := ""
	if req.Config != nil {
		profileName = req.Config.Profile
	}
	if profileName == "" && settings != nil {
		profileName = settings.ActiveProfile
	}

	// Phase 1: Harness-aware env key extraction
	harnessConfigName := s.resolveHarnessConfigName(req, settings)
	if harnessConfigName != "" {
		harnessName, authType := s.resolveHarnessIdentity(harnessConfigName, req.GrovePath, settings, req.Config)
		if harnessName != "" {
			h := harness.New(harnessName)
			for _, key := range h.RequiredEnvKeys(authType) {
				required[key] = struct{}{}
			}
		}
	}

	// Phase 2: Settings-based empty-value env key extraction (preserved)
	if settings != nil {
		// Get profile env keys
		if profileName != "" && settings.Profiles != nil {
			if profile, ok := settings.Profiles[profileName]; ok {
				for k, v := range profile.Env {
					if v == "" {
						required[k] = struct{}{}
					}
				}
				// Check harness overrides within the profile
				for _, override := range profile.HarnessOverrides {
					for k, v := range override.Env {
						if v == "" {
							required[k] = struct{}{}
						}
					}
				}
			}
		}

		// Get harness config env keys
		for _, hcfg := range settings.HarnessConfigs {
			for k, v := range hcfg.Env {
				if v == "" {
					required[k] = struct{}{}
				}
			}
		}
	}

	// Phase 3: Secrets declarations from settings and template
	secretInfo := make(map[string]api.SecretKeyInfo)

	// 3a: Harness RequiredEnvKeys are all secret-eligible (no description available)
	for k := range required {
		secretInfo[k] = api.SecretKeyInfo{Source: "harness"}
	}

	// 3b: Settings harness_configs[*].secrets
	if settings != nil {
		for _, hcfg := range settings.HarnessConfigs {
			for _, sec := range hcfg.Secrets {
				required[sec.Key] = struct{}{}
				secretInfo[sec.Key] = api.SecretKeyInfo{
					Description: sec.Description,
					Source:      "settings",
					Type:        sec.Type,
				}
			}
		}

		// 3c: Profile secrets
		if profileName != "" && settings.Profiles != nil {
			if profile, ok := settings.Profiles[profileName]; ok {
				for _, sec := range profile.Secrets {
					required[sec.Key] = struct{}{}
					secretInfo[sec.Key] = api.SecretKeyInfo{
						Description: sec.Description,
						Source:      "settings",
						Type:        sec.Type,
					}
				}
			}
		}
	}

	// 3d: Template secrets (from request or local template)
	for _, sec := range req.RequiredSecrets {
		required[sec.Key] = struct{}{}
		secretInfo[sec.Key] = api.SecretKeyInfo{
			Description: sec.Description,
			Source:      "template",
			Type:        sec.Type,
		}
	}
	// Also try loading local template config
	if req.Config != nil && req.Config.Template != "" && req.GrovePath != "" {
		if tmpl, err := config.FindTemplateInGrovePath(req.Config.Template, req.GrovePath); err == nil {
			if cfg, err := tmpl.LoadConfig(); err == nil && cfg != nil {
				for _, sec := range cfg.Secrets {
					required[sec.Key] = struct{}{}
					if _, exists := secretInfo[sec.Key]; !exists {
						secretInfo[sec.Key] = api.SecretKeyInfo{
							Description: sec.Description,
							Source:      "template",
							Type:        sec.Type,
						}
					}
				}
			}
		}
	}

	keys := make([]string, 0, len(required))
	for k := range required {
		keys = append(keys, k)
	}
	return keys, secretInfo
}

// resolveHarnessConfigName determines which harness-config to use for the agent.
// Resolution chain:
//  1. req.Config.Harness (explicit harness override)
//  2. req.Config.Template (if it matches a valid harness-config directory)
//  3. profile's DefaultHarnessConfig
//  4. settings' DefaultHarnessConfig
func (s *Server) resolveHarnessConfigName(req CreateAgentRequest, settings *config.VersionedSettings) string {
	// 1. Explicit harness in config
	if req.Config != nil && req.Config.Harness != "" {
		return req.Config.Harness
	}

	// 2. Template name that matches an on-disk harness-config directory
	if req.Config != nil && req.Config.Template != "" {
		if req.GrovePath != "" {
			if _, err := config.FindHarnessConfigDir(req.Config.Template, req.GrovePath); err == nil {
				return req.Config.Template
			}
		}

		// 3. Template name that matches a settings harness_configs entry
		if settings != nil {
			if _, ok := settings.HarnessConfigs[req.Config.Template]; ok {
				return req.Config.Template
			}
		}
	}

	if settings == nil {
		return ""
	}

	// Resolve profile name
	profileName := ""
	if req.Config != nil {
		profileName = req.Config.Profile
	}
	if profileName == "" {
		profileName = settings.ActiveProfile
	}

	// 4. Profile's DefaultHarnessConfig
	if profileName != "" {
		if profile, ok := settings.Profiles[profileName]; ok {
			if profile.DefaultHarnessConfig != "" {
				return profile.DefaultHarnessConfig
			}
		}
	}

	// 5. Settings' DefaultHarnessConfig
	if settings.DefaultHarnessConfig != "" {
		return settings.DefaultHarnessConfig
	}

	return ""
}

// resolveHarnessIdentity loads the on-disk harness-config directory and applies
// settings overrides to determine the harness name and auth_selected_type.
func (s *Server) resolveHarnessIdentity(name, grovePath string, settings *config.VersionedSettings, reqConfig *CreateAgentConfig) (harnessName, authSelectedType string) {
	// Try loading from on-disk harness-config directory
	if grovePath != "" {
		if hcDir, err := config.FindHarnessConfigDir(name, grovePath); err == nil {
			harnessName = hcDir.Config.Harness
			authSelectedType = hcDir.Config.AuthSelectedType
		}
	}

	// If no on-disk config found, check if the name itself is a known harness
	if harnessName == "" {
		// Check if the name matches a settings harness-config entry
		if settings != nil {
			if hcEntry, ok := settings.HarnessConfigs[name]; ok {
				harnessName = hcEntry.Harness
				authSelectedType = hcEntry.AuthSelectedType
			}
		}
		// Fall back to treating the name as a harness name directly
		if harnessName == "" {
			harnessName = name
		}
	}

	// Apply settings-level overrides via ResolveHarnessConfig
	if settings != nil {
		profileName := ""
		if reqConfig != nil {
			profileName = reqConfig.Profile
		}
		resolved, err := settings.ResolveHarnessConfig(profileName, name)
		if err == nil && resolved.AuthSelectedType != "" {
			authSelectedType = resolved.AuthSelectedType
		}
	}

	return harnessName, authSelectedType
}

// finalizeEnv handles the second phase of env-gather: receiving gathered env vars
// from the Hub and starting the agent with the complete environment.
func (s *Server) finalizeEnv(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	var req FinalizeEnvRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Look up pending state
	s.pendingEnvGatherMu.Lock()
	pending, ok := s.pendingEnvGather[id]
	if ok {
		delete(s.pendingEnvGather, id)
	}
	s.pendingEnvGatherMu.Unlock()

	if !ok {
		NotFound(w, "Pending agent")
		return
	}

	// Merge gathered env into the previously merged env
	for k, v := range req.Env {
		pending.MergedEnv[k] = v
	}

	if s.config.Debug {
		slog.Debug("Finalize-env: merging gathered env", "gatheredKeys", len(req.Env), "totalEnv", len(pending.MergedEnv))
	}

	// Build start options from the pending request
	origReq := pending.Request
	opts := api.StartOptions{
		Name:      origReq.Name,
		Detached:  boolPtr(!origReq.Attach),
		GrovePath: origReq.GrovePath,
	}

	if origReq.Config != nil {
		opts.Template = origReq.Config.Template
		opts.Image = origReq.Config.Image
		opts.Task = origReq.Config.Task
		opts.Workspace = origReq.Config.Workspace
		opts.Profile = origReq.Config.Profile
	}

	if s.config.Debug {
		slog.Debug("Finalize-env: StartOptions built from pending request",
			"name", opts.Name,
			"grovePath", opts.GrovePath,
			"template", opts.Template,
			"image", opts.Image,
			"profile", opts.Profile,
			"harnessConfig", opts.HarnessConfig,
			"hasConfig", origReq.Config != nil,
		)
	}

	// Save template slug before hydration may replace opts.Template with a cache path
	templateSlug := ""
	if origReq.Config != nil {
		templateSlug = origReq.Config.Template
	}

	// Hydrate template if needed
	hydrator := s.resolveHydrator(r)
	if hydrator != nil && origReq.Config != nil {
		templatePath, err := s.hydrateTemplate(ctx, origReq.Config, hydrator)
		if err != nil {
			TemplateError(w, "Failed to hydrate template: "+err.Error())
			return
		}
		if templatePath != "" {
			opts.Template = templatePath
		}
	}

	// Preserve human-friendly template slug for container labels
	if templateSlug != "" {
		opts.TemplateName = templateSlug
	}

	// Git clone mode
	if origReq.Config != nil && origReq.Config.GitClone != nil {
		gc := origReq.Config.GitClone
		pending.MergedEnv["SCION_GIT_CLONE_URL"] = gc.URL
		if gc.Branch != "" {
			pending.MergedEnv["SCION_GIT_BRANCH"] = gc.Branch
		}
		if gc.Depth > 0 {
			pending.MergedEnv["SCION_GIT_DEPTH"] = strconv.Itoa(gc.Depth)
		}
		opts.Workspace = ""
		opts.GrovePath = ""
		opts.GitClone = gc
	}

	opts.Env = pending.MergedEnv

	// Pass through resolved secrets
	if len(origReq.ResolvedSecrets) > 0 {
		opts.ResolvedSecrets = origReq.ResolvedSecrets
	}

	// Start the agent
	mgr := s.resolveManagerForOpts(opts)
	agentInfo, err := mgr.Start(ctx, opts)
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

// resolveManagerForOpts returns the appropriate agent.Manager for the given
// start options. If opts.Profile resolves to a different runtime than the
// broker's default, a temporary manager is created with the profile-specific
// runtime. Otherwise the broker's shared manager is returned.
func (s *Server) resolveManagerForOpts(opts api.StartOptions) agent.Manager {
	if opts.Profile == "" {
		return s.manager
	}

	// Load settings to check if the profile explicitly specifies a different runtime.
	// If no settings exist or the profile isn't defined, stick with the default manager.
	projectDir, _ := config.GetResolvedProjectDir(opts.GrovePath)
	vs, _, _ := config.LoadEffectiveSettings(projectDir)
	if vs == nil {
		return s.manager
	}

	_, runtimeType, err := vs.ResolveRuntime(opts.Profile)
	if err != nil {
		// Profile or its runtime not found in settings; use default
		return s.manager
	}

	if runtimeType == s.runtime.Name() {
		return s.manager
	}

	// Profile specifies a different runtime - resolve and create a temporary manager
	resolved := agent.ResolveRuntime(opts.GrovePath, opts.Name, opts.Profile)

	if s.config.Debug {
		slog.Debug("Profile resolved to different runtime",
			"profile", opts.Profile,
			"defaultRuntime", s.runtime.Name(),
			"resolvedRuntime", resolved.Name(),
		)
	}

	return agent.NewManager(resolved)
}

// Helper functions

func boolPtr(b bool) *bool {
	return &b
}

func agentInfoPtr(a AgentResponse) *AgentResponse {
	return &a
}
